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
	// MeasureAnnotation set to "true" on a Ready ServedModel asks for a measurement run (2nd layer Job).
	// The Job itself is DURO-169; this version only moves the phase to Measuring.
	MeasureAnnotation = "serving.kukie.io/measure"
	// ServedModelLabel is put on every child resource (PVC, Job, Deployment, Service, Pod) with the owner's name.
	ServedModelLabel = "serving.kukie.io/servedmodel"
	// TokenSecretKey is the key read from spec.source.tokenSecretRef.
	TokenSecretKey = "token"
)

// ServedModelPhase is a one-word summary of where the model is in its life.
// +kubebuilder:validation:Enum=Pending;Downloading;Loading;Ready;Measuring;Failed
type ServedModelPhase string

const (
	PhasePending     ServedModelPhase = "Pending"
	PhaseDownloading ServedModelPhase = "Downloading"
	PhaseLoading     ServedModelPhase = "Loading"
	PhaseReady       ServedModelPhase = "Ready"
	PhaseMeasuring   ServedModelPhase = "Measuring"
	PhaseFailed      ServedModelPhase = "Failed"
)

// Condition types written to status.conditions.
const (
	// ConditionDownloaded is True once the model files are on the PVC.
	ConditionDownloaded = "Downloaded"
	// ConditionReady is True once a vLLM Pod answers /health (its readiness probe).
	ConditionReady = "Ready"
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
}

// DownloadStatus is the progress of the download Job.
type DownloadStatus struct {
	// percent is 0 while the Job runs and 100 when it has succeeded. Finer progress is a later issue.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percent int32 `json:"percent"`
}

// BaselineStatus is the admission baseline written by a measurement run (DURO-169). Always empty in this version.
type BaselineStatus struct {
	// maxWaiting is the number of waiting requests above which the proxy answers 429.
	// +optional
	MaxWaiting int32 `json:"maxWaiting,omitempty"`
	// maxKvPercent is the KV-cache usage above which the proxy answers 429.
	// +optional
	MaxKvPercent int32 `json:"maxKvPercent,omitempty"`
	// measuredAt is when the baseline was measured.
	// +optional
	MeasuredAt *metav1.Time `json:"measuredAt,omitempty"`
}

// ServedModelStatus defines the observed state of ServedModel.
type ServedModelStatus struct {
	// observedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase: Pending | Downloading | Loading | Ready | Measuring | Failed
	// +optional
	Phase ServedModelPhase `json:"phase,omitempty"`

	// reason is a CamelCase word for the current phase (mostly for Failed). Pattern-table classification is DURO-171.
	// +optional
	Reason string `json:"reason,omitempty"`

	// message is a human-readable detail. For Failed it carries the Pod or Job message as is.
	// +optional
	Message string `json:"message,omitempty"`

	// download is the progress of the download Job.
	// +optional
	Download *DownloadStatus `json:"download,omitempty"`

	// baseline is the admission baseline from the last measurement run.
	// +optional
	Baseline *BaselineStatus `json:"baseline,omitempty"`

	// conditions: Downloaded, Ready.
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
