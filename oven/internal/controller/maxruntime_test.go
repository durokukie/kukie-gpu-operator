/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// fakeClock is real time plus an offset the tests can push forward. Moving it wakes nothing: the tests touch the object
// to get a reconcile, except where the point is that the reconcile comes by itself.
type fakeClock struct {
	mu     sync.Mutex
	offset time.Duration
}

var testClock = &fakeClock{}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func (c *fakeClock) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = 0
}

// unstructuredServedModel sends spec.maxRuntime as the literal string, which metav1.Duration could not carry: "" and
// "1d" do not even parse on our side, and the API server's answer is what is being tested.
type unstructuredServedModel struct {
	*servingv1alpha1.ServedModel
	MaxRuntime string
}

func (u *unstructuredServedModel) create(ctx context.Context) error {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(u.ServedModel)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{Object: content}
	obj.SetGroupVersionKind(servingv1alpha1.GroupVersion.WithKind("ServedModel"))
	if err := unstructured.SetNestedField(obj.Object, u.MaxRuntime, "spec", "maxRuntime"); err != nil {
		return err
	}
	return k8sClient.Create(ctx, obj)
}

func TestStopTime(t *testing.T) {
	startedAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	hour := &metav1.Duration{Duration: time.Hour}
	cases := []struct {
		name      string
		spec      *metav1.Duration
		flag      time.Duration
		keepUntil string
		want      time.Time
		never     bool
		badKeep   bool
	}{
		{name: "spec wins over the flag", spec: hour, flag: 2 * time.Hour, want: startedAt.Add(time.Hour)},
		{name: "empty spec uses the flag", flag: 2 * time.Hour, want: startedAt.Add(2 * time.Hour)},
		{name: "spec 0 turns a flag off", spec: &metav1.Duration{}, flag: 2 * time.Hour, never: true},
		{name: "flag 0 with an empty spec", never: true},
		{name: "spec gives a limit when the flag is off", spec: hour, want: startedAt.Add(time.Hour)},
		{name: "keep-until extends", spec: hour, keepUntil: "2026-11-04T15:00:00Z", want: startedAt.Add(3 * time.Hour)},
		{name: "keep-until with an offset", spec: hour, keepUntil: "2026-11-05T00:00:00+09:00", want: startedAt.Add(3 * time.Hour)},
		{name: "keep-until never shortens", spec: hour, keepUntil: "2026-11-04T12:30:00Z", want: startedAt.Add(time.Hour)},
		{name: "keep-until does nothing without a limit", spec: &metav1.Duration{}, keepUntil: "2026-11-04T15:00:00Z", never: true},
		{name: "keep-until that does not parse is ignored", spec: hour, keepUntil: "tomorrow", want: startedAt.Add(time.Hour), badKeep: true},
		{name: "keep-until without a zone is ignored", spec: hour, keepUntil: "2026-11-04T15:00:00", want: startedAt.Add(time.Hour), badKeep: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := &servingv1alpha1.ServedModel{Spec: servingv1alpha1.ServedModelSpec{MaxRuntime: tc.spec}}
			if tc.keepUntil != "" {
				sm.Annotations = map[string]string{servingv1alpha1.KeepUntilAnnotation: tc.keepUntil}
			}
			got, limited, err := Config{MaxRuntime: tc.flag}.stopTime(sm, startedAt)
			if limited == tc.never {
				t.Fatalf("limited=%v, want never=%v", limited, tc.never)
			}
			if !tc.never && !got.Equal(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if (err != nil) != tc.badKeep {
				t.Fatalf("got error %v, want one: %v", err, tc.badKeep)
			}
		})
	}
}

func TestSooner(t *testing.T) {
	for _, tc := range []struct{ a, b, want time.Duration }{
		{0, time.Hour, time.Hour},
		{30 * time.Second, time.Hour, 30 * time.Second},
		{time.Hour, 30 * time.Second, 30 * time.Second},
	} {
		if got := sooner(tc.a, tc.b); got != tc.want {
			t.Fatalf("sooner(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

var _ = Describe("ServedModel run time limit", func() {
	ctx := context.Background()

	var ns string

	BeforeEach(func() {
		ns = nextWatchedNamespace()
		DeferCleanup(testClock.reset)
	})

	withLimit := func(sm *servingv1alpha1.ServedModel, limit string) *servingv1alpha1.ServedModel {
		if limit != "" {
			d, err := time.ParseDuration(limit)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			sm.Spec.MaxRuntime = &metav1.Duration{Duration: d}
		}
		return sm
	}
	update := func(key types.NamespacedName, change func(*servingv1alpha1.ServedModel)) {
		EventuallyWithOffset(1, func() error {
			got := &servingv1alpha1.ServedModel{}
			if err := k8sClient.Get(ctx, key, got); err != nil {
				return err
			}
			if got.Annotations == nil {
				got.Annotations = map[string]string{}
			}
			change(got)
			return k8sClient.Update(ctx, got)
		}, testTimeout, testPoll).Should(Succeed())
	}
	// touch wakes the reconcile.
	touch := func(key types.NamespacedName) {
		update(key, func(sm *servingv1alpha1.ServedModel) { sm.Annotations["touch"] = time.Now().String() })
	}
	reasonOf := func(key types.NamespacedName) func() string {
		return func() string { return getServedModel(ctx, key).Status.Reason }
	}
	replicasOf := func(key types.NamespacedName) func() int32 {
		return func() int32 {
			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, key, deployment); err != nil || deployment.Spec.Replicas == nil {
				return -1
			}
			return *deployment.Spec.Replicas
		}
	}
	downloading := func(name, limit string) (*servingv1alpha1.ServedModel, types.NamespacedName, *batchv1.Job) {
		sm := withLimit(newServedModel(ns, name, false), limit)
		ExpectWithOffset(1, k8sClient.Create(ctx, sm)).To(Succeed())
		var job *batchv1.Job
		EventuallyWithOffset(1, func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		return sm, client.ObjectKeyFromObject(sm), job
	}
	ready := func(name, limit string) (*servingv1alpha1.ServedModel, types.NamespacedName) {
		sm, key, job := downloading(name, limit)
		markJobComplete(ctx, job)
		pod := createPod(ctx, sm, componentEngine, name+"-engine")
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		EventuallyWithOffset(1, phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		return getServedModel(ctx, key), key
	}
	expectStopped := func(key types.NamespacedName) {
		EventuallyWithOffset(1, phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseStopped))
		got := getServedModel(ctx, key)
		ExpectWithOffset(1, got.Status.Reason).To(Equal("MaxRuntimeReached"))
		ExpectWithOffset(1, got.Status.Message).To(ContainSubstring("다시 만든다"))
		cond := meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionReady)
		ExpectWithOffset(1, cond).NotTo(BeNil())
		ExpectWithOffset(1, cond.Status).To(Equal(metav1.ConditionFalse))
		ExpectWithOffset(1, cond.Reason).To(Equal("MaxRuntimeReached"))
	}
	// direct is a reconciler of its own with a clock of its own, called by hand: what a freshly restarted oven would do.
	direct := func(now time.Time) *ServedModelReconciler {
		return &ServedModelReconciler{
			Client: k8sClient,
			Scheme: scheme.Scheme,
			Config: testConfig,
			Now:    func() time.Time { return now },
		}
	}

	It("runs until the limit, then stops the engine and keeps the PVC", func() {
		_, key := ready("limit", "1h")
		got := getServedModel(ctx, key)
		Expect(got.Status.StartedAt).NotTo(BeNil())
		Expect(got.Status.StopsAt).NotTo(BeNil())
		Expect(got.Status.StopsAt.Time).To(BeTemporally("==", got.Status.StartedAt.Add(time.Hour)))

		By("leaving it alone before the limit")
		testClock.advance(59 * time.Minute)
		touch(key)
		Consistently(phaseOf(ctx, key), 2*time.Second, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		Expect(replicasOf(key)()).To(Equal(int32(1)))

		By("stopping it past the limit. envtest has no ReplicaSet controller: 0 replicas is what takes the Pod away")
		testClock.advance(2 * time.Minute)
		touch(key)
		expectStopped(key)
		Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(0)))

		By("keeping the PVC and the finished download Job")
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "limit-models"}, pvc)).To(Succeed())
		Expect(pvc.DeletionTimestamp).To(BeNil())
		Expect(findDownloadJob(ctx, got)).NotTo(BeNil())
		Expect(getServedModel(ctx, key).Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("uses the manager's limit when the spec has none", func() {
		_, key := ready("default", "")
		got := getServedModel(ctx, key)
		Expect(got.Status.StopsAt.Time).To(BeTemporally("==", got.Status.StartedAt.Add(DefaultMaxRuntime)))
		testClock.advance(DefaultMaxRuntime + time.Minute)
		touch(key)
		expectStopped(key)
	})

	It("never stops a model with maxRuntime 0", func() {
		sm := newServedModel(ns, "forever", false)
		sm.Spec.MaxRuntime = &metav1.Duration{}
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		var job *batchv1.Job
		Eventually(func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		markJobComplete(ctx, job)
		Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(1)))
		got := getServedModel(ctx, key)
		Expect(got.Status.StartedAt).NotTo(BeNil())
		Expect(got.Status.StopsAt).To(BeNil())

		testClock.advance(10000 * time.Hour)
		touch(key)
		Consistently(phaseOf(ctx, key), 2*time.Second, testPoll).ShouldNot(Equal(servingv1alpha1.PhaseStopped))
		Expect(replicasOf(key)()).To(Equal(int32(1)))
	})

	It("waits for keep-until, and only ever later", func() {
		_, key := ready("keep", "1h")
		startedAt := getServedModel(ctx, key).Status.StartedAt.Time

		By("moving stopsAt to keep-until")
		update(key, func(sm *servingv1alpha1.ServedModel) {
			sm.Annotations[servingv1alpha1.KeepUntilAnnotation] = startedAt.Add(3 * time.Hour).Format(time.RFC3339)
		})
		Eventually(func() time.Time { return getServedModel(ctx, key).Status.StopsAt.Time }, testTimeout, testPoll).
			Should(BeTemporally("==", startedAt.Add(3*time.Hour)))

		By("not stopping past maxRuntime while keep-until is ahead")
		testClock.advance(2 * time.Hour)
		touch(key)
		Consistently(phaseOf(ctx, key), 2*time.Second, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		Expect(replicasOf(key)()).To(Equal(int32(1)))

		By("stopping once keep-until has passed")
		testClock.advance(90 * time.Minute)
		touch(key)
		expectStopped(key)
	})

	It("ignores a keep-until that does not parse", func() {
		_, key := ready("badkeep", "1h")
		startedAt := getServedModel(ctx, key).Status.StartedAt.Time
		update(key, func(sm *servingv1alpha1.ServedModel) {
			sm.Annotations[servingv1alpha1.KeepUntilAnnotation] = "내일까지"
		})
		Consistently(func() time.Time { return getServedModel(ctx, key).Status.StopsAt.Time }, time.Second, testPoll).
			Should(BeTemporally("==", startedAt.Add(time.Hour)))
		Expect(phaseOf(ctx, key)()).To(Equal(servingv1alpha1.PhaseReady))
		testClock.advance(time.Hour + time.Minute)
		touch(key)
		expectStopped(key)
	})

	It("stays down after stopping, whatever changes", func() {
		sm, key := ready("final", "1h")
		testClock.advance(time.Hour + time.Minute)
		touch(key)
		expectStopped(key)
		Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(0)))
		stoppedMessage := getServedModel(ctx, key).Status.Message

		By("not coming back for keep-until, a higher limit or a new source")
		update(key, func(got *servingv1alpha1.ServedModel) {
			got.Annotations[servingv1alpha1.KeepUntilAnnotation] = testClock.Now().Add(24 * time.Hour).Format(time.RFC3339)
			got.Spec.MaxRuntime = &metav1.Duration{}
			got.Spec.Source.HuggingFace = "Qwen/Qwen3-8B"
			got.Annotations[servingv1alpha1.MeasureAnnotation] = "true"
		})
		newest := getServedModel(ctx, key)
		Eventually(func() int64 { return getServedModel(ctx, key).Status.ObservedGeneration }, testTimeout, testPoll).
			Should(Equal(newest.Generation))
		Consistently(func() bool {
			got := getServedModel(ctx, key)
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns), client.MatchingLabels{servingv1alpha1.ServedModelLabel: sm.Name})).To(Succeed())
			return got.Status.Phase == servingv1alpha1.PhaseStopped && got.Status.Message == stoppedMessage &&
				replicasOf(key)() == 0 && len(jobs.Items) == 1
		}, 2*time.Second, testPoll).Should(BeTrue())

		By("scaling a replica someone brought back to zero again")
		deployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key, deployment)).To(Succeed())
		deployment.Spec.Replicas = new(int32(1))
		Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
		Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(0)))

		By("still following retainOnDelete, which decides whether the files survive the delete that comes next")
		pvc := &corev1.PersistentVolumeClaim{}
		pvcKey := types.NamespacedName{Namespace: ns, Name: "final-models"}
		Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
		Expect(pvc.OwnerReferences).To(HaveLen(1))
		update(key, func(got *servingv1alpha1.ServedModel) { got.Spec.Storage.RetainOnDelete = true })
		Eventually(func() []metav1.OwnerReference {
			Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
			return pvc.OwnerReferences
		}, testTimeout, testPoll).Should(BeEmpty())
	})

	It("starts again from zero when deleted and created again", func() {
		sm := withLimit(newServedModel(ns, "again", true), "1h")
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		Eventually(func() *batchv1.Job { return findDownloadJob(ctx, sm) }, testTimeout, testPoll).ShouldNot(BeNil())
		testClock.advance(time.Hour + time.Minute)
		touch(key)
		expectStopped(key)

		// envtest has no garbage collector: the Jobs the old ServedModel owned are removed by hand.
		Expect(k8sClient.Delete(ctx, getServedModel(ctx, key))).To(Succeed())
		Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, key, &servingv1alpha1.ServedModel{})) }, testTimeout, testPoll).Should(BeTrue())
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
		for i := range jobs.Items {
			job := &jobs.Items[i]
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))).To(Succeed())
			Eventually(func() error {
				current := &batchv1.Job{}
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(job), current); err != nil {
					return client.IgnoreNotFound(err)
				}
				current.Finalizers = nil
				return k8sClient.Update(ctx, current)
			}, testTimeout, testPoll).Should(Succeed())
		}

		fresh := withLimit(newServedModel(ns, "again", true), "1h")
		Expect(k8sClient.Create(ctx, fresh)).To(Succeed())
		Eventually(func() *batchv1.Job { return findDownloadJob(ctx, fresh) }, testTimeout, testPoll).ShouldNot(BeNil())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))
		got := getServedModel(ctx, key)
		Expect(got.Status.StopsAt.Time).To(BeTemporally(">", testClock.Now().Add(59*time.Minute)))
	})

	It("stops a download in progress and does not go on to vLLM", func() {
		sm, key, job := downloading("midway", "1h")
		dlPod := createPod(ctx, sm, componentDownload, job.Name+"-x1")
		dlPod.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, dlPod)).To(Succeed())

		testClock.advance(time.Hour + time.Minute)
		touch(key)
		expectStopped(key)
		Eventually(func() bool {
			current := &batchv1.Job{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(job), current)
			return apierrors.IsNotFound(err) || current.DeletionTimestamp != nil
		}, testTimeout, testPoll).Should(BeTrue(), "the running download Job is deleted")
		Consistently(func() error { return k8sClient.Get(ctx, key, &appsv1.Deployment{}) }, 2*time.Second, testPoll).ShouldNot(Succeed())
		Expect(findDownloadJob(ctx, sm)).To(BeNil(), "no new download Job")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "midway-models"}, &corev1.PersistentVolumeClaim{})).To(Succeed())

		cond := meta.FindStatusCondition(getServedModel(ctx, key).Status.Conditions, servingv1alpha1.ConditionDownloaded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("MaxRuntimeReached"))
	})

	It("stops a failed model too, and keeps what it last said", func() {
		sm, key, job := downloading("crashing", "1h")
		markJobComplete(ctx, job)
		pod := createPod(ctx, sm, componentEngine, "crashing-engine")
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "vllm",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))

		testClock.advance(time.Hour + time.Minute)
		touch(key)
		expectStopped(key)
		Expect(getServedModel(ctx, key).Status.Message).To(ContainSubstring("직전 상태 Unclassified: 원인을 분류하지 못했다. 쿠버네티스 상태: CrashLoopBackOff."))
		Expect(getServedModel(ctx, key).Status.Hint).To(BeEmpty())
		Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(0)))
	})

	It("leaves a Deployment that is not its own alone when it stops", func() {
		labels := map[string]string{"app": "by-hand"}
		theirs := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: new(int32(1)),
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: testImage}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, theirs)).To(Succeed())
		sm := withLimit(newServedModel(ns, "taken", false), "1h")
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("NameConflict"))

		testClock.advance(time.Hour + time.Minute)
		touch(key)
		expectStopped(key)
		Expect(getServedModel(ctx, key).Status.Message).To(ContainSubstring("직전 상태 NameConflict"))
		Consistently(replicasOf(key), time.Second, testPoll).Should(Equal(int32(1)))
		Expect(k8sClient.Get(ctx, key, theirs)).To(Succeed())
		Expect(theirs.OwnerReferences).To(BeEmpty())
	})

	Context("with a measurement", func() {
		request := func(sm *servingv1alpha1.ServedModel, key types.NamespacedName) *batchv1.Job {
			update(key, func(got *servingv1alpha1.ServedModel) { got.Annotations[servingv1alpha1.MeasureAnnotation] = "true" })
			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Namespace: ns, Name: measureJobName(sm)}
			EventuallyWithOffset(1, func() error { return k8sClient.Get(ctx, jobKey, job) }, testTimeout, testPoll).Should(Succeed())
			EventuallyWithOffset(1, phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseMeasuring))
			return job
		}

		It("lets a running one finish, keeps the result, then stops", func() {
			sm, key := ready("running", "1h")
			job := request(sm, key)

			By("keeping the engine up past the limit while the Job runs")
			testClock.advance(time.Hour + time.Minute)
			touch(key)
			Consistently(phaseOf(ctx, key), 2*time.Second, testPoll).Should(Equal(servingv1alpha1.PhaseMeasuring))
			Expect(replicasOf(key)()).To(Equal(int32(1)))

			By("recording the result and stopping as soon as the Job ends")
			pod := createMeasurePod(ctx, sm, job.Name)
			finishMeasure(ctx, pod, 0, `{"ok":true,"reason":"Measured","message":"동시 8까지","maxConcurrency":8,"maxWaiting":1,"maxKvPercent":12}`)
			markJobComplete(ctx, job)
			expectStopped(key)
			Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(0)))
			got := getServedModel(ctx, key)
			Expect(got.Status.Baseline).NotTo(BeNil())
			Expect(got.Status.Baseline.MaxConcurrency).To(Equal(int32(8)))
			measured := meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionMeasured)
			Expect(measured.Reason).To(Equal("Measured"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: baselineConfigMapName(sm)}, &corev1.ConfigMap{})).To(Succeed())
			Expect(measureResults(ctx, ns, "running")).To(HaveLen(1))
			Expect(got.Status.MeasureRequest).To(Equal("true"))
		})

		It("stops even when the measurement fails", func() {
			sm, key := ready("failing", "1h")
			job := request(sm, key)
			testClock.advance(time.Hour + time.Minute)
			createMeasurePod(ctx, sm, job.Name)
			markJobFailed(ctx, job)
			expectStopped(key)
			measured := meta.FindStatusCondition(getServedModel(ctx, key).Status.Conditions, servingv1alpha1.ConditionMeasured)
			Expect(measured.Reason).To(Equal("MeasureFailed"))
		})

		It("stops at once when the request is withdrawn past the limit, and removes the Job", func() {
			sm, key := ready("withdrawn", "1h")
			job := request(sm, key)
			testClock.advance(time.Hour + time.Minute)
			touch(key)
			Consistently(phaseOf(ctx, key), time.Second, testPoll).Should(Equal(servingv1alpha1.PhaseMeasuring))
			update(key, func(got *servingv1alpha1.ServedModel) { delete(got.Annotations, servingv1alpha1.MeasureAnnotation) })
			expectStopped(key)
			Eventually(func() bool {
				current := &batchv1.Job{}
				err := k8sClient.Get(ctx, client.ObjectKeyFromObject(job), current)
				return apierrors.IsNotFound(err) || current.DeletionTimestamp != nil
			}, testTimeout, testPoll).Should(BeTrue())
		})

		It("does not start one past the limit", func() {
			sm, key := ready("late", "1h")
			testClock.advance(time.Hour + time.Minute)
			update(key, func(got *servingv1alpha1.ServedModel) { got.Annotations[servingv1alpha1.MeasureAnnotation] = "true" })
			expectStopped(key)
			Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureJobName(sm)}, &batchv1.Job{}))
			}, time.Second, testPoll).Should(BeTrue())
		})

		It("does not start a new value's run past the limit, and stops with the older Job finished", func() {
			sm, key := ready("newvalue", "1h")
			job := request(sm, key)
			pod := createMeasurePod(ctx, sm, job.Name)
			finishMeasure(ctx, pod, 0, `{"ok":true,"reason":"Measured","message":"x","maxConcurrency":4,"maxWaiting":0,"maxKvPercent":10}`)
			markJobComplete(ctx, job)
			Eventually(func() *servingv1alpha1.BaselineStatus { return getServedModel(ctx, key).Status.Baseline }, testTimeout, testPoll).ShouldNot(BeNil())
			testClock.advance(time.Hour + time.Minute)
			update(key, func(got *servingv1alpha1.ServedModel) { got.Annotations[servingv1alpha1.MeasureAnnotation] = "m-2" })
			expectStopped(key)
			Consistently(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureJobNameFor(sm, "m-2")}, &batchv1.Job{}))
			}, time.Second, testPoll).Should(BeTrue())
		})

		It("stops at the limit with the finished Job still there and the annotation on", func() {
			sm, key := ready("finished", "1h")
			job := request(sm, key)
			pod := createMeasurePod(ctx, sm, job.Name)
			finishMeasure(ctx, pod, 0, `{"ok":true,"reason":"Measured","message":"x","maxConcurrency":4,"maxWaiting":0,"maxKvPercent":10}`)
			markJobComplete(ctx, job)
			Eventually(func() *servingv1alpha1.BaselineStatus { return getServedModel(ctx, key).Status.Baseline }, testTimeout, testPoll).ShouldNot(BeNil())
			testClock.advance(time.Hour + time.Minute)
			touch(key)
			expectStopped(key)
		})
	})

	Context("across a restart of the oven", func() {
		It("keeps startedAt, and stops at once when the limit passed while it was down", func() {
			_, key := ready("restart", "1h")
			startedAt := getServedModel(ctx, key).Status.StartedAt.Time

			By("not moving startedAt when a new reconciler looks at the model later")
			_, err := direct(startedAt.Add(30*time.Minute)).Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(getServedModel(ctx, key).Status.StartedAt.Time).To(BeTemporally("==", startedAt))
			Expect(phaseOf(ctx, key)()).To(Equal(servingv1alpha1.PhaseReady))

			By("stopping on its first look after the limit")
			result, err := direct(startedAt.Add(time.Hour+time.Second)).Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
			expectStopped(key)
			Expect(getServedModel(ctx, key).Status.StartedAt.Time).To(BeTemporally("==", startedAt))
			Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(0)))
		})
	})

	Context("scheduling the next look", func() {
		It("wakes a Ready model exactly when its time is up", func() {
			_, key := ready("timer", "1h")
			startedAt := getServedModel(ctx, key).Status.StartedAt.Time

			result, err := direct(startedAt.Add(59*time.Minute)).Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("following keep-until")
			update(key, func(sm *servingv1alpha1.ServedModel) {
				sm.Annotations[servingv1alpha1.KeepUntilAnnotation] = startedAt.Add(2 * time.Hour).Format(time.RFC3339)
			})
			result, err = direct(startedAt.Add(59*time.Minute)).Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(61 * time.Minute))
		})

		It("keeps the shorter timer while a model is still changing", func() {
			_, key, _ := downloading("busy", "1h")
			Eventually(func() *metav1.Time { return getServedModel(ctx, key).Status.StartedAt }, testTimeout, testPoll).ShouldNot(BeNil())
			startedAt := getServedModel(ctx, key).Status.StartedAt.Time

			result, err := direct(startedAt.Add(10*time.Minute)).Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(requeueWhileChanging))

			result, err = direct(startedAt.Add(time.Hour-5*time.Second)).Reconcile(ctx, ctrl.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Second))
		})

		It("stops a Ready model on time with nothing else happening to it", func() {
			_, key := ready("alarm", "1h")
			startedAt := getServedModel(ctx, key).Status.StartedAt.Time
			// The limit moves to 3 seconds from now. That update is the last event this model gets.
			left := testClock.Now().Sub(startedAt).Truncate(time.Second) + 3*time.Second
			update(key, func(sm *servingv1alpha1.ServedModel) { sm.Spec.MaxRuntime = &metav1.Duration{Duration: left} })
			Consistently(phaseOf(ctx, key), time.Second, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
			expectStopped(key)
		})
	})

	Context("waiting for a node", func() {
		unschedulable := func(pod *corev1.Pod, message string) {
			pod.Status.Phase = corev1.PodPending
			pod.Status.Conditions = []corev1.PodCondition{{
				Type:               corev1.PodScheduled,
				Status:             corev1.ConditionFalse,
				Reason:             corev1.PodReasonUnschedulable,
				Message:            message,
				LastTransitionTime: metav1.NewTime(testClock.Now()),
			}}
			ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		}
		const message = "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector."

		It("reports the download Pod as Pending/WaitingForNode, then Failed/NoGpuNode after the timeout", func() {
			sm, key, job := downloading("wait-dl", "")
			pod := createPod(ctx, sm, componentDownload, job.Name+"-x1")
			unschedulable(pod, message)
			Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("WaitingForNode"))
			got := getServedModel(ctx, key)
			Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhasePending))
			Expect(got.Status.Message).To(Equal(message))
			cond := meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionDownloaded)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("WaitingForNode"))

			By("not counting as failed just before the timeout")
			testClock.advance(DefaultNodeWaitTimeout - time.Minute)
			touch(key)
			Consistently(phaseOf(ctx, key), time.Second, testPoll).Should(Equal(servingv1alpha1.PhasePending))

			By("not staying hidden in Pending after it")
			testClock.advance(2 * time.Minute)
			touch(key)
			Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))
			got = getServedModel(ctx, key)
			Expect(got.Status.Reason).To(Equal("NoGpuNode"))
			Expect(got.Status.Hint).NotTo(BeEmpty())
			Expect(meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionDownloaded).Message).To(Equal(message))

			By("going on by itself if a node comes after all")
			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))
		})

		It("reports the vLLM Pod the same way and leaves the wait when it runs", func() {
			sm, key, job := downloading("wait-engine", "")
			markJobComplete(ctx, job)
			Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(1)))
			pod := createPod(ctx, sm, componentEngine, "wait-engine-abc")
			unschedulable(pod, "0/1 nodes are available: 1 Insufficient nvidia.com/gpu.")
			Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("WaitingForNode"))
			got := getServedModel(ctx, key)
			Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhasePending))
			Expect(got.Status.Message).To(Equal("0/1 nodes are available: 1 Insufficient nvidia.com/gpu."))
			cond := meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionReady)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("WaitingForNode"))

			pod.Status.Phase = corev1.PodRunning
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
			Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		})

		It("stops a model that is still waiting when its time is up, so no node comes up for nobody", func() {
			sm, key, job := downloading("wait-stop", "1h")
			markJobComplete(ctx, job)
			Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(1)))
			pod := createPod(ctx, sm, componentEngine, "wait-stop-abc")
			unschedulable(pod, message)
			Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("WaitingForNode"))
			testClock.advance(time.Hour + time.Minute)
			touch(key)
			expectStopped(key)
			Eventually(replicasOf(key), testTimeout, testPoll).Should(Equal(int32(0)))
		})
	})

	It("accepts only values Go can parse without overflowing, and what Go writes back for them", func() {
		create := func(name, value string) error {
			sm := newServedModel(ns, name, false)
			raw := &unstructuredServedModel{ServedModel: sm, MaxRuntime: value}
			return raw.create(ctx)
		}
		for i, value := range []string{
			"0", "0s", "2h", "1h30m", "90m", "90s", "1h0m0s", "99999h", "99999h59m59s", "9999m", "9999s", "9999m9999s",
		} {
			name := fmt.Sprintf("good-%d", i)
			Expect(create(name, value)).To(Succeed(), value)
			By("reading " + value + " with the typed client and writing it back")
			update(types.NamespacedName{Namespace: ns, Name: name}, func(sm *servingv1alpha1.ServedModel) {
				sm.Annotations["touch"] = "1"
			})
		}
		for i, value := range []string{
			"", "-1h", "1d", "1.5h", "2 hours", "120", "h", "100000h", "1m1h", "+1h", "1h90m", "1h60s", "10000m",
		} {
			err := create(fmt.Sprintf("bad-%d", i), value)
			Expect(err).To(HaveOccurred(), value)
			Expect(err.Error()).To(ContainSubstring("spec.maxRuntime"), value)
		}
		Expect(k8sClient.List(ctx, &servingv1alpha1.ServedModelList{}, client.InNamespace(ns))).To(Succeed(),
			"everything the API server let in must still decode")
	})
})
