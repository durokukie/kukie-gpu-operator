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
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// fakeLogs stands in for the kubelet, which envtest does not have. It counts reads, so "does not read" can be tested.
type fakeLogs struct {
	mu    sync.Mutex
	text  map[string]string
	err   map[string]error
	calls map[string]int
	last  string
	lines int64
}

var testLogs = &fakeLogs{text: map[string]string{}, err: map[string]error{}, calls: map[string]int{}}

func (f *fakeLogs) TailLines(_ context.Context, namespace, pod, container string, lines int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := namespace + "/" + pod
	f.calls[key]++
	f.last, f.lines = container, lines
	return f.text[key], f.err[key]
}

func (f *fakeLogs) set(pod *corev1.Pod, text string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text[pod.Namespace+"/"+pod.Name], f.err[pod.Namespace+"/"+pod.Name] = text, err
}

func (f *fakeLogs) reads(pod *corev1.Pod) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[pod.Namespace+"/"+pod.Name]
}

func progressLine(downloaded int64, total any) string {
	if total == nil {
		total = "null"
	}
	return fmt.Sprintf(`{"event": "download_progress", "downloadedBytes": %d, "totalBytes": %v}`, downloaded, total)
}

func TestProgressStatus(t *testing.T) {
	giB := int64(1 << 30)
	tests := []struct {
		name             string
		downloaded       int64
		total            any
		percent          int32
		wantDownloaded   int64
		wantTotalIsKnown bool
	}{
		{"nothing yet", 0, 100, 0, 0, true},
		{"rounds down", 299, 1000, 29, 299, true},
		{"real sizes", 12 * giB, 41 * giB, 29, 12 * giB, true},
		{"sizes where percent times bytes overflows int64", 1 << 61, int64(1) << 62, 50, 1 << 61, true},
		{"all files there, Job not done", 1000, 1000, 99, 1000, true},
		{"just under the end", 999, 1000, 99, 999, true},
		{"more than the total", 2000, 1000, 99, 1000, true},
		{"total unknown", 5000, nil, 0, 5000, false},
		{"total zero counts as unknown", 5000, 0, 0, 5000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, ok := lastDownloadProgress(progressLine(tt.downloaded, tt.total))
			if !ok {
				t.Fatal("line not accepted")
			}
			got := line.status()
			if got.Percent != tt.percent || *got.DownloadedBytes != tt.wantDownloaded {
				t.Errorf("percent %d, downloaded %d", got.Percent, *got.DownloadedBytes)
			}
			if (got.TotalBytes != nil) != tt.wantTotalIsKnown {
				t.Errorf("total = %v", got.TotalBytes)
			}
		})
	}
}

func TestLastDownloadProgress(t *testing.T) {
	good := progressLine(10, 100)
	newer := progressLine(20, 100)
	tests := []struct {
		name string
		logs string
		want int64 // downloadedBytes; -1 means no usable line
	}{
		{"empty", "", -1},
		{"no progress at all", "Fetching 3 files\nDownloaded\n", -1},
		{"the newest of several", good + "\n" + newer + "\n", 20},
		{"hf output after the line", good + "\n✓ Downloaded\n  path: /models/hub/x\n", 10},
		{"a cut last line", good + "\n" + newer[:20], 10},
		{"a line that is not ours", good + "\n" + `{"downloadedBytes": 99}`, 10},
		{"another event", good + "\n" + `{"event": "x", "downloadedBytes": 99}`, 10},
		{"no bytes", good + "\n" + `{"event": "download_progress", "totalBytes": 99}`, 10},
		{"negative bytes", good + "\n" + `{"event": "download_progress", "downloadedBytes": -5}`, 10},
		{"negative total", good + "\n" + `{"event": "download_progress", "downloadedBytes": 5, "totalBytes": -5}`, 10},
		{"a string for a number", good + "\n" + `{"event": "download_progress", "downloadedBytes": "5"}`, 10},
		{"beyond int64", good + "\n" + `{"event": "download_progress", "downloadedBytes": 99999999999999999999}`, 10},
		{"only broken lines", "{\n}\nnull\n[]\n42\n", -1},
		{"carriage returns", good + "\r\n", 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, ok := lastDownloadProgress(tt.logs)
			if tt.want < 0 {
				if ok {
					t.Errorf("accepted %+v", line)
				}
				return
			}
			if !ok || *line.DownloadedBytes != tt.want {
				t.Errorf("ok %v, line %+v", ok, line)
			}
		})
	}
}

func TestFinishedDownload(t *testing.T) {
	total, stale := int64(1000), int64(900)
	got := finishedDownload(&servingv1alpha1.DownloadStatus{Percent: 90, DownloadedBytes: &stale, TotalBytes: &total})
	if got.Percent != 100 || *got.DownloadedBytes != 1000 || *got.TotalBytes != 1000 {
		t.Errorf("known total: %+v", got)
	}
	for _, last := range []*servingv1alpha1.DownloadStatus{nil, {DownloadedBytes: &stale}} {
		got := finishedDownload(last)
		if got.Percent != 100 || got.DownloadedBytes != nil || got.TotalBytes != nil {
			t.Errorf("unknown total from %+v: %+v", last, got)
		}
	}
}

// The real reader must ask for the end of the log of the one container, not for the whole log.
func TestClientsetLogReaderAsksForTheTail(t *testing.T) {
	clientset := fake.NewClientset()
	got, err := NewPodLogReader(clientset).TailLines(context.Background(), "llm", "pod", downloadContainer, progressTailLines)
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Error("the fake's body did not come through")
	}
	for _, action := range clientset.Actions() {
		generic, ok := action.(clienttesting.GenericAction)
		if !ok || action.GetSubresource() != "log" {
			continue
		}
		opts := generic.GetValue().(*corev1.PodLogOptions)
		if *opts.TailLines != progressTailLines || opts.Container != downloadContainer || *opts.LimitBytes != progressLimitBytes {
			t.Errorf("options = %+v", opts)
		}
		return
	}
	t.Errorf("no log request among %v", clientset.Actions())
}

func TestClaimLogReadWaitsForTheInterval(t *testing.T) {
	clock := &fakeClock{}
	r := &ServedModelReconciler{Now: clock.Now}
	key := types.NamespacedName{Namespace: "llm", Name: "model"}
	if wait := r.claimLogRead(key); wait != 0 {
		t.Fatalf("first read waits %v", wait)
	}
	if wait := r.claimLogRead(key); wait <= 0 || wait > progressReadInterval {
		t.Errorf("second read right away: wait %v", wait)
	}
	if wait := r.claimLogRead(types.NamespacedName{Namespace: "llm", Name: "other"}); wait != 0 {
		t.Errorf("another model waits %v", wait)
	}
	clock.advance(progressReadInterval)
	if wait := r.claimLogRead(key); wait != 0 {
		t.Errorf("after the interval: wait %v", wait)
	}
	r.forgetLogRead(key)
	if wait := r.claimLogRead(key); wait != 0 {
		t.Errorf("after forgetting: wait %v", wait)
	}
}

// A file named in a go:embed has to be let through .dockerignore, or only the image build breaks. It happened once.
func TestDockerignoreLetsTheScriptsThrough(t *testing.T) {
	ignore, err := os.ReadFile("../../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"measure.py", "download.py"} {
		if !strings.Contains(string(ignore), "!internal/controller/"+name+"\n") {
			t.Errorf(".dockerignore does not include %s", name)
		}
	}
}

func TestDownloadScriptIsEmbedded(t *testing.T) {
	if !strings.Contains(downloadScript, `"hf", "download"`) || !strings.Contains(downloadScript, progressEvent) {
		t.Error("the embedded script does not run hf download or does not write the event the oven looks for")
	}
}

var _ = Describe("Download progress", func() {
	ctx := context.Background()

	var ns string

	BeforeEach(func() {
		ns = nextWatchedNamespace()
		DeferCleanup(testClock.reset)
	})

	// touch wakes the reconcile.
	touch := func(key types.NamespacedName) {
		EventuallyWithOffset(1, func() error {
			got := &servingv1alpha1.ServedModel{}
			if err := k8sClient.Get(ctx, key, got); err != nil {
				return err
			}
			if got.Annotations == nil {
				got.Annotations = map[string]string{}
			}
			got.Annotations["touch"] = time.Now().String()
			return k8sClient.Update(ctx, got)
		}, testTimeout, testPoll).Should(Succeed())
	}
	// nextRead moves past the read interval and wakes the reconcile, so the tests do not sit out 10 seconds each.
	nextRead := func(key types.NamespacedName) {
		testClock.advance(progressReadInterval + time.Second)
		touch(key)
	}
	download := func(key types.NamespacedName) *servingv1alpha1.DownloadStatus {
		return getServedModel(ctx, key).Status.Download
	}
	reasonOf := func(key types.NamespacedName) func() string {
		return func() string { return getServedModel(ctx, key).Status.Reason }
	}
	percentOf := func(key types.NamespacedName) func() int32 {
		return func() int32 {
			if d := download(key); d != nil {
				return d.Percent
			}
			return -1
		}
	}
	readsOf := func(pod *corev1.Pod) func() int {
		return func() int { return testLogs.reads(pod) }
	}
	started := func(name, limit string) (*servingv1alpha1.ServedModel, types.NamespacedName, *batchv1.Job) {
		sm := newServedModel(ns, name, true)
		if limit != "" {
			d, err := time.ParseDuration(limit)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			sm.Spec.MaxRuntime = &metav1.Duration{Duration: d}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, sm)).To(Succeed())
		var job *batchv1.Job
		EventuallyWithOffset(1, func() *batchv1.Job {
			job = findDownloadJob(ctx, sm)
			return job
		}, testTimeout, testPoll).ShouldNot(BeNil())
		return sm, client.ObjectKeyFromObject(sm), job
	}
	setContainer := func(pod *corev1.Pod, phase corev1.PodPhase, state corev1.ContainerState) {
		pod.Status.Phase = phase
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: downloadContainer, State: state}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
	runningPod := func(sm *servingv1alpha1.ServedModel, job *batchv1.Job, logs string) *corev1.Pod {
		pod := createPod(ctx, sm, componentDownload, job.Name+"-x1")
		testLogs.set(pod, logs, nil)
		setContainer(pod, corev1.PodRunning, running)
		return pod
	}

	It("goes from PreparingPod to JobRunning and does not read a log before the container runs", func() {
		sm, key, job := started("prep", "")
		Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("PreparingPod"), "no Pod yet")
		Expect(getServedModel(ctx, key).Status.Phase).To(Equal(servingv1alpha1.PhaseDownloading))

		pod := createPod(ctx, sm, componentDownload, job.Name+"-x1")
		testLogs.set(pod, progressLine(30, 100), nil)
		setContainer(pod, corev1.PodPending, waiting)
		touch(key)
		Consistently(reasonOf(key), time.Second, testPoll).Should(Equal("PreparingPod"))
		Expect(testLogs.reads(pod)).To(BeZero())
		Expect(getServedModel(ctx, key).Status.Phase).To(Equal(servingv1alpha1.PhaseDownloading))

		setContainer(pod, corev1.PodRunning, running)
		Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("JobRunning"))
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(30)))
		Expect(testLogs.last).To(Equal(downloadContainer))
		Expect(testLogs.lines).To(Equal(int64(progressTailLines)))
	})

	It("follows the log every 10 seconds, whatever else wakes it, and ends at 100 on success", func() {
		_, key, job := started("follow", "")
		sm := getServedModel(ctx, key)
		pod := runningPod(sm, job, progressLine(30, 100))
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(30)))
		got := download(key)
		Expect(got.DownloadedBytes).To(HaveValue(Equal(int64(30))))
		Expect(got.TotalBytes).To(HaveValue(Equal(int64(100))))

		By("not reading again before the interval, even when woken")
		reads := testLogs.reads(pod)
		testLogs.set(pod, progressLine(50, 100), nil)
		touch(key)
		touch(key)
		Consistently(readsOf(pod), time.Second, testPoll).Should(Equal(reads))
		Expect(percentOf(key)()).To(Equal(int32(30)))

		By("coming back by itself when the interval is over")
		Eventually(percentOf(key), progressReadInterval+5*time.Second, testPoll).Should(Equal(int32(50)))

		By("stopping at 99 while the Job has not succeeded")
		testLogs.set(pod, progressLine(100, 100), nil)
		nextRead(key)
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(99)))
		Expect(getServedModel(ctx, key).Status.Phase).To(Equal(servingv1alpha1.PhaseDownloading))

		By("showing 100 and no more reading once the Job has succeeded")
		testLogs.set(pod, progressLine(96, 100), nil)
		markJobComplete(ctx, job)
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseLoading))
		got = download(key)
		Expect(got.Percent).To(Equal(int32(100)))
		Expect(got.DownloadedBytes).To(HaveValue(Equal(int64(100))))
		Expect(got.TotalBytes).To(HaveValue(Equal(int64(100))))
		reads = testLogs.reads(pod)
		nextRead(key)
		Consistently(readsOf(pod), time.Second, testPoll).Should(Equal(reads))
		Expect(percentOf(key)()).To(Equal(int32(100)))
	})

	It("keeps the last numbers and the phase when the log cannot be read or says nothing usable", func() {
		_, key, job := started("unreadable", "")
		pod := runningPod(getServedModel(ctx, key), job, "")
		Eventually(readsOf(pod), testTimeout, testPoll).Should(BeNumerically(">", 0))
		Expect(percentOf(key)()).To(Equal(int32(0)))
		Expect(download(key).DownloadedBytes).To(BeNil(), "nothing to say yet")

		testLogs.set(pod, progressLine(40, 100), nil)
		nextRead(key)
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(40)))

		for name, change := range map[string]func(){
			"the read fails": func() { testLogs.set(pod, "", errors.New("the kubelet is away")) },
			"the Pod says no": func() {
				testLogs.set(pod, "", apierrors.NewForbidden(corev1.Resource("pods/log"), pod.Name, errors.New("no")))
			},
			"the line is cut":  func() { testLogs.set(pod, `{"event": "download_prog`, nil) },
			"no progress line": func() { testLogs.set(pod, "Fetching files\n", nil) },
		} {
			By(name)
			change()
			reads := testLogs.reads(pod)
			nextRead(key)
			Eventually(readsOf(pod), testTimeout, testPoll).Should(BeNumerically(">", reads))
			Consistently(percentOf(key), 500*time.Millisecond, testPoll).Should(Equal(int32(40)))
			got := getServedModel(ctx, key)
			Expect(got.Status.Phase).To(Equal(servingv1alpha1.PhaseDownloading))
			Expect(got.Status.Reason).To(Equal("JobRunning"))
			Expect(got.Status.Download.DownloadedBytes).To(HaveValue(Equal(int64(40))))
		}
	})

	It("shows bytes without a percent when the total is unknown", func() {
		_, key, job := started("unknown", "")
		runningPod(getServedModel(ctx, key), job, progressLine(7000, nil))
		Eventually(func() *int64 {
			if d := download(key); d != nil {
				return d.DownloadedBytes
			}
			return nil
		}, testTimeout, testPoll).Should(HaveValue(Equal(int64(7000))))
		got := download(key)
		Expect(got.Percent).To(BeZero())
		Expect(got.TotalBytes).To(BeNil())

		By("not making up a total on success")
		markJobComplete(ctx, job)
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(100)))
		got = download(key)
		Expect(got.DownloadedBytes).To(BeNil())
		Expect(got.TotalBytes).To(BeNil())
	})

	It("keeps a known total when a later line does not have it yet", func() {
		_, key, job := started("keeptotal", "")
		pod := runningPod(getServedModel(ctx, key), job, progressLine(60, 100))
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(60)))

		testLogs.set(pod, progressLine(70, nil), nil)
		nextRead(key)
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(70)))
		Expect(download(key).TotalBytes).To(HaveValue(Equal(int64(100))))

		markJobComplete(ctx, job)
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(100)))
		Expect(download(key).DownloadedBytes).To(HaveValue(Equal(int64(100))))
	})

	It("does not read while the Pod waits for a node or an attempt has ended, and keeps the numbers", func() {
		sm, key, job := started("retry", "")
		pod := runningPod(sm, job, progressLine(60, 100))
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(60)))

		setContainer(pod, corev1.PodFailed, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}})
		Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("Retrying"))
		reads := testLogs.reads(pod)
		nextRead(key)
		Consistently(readsOf(pod), time.Second, testPoll).Should(Equal(reads))
		Expect(percentOf(key)()).To(Equal(int32(60)), "a retry starts where the files are, and the new Pod's line will say")
		Expect(k8sClient.Delete(ctx, pod)).To(Succeed())

		By("waiting for a node")
		retry := createPod(ctx, sm, componentDownload, job.Name+"-x2")
		testLogs.set(retry, progressLine(61, 100), nil)
		retry.Status.Phase = corev1.PodPending
		retry.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "no node",
		}}
		Expect(k8sClient.Status().Update(ctx, retry)).To(Succeed())
		Eventually(reasonOf(key), testTimeout, testPoll).Should(Equal("WaitingForNode"))
		nextRead(key)
		Consistently(readsOf(retry), time.Second, testPoll).Should(BeZero())
		Expect(percentOf(key)()).To(Equal(int32(60)))
	})

	It("does not read the Pod of another Job", func() {
		sm, key, _ := started("oldpod", "")
		pod := createPod(ctx, sm, componentDownload, "oldpod-download-old-x1")
		testLogs.set(pod, progressLine(100, 100), nil)
		setContainer(pod, corev1.PodRunning, running)
		nextRead(key)
		Consistently(readsOf(pod), time.Second, testPoll).Should(BeZero())
		Expect(getServedModel(ctx, key).Status.Reason).To(Equal("PreparingPod"))

		By("not taking its failure for this Job's: the new source may just have fixed the image")
		setContainer(pod, corev1.PodPending, corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "old image"},
		})
		touch(key)
		Consistently(phaseOf(ctx, key), time.Second, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))
		Expect(getServedModel(ctx, key).Status.Reason).To(Equal("PreparingPod"))
	})

	It("goes back to 0 for a new source", func() {
		sm, key, job := started("newsource", "")
		runningPod(sm, job, progressLine(80, 100))
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(80)))
		markJobComplete(ctx, job)
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(100)))

		Eventually(func() error {
			got := getServedModel(ctx, key)
			got.Spec.Source.HuggingFace = "openai/gpt-oss-120b"
			return k8sClient.Update(ctx, got)
		}, testTimeout, testPoll).Should(Succeed())
		Eventually(percentOf(key), testTimeout, testPoll).Should(BeZero())
		got := download(key)
		Expect(got.DownloadedBytes).To(BeNil())
		Expect(got.TotalBytes).To(BeNil())
	})

	It("stops with the run time limit like before, keeps the numbers and reads no more", func() {
		sm, key, job := started("limit", "1h")
		pod := runningPod(sm, job, progressLine(25, 100))
		Eventually(percentOf(key), testTimeout, testPoll).Should(Equal(int32(25)))

		testClock.advance(time.Hour + time.Minute)
		touch(key)
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseStopped))
		Eventually(func() bool {
			current := &batchv1.Job{}
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(job), current)
			return apierrors.IsNotFound(err) || current.DeletionTimestamp != nil
		}, testTimeout, testPoll).Should(BeTrue(), "the running download Job is deleted")
		Expect(percentOf(key)()).To(Equal(int32(25)))

		reads := testLogs.reads(pod)
		testLogs.set(pod, progressLine(90, 100), nil)
		nextRead(key)
		Consistently(readsOf(pod), time.Second, testPoll).Should(Equal(reads))
		Expect(percentOf(key)()).To(Equal(int32(25)))
	})

	It("still stops on time while progress keeps waking it", func() {
		// The read interval must not push the stop back: stopsAt is sooner than the next read here.
		sm, key, job := started("soon", "12s")
		runningPod(sm, job, progressLine(10, 100))
		Eventually(phaseOf(ctx, key), 30*time.Second, testPoll).Should(Equal(servingv1alpha1.PhaseStopped))
	})

	It("rejects numbers below zero at the API", func() {
		_, key, _ := started("validate", "")
		Eventually(phaseOf(ctx, key), testTimeout, testPoll).Should(Equal(servingv1alpha1.PhaseDownloading))
		got := getServedModel(ctx, key)
		negative := int64(-1)
		got.Status.Download = &servingv1alpha1.DownloadStatus{DownloadedBytes: &negative}
		err := k8sClient.Status().Update(ctx, got)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
	})
})
