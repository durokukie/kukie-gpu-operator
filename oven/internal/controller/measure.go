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
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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

	reasonMeasureJobCreated = "JobCreated"
	reasonMeasureJobRunning = "JobRunning"
	reasonMeasureJobGone    = "MeasureJobGone"
	reasonMeasureFailed     = "MeasureFailed"
	reasonBaselineConflict  = "BaselineConflict"
	// reasonNoStepWithinSLA comes from measure.py: not even the first step stayed within the SLA.
	reasonNoStepWithinSLA = "NoStepWithinSLA"

	// measureRequestTrue is the value the annotation had before any value counted as a request. Its Job keeps its old name.
	measureRequestTrue = "true"
	// maxMeasureJobName is what a Job's name may be: the name also goes into its Pods' job-name label.
	maxMeasureJobName = 63

	measureResultPrefix = "kukie-gpu-measure-"
	noPassPrefix        = "kukie-gpu-nopass-"
	// measureResultTime is the UTC time in a result's name. Fixed width, so the names of one model sort by time.
	measureResultTime = "20060102150405"
	// measureResultsKept is how many results stay per model name.
	measureResultsKept = 20

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
	// ReuseMaxAge is how old a measurement result may be and still fill status.baseline of a ServedModel that has none.
	// Zero turns reuse off.
	ReuseMaxAge time.Duration
}

func positiveOr(value, def int) int {
	if value > 0 {
		return value
	}
	return def
}

// concurrency is the steps a run goes through, spaced the one way, so the result records them the same from run to run.
func (m MeasureConfig) concurrency() string {
	if steps := strings.Fields(m.Concurrency); len(steps) > 0 {
		return strings.Join(steps, " ")
	}
	return defaultMeasureConcurrency
}

// sla is the limit and request shape a run measures against, as measure.py writes it into its result.
func (m MeasureConfig) sla() measureSLA {
	return measureSLA{
		TTFTMillis:   float64(positiveOr(m.TTFTMillis, defaultMeasureTTFTMillis)),
		ITLMillis:    float64(positiveOr(m.ITLMillis, defaultMeasureITLMillis)),
		InputTokens:  positiveOr(m.InputLen, defaultMeasureInputLen),
		OutputTokens: positiveOr(m.OutputLen, defaultMeasureOutputLen),
	}
}

func (m MeasureConfig) deadlineSeconds() int64 {
	if m.Deadline <= 0 {
		return int64(DefaultMeasureDeadline.Seconds())
	}
	return int64(m.Deadline.Seconds())
}

func shortHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])[:8]
}

// measureJobName is the Job of the request the annotation holds now.
func measureJobName(sm *servingv1alpha1.ServedModel) string {
	return measureJobNameFor(sm, sm.Annotations[servingv1alpha1.MeasureAnnotation])
}

// measureJobNameFor gives each request at each spec a Job of its own. A name too long to fit is cut, with a hash of the
// whole name so that two long names starting alike do not end up sharing a Job.
func measureJobNameFor(sm *servingv1alpha1.ServedModel, request string) string {
	tail := fmt.Sprintf("-measure-%d", sm.Generation)
	if request != "" && request != measureRequestTrue {
		tail += "-" + shortHash(request)
	}
	room := maxMeasureJobName - len(tail)
	if len(sm.Name) <= room {
		return sm.Name + tail
	}
	hash := shortHash(sm.Name)
	return strings.TrimRight(sm.Name[:room-len(hash)-1], "-.") + "-" + hash + tail
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
	concurrency := m.concurrency()
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
		WithAnnotations(map[string]string{servingv1alpha1.MeasureAnnotation: sm.Annotations[servingv1alpha1.MeasureAnnotation]}).
		WithOwnerReferences(ownerRef(sm)).
		WithSpec(batchv1ac.JobSpec().
			WithBackoffLimit(0).
			WithActiveDeadlineSeconds(m.deadlineSeconds()).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(childLabels(sm, componentMeasure)).
				WithSpec(podSpec)))
}

// measureEnded tells an outcome from a run still in progress. Only those two reasons mean in progress.
func measureEnded(cond *metav1.Condition) bool {
	return cond.Reason != reasonMeasureJobCreated && cond.Reason != reasonMeasureJobRunning
}

// measureResult is the JSON measure.py leaves in its termination message.
type measureResult struct {
	OK             bool   `json:"ok"`
	Partial        bool   `json:"partial"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	MaxConcurrency int32  `json:"maxConcurrency"`
	MaxWaiting     int32  `json:"maxWaiting"`
	MaxKvPercent   int32  `json:"maxKvPercent"`
	// SLA is what the run measured against. Only reuse reads it, from a result kept in a ConfigMap.
	SLA *measureSLA `json:"sla,omitempty"`
}

// measureSLA is the "sla" object of measure.py's result.
type measureSLA struct {
	TTFTMillis   float64 `json:"ttftMs"`
	ITLMillis    float64 `json:"itlMs"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
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

// measureHandled reports that the request in the annotation has its outcome at this spec, so nothing may fill the GPU for
// it again. A Measured condition in an ended state with no status.measureRequest comes from an oven older than that field,
// when "true" was the only request there was.
func measureHandled(sm *servingv1alpha1.ServedModel) bool {
	cond := meta.FindStatusCondition(sm.Status.Conditions, servingv1alpha1.ConditionMeasured)
	if cond == nil || cond.ObservedGeneration != sm.Generation || !measureEnded(cond) {
		return false
	}
	request := sm.Annotations[servingv1alpha1.MeasureAnnotation]
	return sm.Status.MeasureRequest == request || (sm.Status.MeasureRequest == "" && request == measureRequestTrue)
}

// measureOutcomeFinal tells an outcome that stands from one another look at the Job can still change. BaselineConflict
// clears once the ConfigMap in the way is gone. MeasureFailed may only mean the Job's end reached us before its Pod's
// termination message did: they come through separate watches.
func measureOutcomeFinal(cond *metav1.Condition) bool {
	return cond.Reason != reasonBaselineConflict && cond.Reason != reasonMeasureFailed
}

// endMeasure records how the request ended. The model is still serving, so the phase returns to Ready either way.
func endMeasure(sm *servingv1alpha1.ServedModel, status metav1.ConditionStatus, reason, message string) {
	sm.Status.MeasureRequest = sm.Annotations[servingv1alpha1.MeasureAnnotation]
	setPhase(sm, servingv1alpha1.PhaseReady, reason, message)
	setCondition(sm, servingv1alpha1.ConditionMeasured, status, reason, message)
}

// listMeasureJobs returns the live Job named wanted, if any, and the other live measurement Jobs of this model.
func (r *ServedModelReconciler) listMeasureJobs(ctx context.Context, sm *servingv1alpha1.ServedModel, wanted string) (*batchv1.Job, []*batchv1.Job, error) {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   componentMeasure,
	}); err != nil {
		return nil, nil, err
	}
	var current *batchv1.Job
	var others []*batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		switch {
		case job.DeletionTimestamp != nil:
		case job.Name == wanted:
			current = job
		default:
			others = append(others, job)
		}
	}
	return current, others, nil
}

// removeMeasureJobs deletes these Jobs. A running one stops. A finished one takes its Pod and so its termination message
// along, so a result nobody has read yet is written down first.
func (r *ServedModelReconciler) removeMeasureJobs(ctx context.Context, sm *servingv1alpha1.ServedModel, jobs []*batchv1.Job) error {
	for _, job := range jobs {
		if err := r.keepFinishedRun(ctx, sm, job); err != nil {
			return err
		}
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

// keepFinishedRun records a good result of a Job about to go. A Job of an older spec is left out: the result would be
// filed under a model and engine it did not measure. Doing this for a run already recorded changes nothing.
func (r *ServedModelReconciler) keepFinishedRun(ctx context.Context, sm *servingv1alpha1.ServedModel, job *batchv1.Job) error {
	request := job.Annotations[servingv1alpha1.MeasureAnnotation]
	if request == "" || job.Name != measureJobNameFor(sm, request) {
		return nil
	}
	if done, failed, _ := jobOutcome(job); !done || failed {
		return nil
	}
	pod, err := r.measurePod(ctx, sm, job.Name)
	if err != nil {
		return err
	}
	result, raw, parsed := parseMeasureResult(pod)
	if !parsed || !result.OK {
		return nil
	}
	_, err = r.recordMeasurement(ctx, sm, job, pod, result, raw)
	return err
}

// withdrawMeasure stops a running measurement and clears the finished one, so asking again measures again, whatever the value.
func (r *ServedModelReconciler) withdrawMeasure(ctx context.Context, sm *servingv1alpha1.ServedModel) error {
	_, jobs, err := r.listMeasureJobs(ctx, sm, "")
	if err != nil {
		return err
	}
	if err := r.removeMeasureJobs(ctx, sm, jobs); err != nil {
		return err
	}
	meta.RemoveStatusCondition(&sm.Status.Conditions, servingv1alpha1.ConditionMeasured)
	sm.Status.MeasureRequest = ""
	return nil
}

func (r *ServedModelReconciler) measurePod(ctx context.Context, sm *servingv1alpha1.ServedModel, jobName string) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   componentMeasure,
		"job-name":                       jobName,
	}); err != nil {
		return nil, err
	}
	return newestLivePod(pods.Items), nil
}

// reconcileMeasure runs the measurement a Ready model asks for and records how it ended. The annotation stays until a
// person removes it. Its value is the request: status.measureRequest holds the value whose measurement has ended, and
// with a Measured condition at this generation that means handled. Nothing fills the GPU again until the value changes,
// the annotation goes and comes back, or the spec changes. A failed measurement does not fail the model, which is still
// serving, so the phase returns to Ready and the Measured condition carries the outcome.
func (r *ServedModelReconciler) reconcileMeasure(ctx context.Context, sm *servingv1alpha1.ServedModel) (ctrl.Result, error) {
	wanted := measureJobName(sm)
	current, others, err := r.listMeasureJobs(ctx, sm, wanted)
	if err != nil {
		return ctrl.Result{}, err
	}
	cond := meta.FindStatusCondition(sm.Status.Conditions, servingv1alpha1.ConditionMeasured)
	if measureHandled(sm) && (current == nil || measureOutcomeFinal(cond)) {
		// With no Job left (deleted by hand, a cleanup tool) this still holds: measuring again would fill the GPU with
		// nobody asking.
		if err := r.removeMeasureJobs(ctx, sm, others); err != nil {
			return ctrl.Result{}, err
		}
		endMeasure(sm, cond.Status, cond.Reason, cond.Message)
		return ctrl.Result{}, nil
	}

	if current == nil {
		// A run was under way at this spec and no Job is left of it. With another Job still there, that one is the run
		// and the annotation has moved on to a new request. JobCreated is excluded: the cache may simply not show the Job
		// we just applied yet.
		if cond != nil && cond.ObservedGeneration == sm.Generation && cond.Reason == reasonMeasureJobRunning && len(others) == 0 {
			msg := "측정 Job " + wanted + "이 끝나기 전에 사라졌다. 다시 재려면 annotation 값을 바꾸거나 지웠다가 다시 단다"
			endMeasure(sm, metav1.ConditionFalse, reasonMeasureJobGone, msg)
			return ctrl.Result{}, nil
		}
		// The new Job goes in before the old one goes out. A watch delivers events in order, so a cache that no longer
		// shows the old Job shows the new one, and the check above cannot mistake that moment for a lost run.
		if err := r.Apply(ctx, r.Config.desiredMeasureJob(sm), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.removeMeasureJobs(ctx, sm, others); err != nil {
			return ctrl.Result{}, err
		}
		setPhase(sm, servingv1alpha1.PhaseMeasuring, reasonMeasureJobCreated, "측정 Job "+wanted+"을 만들었다")
		setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionFalse, reasonMeasureJobCreated, "측정 Job "+wanted)
		return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
	}

	if err := r.removeMeasureJobs(ctx, sm, others); err != nil {
		return ctrl.Result{}, err
	}
	pod, err := r.measurePod(ctx, sm, wanted)
	if err != nil {
		return ctrl.Result{}, err
	}

	done, failed, message := jobOutcome(current)
	if !done {
		verdict := classifyPod(pod)
		reason, msg := reasonMeasureJobRunning, "측정 Job "+wanted+" 실행 중"
		if verdict.failed {
			// Unschedulable or an image problem: say so rather than sit behind "running".
			msg = verdict.reason + ": " + verdict.message
		}
		setPhase(sm, servingv1alpha1.PhaseMeasuring, reason, msg)
		setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionFalse, reason, msg)
		return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
	}

	result, raw, parsed := parseMeasureResult(pod)
	if !failed && parsed && result.OK {
		conflict, err := r.recordMeasurement(ctx, sm, current, pod, result, raw)
		if err != nil {
			return ctrl.Result{}, err
		}
		if conflict != "" {
			endMeasure(sm, metav1.ConditionFalse, reasonBaselineConflict, conflict)
			return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
		}
		endMeasure(sm, metav1.ConditionTrue, result.Reason, result.Message)
		return ctrl.Result{}, nil
	}
	reason, msg := reasonMeasureFailed, message
	// Only a run whose first step ended over the SLA says the server takes less. One stopped before any step ended
	// (client failure, time limit) is partial and says nothing.
	if parsed && result.Reason == reasonNoStepWithinSLA && !result.Partial {
		if err := r.recordNoPass(ctx, sm, current, pod, raw); err != nil {
			return ctrl.Result{}, err
		}
	}
	if parsed {
		reason, msg = result.Reason, result.Message
	} else if msg == "" {
		msg = "측정 Job이 결과를 남기지 않았다"
	}
	endMeasure(sm, metav1.ConditionFalse, reason, msg)
	return ctrl.Result{}, nil
}

// recordMeasurement writes what one good run measured. Its time comes from the Job, so another pass over the same run
// finds the same result name and the same baseline. A baseline in conflict does not hold back the result: that one has a
// name of its own, and the Job's Pod may not be around for long. The GPU type comes from the node the measure Pod ran on:
// the engine's node, through the Pod affinity.
func (r *ServedModelReconciler) recordMeasurement(ctx context.Context, sm *servingv1alpha1.ServedModel, job *batchv1.Job, pod *corev1.Pod, result measureResult, raw string) (conflict string, err error) {
	measuredAt := measureFinishedAt(job)
	gpuType, err := r.gpuTypeOf(ctx, pod.Spec.NodeName)
	if err != nil {
		return "", fmt.Errorf("GPU type: %w", err)
	}
	if err := r.writeMeasureResult(ctx, sm, result, raw, measuredAt, gpuType, jobConcurrency(job), false); err != nil {
		return "", fmt.Errorf("measure result: %w", err)
	}
	conflict, err = r.writeBaseline(ctx, sm, result, raw, measuredAt, "")
	if err != nil {
		return "", fmt.Errorf("baseline: %w", err)
	}
	return conflict, nil
}

// recordNoPass keeps a run that found no step within the SLA, under the conditions it ran at, so that no older result
// under them is reused from now on. A baseline this ServedModel took from a kept result goes too: the run just said the
// server does not take what that result did. A baseline it measured itself stays, as it always has.
func (r *ServedModelReconciler) recordNoPass(ctx context.Context, sm *servingv1alpha1.ServedModel, job *batchv1.Job, pod *corev1.Pod, raw string) error {
	gpuType, err := r.gpuTypeOf(ctx, pod.Spec.NodeName)
	if err != nil {
		return fmt.Errorf("GPU type: %w", err)
	}
	if err := r.writeMeasureResult(ctx, sm, measureResult{}, raw, measureFinishedAt(job), gpuType, jobConcurrency(job), true); err != nil {
		return fmt.Errorf("no-pass record: %w", err)
	}
	if b := sm.Status.Baseline; b == nil || b.ReusedFrom == "" {
		return nil
	}
	cm := &corev1.ConfigMap{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: baselineConfigMapName(sm)}, cm); {
	case err == nil && metav1.IsControlledBy(cm, sm):
		if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
			return err
		}
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	sm.Status.Baseline = nil
	return nil
}

// jobConcurrency is the steps the Job ran with. The flag may have changed since the Job was made: the oven restarts with
// new flags while a Job runs on. Empty (a Job not made by desiredMeasureJob) keeps the result from being reused.
func jobConcurrency(job *batchv1.Job) string {
	for _, container := range job.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "CONCURRENCY" {
				return strings.Join(strings.Fields(env.Value), " ")
			}
		}
	}
	return ""
}

// measureFinishedAt is when the Job ended. A Job that reports Complete has a completion time, a failed one the time of its
// Failed condition; the creation time is there so that the name stays the same from pass to pass even if neither does.
func measureFinishedAt(job *batchv1.Job) metav1.Time {
	if job.Status.CompletionTime != nil {
		return *job.Status.CompletionTime
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue && !cond.LastTransitionTime.IsZero() {
			return cond.LastTransitionTime
		}
	}
	return job.CreationTimestamp
}

func measureResultName(sm *servingv1alpha1.ServedModel, measuredAt metav1.Time) string {
	return measureResultPrefix + sm.Name + "-" + measuredAt.UTC().Format(measureResultTime)
}

func noPassName(sm *servingv1alpha1.ServedModel, measuredAt metav1.Time) string {
	return noPassPrefix + sm.Name + "-" + measuredAt.UTC().Format(measureResultTime)
}

// resultLabel is the label that tells a kept run's kind: a result with a baseline, or a run that found none.
func resultLabel(noPass bool) string {
	if noPass {
		return servingv1alpha1.MeasureNoPassLabel
	}
	return servingv1alpha1.MeasureResultLabel
}

// writeMeasureResult keeps one run for good: no owner reference, so it outlives the ServedModel, and not the label children
// carry, so neither the name check nor anything listing children ever sees it. Create rather than apply: a result, once
// written, does not change. gpuType and measureConcurrency are what reuse compares besides the spec and the SLA in
// baseline.json; an empty gpuType (the node was gone or has none of the labels) keeps the result from being reused.
// noPass writes the record of a run that found no step within the SLA instead, under a label the agent does not read.
func (r *ServedModelReconciler) writeMeasureResult(ctx context.Context, sm *servingv1alpha1.ServedModel, result measureResult, raw string, measuredAt metav1.Time, gpuType, concurrency string, noPass bool) error {
	args := sm.Spec.Engine.Args
	if args == nil {
		args = []string{}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return err
	}
	name := measureResultName(sm, measuredAt)
	if noPass {
		name = noPassName(sm, measuredAt)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sm.Namespace,
			Labels: map[string]string{
				resultLabel(noPass):                    "true",
				servingv1alpha1.MeasureResultNameLabel: sm.Name,
			},
		},
		Data: map[string]string{
			baselineKey:           strings.TrimSpace(raw),
			"model":               sm.Spec.Source.HuggingFace,
			"gpus":                strconv.Itoa(int(gpuCount(sm))),
			"engineImage":         sm.Spec.Engine.Image,
			"engineArgs":          string(argsJSON),
			"measuredAt":          measuredAt.UTC().Format(time.RFC3339),
			"partial":             strconv.FormatBool(result.Partial),
			"maxConcurrency":      strconv.Itoa(int(result.MaxConcurrency)),
			measureGPUTypeKey:     gpuType,
			measureConcurrencyKey: concurrency,
		},
	}
	switch err := r.Create(ctx, cm, client.FieldOwner(FieldManager)); {
	case apierrors.IsAlreadyExists(err):
		return nil
	case err != nil:
		return err
	}
	r.pruneMeasureResults(ctx, sm, resultLabel(noPass))
	return nil
}

// pruneMeasureResults keeps the newest results of this model name and kind. It only tidies up, and the next result tries
// again, so a failure here must not cost the measurement that just succeeded.
func (r *ServedModelReconciler) pruneMeasureResults(ctx context.Context, sm *servingv1alpha1.ServedModel, label string) {
	log := logf.FromContext(ctx)
	results := &corev1.ConfigMapList{}
	if err := r.List(ctx, results, client.InNamespace(sm.Namespace), client.MatchingLabels{
		label:                                  "true",
		servingv1alpha1.MeasureResultNameLabel: sm.Name,
	}); err != nil {
		log.Error(err, "listing old measurement results")
		return
	}
	if len(results.Items) <= measureResultsKept {
		return
	}
	slices.SortFunc(results.Items, func(a, b corev1.ConfigMap) int { return strings.Compare(b.Name, a.Name) })
	for i := range results.Items[measureResultsKept:] {
		old := &results.Items[measureResultsKept+i]
		if err := r.Delete(ctx, old); client.IgnoreNotFound(err) != nil {
			log.Error(err, "removing an old measurement result", "configMap", old.Name)
		}
	}
}

// writeBaseline records the result in the ConfigMap and in status. A ConfigMap of that name that is not ours is left
// alone and reported as conflict: applying over it would take it over, and its owner reference would delete it with the
// ServedModel. reusedFrom names the kept result the numbers were copied from; empty means this ServedModel measured them.
func (r *ServedModelReconciler) writeBaseline(ctx context.Context, sm *servingv1alpha1.ServedModel, result measureResult, raw string, measuredAt metav1.Time, reusedFrom string) (conflict string, err error) {
	name := baselineConfigMapName(sm)
	existing := &corev1.ConfigMap{}
	switch err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: name}, existing); {
	case err == nil && !metav1.IsControlledBy(existing, sm):
		return "같은 이름의 ConfigMap " + name + "이 이미 있고 이 ServedModel 것이 아니다. 지우거나 옮기면 다시 기록한다", nil
	case err != nil && !apierrors.IsNotFound(err):
		return "", err
	}
	data := map[string]string{
		"maxConcurrency": strconv.Itoa(int(result.MaxConcurrency)),
		"maxWaiting":     strconv.Itoa(int(result.MaxWaiting)),
		"maxKvPercent":   strconv.Itoa(int(result.MaxKvPercent)),
		"measuredAt":     measuredAt.UTC().Format(time.RFC3339),
		"generation":     strconv.FormatInt(sm.Generation, 10),
		"partial":        strconv.FormatBool(result.Partial),
		baselineKey:      strings.TrimSpace(raw),
	}
	if reusedFrom != "" {
		data["reusedFrom"] = reusedFrom
	}
	cm := corev1ac.ConfigMap(name, sm.Namespace).
		WithLabels(childLabels(sm, componentMeasure)).
		WithOwnerReferences(ownerRef(sm)).
		WithData(data)
	if err := r.Apply(ctx, cm, client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return "", err
	}
	sm.Status.Baseline = &servingv1alpha1.BaselineStatus{
		MaxConcurrency: result.MaxConcurrency,
		MaxWaiting:     result.MaxWaiting,
		MaxKvPercent:   result.MaxKvPercent,
		MeasuredAt:     &measuredAt,
		Partial:        result.Partial,
		Generation:     sm.Generation,
		ReusedFrom:     reusedFrom,
	}
	return "", nil
}
