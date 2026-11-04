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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

const (
	testImage   = "vllm/vllm-openai:v0.32.0@sha256:6ee133356d49ed24cee03dd0cda3d3980279a11c107ce3e752c89e76540f41c9"
	testTimeout = 20 * time.Second
	testPoll    = 200 * time.Millisecond
)

// testConfig is what the manager flags would carry on kukie-gpu.
var testConfig = Config{
	GPUNodeSelector: map[string]string{"kukie.io/accelerator": "nvidia-l40s"},
	MaxRuntime:      DefaultMaxRuntime,
	NodeWaitTimeout: DefaultNodeWaitTimeout,
}

func newServedModel(ns, name string, retain bool) *servingv1alpha1.ServedModel {
	return &servingv1alpha1.ServedModel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: servingv1alpha1.ServedModelSpec{
			Source: servingv1alpha1.ModelSource{HuggingFace: "openai/gpt-oss-20b"},
			Engine: servingv1alpha1.EngineSpec{
				Image: testImage,
				GPUs:  1,
				Args:  []string{"--max-model-len", "16384"},
			},
			Storage: servingv1alpha1.StorageSpec{
				Size:           resource.MustParse("40Gi"),
				RetainOnDelete: retain,
			},
		},
	}
}

func getServedModel(ctx context.Context, key types.NamespacedName) *servingv1alpha1.ServedModel {
	sm := &servingv1alpha1.ServedModel{}
	ExpectWithOffset(1, k8sClient.Get(ctx, key, sm)).To(Succeed())
	return sm
}

func phaseOf(ctx context.Context, key types.NamespacedName) func() servingv1alpha1.ServedModelPhase {
	return func() servingv1alpha1.ServedModelPhase {
		sm := &servingv1alpha1.ServedModel{}
		if err := k8sClient.Get(ctx, key, sm); err != nil {
			return ""
		}
		return sm.Status.Phase
	}
}

// findDownloadJob returns the one download Job owned by the ServedModel, or nil.
func findDownloadJob(ctx context.Context, sm *servingv1alpha1.ServedModel) *batchv1.Job {
	jobs := &batchv1.JobList{}
	ExpectWithOffset(1, k8sClient.List(ctx, jobs, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   componentDownload,
	})).To(Succeed())
	var live []batchv1.Job
	for _, j := range jobs.Items {
		if j.DeletionTimestamp == nil {
			live = append(live, j)
		}
	}
	if len(live) != 1 {
		return nil
	}
	return &live[0]
}

func markJobComplete(ctx context.Context, job *batchv1.Job) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Succeeded = 1
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
}

// createPod plays the ReplicaSet or Job: a Pod with the component's labels and no status yet. The tests name a download
// Pod <job>-<something>, as the Job controller does.
func createPod(ctx context.Context, sm *servingv1alpha1.ServedModel, component, name string) *corev1.Pod {
	labels := childLabels(sm, component)
	if component == componentDownload {
		labels[batchv1.JobNameLabel] = name[:strings.LastIndex(name, "-")]
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sm.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "vllm", Image: testImage}},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
	return pod
}

// createMeasurePod plays the Job controller for a measure Job: same labels plus job-name.
func createMeasurePod(ctx context.Context, sm *servingv1alpha1.ServedModel, jobName string) *corev1.Pod {
	labels := childLabels(sm, componentMeasure)
	labels["job-name"] = jobName
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName + "-pod", Namespace: sm.Namespace, Labels: labels},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "measure", Image: testImage}}},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
	return pod
}

// finishMeasure gives the Pod the exit code and termination message the script leaves.
func finishMeasure(ctx context.Context, pod *corev1.Pod, exitCode int32, message string) {
	pod.Status.Phase = corev1.PodSucceeded
	if exitCode != 0 {
		pod.Status.Phase = corev1.PodFailed
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "measure",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message}},
	}}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

func markJobFailed(ctx context.Context, job *batchv1.Job) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "BackoffLimitExceeded"},
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"},
	}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
}

var _ = Describe("ServedModel controller", func() {
	ctx := context.Background()

	var ns string

	BeforeEach(func() {
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "servedmodel-"}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		ns = namespace.Name
	})

	It("walks PVC → download Job → Deployment → Ready and follows the Pod afterwards", func() {
		sm := newServedModel(ns, "gpt-oss-20b", true)
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)

		By("creating the PVC without an owner reference because retainOnDelete is true")
		pvc := &corev1.PersistentVolumeClaim{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gpt-oss-20b-models"}, pvc)
		}, testTimeout, testPoll).Should(Succeed())
		Expect(pvc.OwnerReferences).To(BeEmpty())
		Expect(pvc.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("40Gi")))
		Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))

		By("creating the download Job owned by the ServedModel and reporting Downloading")
		var job *batchv1.Job
		Eventually(func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		Expect(job.OwnerReferences).To(HaveLen(1))
		Expect(job.OwnerReferences[0].Name).To(Equal(sm.Name))
		Expect(job.OwnerReferences[0].Controller).To(HaveValue(BeTrue()))
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		dl := job.Spec.Template.Spec.Containers[0]
		Expect(dl.Image).To(Equal(testImage), "download uses the engine image when --download-image is empty")
		Expect(dl.Command).To(Equal([]string{"python3", "-c", downloadScript}))
		Expect(dl.Env).To(ContainElement(corev1.EnvVar{Name: "HF_HOME", Value: modelsMountPath}))
		Expect(dl.Env).To(ContainElement(corev1.EnvVar{Name: "MODEL", Value: "openai/gpt-oss-20b"}))
		Expect(dl.Env).NotTo(ContainElement(HaveField("Name", "HF_TOKEN")), "no token without tokenSecretRef")
		Expect(job.Spec.Template.Spec.NodeSelector).To(Equal(testConfig.GPUNodeSelector))
		Expect(job.Spec.Template.Spec.Tolerations).To(ContainElement(HaveField("Key", "nvidia.com/gpu")))
		Expect(job.Spec.Template.Spec.Volumes).To(ContainElement(
			HaveField("PersistentVolumeClaim.ClaimName", "gpt-oss-20b-models")))
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))
		Expect(getServedModel(ctx, key).Status.Download).To(HaveValue(Equal(servingv1alpha1.DownloadStatus{Percent: 0})))

		By("reporting Pending/WaitingForNode with the scheduler's message while the download Pod cannot be scheduled")
		dlPod := createPod(ctx, sm, componentDownload, job.Name+"-x1")
		dlPod.Status.Phase = corev1.PodPending
		dlPod.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/2 nodes are available: pod has unbound immediate PersistentVolumeClaims.",
		}}
		Expect(k8sClient.Status().Update(ctx, dlPod)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhasePending))
		got := getServedModel(ctx, key)
		Expect(got.Status.Reason).To(Equal("WaitingForNode"))
		Expect(got.Status.Message).To(Equal("0/2 nodes are available: pod has unbound immediate PersistentVolumeClaims."))

		By("going back to Downloading once the download Pod runs")
		dlPod.Status.Phase = corev1.PodRunning
		dlPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, dlPod)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))

		By("not creating the Deployment before the download finished")
		Consistently(func() error {
			return k8sClient.Get(ctx, key, &appsv1.Deployment{})
		}, 2*time.Second, testPoll).ShouldNot(Succeed())

		By("creating the Deployment and Service once the Job is complete, and reporting Loading")
		markJobComplete(ctx, job)
		deploy := &appsv1.Deployment{}
		Eventually(func() error { return k8sClient.Get(ctx, key, deploy) }, testTimeout, testPoll).Should(Succeed())
		Expect(deploy.OwnerReferences).To(HaveLen(1))
		Expect(deploy.OwnerReferences[0].UID).To(Equal(sm.UID))
		Expect(deploy.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		Expect(deploy.Spec.Template.Spec.NodeSelector).To(Equal(testConfig.GPUNodeSelector))
		Expect(deploy.Spec.Template.Spec.Tolerations).To(ContainElement(HaveField("Key", "nvidia.com/gpu")))
		Expect(deploy.Spec.Template.Spec.Containers).To(HaveLen(1))
		engine := deploy.Spec.Template.Spec.Containers[0]
		Expect(engine.Image).To(Equal(testImage))
		Expect(engine.Args).To(Equal([]string{
			"--model", "openai/gpt-oss-20b", "--tensor-parallel-size", "1", "--port", "8000", "--max-model-len", "16384",
		}))
		Expect(engine.Resources.Limits[gpuResourceName]).To(Equal(resource.MustParse("1")))
		Expect(engine.Resources.Limits[corev1.ResourceMemory]).To(Equal(defaultEngineMemoryLimit))
		Expect(engine.Env).To(ContainElement(corev1.EnvVar{Name: "HF_HUB_OFFLINE", Value: "1"}))
		Expect(engine.StartupProbe).NotTo(BeNil())
		Expect(engine.StartupProbe.HTTPGet.Path).To(Equal("/health"))
		Expect(engine.ReadinessProbe).NotTo(BeNil())
		Expect(engine.VolumeMounts).To(ContainElement(HaveField("MountPath", modelsMountPath)))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key, svc)).To(Succeed())
		Expect(svc.Spec.Selector).To(Equal(engineSelector(sm)))
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8000)))

		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseLoading))
		got = getServedModel(ctx, key)
		Expect(got.Status.Download).To(HaveValue(Equal(servingv1alpha1.DownloadStatus{Percent: 100})))
		Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
			HaveField("Type", servingv1alpha1.ConditionDownloaded),
			HaveField("Status", metav1.ConditionTrue),
		)))

		By("reporting Pending/WaitingForNode with the scheduler's message when the Pod cannot be scheduled yet")
		pod := createPod(ctx, sm, componentEngine, "gpt-oss-20b-abc")
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu.",
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhasePending))
		got = getServedModel(ctx, key)
		Expect(got.Status.Reason).To(Equal("WaitingForNode"))
		Expect(got.Status.Message).To(Equal("0/2 nodes are available: 2 Insufficient nvidia.com/gpu."))

		By("reporting Failed with the scheduler's message when the Pod has waited too long")
		pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-DefaultNodeWaitTimeout - time.Minute))
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))
		got = getServedModel(ctx, key)
		Expect(got.Status.Reason).To(Equal("Unschedulable"))
		Expect(got.Status.Message).To(Equal("0/2 nodes are available: 2 Insufficient nvidia.com/gpu."))

		By("reporting Ready once the Pod's readiness probe (/health) passes")
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		got = getServedModel(ctx, key)
		Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
			HaveField("Type", servingv1alpha1.ConditionReady),
			HaveField("Status", metav1.ConditionTrue),
		)))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))

		By("running a measure Job on request and recording the baseline when it finishes")
		got.Annotations = map[string]string{servingv1alpha1.MeasureAnnotation: "true"}
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseMeasuring))
		measureJob := &batchv1.Job{}
		measureKey := types.NamespacedName{Namespace: ns, Name: fmt.Sprintf("gpt-oss-20b-measure-%d", got.Generation)}
		Eventually(func() error { return k8sClient.Get(ctx, measureKey, measureJob) }, testTimeout, testPoll).Should(Succeed())
		measurePod := createMeasurePod(ctx, sm, measureJob.Name)
		finishMeasure(ctx, measurePod, 0, `{"ok":true,"reason":"Measured","message":"동시 32까지","maxConcurrency":32,"maxWaiting":3,"maxKvPercent":61,"steps":[]}`)
		markJobComplete(ctx, measureJob)
		Eventually(func() *servingv1alpha1.BaselineStatus { return getServedModel(ctx, key).Status.Baseline }, testTimeout, testPoll).ShouldNot(BeNil())
		got = getServedModel(ctx, key)
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseReady))
		Expect(got.Status.Baseline.MaxConcurrency).To(Equal(int32(32)))
		Expect(got.Status.Baseline.MaxWaiting).To(Equal(int32(3)))
		Expect(got.Status.Baseline.MaxKvPercent).To(Equal(int32(61)))
		Expect(got.Status.Baseline.Generation).To(Equal(got.Generation))
		baseline := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kukie-gpu-baseline-gpt-oss-20b"}, baseline)).To(Succeed())
		Expect(baseline.Data).To(HaveKeyWithValue("maxConcurrency", "32"))
		Expect(metav1.IsControlledBy(baseline, got)).To(BeTrue())

		By("keeping the baseline and removing the Job when the request is withdrawn")
		delete(got.Annotations, servingv1alpha1.MeasureAnnotation)
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Eventually(func() bool {
			job := &batchv1.Job{}
			err := k8sClient.Get(ctx, measureKey, job)
			return apierrors.IsNotFound(err) || job.DeletionTimestamp != nil
		}, testTimeout, testPoll).Should(BeTrue())
		Expect(getServedModel(ctx, key).Status.Baseline).NotTo(BeNil())
		Expect(k8sClient.Delete(ctx, measurePod)).To(Succeed())

		By("reporting Failed with the container's message when it crash-loops")
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "vllm",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container=vllm",
			}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Reason: "Error", Message: "ValueError: No available memory for the cache blocks",
			}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))
		got = getServedModel(ctx, key)
		Expect(got.Status.Reason).To(Equal("CrashLoopBackOff"))
		Expect(got.Status.Message).To(ContainSubstring("back-off 5m0s restarting failed container=vllm"))
		Expect(got.Status.Message).To(ContainSubstring("No available memory for the cache blocks"))

		By("starting a new download Job and dropping the old one when the source changes")
		got.Spec.Source.HuggingFace = "Qwen/Qwen3.6-35B-A3B"
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Eventually(func() string {
			j := findDownloadJob(ctx, sm)
			if j == nil {
				return ""
			}
			return j.Name
		}, testTimeout, testPoll).Should(SatisfyAll(Not(BeEmpty()), Not(Equal(job.Name))))
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))
		Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(job), &batchv1.Job{})
		}, testTimeout, testPoll).ShouldNot(Succeed(), "the old Job is deleted")

		By("giving the PVC an owner reference once retainOnDelete is turned off")
		got = getServedModel(ctx, key)
		got.Spec.Storage.RetainOnDelete = false
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Eventually(func() []metav1.OwnerReference {
			_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)
			return pvc.OwnerReferences
		}, testTimeout, testPoll).Should(HaveLen(1))
		Expect(pvc.OwnerReferences[0].UID).To(Equal(sm.UID))
	})

	It("owns the PVC from the start when retainOnDelete is false and passes the token to the download Job", func() {
		sm := newServedModel(ns, "gated", false)
		sm.Spec.Source.TokenSecretRef = "hf-token"
		sm.Spec.Engine.GPUs = 2
		sm.Spec.Engine.Resources = &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Gi")},
		}
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())

		pvc := &corev1.PersistentVolumeClaim{}
		Eventually(func() []metav1.OwnerReference {
			_ = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gated-models"}, pvc)
			return pvc.OwnerReferences
		}, testTimeout, testPoll).Should(HaveLen(1))
		Expect(pvc.OwnerReferences[0].Kind).To(Equal("ServedModel"))
		Expect(pvc.OwnerReferences[0].BlockOwnerDeletion).To(HaveValue(BeTrue()))

		var job *batchv1.Job
		Eventually(func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(SatisfyAll(
			HaveField("Name", "HF_TOKEN"),
			HaveField("ValueFrom.SecretKeyRef.Name", "hf-token"),
			HaveField("ValueFrom.SecretKeyRef.Key", "token"),
		)))

		By("staying Downloading while a failed attempt is retried by the Job, with the exit code shown")
		attempt := createPod(ctx, sm, componentDownload, job.Name+"-a1")
		attempt.Status.Phase = corev1.PodFailed
		attempt.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "download",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}
		Expect(k8sClient.Status().Update(ctx, attempt)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		Eventually(func() string {
			return getServedModel(ctx, key).Status.Reason
		}, testTimeout, testPoll).Should(Equal("Retrying"))
		got := getServedModel(ctx, key)
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseDownloading))
		Expect(got.Status.Message).To(ContainSubstring("exit code 1 (Error)"))

		By("reporting Failed with the Job's message and the last attempt's exit code when the retries run out")
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Failed = 3
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))
		got = getServedModel(ctx, key)
		Expect(got.Status.Reason).To(Equal("DownloadFailed"))
		Expect(got.Status.Message).To(Equal("BackoffLimitExceeded: Job has reached the specified backoff limit\n마지막 종료: exit code 1 (Error)"))
		Consistently(func() error {
			return k8sClient.Get(ctx, key, &appsv1.Deployment{})
		}, 2*time.Second, testPoll).ShouldNot(Succeed(), "no engine after a failed download")

		By("recreating the Job when the failed one is deleted (the retry path), then sizing the engine from spec.engine")
		failedUID := job.UID
		Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func() types.UID {
			job = findDownloadJob(ctx, sm)
			if job == nil {
				return failedUID
			}
			return job.UID
		}, testTimeout, testPoll).ShouldNot(Equal(failedUID))
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))
		markJobComplete(ctx, job)
		deploy := &appsv1.Deployment{}
		Eventually(func() error { return k8sClient.Get(ctx, key, deploy) }, testTimeout, testPoll).Should(Succeed())
		engine := deploy.Spec.Template.Spec.Containers[0]
		Expect(engine.Args).To(ContainElements("--tensor-parallel-size", "2"))
		Expect(engine.Resources.Limits[gpuResourceName]).To(Equal(resource.MustParse("2")))
		Expect(engine.Resources.Requests[gpuResourceName]).To(Equal(resource.MustParse("2")))
		Expect(engine.Resources.Limits[corev1.ResourceMemory]).To(Equal(resource.MustParse("64Gi")))
		Expect(engine.Resources.Requests[corev1.ResourceCPU]).To(Equal(defaultEngineCPURequest), "requests keep the defaults")
	})

	It("refuses names taken by someone else's objects and leaves them alone", func() {
		labels := map[string]string{"app": "by-hand"}
		replicas := int32(1)
		theirs := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: testImage}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, theirs)).To(Succeed())

		sm := newServedModel(ns, "taken", false)
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))
		got := getServedModel(ctx, key)
		Expect(got.Status.Reason).To(Equal("NameConflict"))
		Expect(got.Status.Message).To(ContainSubstring("Deployment taken"))
		Expect(findDownloadJob(ctx, sm)).To(BeNil(), "nothing is downloaded for a model that cannot be served")

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(theirs), theirs)).To(Succeed())
		Expect(theirs.OwnerReferences).To(BeEmpty())
		Expect(theirs.Spec.Template.Labels).To(Equal(labels))

		By("treating a PVC without our label the same way")
		storage := resource.MustParse("1Gi")
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "clash-models", Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storage}},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		clash := newServedModel(ns, "clash", false)
		Expect(k8sClient.Create(ctx, clash)).To(Succeed())
		clashKey := client.ObjectKeyFromObject(clash)
		Eventually(phaseOf(ctx, clashKey), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))
		Expect(getServedModel(ctx, clashKey).Status.Message).To(ContainSubstring("PVC clash-models"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		Expect(pvc.OwnerReferences).To(BeEmpty())
	})

	It("waits for what a deleted ServedModel of the same name left behind instead of calling it a conflict", func() {
		labels := map[string]string{"app": "old"}
		replicas := int32(1)
		isController := true
		leftover := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: "again", Namespace: ns,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: servingv1alpha1.GroupVersion.String(), Kind: "ServedModel", Name: "again",
					UID: types.UID("00000000-0000-0000-0000-000000000001"), Controller: &isController,
				}},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: testImage}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, leftover)).To(Succeed())

		sm := newServedModel(ns, "again", true)
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		Eventually(func() string {
			return getServedModel(ctx, key).Status.Reason
		}, testTimeout, testPoll).Should(Equal("WaitingForOldResources"))
		Expect(getServedModel(ctx, key).Status.Phase).To(Equal(servingv1alpha1.PhasePending))
		Expect(findDownloadJob(ctx, sm)).To(BeNil())

		By("going ahead once garbage collection has removed it")
		Expect(k8sClient.Delete(ctx, leftover)).To(Succeed())
		Eventually(func() *batchv1.Job { return findDownloadJob(ctx, sm) }, testTimeout, testPoll).ShouldNot(BeNil())
	})

	It("waits for a PVC on its way out and a finished Job of the deleted ServedModel", func() {
		By("holding back while the old ServedModel's PVC is still there")
		isController := true
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: "term-models", Namespace: ns,
				Labels: map[string]string{servingv1alpha1.ServedModelLabel: "term"},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: servingv1alpha1.GroupVersion.String(), Kind: "ServedModel", Name: "term",
					UID: types.UID("00000000-0000-0000-0000-000000000003"), Controller: &isController,
				}},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			},
		}
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())

		sm := newServedModel(ns, "term", false)
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).
			Should(Equal("WaitingForOldResources"))
		Expect(findDownloadJob(ctx, sm)).To(BeNil())

		// The API server adds pvc-protection on create and envtest has no controller to take it off.
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)).To(Succeed())
		pvc.Finalizers = nil
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Eventually(func() *batchv1.Job { return findDownloadJob(ctx, sm) }, testTimeout, testPoll).ShouldNot(BeNil())
		// The reconcile creates the Job before it writes status, so the old reason can still be there for a moment.
		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).
			ShouldNot(Equal("WaitingForOldResources"))

		By("not stalling on our own PVC deleted by hand: pvc-protection holds it while our engine runs")
		own := &corev1.PersistentVolumeClaim{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "term-models"}, own)
		}, testTimeout, testPoll).Should(Succeed())
		Expect(k8sClient.Delete(ctx, own)).To(Succeed())
		Consistently(func() string { return getServedModel(ctx, key).Status.Reason }, 7*time.Second, testPoll).
			ShouldNot(Equal("WaitingForOldResources"))
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(own), own)).To(Succeed())
		own.Finalizers = nil
		Expect(k8sClient.Update(ctx, own)).To(Succeed())

		By("not taking a finished Job of the deleted ServedModel for a finished download")
		old := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "reborn-download-old", Namespace: ns,
				Labels: map[string]string{servingv1alpha1.ServedModelLabel: "reborn", componentLabel: componentDownload},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: servingv1alpha1.GroupVersion.String(), Kind: "ServedModel", Name: "reborn",
					UID: types.UID("00000000-0000-0000-0000-000000000002"), Controller: &isController,
				}},
			},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "download", Image: testImage}},
				},
			}},
		}
		Expect(k8sClient.Create(ctx, old)).To(Succeed())
		reborn := newServedModel(ns, "reborn", false)
		Expect(k8sClient.Create(ctx, reborn)).To(Succeed())
		rebornKey := client.ObjectKeyFromObject(reborn)
		Eventually(func() string { return getServedModel(ctx, rebornKey).Status.Reason }, testTimeout, testPoll).
			Should(Equal("WaitingForOldResources"))
		Expect(k8sClient.Get(ctx, rebornKey, &appsv1.Deployment{})).NotTo(Succeed())
	})

	It("writes a Deployment the API server refuses to status", func() {
		sm := newServedModel(ns, "refused", true)
		sm.Spec.Engine.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("48Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
		}
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		var job *batchv1.Job
		Eventually(func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		Expect(job.Spec.ActiveDeadlineSeconds).To(HaveValue(Equal(int64(DefaultDownloadDeadline.Seconds()))))
		markJobComplete(ctx, job)

		key := client.ObjectKeyFromObject(sm)
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseFailed))
		got := getServedModel(ctx, key)
		Expect(got.Status.Reason).To(Equal("ApplyRejected"))
		Expect(got.Status.Message).To(ContainSubstring("must be less than or equal to memory limit"))
		Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
	})

	It("scales the engine to zero while a changed source downloads, then brings it back", func() {
		sm := newServedModel(ns, "swap", true)
		Expect(k8sClient.Create(ctx, sm)).To(Succeed())
		var job *batchv1.Job
		Eventually(func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		markJobComplete(ctx, job)

		replicas := func() int32 {
			deployment := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(sm), deployment); err != nil || deployment.Spec.Replicas == nil {
				return -1
			}
			return *deployment.Spec.Replicas
		}
		Eventually(replicas, testTimeout, testPoll).Should(Equal(int32(1)))

		key := client.ObjectKeyFromObject(sm)
		got := getServedModel(ctx, key)
		got.Spec.Source.HuggingFace = "Qwen/Qwen3-8B"
		Expect(k8sClient.Update(ctx, got)).To(Succeed())
		Eventually(replicas, testTimeout, testPoll).Should(Equal(int32(0)), "the RWO volume must be free for the download Pod")
		Eventually(func() string {
			current := getServedModel(ctx, key)
			if c := meta.FindStatusCondition(current.Status.Conditions, servingv1alpha1.ConditionReady); c != nil {
				return string(c.Status)
			}
			return ""
		}, testTimeout, testPoll).Should(Equal(string(metav1.ConditionFalse)))

		var next *batchv1.Job
		Eventually(func() bool {
			next = findDownloadJob(ctx, sm)
			return next != nil && next.Name != job.Name
		}, testTimeout, testPoll).Should(BeTrue())
		markJobComplete(ctx, next)
		Eventually(replicas, testTimeout, testPoll).Should(Equal(int32(1)))
	})

	It("rejects a ServedModel whose image is not pinned by digest", func() {
		sm := newServedModel(ns, "unpinned", true)
		sm.Spec.Engine.Image = "vllm/vllm-openai:latest"
		err := k8sClient.Create(ctx, sm)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.engine.image"))
	})

	Context("with a Ready model", func() {
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
		requestMeasure := func(sm *servingv1alpha1.ServedModel, key types.NamespacedName) *batchv1.Job {
			sm.Annotations = map[string]string{servingv1alpha1.MeasureAnnotation: "true"}
			ExpectWithOffset(1, k8sClient.Update(ctx, sm)).To(Succeed())
			job := &batchv1.Job{}
			jobKey := types.NamespacedName{Namespace: ns, Name: measureJobName(sm)}
			EventuallyWithOffset(1, func() error { return k8sClient.Get(ctx, jobKey, job) }, testTimeout, testPoll).Should(Succeed())
			return job
		}

		It("keeps the model Ready and reports the reason when the measurement refuses to run", func() {
			sm, key := ready("busy")
			job := requestMeasure(sm, key)
			pod := createMeasurePod(ctx, sm, job.Name)
			finishMeasure(ctx, pod, 3, `{"ok":false,"reason":"EngineBusy","message":"처리 중 2, 대기 0"}`)
			markJobFailed(ctx, job)
			Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal("EngineBusy"))
			got := getServedModel(ctx, key)
			Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseReady))
			Expect(got.Status.Baseline).To(BeNil())
			measured := meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionMeasured)
			Expect(measured).NotTo(BeNil())
			Expect(measured.Status).To(Equal(metav1.ConditionFalse))
			ready := meta.FindStatusCondition(got.Status.Conditions, servingv1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})

		It("reports a failed Job with no result instead of inventing a baseline", func() {
			sm, key := ready("killed")
			job := requestMeasure(sm, key)
			createMeasurePod(ctx, sm, job.Name)
			markJobFailed(ctx, job)
			Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal("MeasureFailed"))
			Expect(getServedModel(ctx, key).Status.Baseline).To(BeNil())
		})

		It("marks a run that ended early as partial", func() {
			sm, key := ready("partial")
			job := requestMeasure(sm, key)
			pod := createMeasurePod(ctx, sm, job.Name)
			finishMeasure(ctx, pod, 0, `{"ok":true,"partial":true,"reason":"MeasuredPartial","message":"끝까지 재지 못했다","maxConcurrency":2,"maxWaiting":0,"maxKvPercent":9}`)
			markJobComplete(ctx, job)
			Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal("MeasuredPartial"))
			got := getServedModel(ctx, key)
			Expect(got.Status.Baseline.Partial).To(BeTrue())
			baseline := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: baselineConfigMapName(sm)}, baseline)).To(Succeed())
			Expect(baseline.Data).To(HaveKeyWithValue("partial", "true"))
		})

		It("does not measure again on its own when the finished Job disappears", func() {
			sm, key := ready("gone")
			job := requestMeasure(sm, key)
			pod := createMeasurePod(ctx, sm, job.Name)
			finishMeasure(ctx, pod, 0, `{"ok":true,"partial":false,"reason":"Measured","message":"x","maxConcurrency":4,"maxWaiting":0,"maxKvPercent":10}`)
			markJobComplete(ctx, job)
			Eventually(func() *servingv1alpha1.BaselineStatus { return getServedModel(ctx, key).Status.Baseline }, testTimeout, testPoll).ShouldNot(BeNil())

			jobKey := client.ObjectKeyFromObject(job)
			Expect(k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
			removeJobFinalizers := func() error {
				current := &batchv1.Job{}
				if err := k8sClient.Get(ctx, jobKey, current); err != nil {
					return client.IgnoreNotFound(err)
				}
				current.Finalizers = nil
				return k8sClient.Update(ctx, current)
			}
			Eventually(removeJobFinalizers, testTimeout, testPoll).Should(Succeed())
			Eventually(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{})) }, testTimeout, testPoll).Should(BeTrue())

			got := getServedModel(ctx, key)
			got.Annotations["touch"] = "1" // wakes the reconcile
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			Consistently(func() bool { return apierrors.IsNotFound(k8sClient.Get(ctx, jobKey, &batchv1.Job{})) }, 2*time.Second, testPoll).Should(BeTrue())
			Expect(getServedModel(ctx, key).Status.Reason).To(Equal("Measured"))

			By("measuring again once the annotation goes and comes back")
			got = getServedModel(ctx, key)
			delete(got.Annotations, servingv1alpha1.MeasureAnnotation)
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			Eventually(func() *metav1.Condition {
				return meta.FindStatusCondition(getServedModel(ctx, key).Status.Conditions, servingv1alpha1.ConditionMeasured)
			}, testTimeout, testPoll).Should(BeNil())
			Expect(getServedModel(ctx, key).Status.Baseline).NotTo(BeNil())
			got = getServedModel(ctx, key)
			got.Annotations[servingv1alpha1.MeasureAnnotation] = "true"
			Expect(k8sClient.Update(ctx, got)).To(Succeed())
			Eventually(func() error { return k8sClient.Get(ctx, jobKey, &batchv1.Job{}) }, testTimeout, testPoll).Should(Succeed())
		})

		It("does not take over a ConfigMap of the same name", func() {
			sm, key := ready("taken")
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: baselineConfigMapName(sm), Namespace: ns},
				Data:       map[string]string{"mine": "yes"},
			}
			Expect(k8sClient.Create(ctx, cm)).To(Succeed())
			job := requestMeasure(sm, key)
			pod := createMeasurePod(ctx, sm, job.Name)
			finishMeasure(ctx, pod, 0, `{"ok":true,"reason":"Measured","message":"x","maxConcurrency":4,"maxWaiting":0,"maxKvPercent":10}`)
			markJobComplete(ctx, job)
			Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal("BaselineConflict"))
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cm), cm)).To(Succeed())
			Expect(cm.Data).To(Equal(map[string]string{"mine": "yes"}))
			Expect(cm.OwnerReferences).To(BeEmpty())
			Expect(getServedModel(ctx, key).Status.Baseline).To(BeNil())
		})
	})
})
