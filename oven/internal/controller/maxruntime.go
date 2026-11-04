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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// A model left running keeps a GPU node billing, and nothing outside the cluster can be relied on to notice: the node
// group Lambda does not see Karpenter nodes and the Kukie server may be down. So the oven itself takes the model down;
// with no GPU Pod left, the node autoscaler removes the node.
const (
	// DefaultMaxRuntime is the limit for a ServedModel without spec.maxRuntime.
	DefaultMaxRuntime = 2 * time.Hour
	// DefaultNodeWaitTimeout leaves room for a new GPU node to boot and register its GPU.
	DefaultNodeWaitTimeout = 15 * time.Minute

	reasonMaxRuntimeReached = "MaxRuntimeReached"
	reasonWaitingForNode    = "WaitingForNode"
)

func (r *ServedModelReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// stopTime is when this model stops: startedAt + the limit, or keep-until when that is later. false means never.
// An annotation that does not parse is ignored; status.stopsAt then shows that it changed nothing.
func (c Config) stopTime(sm *servingv1alpha1.ServedModel, startedAt time.Time) (stopsAt time.Time, limited bool, keepUntilErr error) {
	limit := c.MaxRuntime
	if sm.Spec.MaxRuntime != nil {
		limit = sm.Spec.MaxRuntime.Duration
	}
	if limit <= 0 {
		return time.Time{}, false, nil
	}
	stopsAt = startedAt.Add(limit)
	if raw, ok := sm.Annotations[servingv1alpha1.KeepUntilAnnotation]; ok {
		keepUntil, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return stopsAt, true, err
		}
		if keepUntil.After(stopsAt) {
			stopsAt = keepUntil
		}
	}
	return stopsAt, true, nil
}

// trackRuntime records when the model started, once, and when it stops. status holds whole seconds, so what goes in does
// too: a value that reads back different would count as a change on every pass.
func (r *ServedModelReconciler) trackRuntime(ctx context.Context, sm *servingv1alpha1.ServedModel, now time.Time) (time.Time, bool) {
	if sm.Status.StartedAt == nil {
		sm.Status.StartedAt = new(metav1.NewTime(now.Truncate(time.Second)))
	}
	stopsAt, limited, err := r.Config.stopTime(sm, sm.Status.StartedAt.Time)
	if err != nil {
		logf.FromContext(ctx).Info("ignoring an annotation that is not RFC3339",
			"annotation", servingv1alpha1.KeepUntilAnnotation, "error", err.Error())
	}
	sm.Status.StopsAt = nil
	if limited {
		sm.Status.StopsAt = new(metav1.NewTime(stopsAt.Truncate(time.Second)))
	}
	return stopsAt, limited
}

// sooner picks the earlier of two requeue delays. Zero means "no requeue asked for".
func sooner(a, b time.Duration) time.Duration {
	if a == 0 || b < a {
		return b
	}
	return a
}

// reconcilePastLimit stops the model, unless a measurement a person asked for is still running. That one ends by itself
// within --measure-deadline, and stopping the engine under it would only throw the result away. A finished one gets its
// result read first. No new measurement starts here.
func (r *ServedModelReconciler) reconcilePastLimit(ctx context.Context, sm *servingv1alpha1.ServedModel) (ctrl.Result, error) {
	job, err := r.currentMeasureJob(ctx, sm)
	if err != nil {
		return ctrl.Result{}, err
	}
	if job != nil {
		done, _, _ := jobOutcome(job)
		cond := meta.FindStatusCondition(sm.Status.Conditions, servingv1alpha1.ConditionMeasured)
		if !done || cond == nil || cond.ObservedGeneration != sm.Generation || !measureEnded(cond) {
			result, err := r.reconcileMeasure(ctx, sm)
			switch {
			case err != nil && (!done || !isRejected(err)):
				return ctrl.Result{}, err
			case err != nil:
				// The result cannot be written and asking again will not help. It must not keep the GPU running.
				logf.FromContext(ctx).Error(err, "stopping without the measurement result")
			case !done:
				result.RequeueAfter = sooner(result.RequeueAfter, requeueWhileChanging)
				return result, nil
			}
		}
	}
	return ctrl.Result{}, r.stop(ctx, sm)
}

// currentMeasureJob returns this spec's measurement Job when one is asked for and exists.
func (r *ServedModelReconciler) currentMeasureJob(ctx context.Context, sm *servingv1alpha1.ServedModel) (*batchv1.Job, error) {
	if sm.Annotations[servingv1alpha1.MeasureAnnotation] != "true" {
		return nil, nil
	}
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: measureJobName(sm)}, job)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(job, sm) || job.DeletionTimestamp != nil {
		return nil, nil
	}
	return job, nil
}

// stop takes the workloads down and records it. The phase changes only after that worked, so a passing API error is
// retried from the top. What a failed model last said stays in the message: the person coming back needs it.
func (r *ServedModelReconciler) stop(ctx context.Context, sm *servingv1alpha1.ServedModel) error {
	if err := r.stopWorkloads(ctx, sm); err != nil {
		return err
	}
	message := "실행 시간 상한이 지나 다운로드와 vLLM을 멈췄다. 모델 파일(PVC)은 남아 있다. 다시 시작하려면 이 ServedModel을 지우고 다시 만든다"
	if sm.Status.Phase == servingv1alpha1.PhaseFailed {
		message += "\n직전 상태 " + sm.Status.Reason + ": " + sm.Status.Message
	}
	setPhase(sm, servingv1alpha1.PhaseStopped, reasonMaxRuntimeReached, message)
	setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionFalse, reasonMaxRuntimeReached, message)
	if cond := meta.FindStatusCondition(sm.Status.Conditions, servingv1alpha1.ConditionDownloaded); cond != nil && cond.Status != metav1.ConditionTrue {
		setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, reasonMaxRuntimeReached, message)
	}
	if cond := meta.FindStatusCondition(sm.Status.Conditions, servingv1alpha1.ConditionMeasured); cond != nil && !measureEnded(cond) {
		setCondition(sm, servingv1alpha1.ConditionMeasured, metav1.ConditionFalse, reasonMaxRuntimeReached, message)
	}
	return nil
}

// keepStopped holds a Stopped model down whatever happens to its spec or annotations; starting again means creating it
// again. The one thing still followed is retainOnDelete: that setting decides whether the files survive the delete that
// comes next, so it has to work now of all times. A PVC that is gone is not created again.
func (r *ServedModelReconciler) keepStopped(ctx context.Context, sm *servingv1alpha1.ServedModel) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: pvcName(sm)}, pvc)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case pvc.Labels[servingv1alpha1.ServedModelLabel] == sm.Name && pvc.DeletionTimestamp == nil &&
		(metav1.IsControlledBy(pvc, sm) || !isLeftover(pvc, sm)):
		if err := r.Apply(ctx, desiredPVCMetadata(sm), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return err
		}
	}
	return r.stopWorkloads(ctx, sm)
}

// stopWorkloads removes everything of this model that can hold a node: Jobs that have not finished and the vLLM replica.
// A finished download Job stays as the marker it always is; its Pod no longer holds anything.
func (r *ServedModelReconciler) stopWorkloads(ctx context.Context, sm *servingv1alpha1.ServedModel) error {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
	}); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if done, _, _ := jobOutcome(job); done || job.DeletionTimestamp != nil || !metav1.IsControlledBy(job, sm) {
			continue
		}
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	_, err := r.scaleEngineToZero(ctx, sm)
	return err
}
