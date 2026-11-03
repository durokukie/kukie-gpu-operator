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

	servingv1alpha1 "github.com/durokukie/kukie-gpu-infra/operator/api/v1alpha1"
)

// requeueWhileChanging is how often we look again while a model is downloading, loading or failed.
// Pod and Job watches wake us earlier; this is the safety net the design calls "requeue 30s".
const requeueWhileChanging = 30 * time.Second

// ServedModelReconciler keeps one ServedModel's PVC, download Job, vLLM Deployment and Service in the shape the spec
// asks for, and writes what it sees into status. Every child carries an ownerReference, so deleting the ServedModel
// deletes them through garbage collection; only the PVC stays when spec.storage.retainOnDelete is true.
type ServedModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config
}

// +kubebuilder:rbac:groups=serving.kukie.io,resources=servedmodels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.kukie.io,resources=servedmodels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.kukie.io,resources=servedmodels/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch

// Reconcile moves the ServedModel one step along Pending → Downloading → Loading → Ready (or Failed) and records
// the result in status. It is idempotent: every child is server-side applied to its desired shape.
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
	// On an error the phase and conditions stay as they were (an API hiccup is not a model failure); the error is
	// logged by controller-runtime and the request retried with backoff. Whatever status was gathered before the
	// error is still written.
	sm.Status.ObservedGeneration = sm.Generation

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

// reconcile does the work and mutates sm.Status; Reconcile persists it.
func (r *ServedModelReconciler) reconcile(ctx context.Context, sm *servingv1alpha1.ServedModel) (ctrl.Result, error) {
	if sm.Status.Phase == "" {
		setPhase(sm, servingv1alpha1.PhasePending, "Created", "PVC와 다운로드 Job을 만든다")
	}

	if err := r.reconcilePVC(ctx, sm); err != nil {
		return ctrl.Result{}, fmt.Errorf("PVC: %w", err)
	}

	downloaded, err := r.reconcileDownload(ctx, sm)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("download job: %w", err)
	}
	if !downloaded {
		return ctrl.Result{RequeueAfter: requeueWhileChanging}, nil
	}

	if err := r.reconcileEngine(ctx, sm); err != nil {
		return ctrl.Result{}, fmt.Errorf("engine: %w", err)
	}

	verdict, err := r.observePods(ctx, sm, componentEngine)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("engine pods: %w", err)
	}
	switch {
	case verdict.ready && sm.Annotations[servingv1alpha1.MeasureAnnotation] == "true":
		// TODO(DURO-169): create the measurement Job here and write its result to status.baseline and a ConfigMap.
		setPhase(sm, servingv1alpha1.PhaseMeasuring, "MeasureJobNotImplemented",
			"측정 Job은 아직 없다(DURO-169). annotation "+servingv1alpha1.MeasureAnnotation+"을 지우면 Ready로 돌아간다")
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionTrue, verdict.reason, verdict.message)
		return ctrl.Result{}, nil
	case verdict.ready:
		setPhase(sm, servingv1alpha1.PhaseReady, verdict.reason, verdict.message)
		setCondition(sm, servingv1alpha1.ConditionReady, metav1.ConditionTrue, verdict.reason, verdict.message)
		return ctrl.Result{}, nil
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
		componentLabel:                    componentDownload,
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
		// PVC not bound) or an image problem would otherwise sit behind "Downloading" with no signal.
		verdict, err := r.observePods(ctx, sm, componentDownload)
		if err != nil {
			return false, err
		}
		if verdict.failed {
			setPhase(sm, servingv1alpha1.PhaseFailed, verdict.reason, verdict.message)
			setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, verdict.reason, verdict.message)
			return false, nil
		}
		setPhase(sm, servingv1alpha1.PhaseDownloading, "JobRunning", "허깅페이스에서 "+sm.Spec.Source.HuggingFace+" 받는 중")
		setCondition(sm, servingv1alpha1.ConditionDownloaded, metav1.ConditionFalse, "JobRunning", "다운로드 Job "+wanted)
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

// observePods reads the Pods of one component (engine or download) and turns the newest one into a verdict.
func (r *ServedModelReconciler) observePods(ctx context.Context, sm *servingv1alpha1.ServedModel, component string) (podVerdict, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                    component,
	}); err != nil {
		return podVerdict{}, err
	}
	return classifyPod(newestLivePod(pods.Items)), nil
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
		// Engine and download Pods are owned by a ReplicaSet or Job, not by us; map them back through the label
		// their templates carry.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
			labels := obj.GetLabels()
			name, ok := labels[servingv1alpha1.ServedModelLabel]
			if !ok || (labels[componentLabel] != componentEngine && labels[componentLabel] != componentDownload) {
				return nil
			}
			return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
		})).
		Named("servedmodel").
		Complete(r)
}
