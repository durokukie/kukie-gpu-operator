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
	_ "embed"
	"encoding/json"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

// downloadScript runs inside the download image; see its docstring for the contract with this file.
//
//go:embed download.py
var downloadScript string

const (
	downloadContainer = "download"
	// jobNameLabel is what the Job controller puts on its Pods.
	jobNameLabel = "job-name"

	reasonJobRunning   = "JobRunning"
	reasonPreparingPod = "PreparingPod"

	// progressEvent marks the lines download.py writes, among whatever else `hf` prints.
	progressEvent = "download_progress"
	// progressReadInterval is how often one downloading ServedModel's log is read. The script writes every 5 seconds.
	progressReadInterval = 10 * time.Second
	// progressTailLines is enough for a progress line to be among them: `hf` prints little between two of them.
	progressTailLines = 10
	// progressLimitBytes bounds what one read can cost the oven. A longer tail loses its end and keeps the old value.
	progressLimitBytes = 64 * 1024
	// logReadTimeout keeps a kubelet that does not answer from holding up the reconcile of every other ServedModel.
	logReadTimeout = 5 * time.Second
	// maxPercentBeforeSuccess is the most a download can show until its Job has succeeded. The files can all be there
	// with `hf` still working, and only the Job says it worked.
	maxPercentBeforeSuccess = 99
)

// PodLogReader reads the end of one container's log. An interface because envtest has no kubelet to answer.
type PodLogReader interface {
	TailLines(ctx context.Context, namespace, pod, container string, lines int64) (string, error)
}

type clientsetLogReader struct {
	client kubernetes.Interface
}

// NewPodLogReader reads logs through the API server. It needs pods/log get and nothing else.
func NewPodLogReader(client kubernetes.Interface) PodLogReader {
	return clientsetLogReader{client: client}
}

func (r clientsetLogReader) TailLines(ctx context.Context, namespace, pod, container string, lines int64) (string, error) {
	limit := int64(progressLimitBytes)
	stream, err := r.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  container,
		TailLines:  &lines,
		LimitBytes: &limit,
	}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	body, err := io.ReadAll(io.LimitReader(stream, limit))
	return string(body), err
}

// downloadProgressLine is one line of download.py.
type downloadProgressLine struct {
	Event           string `json:"event"`
	DownloadedBytes *int64 `json:"downloadedBytes"`
	TotalBytes      *int64 `json:"totalBytes"`
}

// lastDownloadProgress finds the newest line that holds a usable progress report. The log is the Pod's to write, so
// anything else, a cut line or impossible numbers is passed over rather than believed.
func lastDownloadProgress(logs string) (downloadProgressLine, bool) {
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var line downloadProgressLine
		if err := json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &line); err != nil {
			continue
		}
		if line.Event != progressEvent || line.DownloadedBytes == nil || *line.DownloadedBytes < 0 {
			continue
		}
		if line.TotalBytes != nil && *line.TotalBytes < 0 {
			continue
		}
		return line, true
	}
	return downloadProgressLine{}, false
}

// status turns the line into what the API promises: without a total percent is 0, and before success it stops at 99.
func (l downloadProgressLine) status() *servingv1alpha1.DownloadStatus {
	downloaded := *l.DownloadedBytes
	if l.TotalBytes == nil || *l.TotalBytes == 0 {
		return &servingv1alpha1.DownloadStatus{DownloadedBytes: &downloaded}
	}
	total := *l.TotalBytes
	// More than the total: files of an older revision of the model are in the same folder.
	downloaded = min(downloaded, total)
	percent := int32(float64(downloaded) / float64(total) * 100)
	return &servingv1alpha1.DownloadStatus{
		Percent:         min(percent, maxPercentBeforeSuccess),
		DownloadedBytes: &downloaded,
		TotalBytes:      &total,
	}
}

// finishedDownload is the status of a Job that succeeded. The last look was up to 10 seconds old and may not have
// happened at all, so a known total is what is on disk now and without one there is no number to give.
func finishedDownload(last *servingv1alpha1.DownloadStatus) *servingv1alpha1.DownloadStatus {
	if last == nil || last.TotalBytes == nil {
		return &servingv1alpha1.DownloadStatus{Percent: 100}
	}
	total := *last.TotalBytes
	return &servingv1alpha1.DownloadStatus{Percent: 100, DownloadedBytes: &total, TotalBytes: &total}
}

// containerStarted tells a Pod still being prepared (no node, image pulling, volume attaching) from one that runs.
func containerStarted(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for i := range pod.Status.ContainerStatuses {
		state := pod.Status.ContainerStatuses[i].State
		if state.Running != nil || state.Terminated != nil {
			return true
		}
	}
	return false
}

// readDownloadProgress puts the Pod's latest report in status, at most once per interval, and says when to come back.
// A log that cannot be read or holds nothing usable changes nothing: the numbers are for showing, and the phase follows
// the Job and the Pod alone.
func (r *ServedModelReconciler) readDownloadProgress(ctx context.Context, sm *servingv1alpha1.ServedModel, pod *corev1.Pod) time.Duration {
	if r.Logs == nil {
		return requeueWhileChanging
	}
	wait := r.claimLogRead(client.ObjectKeyFromObject(sm))
	if wait > 0 {
		return wait
	}
	ctx, cancel := context.WithTimeout(ctx, logReadTimeout)
	defer cancel()
	logs, err := r.Logs.TailLines(ctx, sm.Namespace, pod.Name, downloadContainer, progressTailLines)
	if err != nil {
		logf.FromContext(ctx).Info("cannot read the download Pod's log, keeping the last progress", "pod", pod.Name, "error", err.Error())
		return progressReadInterval
	}
	if line, ok := lastDownloadProgress(logs); ok {
		sm.Status.Download = line.status()
	}
	return progressReadInterval
}

// claimLogRead returns 0 and records the read when it is due, or how long until it is.
func (r *ServedModelReconciler) claimLogRead(key types.NamespacedName) time.Duration {
	r.logReadsMu.Lock()
	defer r.logReadsMu.Unlock()
	now := r.now()
	if last, ok := r.logReads[key]; ok {
		if wait := progressReadInterval - now.Sub(last); wait > 0 {
			return wait
		}
	}
	if r.logReads == nil {
		r.logReads = map[types.NamespacedName]time.Time{}
	}
	r.logReads[key] = now
	return 0
}

func (r *ServedModelReconciler) forgetLogRead(key types.NamespacedName) {
	r.logReadsMu.Lock()
	defer r.logReadsMu.Unlock()
	delete(r.logReads, key)
}
