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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

const (
	testGPUType = "nvidia.com/gpu.product=NVIDIA-L40S"
	measured16  = `{"ok":true,"reason":"Measured","partial":false,"message":"동시 16까지","sla":{"ttftMs":1000.0,"itlMs":50.0,"inputTokens":1000,"outputTokens":200},` +
		`"maxConcurrency":16,"maxWaiting":0,"maxKvPercent":31}`
)

// keptData is what writeMeasureResult leaves for a run of newServedModel under testConfig on an L40S node.
func keptData(at time.Time) map[string]string {
	return map[string]string{
		baselineKey:           measured16,
		"model":               "openai/gpt-oss-20b",
		"gpus":                "1",
		"engineImage":         testImage,
		"engineArgs":          `["--max-model-len","16384"]`,
		"measuredAt":          at.UTC().Format(time.RFC3339),
		"partial":             "false",
		"maxConcurrency":      "16",
		measureGPUTypeKey:     testGPUType,
		measureConcurrencyKey: defaultMeasureConcurrency,
	}
}

func TestReusableResult(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	sm := newServedModel("ns", "model", false)
	want := (&ServedModelReconciler{Config: testConfig}).currentConditions(sm, testGPUType)
	maxAge := DefaultMeasureReuseMaxAge

	cases := []struct {
		name   string
		change func(d map[string]string)
		want   func(w *measureConditions)
		now    time.Time
		maxAge time.Duration
		ok     bool
	}{
		{name: "all the same", ok: true},
		{name: "another image digest", change: func(d map[string]string) { d["engineImage"] = "vllm/vllm-openai@sha256:" + fmt.Sprintf("%064d", 1) }},
		{name: "other engine args", change: func(d map[string]string) { d["engineArgs"] = `["--max-model-len","8192"]` }},
		{name: "the same args in another order", change: func(d map[string]string) { d["engineArgs"] = `["16384","--max-model-len"]` }},
		{name: "no engine args against some", change: func(d map[string]string) { d["engineArgs"] = `[]` }},
		{name: "another GPU count", change: func(d map[string]string) { d["gpus"] = "2" }},
		{name: "another model", change: func(d map[string]string) { d["model"] = "openai/gpt-oss-120b" }},
		{name: "another GPU type", change: func(d map[string]string) { d[measureGPUTypeKey] = "nvidia.com/gpu.product=NVIDIA-L4" }},
		{name: "the same GPU named by another label", change: func(d map[string]string) { d[measureGPUTypeKey] = "node.kubernetes.io/instance-type=g6e.xlarge" }},
		{name: "a result from before the GPU type was kept", change: func(d map[string]string) { delete(d, measureGPUTypeKey) }},
		{name: "a GPU type unknown now", want: func(w *measureConditions) { w.gpuType = "" }},
		{name: "a result from before the steps were kept", change: func(d map[string]string) { delete(d, measureConcurrencyKey) }},
		{name: "other concurrency steps", want: func(w *measureConditions) { w.concurrency = "1 2 4 8" }},
		{name: "another TTFT limit", want: func(w *measureConditions) { w.sla.TTFTMillis = 500 }},
		{name: "another ITL limit", want: func(w *measureConditions) { w.sla.ITLMillis = 30 }},
		{name: "longer prompts", want: func(w *measureConditions) { w.sla.InputTokens = 4000 }},
		{name: "longer answers", want: func(w *measureConditions) { w.sla.OutputTokens = 500 }},
		{name: "no SLA in the result", change: func(d map[string]string) {
			d[baselineKey] = `{"ok":true,"reason":"Measured","maxConcurrency":16}`
		}},
		{name: "partial", change: func(d map[string]string) {
			d["partial"] = "true"
			d[baselineKey] = `{"ok":true,"reason":"MeasuredPartial","partial":true,"sla":{"ttftMs":1000,"itlMs":50,"inputTokens":1000,"outputTokens":200},"maxConcurrency":8}`
		}},
		{name: "partial in the JSON only", change: func(d map[string]string) {
			d[baselineKey] = `{"ok":true,"reason":"MeasuredPartial","partial":true,"sla":{"ttftMs":1000,"itlMs":50,"inputTokens":1000,"outputTokens":200},"maxConcurrency":8}`
		}},
		{name: "unreadable result", change: func(d map[string]string) { d[baselineKey] = "{" }},
		{name: "just within the age limit", now: at.Add(maxAge), ok: true},
		{name: "too old", now: at.Add(maxAge + time.Second)},
		{name: "reuse turned off", maxAge: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{Data: keptData(at)}
			if tc.change != nil {
				tc.change(cm.Data)
			}
			w := want
			w.args = append([]string(nil), want.args...)
			if tc.want != nil {
				tc.want(&w)
			}
			n, age := now, maxAge
			if !tc.now.IsZero() {
				n = tc.now
			}
			if tc.maxAge != 0 {
				age = tc.maxAge
			}
			result, measuredAt, ok := reusableResult(cm, w, n, age)
			if ok != tc.ok {
				t.Fatalf("got %v, want %v", ok, tc.ok)
			}
			if ok && (result.MaxConcurrency != 16 || !measuredAt.Time.Equal(at)) {
				t.Errorf("got %d at %v", result.MaxConcurrency, measuredAt)
			}
		})
	}
}

var _ = Describe("Reusing a kept measurement", func() {
	ctx := context.Background()
	var ns string

	BeforeEach(func() {
		ns = nextWatchedNamespace()
	})

	gpuNode := func(product string) string {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			GenerateName: "gpu-",
			Labels:       map[string]string{"nvidia.com/gpu.product": product, corev1.LabelInstanceTypeStable: "g6e.xlarge"},
		}}
		ExpectWithOffset(1, k8sClient.Create(ctx, node)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, node))).To(Succeed()) })
		return node.Name
	}
	keep := func(name string, at time.Time, change func(map[string]string)) string {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: measureResultPrefix + name + "-" + at.UTC().Format(measureResultTime), Namespace: ns,
				Labels: map[string]string{servingv1alpha1.MeasureResultLabel: "true", servingv1alpha1.MeasureResultNameLabel: name},
			},
			Data: keptData(at),
		}
		if change != nil {
			change(cm.Data)
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, cm)).To(Succeed())
		return cm.Name
	}
	// serveAs creates the model with these annotations and plays the download and an engine Pod that is ready on the node.
	// podOf shapes the Pod's container as the Deployment of that ServedModel would.
	serveAs := func(name, node string, annotations map[string]string, podOf *servingv1alpha1.ServedModel) (*servingv1alpha1.ServedModel, types.NamespacedName) {
		sm := newServedModel(ns, name, false)
		if podOf == nil {
			podOf = sm
		}
		sm.Annotations = annotations
		ExpectWithOffset(1, k8sClient.Create(ctx, sm)).To(Succeed())
		var job *batchv1.Job
		EventuallyWithOffset(1, func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		markJobComplete(ctx, job)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-engine", Namespace: ns, Labels: childLabels(sm, componentEngine)},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
				Name: "vllm", Image: podOf.Spec.Engine.Image, Args: engineArgs(podOf),
			}}},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
		key := client.ObjectKeyFromObject(sm)
		return getServedModel(ctx, key), key
	}
	serve := func(name, node string, annotations map[string]string) (*servingv1alpha1.ServedModel, types.NamespacedName) {
		return serveAs(name, node, annotations, nil)
	}
	baselineOf := func(key types.NamespacedName) func() *servingv1alpha1.BaselineStatus {
		return func() *servingv1alpha1.BaselineStatus { return getServedModel(ctx, key).Status.Baseline }
	}
	measureJobsOf := func(name string) func() int {
		return func() int {
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(ns), client.MatchingLabels{
				servingv1alpha1.ServedModelLabel: name, componentLabel: componentMeasure,
			})).To(Succeed())
			return len(jobs.Items)
		}
	}

	It("fills the baseline from the newest result under the same conditions, and measures nothing", func() {
		node := gpuNode("NVIDIA-L40S")
		now := time.Now().UTC().Truncate(time.Second)
		keep("same", now.Add(-48*time.Hour), nil)
		newest := keep("same", now.Add(-3*time.Hour), nil)
		keep("same", now.Add(-2*time.Hour), func(d map[string]string) {
			d["partial"] = "true"
			d[baselineKey] = `{"ok":true,"reason":"MeasuredPartial","partial":true,"sla":{"ttftMs":1000,"itlMs":50,"inputTokens":1000,"outputTokens":200},"maxConcurrency":8}`
		})
		keep("same", now.Add(-60*time.Minute), func(d map[string]string) { d["engineImage"] = "vllm/vllm-openai@sha256:" + fmt.Sprintf("%064d", 7) })
		otherArgs := keep("same", now.Add(-59*time.Minute), func(d map[string]string) { d["engineArgs"] = `["--max-model-len","8192"]` })
		keep("same", now.Add(-58*time.Minute), func(d map[string]string) { d[measureGPUTypeKey] = "nvidia.com/gpu.product=NVIDIA-L4" })
		keep("same", now.Add(-57*time.Minute), func(d map[string]string) { delete(d, measureGPUTypeKey) })
		keep("same", now.Add(-56*time.Minute), func(d map[string]string) { d[measureConcurrencyKey] = "1 2 4" })
		keep("other-name", now.Add(-time.Minute), nil)

		sm, key := serve("same", node, nil)
		Eventually(baselineOf(key), testTimeout, testPoll).ShouldNot(BeNil())
		got := getServedModel(ctx, key)
		Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseReady))
		b := got.Status.Baseline
		Expect(b.ReusedFrom).To(Equal(newest))
		Expect(b.MaxConcurrency).To(Equal(int32(16)))
		Expect(b.MaxKvPercent).To(Equal(int32(31)))
		Expect(b.Partial).To(BeFalse())
		Expect(b.MeasuredAt.Time).To(BeTemporally("==", now.Add(-3*time.Hour)), "the time of the run, not of the copy")
		Expect(b.Generation).To(Equal(got.Generation))

		By("writing the proxy's copy with where it came from")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: baselineConfigMapName(sm)}, cm)).To(Succeed())
		Expect(cm.Data).To(HaveKeyWithValue("reusedFrom", newest))
		Expect(cm.Data).To(HaveKeyWithValue("maxConcurrency", "16"))
		Expect(metav1.IsControlledBy(cm, got)).To(BeTrue())

		Consistently(measureJobsOf("same"), 2*time.Second, testPoll).Should(BeZero())
		Expect(measureResults(ctx, ns, "same")).To(HaveLen(8), "reuse writes no result of its own")

		By("finding nothing for a changed spec and leaving the old baseline as it is, stale")
		Eventually(func() error {
			current := getServedModel(ctx, key)
			current.Spec.Engine.Args = []string{"--max-model-len", "4096"}
			return k8sClient.Update(ctx, current)
		}, testTimeout, testPoll).Should(Succeed())
		Eventually(func() int64 { return getServedModel(ctx, key).Status.ObservedGeneration }, testTimeout, testPoll).
			Should(Equal(got.Generation + 1))
		Consistently(func() int64 { return getServedModel(ctx, key).Status.Baseline.Generation }, time.Second, testPoll).
			Should(Equal(got.Generation))

		By("taking a result again once the spec comes back to one that was measured and its Pod is up")
		Eventually(func() error {
			current := getServedModel(ctx, key)
			current.Spec.Engine.Args = []string{"--max-model-len", "8192"}
			return k8sClient.Update(ctx, current)
		}, testTimeout, testPoll).Should(Succeed())
		Eventually(func() int64 { return getServedModel(ctx, key).Status.ObservedGeneration }, testTimeout, testPoll).
			Should(Equal(got.Generation + 2))
		Consistently(func() int64 { return getServedModel(ctx, key).Status.Baseline.Generation }, time.Second, testPoll).
			Should(Equal(got.Generation), "the ready Pod is still the one of the first spec")
		engine := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "same-engine"}, engine)).To(Succeed())
		Expect(k8sClient.Delete(ctx, engine)).To(Succeed())
		current := getServedModel(ctx, key)
		newPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "same-engine-2", Namespace: ns, Labels: childLabels(current, componentEngine)},
			Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
				Name: "vllm", Image: testImage, Args: engineArgs(current),
			}}},
		}
		Expect(k8sClient.Create(ctx, newPod)).To(Succeed())
		newPod.Status.Phase = corev1.PodRunning
		newPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, newPod)).To(Succeed())
		Eventually(func() int64 { return getServedModel(ctx, key).Status.Baseline.Generation }, testTimeout, testPoll).
			Should(Equal(got.Generation + 2))
		Expect(getServedModel(ctx, key).Status.Baseline.ReusedFrom).To(Equal(otherArgs))
	})

	It("does not take the GPU type from a ready Pod of an earlier spec", func() {
		node := gpuNode("NVIDIA-L40S")
		keep("oldpod", time.Now().Add(-time.Hour), nil)
		earlier := newServedModel(ns, "oldpod", false)
		earlier.Spec.Engine.Args = []string{"--max-model-len", "4096"}
		_, key := serveAs("oldpod", node, nil, earlier)
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		Consistently(baselineOf(key), 2*time.Second, testPoll).Should(BeNil())
	})

	It("drops a reused baseline when a run under the same conditions finds no step within the SLA, and reuses nothing after", func() {
		node := gpuNode("NVIDIA-L40S")
		kept := keep("nopass", time.Now().UTC().Truncate(time.Second).Add(-time.Hour), nil)
		sm, key := serve("nopass", node, nil)
		Eventually(func() string {
			if b := getServedModel(ctx, key).Status.Baseline; b != nil {
				return b.ReusedFrom
			}
			return ""
		}, testTimeout, testPoll).Should(Equal(kept))

		// run asks for a measurement with this value and ends it with this result on the engine's node.
		run := func(value, result string) {
			Eventually(func() error {
				current := getServedModel(ctx, key)
				current.Annotations = map[string]string{servingv1alpha1.MeasureAnnotation: value}
				return k8sClient.Update(ctx, current)
			}, testTimeout, testPoll).Should(Succeed())
			job := &batchv1.Job{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureJobNameFor(sm, value)}, job)
			}, testTimeout, testPoll).Should(Succeed())
			labels := childLabels(sm, componentMeasure)
			labels["job-name"] = job.Name
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: ns, Labels: labels},
				Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "measure", Image: testImage}}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			finishMeasure(ctx, pod, 4, result)
			markJobFailed(ctx, job)
			Eventually(func() string { return getServedModel(ctx, key).Status.MeasureRequest }, testTimeout, testPoll).Should(Equal(value))
		}

		By("leaving everything as it is when the client fails before any step ends")
		run("m-0", `{"ok":false,"reason":"NoStepWithinSLA","partial":true,"message":"SLA 안에 든 단계가 없다. vllm bench serve exited 7","sla":{"ttftMs":1000.0,"itlMs":50.0,"inputTokens":1000,"outputTokens":200},"steps":[]}`)
		Expect(getServedModel(ctx, key).Status.Baseline.ReusedFrom).To(Equal(kept))
		Expect(getServedModel(ctx, key).Status.ReuseBlockedGeneration).To(BeZero())

		By("measuring because a person asks, and finding nothing within the SLA")
		run("m-1", `{"ok":false,"reason":"NoStepWithinSLA","partial":false,"message":"SLA 안에 든 단계가 없다","sla":{"ttftMs":1000.0,"itlMs":50.0,"inputTokens":1000,"outputTokens":200},"steps":[{"c":1,"sla":false}]}`)
		Expect(getServedModel(ctx, key).Status.Reason).To(Equal("NoStepWithinSLA"))
		Expect(getServedModel(ctx, key).Status.Baseline).To(BeNil(), "the copied numbers go")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: baselineConfigMapName(sm)}, &corev1.ConfigMap{}))).
			To(BeTrue(), "the proxy's copy goes too")

		Expect(getServedModel(ctx, key).Status.ReuseBlockedGeneration).To(Equal(sm.Generation))
		Expect(measureResults(ctx, ns, "nopass")).To(HaveLen(1), "a run that measured nothing leaves no result")

		By("not taking the older result back once the annotation goes")
		Eventually(func() error {
			current := getServedModel(ctx, key)
			delete(current.Annotations, servingv1alpha1.MeasureAnnotation)
			return k8sClient.Update(ctx, current)
		}, testTimeout, testPoll).Should(Succeed())
		Eventually(func() string { return getServedModel(ctx, key).Status.MeasureRequest }, testTimeout, testPoll).Should(BeEmpty())
		Consistently(baselineOf(key), 2*time.Second, testPoll).Should(BeNil())
	})

	It("looks again after a ConfigMap in the way is gone, and asks to be woken meanwhile", func() {
		node := gpuNode("NVIDIA-L40S")
		keep("blocked", time.Now().Add(-time.Hour), nil)
		// Not in the API server, so the running controller leaves it alone and the calls here are the only ones.
		sm := newServedModel(ns, "blocked", false)
		sm.UID = types.UID("00000000-0000-0000-0000-000000000019")
		sm.Generation = 1
		theirs := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: baselineConfigMapName(sm), Namespace: ns}}
		Expect(k8sClient.Create(ctx, theirs)).To(Succeed())
		engine := &corev1.Pod{Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{
			Name: "vllm", Image: testImage, Args: engineArgs(sm),
		}}}}
		direct := &ServedModelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Config: testConfig}

		Expect(direct.reuseBaseline(ctx, sm, engine)).To(Equal(requeueWhileChanging))
		Expect(sm.Status.Baseline).To(BeNil())
		Expect(k8sClient.Delete(ctx, theirs)).To(Succeed())
		Expect(direct.reuseBaseline(ctx, sm, engine)).To(BeZero())
		Expect(sm.Status.Baseline).NotTo(BeNil())
		Expect(sm.Status.Baseline.MaxConcurrency).To(Equal(int32(16)))
	})

	It("does not reuse on another GPU type", func() {
		node := gpuNode("NVIDIA-L4")
		keep("l4", time.Now().Add(-time.Hour), nil)
		_, key := serve("l4", node, nil)
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		Consistently(baselineOf(key), 2*time.Second, testPoll).Should(BeNil())
	})

	It("does not reuse while the engine's node is unknown", func() {
		keep("nonode", time.Now().Add(-time.Hour), nil)
		_, key := serve("nonode", "", nil)
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseReady))
		Consistently(baselineOf(key), 2*time.Second, testPoll).Should(BeNil())
	})

	It("measures anew when the annotation asks, and records the GPU type and steps with the new result", func() {
		node := gpuNode("NVIDIA-L40S")
		keep("asked", time.Now().Add(-time.Hour), nil)
		sm, key := serve("asked", node, map[string]string{servingv1alpha1.MeasureAnnotation: "m-1"})
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseMeasuring))
		Expect(getServedModel(ctx, key).Status.Baseline).To(BeNil(), "no reuse while a run is asked for")

		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureJobNameFor(sm, "m-1")}, job)).To(Succeed())
		labels := childLabels(sm, componentMeasure)
		labels["job-name"] = job.Name
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: ns, Labels: labels},
			Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "measure", Image: testImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		finishMeasure(ctx, pod, 0, measuredOK)
		at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
		markJobCompleteAt(ctx, job, at)

		Eventually(func() string { return getServedModel(ctx, key).Status.Reason }, testTimeout, testPoll).Should(Equal("Measured"))
		b := getServedModel(ctx, key).Status.Baseline
		Expect(b.ReusedFrom).To(BeEmpty())
		Expect(b.MaxConcurrency).To(Equal(int32(4)))
		Expect(b.MeasuredAt.Time).To(BeTemporally("==", at))

		fresh := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: measureResultName(sm, metav1.NewTime(at))}, fresh)).To(Succeed())
		Expect(fresh.Data).To(HaveKeyWithValue(measureGPUTypeKey, testGPUType))
		Expect(fresh.Data).To(HaveKeyWithValue(measureConcurrencyKey, defaultMeasureConcurrency))
		baseline := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: baselineConfigMapName(sm)}, baseline)).To(Succeed())
		Expect(baseline.Data).NotTo(HaveKey("reusedFrom"))

		By("keeping the measured baseline when the annotation goes")
		Eventually(func() error {
			current := getServedModel(ctx, key)
			delete(current.Annotations, servingv1alpha1.MeasureAnnotation)
			return k8sClient.Update(ctx, current)
		}, testTimeout, testPoll).Should(Succeed())
		Eventually(func() int { return measureJobsOf("asked")() }, testTimeout, testPoll).Should(BeZero())
		Consistently(func() string { return getServedModel(ctx, key).Status.Baseline.ReusedFrom }, time.Second, testPoll).Should(BeEmpty())
		Expect(getServedModel(ctx, key).Status.Baseline.MeasuredAt.Time).To(BeTemporally("==", at))
	})
})
