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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/operator/api/v1alpha1"
)

// Names, paths and ports shared by the child resources.
const (
	// FieldManager is the server-side-apply owner of every child resource.
	FieldManager = "kukie-serving-operator"

	componentLabel    = "app.kubernetes.io/component"
	componentEngine   = "engine"
	componentDownload = "download"
	nameLabel         = "app.kubernetes.io/name"
	instanceLabel     = "app.kubernetes.io/instance"
	managedByLabel    = "app.kubernetes.io/managed-by"

	// modelsMountPath is where the PVC is mounted in both the download Job and the vLLM container.
	// HF_HOME points here, so `hf download` writes the files where vLLM looks for them.
	modelsMountPath = "/models"
	enginePort      = 8000
	// engineServicePort is what the Service exposes. The admission proxy sidecar (later issue) will listen on
	// 8080 and the Service will target it when spec.admission.enabled is true; today it goes straight to vLLM.
	engineServicePort = 8000

	gpuResourceName = corev1.ResourceName("nvidia.com/gpu")
	gpuTaintKey     = "nvidia.com/gpu"
)

// Defaults for the vLLM container when spec.engine.resources is empty. They match kubernetes/llm/vllm-gpt-oss-20b.yaml
// (g6e.xlarge: 4 vCPU, 32 GiB).
var (
	defaultEngineCPURequest    = resource.MustParse("2")
	defaultEngineMemoryRequest = resource.MustParse("16Gi")
	defaultEngineMemoryLimit   = resource.MustParse("28Gi")
	defaultShmSize             = resource.MustParse("2Gi")
	downloadCPURequest         = resource.MustParse("500m")
	downloadMemoryRequest      = resource.MustParse("1Gi")
	downloadMemoryLimit        = resource.MustParse("4Gi")
)

// Config is what the manager passes to the reconciler from its flags.
type Config struct {
	// DownloadImage runs `hf download`. Empty means spec.engine.image (the vLLM image ships the hf CLI), which also
	// pre-pulls the engine image on the node that will serve the model.
	DownloadImage string
	// GPUNodeSelector is put on the download Job and the vLLM Deployment so both land on the same kind of node
	// (and, with a zonal storage class, in the same zone). Empty means "any node the scheduler picks".
	GPUNodeSelector map[string]string
	// DownloadDeadline bounds one download Job across its retries. A stuck `hf download` would otherwise hold a GPU
	// node forever: backoffLimit only counts attempts that fail. Zero means DefaultDownloadDeadline.
	DownloadDeadline time.Duration
}

// DefaultDownloadDeadline is plenty for the 130B-class models the catalog targets (72 GB takes about 12 minutes at
// 100 MB/s), with room for retries.
const DefaultDownloadDeadline = 2 * time.Hour

func (c Config) downloadDeadlineSeconds() int64 {
	if c.DownloadDeadline <= 0 {
		return int64(DefaultDownloadDeadline.Seconds())
	}
	return int64(c.DownloadDeadline.Seconds())
}

// pvcName is the PVC that holds the model files.
func pvcName(sm *servingv1alpha1.ServedModel) string {
	return sm.Name + "-models"
}

// downloadImage returns the image for the download Job.
func (c Config) downloadImage(sm *servingv1alpha1.ServedModel) string {
	if c.DownloadImage != "" {
		return c.DownloadImage
	}
	return sm.Spec.Engine.Image
}

// downloadJobName changes whenever an input of the download changes, so a changed spec gets a fresh Job and an
// unchanged spec keeps the finished one as the "already downloaded" marker.
func (c Config) downloadJobName(sm *servingv1alpha1.ServedModel) string {
	h := sha256.Sum256([]byte(sm.Spec.Source.HuggingFace + "\x00" + sm.Spec.Source.TokenSecretRef + "\x00" + c.downloadImage(sm)))
	return fmt.Sprintf("%s-download-%s", sm.Name, hex.EncodeToString(h[:])[:8])
}

func childLabels(sm *servingv1alpha1.ServedModel, component string) map[string]string {
	return map[string]string{
		nameLabel:                        "vllm",
		instanceLabel:                    sm.Name,
		managedByLabel:                   FieldManager,
		componentLabel:                   component,
		servingv1alpha1.ServedModelLabel: sm.Name,
	}
}

// engineSelector is the immutable Deployment selector and the label set the reconciler lists Pods by.
func engineSelector(sm *servingv1alpha1.ServedModel) map[string]string {
	return map[string]string{
		servingv1alpha1.ServedModelLabel: sm.Name,
		componentLabel:                   componentEngine,
	}
}

func ownerRef(sm *servingv1alpha1.ServedModel) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(servingv1alpha1.GroupVersion.String()).
		WithKind("ServedModel").
		WithName(sm.Name).
		WithUID(sm.UID).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

func gpuToleration() *corev1ac.TolerationApplyConfiguration {
	return corev1ac.Toleration().
		WithKey(gpuTaintKey).
		WithOperator(corev1.TolerationOpExists).
		WithEffect(corev1.TaintEffectNoSchedule)
}

// pvcSpecFieldManager owns the PVC spec, separately from the metadata, so re-applying only the metadata later
// (reconcilePVC) does not release the spec fields: server-side apply removes what a manager stops stating.
const pvcSpecFieldManager = FieldManager + "/pvc-spec"

// desiredPVCSpec is the PVC spec, applied once at creation (storageClassName is immutable afterwards and a size change
// is a volume expansion the user should ask for deliberately). The label is there from the first moment: findNameConflict
// goes by it, and a PVC of ours without it would look like someone else's.
func desiredPVCSpec(sm *servingv1alpha1.ServedModel) *corev1ac.PersistentVolumeClaimApplyConfiguration {
	spec := corev1ac.PersistentVolumeClaimSpec().
		WithAccessModes(corev1.ReadWriteOnce).
		WithResources(corev1ac.VolumeResourceRequirements().
			WithRequests(corev1.ResourceList{corev1.ResourceStorage: sm.Spec.Storage.Size}))
	if sm.Spec.Storage.StorageClassName != nil {
		spec = spec.WithStorageClassName(*sm.Spec.Storage.StorageClassName)
	}
	return corev1ac.PersistentVolumeClaim(pvcName(sm), sm.Namespace).
		WithLabels(childLabels(sm, "models")).
		WithSpec(spec)
}

// desiredPVCMetadata is the PVC's labels and owner reference, re-applied on every reconcile. The owner reference
// follows spec.storage.retainOnDelete: without it, garbage collection leaves the model files behind on delete.
func desiredPVCMetadata(sm *servingv1alpha1.ServedModel) *corev1ac.PersistentVolumeClaimApplyConfiguration {
	pvc := corev1ac.PersistentVolumeClaim(pvcName(sm), sm.Namespace).WithLabels(childLabels(sm, "models"))
	if !sm.Spec.Storage.RetainOnDelete {
		pvc = pvc.WithOwnerReferences(ownerRef(sm))
	}
	return pvc
}

// desiredDownloadJob runs `hf download <repo>` once with the PVC as HF_HOME.
// It tolerates the GPU taint and uses the same node selector as the engine so a zonal volume ends up where vLLM runs.
func (c Config) desiredDownloadJob(sm *servingv1alpha1.ServedModel) *batchv1ac.JobApplyConfiguration {
	env := []*corev1ac.EnvVarApplyConfiguration{
		corev1ac.EnvVar().WithName("HF_HOME").WithValue(modelsMountPath),
	}
	if sm.Spec.Source.TokenSecretRef != "" {
		env = append(env, corev1ac.EnvVar().WithName("HF_TOKEN").WithValueFrom(
			corev1ac.EnvVarSource().WithSecretKeyRef(
				corev1ac.SecretKeySelector().
					WithName(sm.Spec.Source.TokenSecretRef).
					WithKey(servingv1alpha1.TokenSecretKey))))
	}
	container := corev1ac.Container().
		WithName("download").
		WithImage(c.downloadImage(sm)).
		WithCommand("hf", "download", sm.Spec.Source.HuggingFace).
		WithEnv(env...).
		WithResources(corev1ac.ResourceRequirements().
			WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    downloadCPURequest,
				corev1.ResourceMemory: downloadMemoryRequest,
			}).
			WithLimits(corev1.ResourceList{corev1.ResourceMemory: downloadMemoryLimit})).
		WithVolumeMounts(corev1ac.VolumeMount().WithName("models").WithMountPath(modelsMountPath))

	podSpec := corev1ac.PodSpec().
		WithRestartPolicy(corev1.RestartPolicyNever).
		WithAutomountServiceAccountToken(false).
		WithTolerations(gpuToleration()).
		WithContainers(container).
		WithVolumes(corev1ac.Volume().WithName("models").
			WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(pvcName(sm))))
	if len(c.GPUNodeSelector) > 0 {
		podSpec = podSpec.WithNodeSelector(c.GPUNodeSelector)
	}

	return batchv1ac.Job(c.downloadJobName(sm), sm.Namespace).
		WithLabels(childLabels(sm, componentDownload)).
		WithOwnerReferences(ownerRef(sm)).
		WithSpec(batchv1ac.JobSpec().
			WithBackoffLimit(2).
			WithActiveDeadlineSeconds(c.downloadDeadlineSeconds()).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(childLabels(sm, componentDownload)).
				WithSpec(podSpec)))
}

// engineResources merges spec.engine.resources over the defaults key by key and always sets the GPU count from
// spec.engine.gpus. A default never contradicts what the user set: asking for more memory than the default limit raises
// that limit, and a limit below the default request lowers that request. Two values the user set themselves stay as
// written; if they contradict, the API server refuses the Deployment and status says so (ApplyRejected).
func engineResources(sm *servingv1alpha1.ServedModel) *corev1ac.ResourceRequirementsApplyConfiguration {
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    defaultEngineCPURequest,
		corev1.ResourceMemory: defaultEngineMemoryRequest,
	}
	limits := corev1.ResourceList{
		corev1.ResourceMemory: defaultEngineMemoryLimit,
	}
	if res := sm.Spec.Engine.Resources; res != nil {
		for name, quantity := range res.Requests {
			requests[name] = quantity
		}
		for name, quantity := range res.Limits {
			limits[name] = quantity
		}
		for name, limit := range limits {
			request, hasRequest := requests[name]
			if !hasRequest || request.Cmp(limit) <= 0 {
				continue
			}
			_, userRequest := res.Requests[name]
			_, userLimit := res.Limits[name]
			switch {
			case userRequest && !userLimit:
				limits[name] = request
			case userLimit && !userRequest:
				requests[name] = limit
			}
		}
	}
	gpus := resource.MustParse(strconv.FormatInt(int64(gpuCount(sm)), 10))
	requests[gpuResourceName] = gpus
	limits[gpuResourceName] = gpus
	return corev1ac.ResourceRequirements().WithRequests(requests).WithLimits(limits)
}

func gpuCount(sm *servingv1alpha1.ServedModel) int32 {
	if sm.Spec.Engine.GPUs < 1 {
		return 1
	}
	return sm.Spec.Engine.GPUs
}

// engineArgs is the vLLM command line: model, tensor parallel, port, then the user's extra flags.
func engineArgs(sm *servingv1alpha1.ServedModel) []string {
	args := []string{
		"--model", sm.Spec.Source.HuggingFace,
		"--tensor-parallel-size", strconv.FormatInt(int64(gpuCount(sm)), 10),
		"--port", strconv.Itoa(enginePort),
	}
	return append(args, sm.Spec.Engine.Args...)
}

// desiredDeployment is the vLLM Deployment, shaped like kubernetes/llm/vllm-gpt-oss-20b.yaml:
// Recreate (one GPU per node), GPU toleration, /health startup and readiness probes, /dev/shm in memory.
// The admission proxy sidecar is not here yet; see engineServicePort.
func (c Config) desiredDeployment(sm *servingv1alpha1.ServedModel) *appsv1ac.DeploymentApplyConfiguration {
	healthProbe := func() *corev1ac.ProbeApplyConfiguration {
		return corev1ac.Probe().WithHTTPGet(corev1ac.HTTPGetAction().
			WithPath("/health").WithPort(intstr.FromString("http")))
	}
	container := corev1ac.Container().
		WithName("vllm").
		WithImage(sm.Spec.Engine.Image).
		WithArgs(engineArgs(sm)...).
		WithEnv(
			corev1ac.EnvVar().WithName("HF_HOME").WithValue(modelsMountPath),
			// The files are on the PVC already; never reach for the Hub from the engine.
			corev1ac.EnvVar().WithName("HF_HUB_OFFLINE").WithValue("1"),
		).
		WithPorts(corev1ac.ContainerPort().WithName("http").WithContainerPort(enginePort).WithProtocol(corev1.ProtocolTCP)).
		WithResources(engineResources(sm)).
		// Loading a big model can take a long time; 40 minutes before the kubelet gives up (same as the manifest).
		WithStartupProbe(healthProbe().WithPeriodSeconds(10).WithFailureThreshold(240)).
		WithReadinessProbe(healthProbe().WithPeriodSeconds(10)).
		WithVolumeMounts(
			corev1ac.VolumeMount().WithName("models").WithMountPath(modelsMountPath),
			corev1ac.VolumeMount().WithName("shm").WithMountPath("/dev/shm"),
		)

	podSpec := corev1ac.PodSpec().
		WithAutomountServiceAccountToken(false).
		WithTolerations(gpuToleration()).
		WithContainers(container).
		WithVolumes(
			corev1ac.Volume().WithName("models").
				WithPersistentVolumeClaim(corev1ac.PersistentVolumeClaimVolumeSource().WithClaimName(pvcName(sm))),
			corev1ac.Volume().WithName("shm").
				WithEmptyDir(corev1ac.EmptyDirVolumeSource().WithMedium(corev1.StorageMediumMemory).WithSizeLimit(defaultShmSize)),
		)
	if len(c.GPUNodeSelector) > 0 {
		podSpec = podSpec.WithNodeSelector(c.GPUNodeSelector)
	}

	return appsv1ac.Deployment(sm.Name, sm.Namespace).
		WithLabels(childLabels(sm, componentEngine)).
		WithOwnerReferences(ownerRef(sm)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(1).
			WithStrategy(appsv1ac.DeploymentStrategy().WithType(appsv1.RecreateDeploymentStrategyType)).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(engineSelector(sm))).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(childLabels(sm, componentEngine)).
				WithSpec(podSpec)))
}

// desiredService exposes the engine inside the cluster under the ServedModel's name.
func desiredService(sm *servingv1alpha1.ServedModel) *corev1ac.ServiceApplyConfiguration {
	return corev1ac.Service(sm.Name, sm.Namespace).
		WithLabels(childLabels(sm, componentEngine)).
		WithOwnerReferences(ownerRef(sm)).
		WithSpec(corev1ac.ServiceSpec().
			WithSelector(engineSelector(sm)).
			WithPorts(corev1ac.ServicePort().
				WithName("http").
				WithPort(engineServicePort).
				WithTargetPort(intstr.FromString("http")).
				WithProtocol(corev1.ProtocolTCP)))
}
