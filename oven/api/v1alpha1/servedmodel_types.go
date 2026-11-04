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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Annotation and label keys the operator reads or writes.
const (
	// MeasureAnnotation set to "true" on a Ready ServedModel asks for one measurement run per spec. Remove it to stop a
	// run; set it again to measure again. The result stays in status.baseline.
	MeasureAnnotation = "serving.kukie.io/measure"
	// KeepUntilAnnotation holds an RFC3339 time. The model is not stopped for running too long before that time.
	// It only ever extends: an earlier time, or one that does not parse, changes nothing.
	KeepUntilAnnotation = "serving.kukie.io/keep-until"
	// ServedModelLabel is put on every child resource (PVC, Job, Deployment, Service, Pod) with the owner's name.
	ServedModelLabel = "serving.kukie.io/servedmodel"
	// TokenSecretKey is the key read from spec.source.tokenSecretRef.
	TokenSecretKey = "token"
)

// ServedModelPhase is a one-word summary of where the model is in its life.
// +kubebuilder:validation:Enum=Pending;Downloading;Loading;Ready;Measuring;Failed;Stopped
type ServedModelPhase string

const (
	PhasePending     ServedModelPhase = "Pending"
	PhaseDownloading ServedModelPhase = "Downloading"
	PhaseLoading     ServedModelPhase = "Loading"
	PhaseReady       ServedModelPhase = "Ready"
	PhaseMeasuring   ServedModelPhase = "Measuring"
	PhaseFailed      ServedModelPhase = "Failed"
	// PhaseStopped is final: the run time limit passed and nothing runs any more. Starting again means deleting the
	// ServedModel and creating it again.
	PhaseStopped ServedModelPhase = "Stopped"
)

// Condition types written to status.conditions.
const (
	// ConditionDownloaded is True once the model files are on the PVC.
	ConditionDownloaded = "Downloaded"
	// ConditionReady is True once a vLLM Pod answers /health (its readiness probe).
	ConditionReady = "Ready"
	// ConditionMeasured reports the outcome of the last measurement. It does not say the model is unhealthy.
	ConditionMeasured = "Measured"
)

// ModelSource says where the model files come from. Only Hugging Face in this version (S3 and PVC sources are later).
type ModelSource struct {
	// huggingface is the Hub repository, e.g. "Qwen/Qwen3.6-35B-A3B".
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=200
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`
	// +required
	HuggingFace string `json:"huggingface"`

	// tokenSecretRef names a Secret in the same namespace whose key "token" holds a Hugging Face token.
	// Needed only for gated models.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	TokenSecretRef string `json:"tokenSecretRef,omitempty"`
}

// EngineSpec describes the vLLM container.
type EngineSpec struct {
	// image is the vLLM image. It must be pinned by digest so the catalog entry "this image works" stays true.
	// +kubebuilder:validation:Pattern=`@sha256:[a-f0-9]{64}$`
	// +kubebuilder:validation:MaxLength=512
	// +required
	Image string `json:"image"`

	// gpus is the number of GPUs for one Pod (tensor-parallel-size). Multi-node is out of scope.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	// +optional
	GPUs int32 `json:"gpus,omitempty"`

	// args are extra vLLM flags appended after --model, --tensor-parallel-size and --port.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Args []string `json:"args,omitempty"`

	// resources for the vLLM container. The GPU limit is always set from gpus and cannot be overridden here.
	// Empty means the defaults for a g6e.xlarge-class node (2 CPU / 16Gi request, 28Gi limit).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// StorageSpec describes the PVC that holds the downloaded model files.
type StorageSpec struct {
	// size of the PVC, e.g. 120Gi. Must cover the model files; the download fails with "No space left" otherwise.
	// +required
	Size resource.Quantity `json:"size"`

	// storageClassName of the PVC. Empty uses the cluster default.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// retainOnDelete keeps the PVC (the model files) when the ServedModel is deleted.
	// +optional
	RetainOnDelete bool `json:"retainOnDelete,omitempty"`
}

// AdmissionSpec configures the admission proxy sidecar. Fields only in this version; the sidecar is a later issue.
type AdmissionSpec struct {
	// enabled turns the proxy sidecar on. Not implemented yet: the Deployment has no sidecar whatever the value.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// baselineRef names the ConfigMap with the measured baseline. Empty means conservative defaults.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	BaselineRef string `json:"baselineRef,omitempty"`
}

// ServedModelSpec defines the desired state of ServedModel
type ServedModelSpec struct {
	// source is where the model files come from.
	// +required
	Source ModelSource `json:"source"`

	// engine is the vLLM container.
	// +required
	Engine EngineSpec `json:"engine"`

	// storage is the PVC for the model files.
	// +required
	Storage StorageSpec `json:"storage"`

	// admission is the admission proxy sidecar (fields only in this version).
	// +optional
	Admission AdmissionSpec `json:"admission,omitzero"`

	// maxRuntime is how long the model may run, counted from status.startedAt, so download and load time are included.
	// After that the download Job and vLLM stop and the phase becomes Stopped; the PVC stays. Empty uses the manager flag
	// --max-runtime (chart value oven.maxRuntime). "0" never stops on its own. Hours, minutes and seconds, e.g. 2h, 1h30m
	// or 90m. With hours, minutes and seconds stay under 60.
	// The pattern keeps out what Go cannot parse: one such value would break reading every ServedModel. It also holds what
	// Go writes back (1h0m0s, 169h25m39s), so reading an object and updating it can never be refused.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=12
	// +kubebuilder:validation:Pattern=`^(0|[0-9]{1,5}h([0-5]?[0-9]m)?([0-5]?[0-9]s)?|([0-9]{1,4}m)?([0-9]{1,4}s)?)$`
	// +optional
	MaxRuntime *metav1.Duration `json:"maxRuntime,omitempty"`
}

// DownloadStatus is the progress of the download Job. The Kukie agent and web read these fields. Do not rename them.
type DownloadStatus struct {
	// percent is downloadedBytes / totalBytes rounded down. It stays at 99 at most until the Job has succeeded, and is 100
	// after that. It is 0 while totalBytes is unknown, so a 0 without totalBytes means "unknown", not "nothing yet".
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percent int32 `json:"percent"`

	// downloadedBytes is what the model's cache folder held at the last look, files from an earlier attempt included.
	// Empty until the first look.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DownloadedBytes *int64 `json:"downloadedBytes,omitempty"`

	// totalBytes is the size of the model on Hugging Face. Empty while it is unknown.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalBytes *int64 `json:"totalBytes,omitempty"`
}

// BaselineStatus is the admission baseline from a measurement run: what the server looked like at the highest
// concurrency that stayed within the SLA.
type BaselineStatus struct {
	// maxConcurrency is the highest concurrent request count that stayed within the SLA.
	// +optional
	MaxConcurrency int32 `json:"maxConcurrency,omitempty"`
	// maxWaiting is the number of waiting requests above which the proxy answers 429.
	// +optional
	MaxWaiting int32 `json:"maxWaiting,omitempty"`
	// maxKvPercent is the KV-cache usage above which the proxy answers 429.
	// +optional
	MaxKvPercent int32 `json:"maxKvPercent,omitempty"`
	// measuredAt is when the baseline was measured.
	// +optional
	MeasuredAt *metav1.Time `json:"measuredAt,omitempty"`
	// partial is true when the run ended early (the client failed or the time limit came) rather than at an SLA limit.
	// The numbers are then a lower bound: the server may take more.
	// +optional
	Partial bool `json:"partial,omitempty"`
	// generation is the spec generation that was measured. Older than metadata.generation means the numbers describe
	// a different model or engine setting.
	// +optional
	Generation int64 `json:"generation,omitempty"`
}

// ServedModelStatus defines the observed state of ServedModel.
type ServedModelStatus struct {
	// observedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase: Pending | Downloading | Loading | Ready | Measuring | Failed | Stopped
	// +optional
	Phase ServedModelPhase `json:"phase,omitempty"`

	// reason is a CamelCase word for the current phase (mostly for Failed). Pattern-table classification is DURO-171.
	// +optional
	Reason string `json:"reason,omitempty"`

	// message is a human-readable detail. For Failed it carries the Pod or Job message as is.
	// +optional
	Message string `json:"message,omitempty"`

	// startedAt is when the operator first handled this ServedModel. The run time limit counts from here. It never
	// changes afterwards, so a restart of the operator does not extend the limit.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// stopsAt is when the model stops for running too long: the later of startedAt + maxRuntime and the keep-until
	// annotation. Empty means it never stops on its own. A measurement in progress may run past it, and it keeps its last
	// value once the phase is Stopped. The Kukie agent and web read this field to show the time left: the limit may come
	// from a manager flag they cannot see, so they must not compute it. Do not rename it.
	// +optional
	StopsAt *metav1.Time `json:"stopsAt,omitempty"`

	// download is the progress of the download Job.
	// +optional
	Download *DownloadStatus `json:"download,omitempty"`

	// baseline is the admission baseline from the last measurement run.
	// +optional
	Baseline *BaselineStatus `json:"baseline,omitempty"`

	// conditions: Downloaded, Ready, Measured.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sm
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.source.huggingface`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ServedModel is the Schema for the servedmodels API
type ServedModel struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ServedModel
	// +required
	Spec ServedModelSpec `json:"spec"`

	// status defines the observed state of ServedModel
	// +optional
	Status ServedModelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ServedModelList contains a list of ServedModel
type ServedModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ServedModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ServedModel{}, &ServedModelList{})
		return nil
	})
}
