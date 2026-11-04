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
	"regexp"
	"strings"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

const measuredOK = `{"ok":true,"reason":"Measured","message":"동시 4까지","sla":{"ttftMs":1000,"itlMs":50,"inputTokens":1000,"outputTokens":200},` +
	`"steps":[{"c":4,"ttft95":41,"itl95":11.0,"tps":407,"sla":true}],"maxConcurrency":4,"maxWaiting":0,"maxKvPercent":10}`

func TestMeasureJobName(t *testing.T) {
	sm := func(name string, generation int64) *servingv1alpha1.ServedModel {
		return &servingv1alpha1.ServedModel{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: generation}}
	}
	if got := measureJobNameFor(sm("model", 3), "true"); got != "model-measure-3" {
		t.Errorf("true keeps the old name, got %s", got)
	}
	if got := measureJobNameFor(sm("model", 3), ""); got != "model-measure-3" {
		t.Errorf("no request, got %s", got)
	}
	one := measureJobNameFor(sm("model", 3), "m-1")
	if !regexp.MustCompile(`^model-measure-3-[0-9a-f]{8}$`).MatchString(one) {
		t.Errorf("another value gets a hash, got %s", one)
	}
	if one == measureJobNameFor(sm("model", 3), "m-2") {
		t.Error("two values must not share a Job")
	}
	if one == measureJobNameFor(sm("model", 4), "m-1") {
		t.Error("two generations must not share a Job")
	}

	long := strings.Repeat("a", 60)
	for _, request := range []string{"true", "m-1"} {
		for _, generation := range []int64{1, 1 << 62} {
			got := measureJobNameFor(sm(long+"xyz", generation), request)
			if len(got) > maxMeasureJobName {
				t.Errorf("%d characters: %s", len(got), got)
			}
			if got == measureJobNameFor(sm(long+"xyw", generation), request) {
				t.Errorf("two long names must not share a Job: %s", got)
			}
		}
	}
	if got := measureJobNameFor(sm(strings.Repeat("a", 30)+"-"+strings.Repeat("b", 32), 1), "m-1"); strings.Contains(got, "--") {
		t.Errorf("a cut must not leave a hyphen behind: %s", got)
	}
}

func TestMeasureHandled(t *testing.T) {
	const generation = 5
	cases := []struct {
		name           string
		annotation     string
		measureRequest string
		condGeneration int64
		reason         string
		want           bool
	}{
		{name: "same value, same generation", annotation: "m-1", measureRequest: "m-1", condGeneration: generation, reason: "Measured", want: true},
		{name: "same value after a failure", annotation: "m-1", measureRequest: "m-1", condGeneration: generation, reason: "EngineBusy", want: true},
		{name: "value changed", annotation: "m-2", measureRequest: "m-1", condGeneration: generation, reason: "Measured"},
		{name: "spec changed", annotation: "m-1", measureRequest: "m-1", condGeneration: generation - 1, reason: "Measured"},
		{name: "still running", annotation: "m-1", measureRequest: "m-1", condGeneration: generation, reason: reasonMeasureJobRunning},
		{name: "just created", annotation: "m-1", measureRequest: "m-1", condGeneration: generation, reason: reasonMeasureJobCreated},
		{name: "no condition", annotation: "m-1", measureRequest: "m-1"},
		{name: "an older oven's true", annotation: "true", condGeneration: generation, reason: "Measured", want: true},
		{name: "any other value is new to an older oven's status", annotation: "m-1", condGeneration: generation, reason: "Measured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm := &servingv1alpha1.ServedModel{ObjectMeta: metav1.ObjectMeta{
				Generation:  generation,
				Annotations: map[string]string{servingv1alpha1.MeasureAnnotation: tc.annotation},
			}}
			sm.Status.MeasureRequest = tc.measureRequest
			if tc.reason != "" {
				sm.Status.Conditions = []metav1.Condition{{Type: servingv1alpha1.ConditionMeasured, Reason: tc.reason, ObservedGeneration: tc.condGeneration}}
			}
			if got := measureHandled(sm); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// measureResults lists what the runs of one model name left behind, as the agent would.
func measureResults(ctx context.Context, ns, name string) []corev1.ConfigMap {
	list := &corev1.ConfigMapList{}
	ExpectWithOffset(1, k8sClient.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{
		"serving.kukie.io/measure-result":   "true",
		"serving.kukie.io/servedmodel-name": name,
	})).To(Succeed())
	return list.Items
}

// markJobCompleteAt is markJobComplete at a chosen time. The API server refuses a completion time in the future.
func markJobCompleteAt(ctx context.Context, job *batchv1.Job, at time.Time) {
	t := metav1.NewTime(at)
	job.Status.StartTime = &t
	job.Status.Succeeded = 1
	job.Status.CompletionTime = &t
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: t},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: t},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
}

var _ = Describe("Measure requests", func() {
	ctx := context.Background()

	var ns string
	// earlier hands out completion times one minute apart, oldest first, all in the past.
	var earlier func() time.Time

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "measure-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		ns = namespace.Name
		next := time.Now().UTC().Truncate(time.Second).Add(-30 * time.Minute)
		earlier = func() time.Time {
			next = next.Add(time.Minute)
			return next
		}
	})

	ready := func(name string) (*servingv1alpha1.ServedModel, types.NamespacedName) {
		sm := newServedModel(ns, name, false)
		ExpectWithOffset(1, k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		var job *batchv1.Job
		EventuallyWithOffset(1, func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		markJobComplete(ctx, job)
		pod := createPod(ctx, sm, componentEngine, name+"-engine")
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		EventuallyWithOffset(1, phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		return getServedModel(ctx, key), key
	}
	update := func(key types.NamespacedName, change func(*servingv1alpha1.ServedModel)) {
		EventuallyWithOffset(2, func() error {
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
	touch := func(key types.NamespacedName) {
		update(key, func(sm *servingv1alpha1.ServedModel) { sm.Annotations["touch"] = time.Now().String() })
	}
	// request sets the annotation and returns the live Job it asked for.
	request := func(key types.NamespacedName, value string) *batchv1.Job {
		update(key, func(sm *servingv1alpha1.ServedModel) { sm.Annotations[servingv1alpha1.MeasureAnnotation] = value })
		sm := getServedModel(ctx, key)
		job := &batchv1.Job{}
		jobKey := types.NamespacedName{Namespace: ns, Name: measureJobNameFor(sm, value)}
		EventuallyWithOffset(1, func() error {
			if err := k8sClient.Get(ctx, jobKey, job); err != nil {
				return err
			}
			if job.DeletionTimestamp != nil {
				return fmt.Errorf("job %s is on its way out", job.Name)
			}
			return nil
		}, testTimeout, testPoll).Should(Succeed())
		EventuallyWithOffset(1, phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseMeasuring))
		return job
	}
	measureRequestOf := func(key types.NamespacedName) func() string {
		return func() string { return getServedModel(ctx, key).Status.MeasureRequest }
	}
	// finish ends the run well at the given time, as the Job controller and the kubelet would, and waits for the result.
	finish := func(sm *servingv1alpha1.ServedModel, key types.NamespacedName, job *batchv1.Job, value string, at time.Time) {
		pod := createMeasurePod(ctx, sm, job.Name)
		finishMeasure(ctx, pod, 0, measuredOK)
		markJobCompleteAt(ctx, job, at)
		EventuallyWithOffset(1, func() string {
			got := getServedModel(ctx, key)
			if got.Status.Baseline == nil || !got.Status.Baseline.MeasuredAt.Time.Equal(at) {
				return ""
			}
			return got.Status.MeasureRequest
		}, testTimeout, testPoll).Should(Equal(value))
	}
	removeJob := func(job *batchv1.Job) {
		jobKey := client.ObjectKeyFromObject(job)
		ExpectWithOffset(1, k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		EventuallyWithOffset(1, func() error {
			current := &batchv1.Job{}
			if err := k8sClient.Get(ctx, jobKey, current); err != nil {
				return client.IgnoreNotFound(err)
			}
			current.Finalizers = nil
			return k8sClient.Update(ctx, current)
		}, testTimeout, testPoll).Should(Succeed())
		EventuallyWithOffset(1, func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{}))
		}, testTimeout, testPoll).Should(BeTrue())
	}
	measureJobs := func(sm *servingv1alpha1.ServedModel) []string {
		jobs := &batchv1.JobList{}
		ExpectWithOffset(1, k8sClient.List(ctx, jobs, client.InNamespace(ns), client.MatchingLabels{
			servingv1alpha1.ServedModelLabel: sm.Name,
			componentLabel:                   componentMeasure,
		})).To(Succeed())
		var names []string
		for i := range jobs.Items {
			if jobs.Items[i].DeletionTimestamp == nil {
				names = append(names, jobs.Items[i].Name)
			}
		}
		return names
	}

	It("leaves one result per run, whatever the reconcile does afterwards, and a new value measures again", func() {
		sm, key := ready("flow")
		first := request(key, "m-1")
		Expect(first.Name).To(MatchRegexp(`^flow-measure-1-[0-9a-f]{8}$`))
		Expect(measureResults(ctx, ns, "flow")).To(BeEmpty(), "nothing is recorded before the run ends")
		Expect(measureRequestOf(key)()).To(BeEmpty(), "the value goes in when the run ends")

		firstAt := earlier()
		finish(sm, key, first, "m-1", firstAt)
		got := getServedModel(ctx, key)
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseReady))
		Expect(got.Status.Reason).To(Equal("Measured"))
		measured := meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionMeasured)
		Expect(measured.Status).To(Equal(metav1.ConditionTrue))

		By("writing the result the agent reads, with no owner and not as a child")
		results := measureResults(ctx, ns, "flow")
		Expect(results).To(HaveLen(1))
		result := results[0]
		Expect(result.Name).To(Equal("kukie-gpu-measure-flow-" + firstAt.Format("20060102150405")))
		Expect(result.OwnerReferences).To(BeEmpty())
		Expect(result.Labels).To(Equal(map[string]string{
			"serving.kukie.io/measure-result":   "true",
			"serving.kukie.io/servedmodel-name": "flow",
		}))
		Expect(result.Data).To(Equal(map[string]string{
			"baseline.json":  measuredOK,
			"model":          "openai/gpt-oss-20b",
			"gpus":           "1",
			"engineImage":    testImage,
			"engineArgs":     `["--max-model-len","16384"]`,
			"measuredAt":     firstAt.Format(time.RFC3339),
			"partial":        "false",
			"maxConcurrency": "4",
		}))
		baseline := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: baselineConfigMapName(sm)}, baseline)).To(Succeed())
		Expect(metav1.IsControlledBy(baseline, got)).To(BeTrue(), "the proxy's copy still goes with the model")

		By("not writing it again or measuring again while the reconcile keeps coming")
		for range 3 {
			touch(key)
		}
		sameRun := func() []string {
			names := []string{}
			for _, cm := range measureResults(ctx, ns, "flow") {
				names = append(names, cm.Name+"@"+cm.ResourceVersion)
			}
			return append(names, measureJobs(sm)...)
		}
		Consistently(sameRun, 2*time.Second, testPoll).Should(Equal([]string{result.Name + "@" + result.ResourceVersion, first.Name}))

		By("not measuring again when the Job goes and the value stays")
		removeJob(first)
		touch(key)
		Consistently(func() []string { return measureJobs(sm) }, 2*time.Second, testPoll).Should(BeEmpty())
		Expect(getServedModel(ctx, key).Status.Reason).To(Equal("Measured"))
		Expect(measureResults(ctx, ns, "flow")).To(HaveLen(1))

		By("measuring again at the same generation when the value changes, with a time of its own")
		second := request(key, "m-2")
		Expect(second.Name).NotTo(Equal(first.Name))
		Expect(getServedModel(ctx, key).Generation).To(Equal(got.Generation))
		secondAt := earlier()
		finish(sm, key, second, "m-2", secondAt)
		Expect(measureResults(ctx, ns, "flow")).To(HaveLen(2))
		Expect(getServedModel(ctx, key).Status.Baseline.MeasuredAt.Time).To(BeTemporally("==", secondAt))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(baseline), baseline)).To(Succeed())
		Expect(baseline.Data).To(HaveKeyWithValue("measuredAt", secondAt.Format(time.RFC3339)))

		By("going back to an earlier value being a new request too")
		Expect(request(key, "m-1").Name).To(Equal(first.Name))
	})

	It("drops the running Job for the new one when the value changes during a run", func() {
		sm, key := ready("change")
		first := request(key, "m-1")
		createMeasurePod(ctx, sm, first.Name)
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal(reasonMeasureJobRunning))

		second := request(key, "m-2")
		Eventually(func() []string { return measureJobs(sm) }, testTimeout, testPoll).Should(Equal([]string{second.Name}))
		Consistently(func() string { return getServedModel(ctx, key).Status.Reason }, 2*time.Second, testPoll).
			ShouldNot(Equal(reasonMeasureJobGone))
		Expect(measureRequestOf(key)()).To(BeEmpty(), "neither run has ended")

		finish(sm, key, second, "m-2", earlier())
		Expect(measureResults(ctx, ns, "change")).To(HaveLen(1))
	})

	It("does not try again after a failure while the same value stays", func() {
		sm, key := ready("busy")
		job := request(key, "m-1")
		pod := createMeasurePod(ctx, sm, job.Name)
		finishMeasure(ctx, pod, 3, `{"ok":false,"reason":"EngineBusy","message":"처리 중 2, 대기 0"}`)
		markJobFailed(ctx, job)
		Eventually(measureRequestOf(key), testTimeout, testPoll).Should(Equal("m-1"))
		got := getServedModel(ctx, key)
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseReady))
		Expect(got.Status.Reason).To(Equal("EngineBusy"))
		Expect(got.Status.Baseline).To(BeNil())
		Expect(measureResults(ctx, ns, "busy")).To(BeEmpty(), "a run that measured nothing leaves no result")

		removeJob(job)
		touch(key)
		Consistently(func() []string { return measureJobs(sm) }, 2*time.Second, testPoll).Should(BeEmpty())
		Expect(getServedModel(ctx, key).Status.Reason).To(Equal("EngineBusy"))

		By("trying again for a new value")
		finish(sm, key, request(key, "m-2"), "m-2", earlier())
		Expect(measureResults(ctx, ns, "busy")).To(HaveLen(1))
	})

	It("reports a Job lost during a run as gone and leaves it there, whatever the value", func() {
		sm, key := ready("lost")
		job := request(key, "m-1")
		createMeasurePod(ctx, sm, job.Name)
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal(reasonMeasureJobRunning))
		removeJob(job)
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal(reasonMeasureJobGone))
		got := getServedModel(ctx, key)
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseReady))
		Expect(got.Status.MeasureRequest).To(Equal("m-1"))
		touch(key)
		Consistently(func() []string { return measureJobs(sm) }, 2*time.Second, testPoll).Should(BeEmpty())
	})

	It("corrects a failure when the Pod's result arrives after the Job's end", func() {
		sm, key := ready("late")
		job := request(key, "m-1")
		pod := createMeasurePod(ctx, sm, job.Name)
		at := earlier()
		markJobCompleteAt(ctx, job, at)
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal(reasonMeasureFailed))
		Expect(measureResults(ctx, ns, "late")).To(BeEmpty())

		finishMeasure(ctx, pod, 0, measuredOK)
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal("Measured"))
		Expect(measureResults(ctx, ns, "late")).To(HaveLen(1))
		Expect(getServedModel(ctx, key).Status.Baseline.MeasuredAt.Time).To(BeTemporally("==", at))
	})

	It("keeps the result of a partial run and of a run whose baseline is in conflict", func() {
		sm, key := ready("taken")
		theirs := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: baselineConfigMapName(sm), Namespace: ns},
			Data:       map[string]string{"mine": "yes"},
		}
		Expect(k8sClient.Create(ctx, theirs)).To(Succeed())
		job := request(key, "m-1")
		pod := createMeasurePod(ctx, sm, job.Name)
		finishMeasure(ctx, pod, 0, `{"ok":true,"partial":true,"reason":"MeasuredPartial","message":"끝까지 재지 못했다","maxConcurrency":2,"maxWaiting":0,"maxKvPercent":9}`)
		markJobCompleteAt(ctx, job, earlier())
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal(reasonBaselineConflict))
		results := measureResults(ctx, ns, "taken")
		Expect(results).To(HaveLen(1))
		Expect(results[0].Data).To(HaveKeyWithValue("partial", "true"))
		Expect(results[0].Data).To(HaveKeyWithValue("maxConcurrency", "2"))
		Expect(measureRequestOf(key)()).To(Equal("m-1"))

		By("writing the baseline after all once the ConfigMap in the way is gone, with no second result")
		Expect(k8sClient.Delete(ctx, theirs)).To(Succeed())
		touch(key)
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal("MeasuredPartial"))
		Expect(getServedModel(ctx, key).Status.Baseline.Partial).To(BeTrue())
		Expect(measureResults(ctx, ns, "taken")).To(HaveLen(1))
	})

	It("measures again with the same value when the spec changes", func() {
		sm, key := ready("spec")
		first := request(key, "m-1")
		finish(sm, key, first, "m-1", earlier())

		update(key, func(got *servingv1alpha1.ServedModel) { got.Spec.Engine.GPUs = 2 })
		current := getServedModel(ctx, key)
		Expect(current.Generation).To(Equal(sm.Generation + 1))
		next := &batchv1.Job{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureJobNameFor(current, "m-1")}, next)
		}, testTimeout, testPoll).Should(Succeed())
		Expect(next.Name).NotTo(Equal(first.Name))
		finish(current, key, next, "m-1", earlier())

		results := measureResults(ctx, ns, "spec")
		Expect(results).To(HaveLen(2))
		Expect(results[1].Data).To(HaveKeyWithValue("gpus", "2"))
		Expect(getServedModel(ctx, key).Status.Baseline.Generation).To(Equal(current.Generation))
	})

	It("ends with the annotation gone: the Job and the marker go, the results stay, and the same value measures again", func() {
		sm, key := ready("withdrawn")
		first := request(key, "m-1")
		finish(sm, key, first, "m-1", earlier())

		update(key, func(got *servingv1alpha1.ServedModel) { delete(got.Annotations, servingv1alpha1.MeasureAnnotation) })
		Eventually(func() []string { return measureJobs(sm) }, testTimeout, testPoll).Should(BeEmpty())
		Eventually(measureRequestOf(key), testTimeout, testPoll).Should(BeEmpty())
		got := getServedModel(ctx, key)
		Expect(meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionMeasured)).To(BeNil())
		Expect(got.Status.Baseline).NotTo(BeNil())
		Expect(measureResults(ctx, ns, "withdrawn")).To(HaveLen(1))

		By("treating an empty value as no request")
		update(key, func(current *servingv1alpha1.ServedModel) {
			current.Annotations[servingv1alpha1.MeasureAnnotation] = ""
		})
		Consistently(func() []string { return measureJobs(sm) }, time.Second, testPoll).Should(BeEmpty())
		update(key, func(current *servingv1alpha1.ServedModel) {
			delete(current.Annotations, servingv1alpha1.MeasureAnnotation)
		})

		By("measuring again when the same value comes back")
		Expect(request(key, "m-1").Name).To(Equal(first.Name))
	})

	It("takes over from an older oven: its handled true stays handled", func() {
		sm, key := ready("upgraded")
		job := request(key, "true")
		Expect(job.Name).To(Equal(fmt.Sprintf("upgraded-measure-%d", sm.Generation)))
		finish(sm, key, job, "true", earlier())
		removeJob(job)

		By("having a status without the new field, as the older oven wrote it")
		got := getServedModel(ctx, key)
		patch := client.MergeFrom(got.DeepCopy())
		got.Status.MeasureRequest = ""
		Expect(k8sClient.Status().Patch(ctx, got, patch)).To(Succeed())

		Eventually(measureRequestOf(key), testTimeout, testPoll).Should(Equal("true"))
		Consistently(func() []string { return measureJobs(sm) }, 2*time.Second, testPoll).Should(BeEmpty())
		Expect(getServedModel(ctx, key).Status.Reason).To(Equal("Measured"))
	})

	It("keeps the newest 20 results of a model and leaves everything else alone", func() {
		sm, key := ready("many")
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for i := range 25 {
			name := measureResultName(sm, metav1.NewTime(base.Add(time.Duration(i)*time.Hour)))
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns,
				Labels: map[string]string{servingv1alpha1.MeasureResultLabel: "true", servingv1alpha1.MeasureResultNameLabel: "many"},
			}})).To(Succeed())
		}
		otherModel := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: "kukie-gpu-measure-many2-20200101000000", Namespace: ns,
			Labels: map[string]string{servingv1alpha1.MeasureResultLabel: "true", servingv1alpha1.MeasureResultNameLabel: "many2"},
		}}
		unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kukie-gpu-measure-many-20200101000000", Namespace: ns}}
		Expect(k8sClient.Create(ctx, otherModel)).To(Succeed())
		Expect(k8sClient.Create(ctx, unrelated)).To(Succeed())

		at := earlier()
		finish(sm, key, request(key, "m-1"), "m-1", at)

		results := measureResults(ctx, ns, "many")
		Expect(results).To(HaveLen(20))
		names := make([]string, 0, len(results))
		for _, cm := range results {
			names = append(names, cm.Name)
		}
		Expect(names).To(ContainElement(measureResultName(sm, metav1.NewTime(at))), "the new one stays")
		Expect(names).To(ContainElement(measureResultName(sm, metav1.NewTime(base.Add(24*time.Hour)))))
		Expect(names).To(ContainElement(measureResultName(sm, metav1.NewTime(base.Add(6*time.Hour)))))
		Expect(names).NotTo(ContainElement(measureResultName(sm, metav1.NewTime(base.Add(5*time.Hour)))))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(otherModel), otherModel)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(unrelated), unrelated)).To(Succeed())
	})

	It("keeps results across delete and create again, and they do not hold up the new model", func() {
		sm, key := ready("reborn")
		first := request(key, "m-1")
		finish(sm, key, first, "m-1", earlier())
		result := measureResults(ctx, ns, "reborn")[0]

		By("not carrying the label anything looking for children lists by")
		children := &corev1.ConfigMapList{}
		Expect(k8sClient.List(ctx, children, client.InNamespace(ns), client.MatchingLabels{servingv1alpha1.ServedModelLabel: "reborn"})).To(Succeed())
		for _, child := range children.Items {
			Expect(child.Name).NotTo(Equal(result.Name))
		}

		By("deleting the model and playing garbage collection for what it owned, and only that")
		Expect(k8sClient.Delete(ctx, getServedModel(ctx, key))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &servingv1alpha1.ServedModel{}))
		}, testTimeout, testPoll).Should(BeTrue())
		Expect(k8sClient.DeleteAllOf(ctx, &batchv1.Job{}, client.InNamespace(ns), client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Expect(k8sClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "reborn", Namespace: ns}})).To(Succeed())
		Expect(k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "reborn", Namespace: ns}})).To(Succeed())
		Expect(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: baselineConfigMapName(sm), Namespace: ns}})).To(Succeed())
		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName(sm)}, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		pvc.Finalizers = nil
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Eventually(func() int {
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns))).To(Succeed())
			return len(jobs.Items)
		}, testTimeout, testPoll).Should(BeZero())

		Expect(measureResults(ctx, ns, "reborn")).To(HaveLen(1), "the result outlives its model")

		By("creating it again without waiting for anything or reading the old run as its own")
		again := newServedModel(ns, "reborn", false)
		Expect(k8sClient.Create(ctx, again)).To(Succeed())
		Eventually(func() *batchv1.Job { return findDownloadJob(ctx, again) }, testTimeout, testPoll).ShouldNot(BeNil())
		// The reconcile creates the Job before it writes status, and the cache may have shown the deleted children at first.
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).
			ShouldNot(Equal("WaitingForOldResources"))
		Consistently(func() string { return getServedModel(ctx, key).Status.Reason }, 2*time.Second, testPoll).
			ShouldNot(Equal("WaitingForOldResources"))
		got := getServedModel(ctx, key)
		Expect(got.Status.Phase).NotTo(Equal(servingv1alpha1.PhaseFailed))
		Expect(got.Status.Baseline).To(BeNil())
		Expect(got.Status.MeasureRequest).To(BeEmpty())

		kept := measureResults(ctx, ns, "reborn")
		Expect(kept).To(HaveLen(1))
		Expect(kept[0].UID).To(Equal(result.UID))
		Expect(kept[0].OwnerReferences).To(BeEmpty())
	})

	Context("when a Job about to be removed has a result nobody has read", func() {
		// The ServedModel is not in the API server, so the running controller leaves these Jobs and Pods alone and the moment
		// a live reconcile would close at once stays open. Nothing here depends on who holds the object.
		var sm *servingv1alpha1.ServedModel
		var direct *ServedModelReconciler

		finishedJob := func(value string, generation int64) *batchv1.Job {
			owner := sm.DeepCopy()
			owner.Generation = generation
			owner.Annotations = map[string]string{servingv1alpha1.MeasureAnnotation: value}
			ExpectWithOffset(1, k8sClient.Apply(ctx, testConfig.desiredMeasureJob(owner), client.FieldOwner(FieldManager), client.ForceOwnership)).To(Succeed())
			job := &batchv1.Job{}
			ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureJobNameFor(owner, value)}, job)).To(Succeed())
			finishMeasure(ctx, createMeasurePod(ctx, owner, job.Name), 0, measuredOK)
			markJobCompleteAt(ctx, job, earlier())
			return job
		}

		BeforeEach(func() {
			sm = newServedModel(ns, "ghost", false)
			sm.UID = types.UID("00000000-0000-0000-0000-000000000009")
			sm.Generation = 2
			direct = &ServedModelReconciler{Client: k8sClient, Scheme: scheme.Scheme, Config: testConfig}
		})

		gone := func(job *batchv1.Job) bool {
			current := &batchv1.Job{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(job), current)
			return apierrors.IsNotFound(err) || current.DeletionTimestamp != nil
		}

		It("writes it down before the Job and its Pod go", func() {
			job := finishedJob("m-1", sm.Generation)
			Expect(direct.removeMeasureJobs(ctx, sm, []*batchv1.Job{job})).To(Succeed())
			Expect(gone(job)).To(BeTrue())
			Expect(measureResults(ctx, ns, "ghost")).To(HaveLen(1))
			Expect(sm.Status.Baseline).NotTo(BeNil())
			Expect(sm.Status.Baseline.MeasuredAt.Time).To(BeTemporally("==", job.Status.CompletionTime.Time))
		})

		It("does not file a run of an older spec under the current one", func() {
			job := finishedJob("m-1", sm.Generation-1)
			Expect(direct.removeMeasureJobs(ctx, sm, []*batchv1.Job{job})).To(Succeed())
			Expect(gone(job)).To(BeTrue())
			Expect(measureResults(ctx, ns, "ghost")).To(BeEmpty())
			Expect(sm.Status.Baseline).To(BeNil())
		})

		It("has nothing to write for a run that did not measure", func() {
			owner := sm.DeepCopy()
			owner.Annotations = map[string]string{servingv1alpha1.MeasureAnnotation: "m-1"}
			Expect(k8sClient.Apply(ctx, testConfig.desiredMeasureJob(owner), client.FieldOwner(FieldManager), client.ForceOwnership)).To(Succeed())
			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureJobName(owner)}, job)).To(Succeed())
			finishMeasure(ctx, createMeasurePod(ctx, owner, job.Name), 3, `{"ok":false,"reason":"EngineBusy","message":"처리 중 2, 대기 0"}`)
			markJobFailed(ctx, job)
			Expect(direct.removeMeasureJobs(ctx, sm, []*batchv1.Job{job})).To(Succeed())
			Expect(gone(job)).To(BeTrue())
			Expect(measureResults(ctx, ns, "ghost")).To(BeEmpty())
		})
	})
})
