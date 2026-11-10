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
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

const (
	// measureGPUTypeKey in a kept result is "label=value" of the GPU the run measured, e.g.
	// "nvidia.com/gpu.product=NVIDIA-L40S". Results written before this key existed are never reused.
	measureGPUTypeKey = "gpuType"
	// measureConcurrencyKey in a kept result is the steps the run went through (--measure-concurrency).
	measureConcurrencyKey = "measureConcurrency"

	// DefaultMeasureReuseMaxAge keeps a node driver or vLLM runtime change from living on in an old number for long.
	DefaultMeasureReuseMaxAge = 30 * 24 * time.Hour
)

// DefaultGPUTypeLabels name the GPU on most clusters: the product from NVIDIA GPU feature discovery (GPU Operator), and
// failing that the cloud instance type, which fixes the GPU model too.
var DefaultGPUTypeLabels = []string{"nvidia.com/gpu.product", corev1.LabelInstanceTypeStable}

func (c Config) gpuTypeLabels() []string {
	if len(c.GPUTypeLabels) > 0 {
		return c.GPUTypeLabels
	}
	return DefaultGPUTypeLabels
}

// gpuTypeOf reads the node and returns "label=value" for the first GPU type label it carries. No node name, a node that is
// gone or one with none of the labels gives "": the GPU type is unknown, and nothing is reused on an unknown type.
func (r *ServedModelReconciler) gpuTypeOf(ctx context.Context, nodeName string) (string, error) {
	if nodeName == "" {
		return "", nil
	}
	node := &corev1.Node{}
	switch err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", err
	}
	for _, label := range r.Config.gpuTypeLabels() {
		if value := node.Labels[label]; value != "" {
			return label + "=" + value, nil
		}
	}
	return "", nil
}

// measureConditions is everything a measured number depends on that the oven can see. Two runs under the same conditions
// gave the same answer on kukie-gpu (16 and 16 concurrent, TTFT p95 486 and 474 ms), so a kept result under the current
// conditions stands in for a new run.
type measureConditions struct {
	model       string
	image       string
	gpus        string
	args        []string
	gpuType     string
	concurrency string
	sla         measureSLA
}

func (r *ServedModelReconciler) currentConditions(sm *servingv1alpha1.ServedModel, gpuType string) measureConditions {
	args := sm.Spec.Engine.Args
	if args == nil {
		args = []string{}
	}
	return measureConditions{
		model:       sm.Spec.Source.HuggingFace,
		image:       sm.Spec.Engine.Image,
		gpus:        strconv.Itoa(int(gpuCount(sm))),
		args:        args,
		gpuType:     gpuType,
		concurrency: r.Config.Measure.concurrency(),
		sla:         r.Config.Measure.sla(),
	}
}

// reusableResult reads a kept result and says whether it may stand in for a run under want at now. A partial result is
// not reused: it is a lower bound from a run cut short (time limit, client failure), and copying it would keep the lower
// bound around with nothing that asks for a full run. A person who wants it can measure again. An empty maxAge means no
// reuse at all.
func reusableResult(cm *corev1.ConfigMap, want measureConditions, now time.Time, maxAge time.Duration) (measureResult, metav1.Time, bool) {
	d := cm.Data
	if maxAge <= 0 || want.gpuType == "" || d[measureGPUTypeKey] == "" {
		return measureResult{}, metav1.Time{}, false
	}
	if d["model"] != want.model || d["engineImage"] != want.image || d["gpus"] != want.gpus ||
		d[measureGPUTypeKey] != want.gpuType || d[measureConcurrencyKey] != want.concurrency || d["partial"] != "false" {
		return measureResult{}, metav1.Time{}, false
	}
	var args []string
	if err := json.Unmarshal([]byte(d["engineArgs"]), &args); err != nil || args == nil || !slices.Equal(args, want.args) {
		return measureResult{}, metav1.Time{}, false
	}
	measuredAt, err := time.Parse(time.RFC3339, d["measuredAt"])
	if err != nil || now.Sub(measuredAt) > maxAge {
		return measureResult{}, metav1.Time{}, false
	}
	var result measureResult
	if err := json.Unmarshal([]byte(d[baselineKey]), &result); err != nil {
		return measureResult{}, metav1.Time{}, false
	}
	if !result.OK || result.Partial || result.MaxConcurrency <= 0 || result.SLA == nil || *result.SLA != want.sla {
		return measureResult{}, metav1.Time{}, false
	}
	return result, metav1.NewTime(measuredAt), true
}

// reuseKey says what a look for a kept result looked at. The same key finds the same nothing, so it is not repeated: the
// list goes to the API server (ConfigMaps are not cached). A new generation, a new engine node or a new ServedModel of
// the same name looks again.
func reuseKey(sm *servingv1alpha1.ServedModel, nodeName string) string {
	return fmt.Sprintf("%s/%d/%s", sm.UID, sm.Generation, nodeName)
}

// reuseBaseline fills status.baseline of a Ready model that has none for this spec, from the newest kept result of a
// ServedModel of this name measured under the same conditions, so it does not have to fill the GPU again for the same
// answer. It is called only while no measurement is asked for: the annotation always measures anew. Nothing here may
// fail the model, which is serving: an error is logged and the next reconcile looks again.
func (r *ServedModelReconciler) reuseBaseline(ctx context.Context, sm *servingv1alpha1.ServedModel, engine *corev1.Pod) {
	if r.Config.Measure.ReuseMaxAge <= 0 || engine == nil || engine.Spec.NodeName == "" {
		return
	}
	if b := sm.Status.Baseline; b != nil && b.Generation >= sm.Generation {
		return
	}
	nsName := client.ObjectKeyFromObject(sm)
	key := reuseKey(sm, engine.Spec.NodeName)
	if r.reuseLooked(nsName, key) {
		return
	}
	log := logf.FromContext(ctx)
	found, err := r.findReusableResult(ctx, sm, engine.Spec.NodeName)
	if err != nil {
		log.Error(err, "looking for a measurement result to reuse")
		return
	}
	if found == nil {
		r.rememberReuseLook(nsName, key)
		return
	}
	conflict, err := r.writeBaseline(ctx, sm, found.result, found.cm.Data[baselineKey], found.measuredAt, found.cm.Name)
	if err != nil {
		log.Error(err, "writing a reused baseline", "configMap", found.cm.Name)
		return
	}
	if conflict != "" {
		// Measuring would hit the same ConfigMap; there is nothing to tell apart from that here.
		log.Info("not reusing a baseline: "+conflict, "configMap", found.cm.Name)
		r.rememberReuseLook(nsName, key)
		return
	}
	log.Info("reused a baseline instead of measuring again", "configMap", found.cm.Name, "maxConcurrency", found.result.MaxConcurrency)
}

type reusable struct {
	cm         *corev1.ConfigMap
	result     measureResult
	measuredAt metav1.Time
}

// findReusableResult returns the newest kept result of this name that fits the current conditions, or nil.
func (r *ServedModelReconciler) findReusableResult(ctx context.Context, sm *servingv1alpha1.ServedModel, nodeName string) (*reusable, error) {
	gpuType, err := r.gpuTypeOf(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("GPU type: %w", err)
	}
	if gpuType == "" {
		return nil, nil
	}
	results := &corev1.ConfigMapList{}
	if err := r.List(ctx, results, client.InNamespace(sm.Namespace), client.MatchingLabels{
		servingv1alpha1.MeasureResultLabel:     "true",
		servingv1alpha1.MeasureResultNameLabel: sm.Name,
	}); err != nil {
		return nil, err
	}
	want := r.currentConditions(sm, gpuType)
	now := r.now()
	var best *reusable
	for i := range results.Items {
		cm := &results.Items[i]
		result, measuredAt, ok := reusableResult(cm, want, now, r.Config.Measure.ReuseMaxAge)
		if !ok {
			continue
		}
		// The name breaks a tie the same way on every pass.
		if best == nil || measuredAt.After(best.measuredAt.Time) || (measuredAt.Equal(&best.measuredAt) && cm.Name > best.cm.Name) {
			best = &reusable{cm: cm, result: result, measuredAt: measuredAt}
		}
	}
	return best, nil
}

func (r *ServedModelReconciler) reuseLooked(nsName types.NamespacedName, key string) bool {
	r.reuseLooksMu.Lock()
	defer r.reuseLooksMu.Unlock()
	return r.reuseLooks[nsName] == key
}

func (r *ServedModelReconciler) rememberReuseLook(nsName types.NamespacedName, key string) {
	r.reuseLooksMu.Lock()
	defer r.reuseLooksMu.Unlock()
	if r.reuseLooks == nil {
		r.reuseLooks = map[types.NamespacedName]string{}
	}
	r.reuseLooks[nsName] = key
}

func (r *ServedModelReconciler) forgetReuseLook(nsName types.NamespacedName) {
	r.reuseLooksMu.Lock()
	defer r.reuseLooksMu.Unlock()
	delete(r.reuseLooks, nsName)
}
