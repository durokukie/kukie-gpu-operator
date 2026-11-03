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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/operator/api/v1alpha1"
)

func TestEngineResourcesMergesOverDefaults(t *testing.T) {
	memory := func(s string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(s)}
	}
	tests := []struct {
		name                   string
		user                   *corev1.ResourceRequirements
		wantRequest, wantLimit string
	}{
		{"no spec keeps the defaults", nil, "16Gi", "28Gi"},
		{"a request above the default limit raises the limit", &corev1.ResourceRequirements{Requests: memory("48Gi")}, "48Gi", "48Gi"},
		{"a request below the default limit keeps the limit", &corev1.ResourceRequirements{Requests: memory("20Gi")}, "20Gi", "28Gi"},
		{"a limit below the default request lowers the request", &corev1.ResourceRequirements{Limits: memory("8Gi")}, "8Gi", "8Gi"},
		{"a limit above the default request keeps the request", &corev1.ResourceRequirements{Limits: memory("64Gi")}, "16Gi", "64Gi"},
		{"two user values stay as written even if they contradict",
			&corev1.ResourceRequirements{Requests: memory("48Gi"), Limits: memory("8Gi")}, "48Gi", "8Gi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := &servingv1alpha1.ServedModel{}
			sm.Spec.Engine.GPUs = 1
			sm.Spec.Engine.Resources = tt.user
			got := engineResources(sm)
			request, limit := (*got.Requests)[corev1.ResourceMemory], (*got.Limits)[corev1.ResourceMemory]
			if request.Cmp(resource.MustParse(tt.wantRequest)) != 0 {
				t.Errorf("memory request = %s, want %s", request.String(), tt.wantRequest)
			}
			if limit.Cmp(resource.MustParse(tt.wantLimit)) != 0 {
				t.Errorf("memory limit = %s, want %s", limit.String(), tt.wantLimit)
			}
			cpu := (*got.Requests)[corev1.ResourceCPU]
			if cpu.Cmp(defaultEngineCPURequest) != 0 {
				t.Errorf("cpu request = %s, want the default %s", cpu.String(), defaultEngineCPURequest.String())
			}
			gpu := (*got.Requests)[gpuResourceName]
			if gpu.Value() != 1 {
				t.Errorf("gpu request = %s, want 1", gpu.String())
			}
		})
	}
}

// A PVC we are creating must never look like someone else's to findNameConflict, even if the metadata apply that
// follows never happens.
func TestPVCCarriesItsLabelFromTheFirstApply(t *testing.T) {
	sm := &servingv1alpha1.ServedModel{}
	sm.Name = "model"
	sm.Namespace = "llm"
	sm.Spec.Storage.Size = resource.MustParse("40Gi")
	if got := desiredPVCSpec(sm).Labels[servingv1alpha1.ServedModelLabel]; got != sm.Name {
		t.Errorf("label on the first apply = %q, want %q", got, sm.Name)
	}
}
