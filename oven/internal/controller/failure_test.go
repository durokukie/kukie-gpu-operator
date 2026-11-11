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
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// tail keeps the last n lines, as the kubelet's tailLines does.
func tail(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "failures", name))
	if err != nil {
		t.Fatal(err)
	}
	return tail(string(data), failureLogLines)
}

// TestFailurePatternsOnSeenLogs runs the built-in table over the logs in testdata/failures (see its README for where
// each one comes from) and over scheduler, kubelet and event messages as Kubernetes writes them.
func TestFailurePatternsOnSeenLogs(t *testing.T) {
	cases := []struct {
		name     string
		ev       failureEvidence
		reason   string
		contains string
	}{
		{name: "hf CLI 2.x, gated model, no token", ev: failureEvidence{stage: stageDownload, log: fixture(t, "hf-gated.log")}, reason: "HfAuth"},
		{name: "hf with HF_DEBUG, GatedRepoError", ev: failureEvidence{stage: stageDownload, log: fixture(t, "hf-gated-debug.log")}, reason: "HfAuth"},
		{name: "hf, repository not found", ev: failureEvidence{stage: stageDownload, log: fixture(t, "hf-not-found.log")}, reason: "HfAuth"},
		{name: "hf, disk full", ev: failureEvidence{stage: stageDownload, log: fixture(t, "hf-disk-full.log")}, reason: "DiskShort"},
		{
			name:   "Qwen3.8-27B without --max-num-seqs",
			ev:     failureEvidence{stage: stageEngine, log: fixture(t, "vllm-mamba-max-num-seqs.log")},
			reason: "GpuMemoryShort", contains: "동시 요청 256개 몫의 상태 캐시(Mamba)를 잡지 못했다. 잡을 수 있는 것은 185개다",
		},
		{name: "KV cache short of max seq len", ev: failureEvidence{stage: stageEngine, log: fixture(t, "vllm-kv-cache-short.log")}, reason: "GpuMemoryShort"},
		{name: "CUDA OOM while loading", ev: failureEvidence{stage: stageEngine, log: fixture(t, "vllm-cuda-oom.log")}, reason: "GpuMemoryShort"},
		{
			name:   "unsupported architecture",
			ev:     failureEvidence{stage: stageEngine, log: fixture(t, "vllm-unsupported-arch.log")},
			reason: "UnsupportedArch", contains: "모델 구조 'NewArchForCausalLM'를 모른다",
		},
		{
			// runbooks/oven.md: what the scheduler writes while no GPU node is up.
			name:   "no GPU node",
			ev:     failureEvidence{stage: stageEngine, status: "Unschedulable: 0/2 nodes are available: 2 Insufficient nvidia.com/gpu."},
			reason: "NoGpuNode",
		},
		{
			name: "node selector matches no node",
			ev: failureEvidence{stage: stageDownload, status: "Unschedulable: 0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector. " +
				"preemption: 0/3 nodes are available: 3 Preemption is not helpful for scheduling."},
			reason: "NoGpuNode",
		},
		{
			name:   "PVC not bound",
			ev:     failureEvidence{stage: stageDownload, status: "Unschedulable: 0/2 nodes are available: pod has unbound immediate PersistentVolumeClaims."},
			reason: "DiskShort", contains: "PVC",
		},
		{
			// history/2026-11.md: the CSI driver was not up on the new node yet.
			name:   "PVC provisioning failed, from events",
			ev:     failureEvidence{stage: stageDownload, status: "Unschedulable: 0/1 nodes are available", events: "ProvisioningFailed: failed to provision volume with StorageClass \"ebs-csi-default-sc\": error generating accessibility requirements: no topology key found for node ip-10-0-1-2"},
			reason: "DiskShort",
		},
		{
			name:   "image pull",
			ev:     failureEvidence{stage: stageEngine, status: "ImagePullBackOff: Back-off pulling image \"vllm/vllm-openai:v9.9.9\""},
			reason: "ImagePull",
		},
		{
			name:   "host memory",
			ev:     failureEvidence{stage: stageEngine, status: "CrashLoopBackOff: back-off 40s\n마지막 종료: exit code 137 (OOMKilled)"},
			reason: "HostMemoryShort",
		},
		{
			name:   "download deadline",
			ev:     failureEvidence{stage: stageDownload, status: "DownloadFailed: DeadlineExceeded: Job was active longer than specified deadline"},
			reason: "DownloadTimeout",
		},
	}
	patterns := DefaultFailurePatterns()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := patterns.classify(tc.ev)
			if got.reason != tc.reason {
				t.Fatalf("reason = %s, want %s (message %q)", got.reason, tc.reason, got.message)
			}
			if got.message == "" || got.hint == "" {
				t.Fatalf("message and hint must be set: %+v", got)
			}
			if tc.contains != "" && !strings.Contains(got.message, tc.contains) {
				t.Fatalf("message %q does not contain %q", got.message, tc.contains)
			}
		})
	}
}

func TestFailurePatternsKeepLogsOutOfStatus(t *testing.T) {
	patterns := DefaultFailurePatterns()
	secretLog := "INFO Received request chatcmpl-1: prompt: 'my secret plan for hf_abcdefghijklmnopqrstuvwxyz0123'\n" +
		"Traceback (most recent call last):\nRuntimeError: something nobody has seen before"
	got := patterns.classify(failureEvidence{
		stage: stageEngine, status: "CrashLoopBackOff: back-off 10s", log: secretLog,
		fallback: "쿠버네티스 상태: CrashLoopBackOff, 종료 코드 1.",
		logHint:  "원문 로그는 kubectl -n llm logs m-1 -c vllm --previous 로 본다.",
	})
	if got.reason != ReasonUnclassified {
		t.Fatalf("reason = %s", got.reason)
	}
	for _, leak := range []string{"secret plan", "hf_", "RuntimeError", "nobody"} {
		if strings.Contains(got.message+got.hint, leak) {
			t.Fatalf("status would carry %q: %+v", leak, got)
		}
	}
	if !strings.Contains(got.message, "종료 코드 1") || !strings.Contains(got.hint, "--previous") {
		t.Fatalf("unclassified should say the exit code and where the log is: %+v", got)
	}

	// Stage: a download row never classifies the engine (the engine reads no Hub, HF_HUB_OFFLINE=1).
	if got := patterns.classify(failureEvidence{stage: stageEngine, log: fixture(t, "hf-gated.log")}); got.reason != ReasonUnclassified {
		t.Fatalf("engine log classified by a download row: %s", got.reason)
	}
}

func TestFailurePatternCaptureIsChecked(t *testing.T) {
	patterns := DefaultFailurePatterns()
	for _, arch := range []string{
		"'hf_abcdefghijklmnopqrstuvwxyz0123'",
		"'" + strings.Repeat("A", 100) + "'",
		"'<script>'",
	} {
		got := patterns.classify(failureEvidence{stage: stageEngine,
			log: "ValueError: Model architectures [" + arch + "] are not supported for now."})
		if got.reason != "UnsupportedArch" || !strings.Contains(got.message, "(생략)") || strings.Contains(got.message, "hf_") {
			t.Fatalf("%s: %+v", arch, got)
		}
	}
}

func TestRedactSecrets(t *testing.T) {
	for in, want := range map[string]string{
		"token hf_abcdefghijklmnopqrstuvwxyz0123 bad":             "token [가림] bad",
		"Authorization: Bearer abc.def.ghi-123":                   "Authorization: [가림]",
		"HF_TOKEN=xyz123 and more":                                "HF_TOKEN=[가림] and more",
		`{"api_key": "sk-proj-abcdefghijklmnopqrstu"}`:            `{"api_key": "[가림]"}`,
		"0/2 nodes are available: 2 Insufficient nvidia.com/gpu.": "0/2 nodes are available: 2 Insufficient nvidia.com/gpu.",
	} {
		if got := redactSecrets(in); got != want {
			t.Errorf("redactSecrets(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFailurePatternsRejects(t *testing.T) {
	row := func(extra string) string {
		return "patterns:\n  - reason: Foo\n    match: 'foo'\n    message: m\n    hint: h\n" + extra
	}
	if _, err := ParseFailurePatterns([]byte(row(""))); err != nil {
		t.Fatalf("a plain row: %v", err)
	}
	for name, text := range map[string]string{
		"no rows":            "patterns: []\n",
		"bad regex":          "patterns:\n  - {reason: Foo, match: '(', message: m, hint: h}\n",
		"empty match":        "patterns:\n  - {reason: Foo, match: 'x*', message: m, hint: h}\n",
		"lower-case reason":  "patterns:\n  - {reason: foo, match: 'x', message: m, hint: h}\n",
		"oven's own reason":  "patterns:\n  - {reason: Unclassified, match: 'x', message: m, hint: h}\n",
		"no hint":            "patterns:\n  - {reason: Foo, match: 'x', message: m}\n",
		"unknown source":     "patterns:\n  - {reason: Foo, match: 'x', message: m, hint: h, in: [stdout]}\n",
		"unknown stage":      "patterns:\n  - {reason: Foo, match: 'x', message: m, hint: h, stage: measure}\n",
		"unknown field":      "patterns:\n  - {reason: Foo, match: 'x', message: m, hint: h, hints: h}\n",
		"uncaptured ${name}": "patterns:\n  - {reason: Foo, match: 'x', message: 'm ${arch}', hint: h}\n",
		"long message":       "patterns:\n  - {reason: Foo, match: 'x', message: '" + strings.Repeat("m", 513) + "', hint: h}\n",
	} {
		if _, err := ParseFailurePatterns([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFailurePatternSourceReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "patterns.yaml")
	var errs []error
	source := &FailurePatternSource{Path: path, OnError: func(err error) { errs = append(errs, err) }}

	// No file yet: the built-in table, and the error is reported.
	if got := source.Patterns().classify(failureEvidence{stage: stageEngine, log: "CUDA out of memory"}); got.reason != "GpuMemoryShort" {
		t.Fatalf("built-in table not used: %+v", got)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v", errs)
	}

	write := func(text string, at time.Time) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now()
	write("patterns:\n  - {reason: NcclTimeout, match: 'NCCL.*timed out', message: GPU 사이 통신이 멈췄다, hint: 노드를 바꾼다}\n", base)
	if got := source.Patterns().classify(failureEvidence{stage: stageEngine, log: "NCCL watchdog timed out"}); got.reason != "NcclTimeout" {
		t.Fatalf("new row not read: %+v", got)
	}
	// A broken edit keeps the last good table.
	write("patterns:\n  - {reason: Broken, match: '(', message: m, hint: h}\n", base.Add(time.Minute))
	if got := source.Patterns().classify(failureEvidence{stage: stageEngine, log: "NCCL watchdog timed out"}); got.reason != "NcclTimeout" {
		t.Fatalf("broken file replaced the table: %+v", got)
	}
	if len(errs) != 2 {
		t.Fatalf("errors = %v", errs)
	}
}

// The chart installs the same table the manager has built in (make manifests copies it).
func TestChartFailurePatternsMatchBuiltIn(t *testing.T) {
	chart, err := os.ReadFile(filepath.Join("..", "..", "..", "charts", "kukie-gpu", "files", "oven", "failure-patterns.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(chart, defaultFailurePatterns) {
		t.Fatal("charts/kukie-gpu/files/oven/failure-patterns.yaml differs from failure_patterns.yaml: run make manifests")
	}
}

// fakeDiagnostics answers LogTail and WarningEvents from fixed text and counts the reads.
type fakeDiagnostics struct {
	log, previousLog string
	events           map[string][]string
	err              error
	logReads         int
	lastPrevious     bool
}

func (f *fakeDiagnostics) LogTail(_ context.Context, _, _, _ string, previous bool, _ int64) (string, error) {
	f.logReads++
	f.lastPrevious = previous
	if f.err != nil {
		return "", f.err
	}
	if previous {
		return f.previousLog, nil
	}
	return f.log, nil
}

func (f *fakeDiagnostics) WarningEvents(_ context.Context, _, kind, _ string) ([]string, error) {
	return f.events[kind], nil
}

func crashLoopPod(restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "m-abc", Namespace: "llm", UID: types.UID("uid-1")},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: engineContainer, RestartCount: restarts,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 40s"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}},
	}
}

func TestDiagnoseReadsThePreviousLogOncePerAttempt(t *testing.T) {
	sm := &servingv1alpha1.ServedModel{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "llm"}}
	fake := &fakeDiagnostics{previousLog: fixture(t, "vllm-mamba-max-num-seqs.log")}
	r := &ServedModelReconciler{Diagnostics: fake}

	pod := crashLoopPod(3)
	verdict := classifyPod(pod)
	got := r.diagnose(context.Background(), sm, stageEngine, pod, verdict)
	if got.reason != "GpuMemoryShort" || !fake.lastPrevious {
		t.Fatalf("got %+v, previous=%v", got, fake.lastPrevious)
	}
	r.diagnose(context.Background(), sm, stageEngine, pod, verdict)
	if fake.logReads != 1 {
		t.Fatalf("same attempt read the log %d times", fake.logReads)
	}
	pod = crashLoopPod(4)
	r.diagnose(context.Background(), sm, stageEngine, pod, classifyPod(pod))
	if fake.logReads != 2 {
		t.Fatalf("a restart did not read the log again: %d", fake.logReads)
	}

	// A read that failed is not remembered: the next look tries again.
	r.forgetDiagnoses(types.NamespacedName{Namespace: "llm", Name: "m"})
	fake.err = errors.New("kubelet down")
	if got := r.diagnose(context.Background(), sm, stageEngine, pod, classifyPod(pod)); got.reason != ReasonUnclassified {
		t.Fatalf("without the log: %+v", got)
	}
	r.diagnose(context.Background(), sm, stageEngine, pod, classifyPod(pod))
	if fake.logReads != 4 {
		t.Fatalf("a failed read was cached: %d reads", fake.logReads)
	}
}

func TestDiagnoseUsesEventsAndSkipsTheLogOfAPodThatNeverRan(t *testing.T) {
	sm := &servingv1alpha1.ServedModel{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "llm"}}
	fake := &fakeDiagnostics{events: map[string][]string{
		"PersistentVolumeClaim": {"ProvisioningFailed: failed to provision volume with StorageClass \"gp3\": rpc error"},
	}}
	r := &ServedModelReconciler{Diagnostics: fake}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "m-dl", Namespace: "llm", UID: "uid-2"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/1 nodes are available: 1 node(s) did not have enough free storage."}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: downloadContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}},
	}
	got := r.diagnose(context.Background(), sm, stageDownload, pod, classifyPod(pod))
	if got.reason != "DiskShort" || fake.logReads != 0 {
		t.Fatalf("got %+v after %d log reads", got, fake.logReads)
	}
}

func TestStillLoading(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	r := &ServedModelReconciler{Config: Config{StillLoadingAfter: 15 * time.Minute}, Now: func() time.Time { return now }}
	pod := func(started time.Time) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: engineContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
		}}}}
	}
	if _, long := r.stillLoading(pod(now.Add(-14 * time.Minute))); long {
		t.Fatal("14 minutes is not long")
	}
	d, long := r.stillLoading(pod(now.Add(-22 * time.Minute)))
	if !long || d.reason != ReasonStillLoading || !strings.Contains(d.message, "22분째") || d.hint == "" {
		t.Fatalf("got %+v", d)
	}
	if _, long := r.stillLoading(nil); long {
		t.Fatal("no Pod is not loading")
	}
	r.Config.StillLoadingAfter = 0
	if _, long := r.stillLoading(pod(now.Add(-time.Hour))); long {
		t.Fatal("0 turns it off")
	}
}

// podDiagnostics is the envtest suite's DiagnosticsReader: previous-container logs by Pod name, no events.
type podDiagnostics struct {
	mu       sync.Mutex
	previous map[string]string
}

var testDiagnostics = &podDiagnostics{previous: map[string]string{}}

func (d *podDiagnostics) setPrevious(pod *corev1.Pod, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.previous[pod.Namespace+"/"+pod.Name] = text
}

func (d *podDiagnostics) LogTail(_ context.Context, namespace, pod, _ string, previous bool, _ int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !previous {
		return "", nil
	}
	return d.previous[namespace+"/"+pod], nil
}

func (d *podDiagnostics) WarningEvents(context.Context, string, string, string) ([]string, error) {
	return nil, nil
}

// fixtureText reads a testdata/failures log for a Ginkgo spec (no *testing.T there).
func fixtureText(name string) string {
	data, err := os.ReadFile(filepath.Join("testdata", "failures", name))
	if err != nil {
		panic(err)
	}
	return tail(string(data), failureLogLines)
}

// Review of #44: a FailedScheduling left from waiting for a node must not name the cause of a later failure.
func TestOldEventsDoNotWinOverTheCurrentFailure(t *testing.T) {
	stale := "FailedScheduling: 0/2 nodes are available: 2 Insufficient nvidia.com/gpu."
	patterns := DefaultFailurePatterns()
	if got := patterns.classify(failureEvidence{stage: stageEngine, status: "ImagePullBackOff: Back-off pulling image", events: stale}); got.reason != "ImagePull" {
		t.Fatalf("status lost to an event: %s", got.reason)
	}
	if got := patterns.classify(failureEvidence{stage: stageEngine, log: "torch.OutOfMemoryError: CUDA out of memory.",
		events: "ProvisioningFailed: failed to provision volume"}); got.reason != "GpuMemoryShort" {
		t.Fatalf("log lost to an event: %s", got.reason)
	}

	// A Pod whose container ran is past scheduling and volumes: its events are not read at all.
	sm := &servingv1alpha1.ServedModel{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "llm"}}
	fake := &fakeDiagnostics{previousLog: "RuntimeError: something new", events: map[string][]string{"Pod": {stale}}}
	r := &ServedModelReconciler{Diagnostics: fake}
	pod := crashLoopPod(2)
	if got := r.diagnose(context.Background(), sm, stageEngine, pod, classifyPod(pod)); got.reason != ReasonUnclassified {
		t.Fatalf("a crash after the node came was called %s", got.reason)
	}
}

// Review of #44: a server that passed its startup probe and then lost readiness is not loading.
func TestStillLoadingSkipsAContainerThatStartedOnce(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	r := &ServedModelReconciler{Config: Config{StillLoadingAfter: 15 * time.Minute}, Now: func() time.Time { return now }}
	started := true
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: engineContainer, Started: &started,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-3 * time.Hour))}},
	}}}}
	if d, long := r.stillLoading(pod); long {
		t.Fatalf("a served-then-unready engine reads as loading: %+v", d)
	}
}
