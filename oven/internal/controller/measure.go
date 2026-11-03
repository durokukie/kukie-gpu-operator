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
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// measureScript runs inside the engine image; see its docstring for the contract with this file.
//
//go:embed measure.py
var measureScript string

const (
	componentMeasure = "measure"
	// baselineKey is the ConfigMap key holding the whole result as JSON.
	baselineKey = "baseline.json"

	// DefaultMeasureDeadline bounds one measurement. It fills the GPU, so it must not run forever.
	DefaultMeasureDeadline    = time.Hour
	defaultMeasureConcurrency = "1 2 4 8 16 32 64"
	defaultMeasureInputLen    = 1000
	defaultMeasureOutputLen   = 200
	defaultMeasureTTFTMillis  = 1000
	defaultMeasureITLMillis   = 50
)

var (
	measureCPURequest    = resource.MustParse("500m")
	measureMemoryRequest = resource.MustParse("1Gi")
	measureMemoryLimit   = resource.MustParse("4Gi")
)

// MeasureConfig is the shape and limit of a measurement. Zero values mean the defaults.
type MeasureConfig struct {
	Deadline    time.Duration
	Concurrency string
	InputLen    int
	OutputLen   int
	TTFTMillis  int
	ITLMillis   int
}

func positiveOr(value, def int) int {
	if value > 0 {
		return value
	}
	return def
}

func (m MeasureConfig) deadlineSeconds() int64 {
	if m.Deadline <= 0 {
		return int64(DefaultMeasureDeadline.Seconds())
	}
	return int64(m.Deadline.Seconds())
}

func measureJobName(sm *servingv1alpha1.ServedModel) string {
	return fmt.Sprintf("%s-measure-%d", sm.Name, sm.Generation)
}

// baselineConfigMapName is the ConfigMap the admission proxy will read through spec.admission.baselineRef.
func baselineConfigMapName(sm *servingv1alpha1.ServedModel) string {
	return "kukie-gpu-baseline-" + sm.Name
}

// desiredMeasureJob runs measure.py once. It sits on the engine's node: that node already has the engine image and
// the model's tokenizer on the PVC, whose ReadWriteOnce volume no other node could mount, and stage 1 measured with the
// client next to the server too. It asks for no GPU and gets no Kubernetes token; the result leaves through the
// termination message. backoffLimit is 0: a repeat would only fill the GPU again for the same answer.
func (c Config) desiredMeasureJob(sm *servingv1alpha1.ServedModel) *batchv1ac.JobApplyConfiguration {
	m := c.Measure
	concurrency := m.Concurrency
	if concurrency == "" {
		concurrency = defaultMeasureConcurrency
	}
	env := func(name, value string) *corev1ac.EnvVarApplyConfiguration {
		return corev1ac.EnvVar().WithName(name).WithValue(value)
	}
	container := corev1ac.Container().
		WithName("measure").
		WithImage(sm.Spec.Engine.Image).
		WithCommand("python3", "-c", measureScript).
		WithEnv(
			env("BASE_URL", fmt.Sprintf("http://%s:%d", sm.Name, engineServicePort)),
			env("MODEL", sm.Spec.Source.HuggingFace),
			env("OUT", "/out"),
			env("CONCURRENCY", concurrency),
			env("INPUT_LEN", strconv.Itoa(positiveOr(m.InputLen, defaultMeasureInputLen))),
			env("OUTPUT_LEN", strconv.Itoa(positiveOr(m.OutputLen, defaultMeasureOutputLen))),
			env("TTFT_MS", strconv.Itoa(positiveOr(m.TTFTMillis, defaultMeasureTTFTMillis))),
			env("ITL_MS", strconv.Itoa(positiveOr(m.ITLMillis, defaultMeasureITLMillis))),
			env("DEADLINE_SECONDS", strconv.FormatInt(m.deadlineSeconds(), 10)),
			// The client needs the tokenizer. It is on the PVC, so it never has to reach Hugging Face.
			env("HF_HOME", modelsMountPath),
			env("HF_HUB_OFFLINE", "1"),
		).
		WithResources(corev1ac.ResourceRequirements().
			WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    measureCPURequest,
				corev1.ResourceMemory: measureMemoryRequest,
			}).
			WithLimits(corev1.ResourceList{corev1.ResourceMemory: measureMemoryLimit})).
		WithVolumeMounts(
			corev1ac.VolumeMount().WithName("models").WithMountPath(modelsMountPath).WithReadOnly(true),
			corev1ac.VolumeMount().WithName("out").WithMountPath("/out"),
		)

	podSpec := corev1ac.PodSpec().
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithAutomountServiceAccountToken(false).
		WithTolerations(gpuToleration()).
		WithAffinity(corev1ac.Affinity().WithPodAffinity(corev1ac.PodAffinity().
			WithRequiredDuringSchedulingIgnoredDuringExecution(corev1ac.PodAffinityTerm().
				WithTopologyKey(corev1.LabelHostname).
				WithLabelSelector(metav1ac.LabelSelector().WithMatchLabels(engineSelector(sm)))))).
		WithContainers(container).
		WithVolumes(
			corev1ac.Volume().WithName("models").
				WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(pvcName(sm)).WithReadOnly(true)),
			corev1ac.Volume().WithName("out").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
		)
	if len(c.GPUNodeSelector) > 0 {
		podSpec = podSpec.WithNodeSelector(c.GPUNodeSelector)
	}

	return batchv1ac.Job(measureJobName(sm), sm.Namespace).
		WithLabels(childLabels(sm, componentMeasure)).
		WithOwnerReferences(ownerRef(sm)).
		WithSpec(batchv1ac.JobSpec().
			WithBackoffLimit(0).
			WithActiveDeadlineSeconds(m.deadlineSeconds()).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(childLabels(sm, componentMeasure)).
				WithSpec(podSpec)))
}

// measureResult is the JSON measure.py leaves in its termination message.
type measureResult struct {
	OK             bool   `json:"ok"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	MaxConcurrency int32  `json:"maxConcurrency"`
	MaxWaiting     int32  `json:"maxWaiting"`
	MaxKvPercent   int32  `json:"maxKvPercent"`
}

// parseMeasureResult reads the termination message of a finished measure Pod.
func parseMeasureResult(pod *corev1.Pod) (measureResult, string, bool) {
	if pod == nil {
		return measureResult{}, "", false
	}
	for i := range pod.Status.ContainerStatuses {
		terminated := pod.Status.ContainerStatuses[i].State.Terminated
		if terminated == nil || terminated.Message == "" {
			continue
		}
		var result measureResult
		if err := json.Unmarshal([]byte(terminated.Message), &result); err == nil && result.Reason != "" {
			return result, terminated.Message, true
		}
	}
	return measureResult{}, "", false
}

// removeMeasureJobs deletes measurement Jobs other than keep. A running one stops; status.baseline stays.
func (r *ServedModelReconciler) removeMeasureJobs(ctx context.Context, sm *servingv1alpha1.ServedModel, keep string) (*batchv1.Job, error) {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   componentMeasure,
	}); err != nil {
		return nil, err
	}
	var current *batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == keep && job.DeletionTimestamp == nil {
			current = job
			continue
		}
		if job.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}
	return current, nil
}

// reconcileMeasure runs the measurement a Ready model asks for and records how it ended. The annotation stays until
// a person removes it, so a finished Job is the marker that this spec has been measured: nothing fills the GPU again
// until the annotation goes and comes back, or the spec changes. A failed measurement does not fail the model, which
// is still serving, so the phase returns to Ready and the Measured condition carries the outcome.
func (r *ServedModelReconciler) reconcileMeasure(ctx context.Context, sm *servingv1alpha1.ServedModel) (ctrl.Result, error) {
	wanted := measureJobName(sm)
	current, err := r.removeMeasureJobs(ctx, sm, wanted)
	if err != nil {
		return ctrl.Result{}, err
	}
	if current == nil {
		if err := r.Apply(ctx, r.Config.desiredMeasureJob(sm), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
		setPhase(sm, servingv1alpha1.PhaseMeasuring, "JobCreated", "측정 Job "+wanted+"을 만들었다")
		setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionFalse, "JobCreated", "측정 Job "+wanted)
		return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   componentMeasure,
		"job-name":                       wanted,
	}); err != nil {
		return ctrl.Result{}, err
	}
	pod := newestLivePod(pods.Items)

	done, failed, message := jobOutcome(current)
	if !done {
		verdict := classifyPod(pod)
		reason, msg := "JobRunning", "측정 Job "+wanted+" 실행 중"
		if verdict.failed {
			// Unschedulable or an image problem: say so rather than sit behind "running".
			reason, msg = verdict.reason, verdict.message
		}
		setPhase(sm, servingv1alpha1.PhaseMeasuring, reason, msg)
		setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionFalse, reason, msg)
		return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
	}

	result, raw, parsed := parseMeasureResult(pod)
	if !failed && parsed && result.OK {
		conflict, err := r.writeBaseline(ctx, sm, result, raw)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("baseline: %w", err)
		}
		if conflict != "" {
			setPhase(sm, servingv1alpha1.PhaseReady, "BaselineConflict", conflict)
			setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionFalse, "BaselineConflict", conflict)
			return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
		}
		setPhase(sm, servingv1alpha1.PhaseReady, result.Reason, result.Message)
		setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionTrue, result.Reason, result.Message)
		return ctrl.Result{}, nil
	}
	// The Pod may be gone by the time we look again; what was recorded for this spec still stands.
	if !failed && !parsed && sm.Status.Baseline != nil && sm.Status.Baseline.Generation == sm.Generation {
		setPhase(sm, servingv1alpha1.PhaseReady, "Measured", "측정 결과는 status.baseline에 있다")
		return ctrl.Result{}, nil
	}
	reason, msg := "MeasureFailed", message
	if parsed {
		reason, msg = result.Reason, result.Message
	} else if msg == "" {
		msg = "측정 Job이 결과를 남기지 않았다"
	}
	setPhase(sm, servingv1alpha1.PhaseReady, reason, msg)
	setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionFalse, reason, msg)
	return ctrl.Result{}, nil
}

// writeBaseline records the result in the ConfigMap and in status. A ConfigMap of that name that is not ours is left
// alone and reported as conflict: applying over it would take it over, and its owner reference would delete it with the
// ServedModel.
func (r *ServedModelReconciler) writeBaseline(ctx context.Context, sm *servingv1alpha1.ServedModel, result measureResult, raw string) (conflict string, err error) {
	name := baselineConfigMapName(sm)
	existing := &corev1.ConfigMap{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: name}, existing); {
	case err == nil && !metav1.IsControlledBy(existing, sm):
		return "같은 이름의 ConfigMap " + name + "이 이미 있고 이 ServedModel 것이 아니다. 지우거나 옮기면 다시 기록한다", nil
	case err != nil && !apierrors.IsNotFound(err):
		return "", err
	}
	measuredAt := metav1.Now()
	if sm.Status.Baseline != nil && sm.Status.Baseline.Generation == sm.Generation && sm.Status.Baseline.MeasuredAt != nil {
		measuredAt = *sm.Status.Baseline.MeasuredAt // A later pass over the same finished Job is not a new measurement.
	}
	cm := corev1ac.ConfigMap(name, sm.Namespace).
		WithLabels(childLabels(sm, componentMeasure)).
		WithOwnerReferences(ownerRef(sm)).
		WithData(map[string]string{
			"maxConcurrency": strconv.Itoa(int(result.MaxConcurrency)),
			"maxWaiting":     strconv.Itoa(int(result.MaxWaiting)),
			"maxKvPercent":   strconv.Itoa(int(result.MaxKvPercent)),
			"measuredAt":     measuredAt.UTC().Format(time.RFC3339),
			"generation":     strconv.FormatInt(sm.Generation, 10),
			baselineKey:      strings.TrimSpace(raw),
		})
	if err := r.Apply(ctx, cm, client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return "", err
	}
	sm.Status.Baseline = &servingv1alpha1.BaselineStatus{
		MaxConcurrency: result.MaxConcurrency,
		MaxWaiting:     result.MaxWaiting,
		MaxKvPercent:   result.MaxKvPercent,
		MeasuredAt:     &measuredAt,
		Generation:     sm.Generation,
	}
	return "", nil
}
