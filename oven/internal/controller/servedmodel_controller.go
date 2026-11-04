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

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// requeueWhileChanging is how often we look again while a model is downloading, loading or failed.
// Pod and Job watches wake us earlier; this is the safety net the design calls "requeue 30s".
const requeueWhileChanging = 30 * time.Second

// leftoverRequeue is how soon we look again while garbage collection clears what a deleted ServedModel of the same
// name left behind. Nothing wakes us when that happens: those objects are not owned by this ServedModel.
const leftoverRequeue = 5 * time.Second

// ServedModelReconciler keeps one ServedModel's PVC, download Job, vLLM Deployment and Service in the shape the spec
// asks for, and writes what it sees into status. Every child carries an ownerReference, so deleting the ServedModel
// deletes them through garbage collection; only the PVC stays when spec.storage.retainOnDelete is true.
type ServedModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
	// Now reads the clock. Empty means time.Now; tests set it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=serving.kukie.io,resources=servedmodels,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.kukie.io,resources=servedmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.kukie.io,resources=servedmodels/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch

// Reconcile moves the ServedModel one step along Pending → Downloading → Loading → Ready (or Failed), or to Stopped when
// its run time is up, and records the result in status. It is idempotent: every child is server-side applied to its
// desired shape.
func (r *ServedModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	sm := &servingv1alpha1.ServedModel{}
	if err := r.Get(ctx, req.NamespacedName, sm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sm.DeletionTimestamp.IsZero() {
		// Children go with the owner through garbage collection; nothing to do here.
		return ctrl.Result{}, nil
	}

	before := sm.DeepCopy()
	result, reconcileErr := r.reconcile(ctx, sm)
	// On a passing error the phase and conditions stay as they were (an API hiccup is not a model failure) and
	// observedGeneration stays too: this spec has not been acted on yet. The error is logged by controller-runtime and
	// the request retried with backoff. An API server that refuses what we send will refuse it again, so that one
	// goes to status, where the user can read it.
	switch {
	case reconcileErr == nil:
		sm.Status.ObservedGeneration = sm.Generation
	case isRejected(reconcileErr) && sm.Status.Phase != servingv1alpha1.PhaseStopped:
		setPhase(sm, servingv1alpha1.PhaseFailed, "ApplyRejected", reconcileErr.Error())
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionFalse, "ApplyRejected", reconcileErr.Error())
		sm.Status.ObservedGeneration = sm.Generation
	}

	if !equality.Semantic.DeepEqual(before.Status, sm.Status) {
		if err := r.Status().Patch(ctx, sm, client.MergeFrom(before)); err != nil {
			if reconcileErr != nil {
				log.Error(err, "status patch failed after reconcile error")
				return ctrl.Result{}, reconcileErr
			}
			return ctrl.Result{}, err
		}
	}
	return result, reconcileErr
}

// reconcile does the work and mutates sm.Status; Reconcile persists it. The run time limit comes first: nothing below may
// start anything once the time is up.
func (r *ServedModelReconciler) reconcile(ctx context.Context, sm *servingv1alpha1.ServedModel) (ctrl.Result, error) {
	if sm.Status.Phase == servingv1alpha1.PhaseStopped {
		return ctrl.Result{}, r.keepStopped(ctx, sm)
	}
	now := r.now()
	stopsAt, limited := r.trackRuntime(ctx, sm, now)
	if limited && !now.Before(stopsAt) {
		return r.reconcilePastLimit(ctx, sm)
	}
	result, err := r.reconcileServing(ctx, sm)
	if err != nil || !limited {
		return result, err
	}
	// A Ready model has no timer of its own and no event will come when the time is up.
	result.RequeueAfter = sooner(result.RequeueAfter, stopsAt.Sub(now))
	return result, nil
}

// reconcileServing keeps the model running: PVC, download, engine, measurement, and what the Pods say about them.
func (r *ServedModelReconciler) reconcileServing(ctx context.Context, sm *servingv1alpha1.ServedModel) (ctrl.Result, error) {
	if sm.Status.Phase == "" {
		setPhase(sm, servingv1alpha1.PhasePending, "Created", "PVC와 다운로드 Job을 만든다")
	}

	// Before anything is created or downloaded: applying with ForceOwnership would take over an object someone else
	// made, and its owner reference would then delete that object together with the ServedModel.
	conflict, leftover, err := r.findNameConflict(ctx, sm)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("name check: %w", err)
	}
	if leftover != "" {
		setPhase(sm, servingv1alpha1.PhasePending, "WaitingForOldResources", leftover)
		return ctrl.Result{RequeueAfter: leftoverRequeue}, nil
	}
	if conflict != "" {
		setPhase(sm, servingv1alpha1.PhaseFailed, "NameConflict", conflict)
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionFalse, "NameConflict", conflict)
		return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
	}

	if err := r.reconcilePVC(ctx, sm); err != nil {
		return ctrl.Result{}, fmt.Errorf("PVC: %w", err)
	}

	downloaded, err := r.reconcileDownload(ctx, sm)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("download job: %w", err)
	}
	if !downloaded {
		if err := r.stopEngine(ctx, sm); err != nil {
			return ctrl.Result{}, fmt.Errorf("engine: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
	}

	if err := r.reconcileEngine(ctx, sm); err != nil {
		return ctrl.Result{}, fmt.Errorf("engine: %w", err)
	}

	verdict, err := r.observePods(ctx, sm, componentEngine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("engine pods: %w", err)
	}
	measuring := sm.Annotations[servingv1alpha1.MeasureAnnotation] == "true"
	if !measuring {
		// The request was withdrawn: stop a running measurement and clear the finished one, so asking again measures again.
		if _, err := r.removeMeasureJobs(ctx, sm, ""); err != nil {
			return ctrl.Result{}, fmt.Errorf("measure job: %w", err)
		}
		meta.RemoveStatusCondition(&sm.Status.Conditions, servingv1alpha1.ConditionMeasured)
	}
	switch {
	case verdict.ready && measuring:
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionTrue, verdict.reason, verdict.message)
		result, err := r.reconcileMeasure(ctx, sm)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("measure job: %w", err)
		}
		return result, nil
	case verdict.ready:
		setPhase(sm, servingv1alpha1.PhaseReady, verdict.reason, verdict.message)
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionTrue, verdict.reason, verdict.message)
		return ctrl.Result{}, nil
	case verdict.waitingForNode:
		setPhase(sm, servingv1alpha1.PhasePending, reasonWaitingForNode, verdict.message)
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionFalse, reasonWaitingForNode, verdict.message)
	case verdict.failed:
		setPhase(sm, servingv1alpha1.PhaseFailed, verdict.reason, verdict.message)
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionFalse, verdict.reason, verdict.message)
	default:
		setPhase(sm, servingv1alpha1.PhaseLoading, verdict.reason, verdict.message)
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionFalse, verdict.reason, verdict.message)
	}
	return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
}

// reconcilePVC creates the PVC spec once and keeps its labels and owner reference in shape on every pass.
func (r *ServedModelReconciler) reconcilePVC(ctx context.Context, sm *servingv1alpha1.ServedModel) error {
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: pvcName(sm)}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Apply(ctx, desiredPVCSpec(sm), client.FieldOwner(pvcSpecFieldManager), client.ForceOwnership); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return r.Apply(ctx, desiredPVCMetadata(sm), client.FieldOwner(FieldManager), client.ForceOwnership)
}

// reconcileDownload makes sure the download Job for the current source exists, removes Jobs for older sources,
// and reports whether the files are on the PVC. A finished Job is kept as the marker: the spec hash in its name
// means a changed source starts a new download and an unchanged one never downloads twice.
func (r *ServedModelReconciler) reconcileDownload(ctx context.Context, sm *servingv1alpha1.ServedModel) (bool, error) {
	wanted := r.Config.downloadJobName(sm)

	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   componentDownload,
	}); err != nil {
		return false, err
	}
	var current *batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Name == wanted {
			current = job
			continue
		}
		if job.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}

	if current == nil {
		if err := r.Apply(ctx, r.Config.desiredDownloadJob(sm), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
			return false, err
		}
		sm.Status.Download = &servingv1alpha1.DownloadStatus{Percent: 0}
		setPhase(sm, servingv1alpha1.PhaseDownloading, "JobCreated", "허깅페이스에서 "+sm.Spec.Source.HuggingFace+" 받는 중")
		setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, "JobCreated", "다운로드 Job "+wanted)
		return false, nil
	}

	done, failed, message := jobOutcome(current)
	switch {
	case done && failed:
		sm.Status.Download = &servingv1alpha1.DownloadStatus{Percent: 0}
		// The Job condition only says the retries ran out; the last attempt's exit code is on its Pod.
		if verdict, err := r.observePods(ctx, sm, componentDownload); err != nil {
			return false, err
		} else if (verdict.failed || verdict.waitingForNode) && verdict.message != "" {
			message = strings.TrimSpace(message + "\n" + verdict.message)
		}
		setPhase(sm, servingv1alpha1.PhaseFailed, "DownloadFailed", message)
		setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, "DownloadFailed", message)
		return false, nil
	case done:
		sm.Status.Download = &servingv1alpha1.DownloadStatus{Percent: 100}
		setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionTrue, "JobComplete", "다운로드 Job "+wanted+" 완료")
		return true, nil
	default:
		sm.Status.Download = &servingv1alpha1.DownloadStatus{Percent: 0}
		// The Job's own conditions say nothing until it ends, so look at its Pod: an unschedulable Pod (no node,
		// PVC not bound) or an image problem would otherwise sit behind "Downloading" with no signal. Unschedulable
		// starts as Pending/WaitingForNode (holdForNode).
		// A Pod that already ended (restartPolicy Never) is one failed attempt the Job retries on its own
		// (backoffLimit); that stays Downloading with the exit code shown, and only the Job's Failed condition
		// above turns it into Failed.
		verdict, err := r.observePods(ctx, sm, componentDownload)
		if err != nil {
			return false, err
		}
		switch {
		case verdict.waitingForNode:
			setPhase(sm, servingv1alpha1.PhasePending, reasonWaitingForNode, verdict.message)
			setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, reasonWaitingForNode, verdict.message)
		case verdict.failed && !verdict.ended:
			setPhase(sm, servingv1alpha1.PhaseFailed, verdict.reason, verdict.message)
			setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, verdict.reason, verdict.message)
		case verdict.failed:
			msg := "다운로드 시도 실패, Job이 다시 시도 중: " + verdict.message
			setPhase(sm, servingv1alpha1.PhaseDownloading, "Retrying", msg)
			setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, "Retrying", msg)
		default:
			setPhase(sm, servingv1alpha1.PhaseDownloading, "JobRunning", "허깅페이스에서 "+sm.Spec.Source.HuggingFace+" 받는 중")
			setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, "JobRunning", "다운로드 Job "+wanted)
		}
		return false, nil
	}
}

// reconcileEngine applies the vLLM Deployment and its Service.
func (r *ServedModelReconciler) reconcileEngine(ctx context.Context, sm *servingv1alpha1.ServedModel) error {
	if err := r.Apply(ctx, r.Config.desiredDeployment(sm), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if err := r.Apply(ctx, desiredService(sm), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	return nil
}

// stopEngine scales a running engine to zero while a new download is due. The PVC is ReadWriteOnce: a vLLM Pod on another
// node would keep the download Pod in ContainerCreating (Multi-Attach), and the old model is not what the spec asks for.
// reconcileEngine brings the replica back once the download is done.
func (r *ServedModelReconciler) stopEngine(ctx context.Context, sm *servingv1alpha1.ServedModel) error {
	scaled, err := r.scaleEngineToZero(ctx, sm)
	if err != nil {
		return err
	}
	if scaled {
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionFalse, "Redownloading", "새 다운로드 동안 vLLM을 0대로 내렸다")
	}
	return nil
}

// scaleEngineToZero changes the replica count alone, so a spec the API server would refuse cannot keep a GPU Pod running.
// It reports whether it had anything to do. A Deployment that is not ours is left alone.
func (r *ServedModelReconciler) scaleEngineToZero(ctx context.Context, sm *servingv1alpha1.ServedModel) (bool, error) {
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: sm.Name}, existing)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !metav1.IsControlledBy(existing, sm) || (existing.Spec.Replicas != nil && *existing.Spec.Replicas == 0) {
		return false, nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Replicas = new(int32(0))
	if err := r.Patch(ctx, existing, patch, client.FieldOwner(FieldManager)); err != nil {
		return false, err
	}
	return true, nil
}

// findNameConflict looks at the names this ServedModel needs. conflict is set when one is taken by someone else's object.
// leftover is set when the object is ours from a deleted ServedModel of the same name, on its way out: delete and create
// again (kubectl replace --force, a GitOps resync) gets here. Applying over it would lose to garbage collection, so the
// caller waits. The PVC goes by label, not owner: with retainOnDelete it has no owner, and keeping it for the next
// ServedModel of the same name is the point of that setting.
func (r *ServedModelReconciler) findNameConflict(ctx context.Context, sm *servingv1alpha1.ServedModel) (conflict, leftover string, err error) {
	key := types.NamespacedName{Namespace: sm.Namespace, Name: sm.Name}
	for _, child := range []struct {
		kind string
		obj  client.Object
	}{
		{"Deployment", &appsv1.Deployment{}},
		{"Service", &corev1.Service{}},
	} {
		switch err := r.Get(ctx, key, child.obj); {
		case apierrors.IsNotFound(err):
			continue
		case err != nil:
			return "", "", err
		}
		if metav1.IsControlledBy(child.obj, sm) && child.obj.GetDeletionTimestamp() == nil {
			continue
		}
		if isLeftover(child.obj, sm) {
			return "", "같은 이름의 예전 ServedModel이 남긴 " + child.kind + " " + sm.Name + "이 지워지기를 기다린다", nil
		}
		return "같은 이름의 " + child.kind + " " + sm.Name + "이 이미 있고 이 ServedModel 것이 아니다. ServedModel 이름을 바꾸거나 그 " + child.kind + "를 지운다", "", nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sm.Namespace, Name: pvcName(sm)}, pvc); err == nil {
		if pvc.Labels[servingv1alpha1.ServedModelLabel] != sm.Name {
			return "같은 이름의 PVC " + pvcName(sm) + "가 이미 있고 이 ServedModel 것이 아니다. ServedModel 이름을 바꾸거나 그 PVC를 지운다", "", nil
		}
		// The PVC outlives the Deployment: pvc-protection holds it until the engine Pod is gone. Only a PVC of the
		// deleted ServedModel is worth waiting for. Our own PVC deleted by hand stays Terminating for as long as our
		// engine runs, and one without an owner (retainOnDelete) is not garbage collection's to remove.
		if !metav1.IsControlledBy(pvc, sm) && isLeftover(pvc, sm) {
			return "", "같은 이름의 예전 ServedModel이 남긴 PVC " + pvcName(sm) + "가 지워지기를 기다린다", nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", "", err
	}
	// A finished Job of the deleted ServedModel must not count as "already downloaded" (the new PVC may be empty) or
	// as "already measured".
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
	}); err != nil {
		return "", "", err
	}
	for i := range jobs.Items {
		if job := &jobs.Items[i]; !metav1.IsControlledBy(job, sm) && isLeftover(job, sm) {
			return "", "같은 이름의 예전 ServedModel이 남긴 Job " + job.Name + "이 지워지기를 기다린다", nil
		}
	}
	return "", "", nil
}

// isLeftover reports an object that belongs to a ServedModel of this name but not to this one, or is being deleted while
// still ours. Garbage collection may not have marked it yet, so a missing DeletionTimestamp does not make it someone else's.
func isLeftover(obj client.Object, sm *servingv1alpha1.ServedModel) bool {
	owner := metav1.GetControllerOf(obj)
	if owner == nil {
		return false
	}
	return owner.Kind == "ServedModel" && owner.Name == sm.Name && strings.HasPrefix(owner.APIVersion, servingv1alpha1.GroupVersion.Group+"/")
}

// isRejected tells an answer the API server will repeat (invalid spec, forbidden, bad request) from a passing error.
func isRejected(err error) bool {
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err)
}

// observePods reads the Pods of one component (engine or download) and turns the newest one into a verdict.
func (r *ServedModelReconciler) observePods(ctx context.Context, sm *servingv1alpha1.ServedModel, component string) (podVerdict, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   component,
	}); err != nil {
		return podVerdict{}, err
	}
	return holdForNode(classifyPod(newestLivePod(pods.Items)), r.now(), r.Config.NodeWaitTimeout), nil
}

func setPhase(sm *servingv1alpha1.ServedModel, phase servingv1alpha1.ServedModelPhase, reason, message string) {
	sm.Status.Phase = phase
	sm.Status.Reason = reason
	sm.Status.Message = message
}

func setCondition(sm *servingv1alpha1.ServedModel, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&sm.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: sm.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServedModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.ServedModel{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// Engine, download and measure Pods are owned by a ReplicaSet or Job, not by us; map them back through the label
		// their templates carry.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
			labels := obj.GetLabels()
			name, ok := labels[servingv1alpha1.ServedModelLabel]
			if !ok || (labels[componentLabel] != componentEngine && labels[componentLabel] != componentDownload && labels[componentLabel] != componentMeasure) {
				return nil
			}
			return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
		})).
		Named("servedmodel").
		Complete(r)
}
