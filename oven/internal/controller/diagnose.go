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
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/durokukie/kukie-gpu-operator/oven/api/v1alpha1"
)

const (
	engineContainer = "vllm"
	// maxWarningEvents bounds one events read. A failing Pod repeats the same few events with a count.
	maxWarningEvents = 50
)

// DiagnosticsReader reads what a failed Pod left beyond its status: the end of a container's log and the Warning events
// of an object. An interface because envtest has no kubelet and no event recorder.
type DiagnosticsReader interface {
	LogTail(ctx context.Context, namespace, pod, container string, previous bool, lines int64) (string, error)
	WarningEvents(ctx context.Context, namespace, kind, name string) ([]string, error)
}

// NewDiagnosticsReader reads through the API server. It needs pods/log get and events list in the watched namespaces.
func NewDiagnosticsReader(client kubernetes.Interface) DiagnosticsReader {
	return clientsetLogReader{client: client}
}

func (r clientsetLogReader) LogTail(ctx context.Context, namespace, pod, container string, previous bool, lines int64) (string, error) {
	limit := int64(progressLimitBytes)
	stream, err := r.client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container:  container,
		Previous:   previous,
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

func (r clientsetLogReader) WarningEvents(ctx context.Context, namespace, kind, name string) ([]string, error) {
	list, err := r.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fields.Set{
			"involvedObject.kind": kind,
			"involvedObject.name": name,
			"type":                corev1.EventTypeWarning,
		}.String(),
		Limit: maxWarningEvents,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, e := range list.Items {
		out = append(out, e.Reason+": "+e.Message)
	}
	return out, nil
}

// diagnosisKey is one ServedModel's download or engine.
type diagnosisKey struct {
	types.NamespacedName
	stage string
}

// cachedDiagnosis keeps a result for as long as the attempt it was made for: the same Pod, restart count and verdict.
// A failed model is looked at every 30 seconds, and each look would otherwise read a log through the kubelet.
type cachedDiagnosis struct {
	attempt string
	result  diagnosis
}

// diagnose classifies a failed Pod (or, with pod nil, a failure only the Job tells of) with the failure pattern table.
func (r *ServedModelReconciler) diagnose(ctx context.Context, sm *servingv1alpha1.ServedModel, stage string,
	pod *corev1.Pod, verdict podVerdict, extra ...string) diagnosis {
	key := diagnosisKey{NamespacedName: client.ObjectKeyFromObject(sm), stage: stage}
	attempt := attemptOf(pod, verdict, extra)
	if d, ok := r.cachedDiagnosis(key, attempt); ok {
		return d
	}

	container := downloadContainer
	if stage == stageEngine {
		container = engineContainer
	}
	ev := failureEvidence{stage: stage, status: statusText(pod, verdict, extra)}
	exit, previous, ran := lastExit(pod, container)
	ev.fallback = "쿠버네티스 상태: " + orDefault(verdict.reason, "알 수 없음")
	if exit != nil {
		ev.fallback += ", 종료 코드 " + strconv.Itoa(int(*exit))
	}
	ev.fallback += "."

	complete := true
	if pod != nil {
		ev.logHint = fmt.Sprintf("원문 로그는 kubectl -n %s logs %s -c %s", pod.Namespace, pod.Name, container)
		if previous {
			ev.logHint += " --previous"
		}
		ev.logHint += " 로 본다."
		if r.Diagnostics != nil {
			readCtx, cancel := context.WithTimeout(ctx, logReadTimeout)
			defer cancel()
			var events []string
			// Events stay for an hour and say nothing about which attempt they belong to. A Pod that waited for a node
			// keeps its FailedScheduling, and a PVC its passing ProvisioningFailed, after the Pod got going. So they only
			// count while no container of this Pod has started: then they are what is in the way.
			objects := []struct{ kind, name string }{{"Pod", pod.Name}, {"PersistentVolumeClaim", pvcName(sm)}}
			if anyContainerStarted(pod) {
				objects = nil
			}
			for _, obj := range objects {
				got, err := r.Diagnostics.WarningEvents(readCtx, sm.Namespace, obj.kind, obj.name)
				if err != nil {
					complete = false
					logf.FromContext(ctx).Info("cannot read events for the failure pattern table", "kind", obj.kind, "name", obj.name, "error", err.Error())
					continue
				}
				events = append(events, got...)
			}
			ev.events = strings.Join(events, "\n")
			if ran {
				log, err := r.Diagnostics.LogTail(readCtx, pod.Namespace, pod.Name, container, previous, failureLogLines)
				if err != nil {
					complete = false
					logf.FromContext(ctx).Info("cannot read the log for the failure pattern table", "pod", pod.Name, "error", err.Error())
				}
				ev.log = log
			}
		}
	}

	d := r.Patterns.Patterns().classify(ev)
	if complete {
		r.rememberDiagnosis(key, attempt, d)
	}
	return d
}

// attemptOf names one failed attempt: a new Pod, a restart or a different verdict is a new one.
func attemptOf(pod *corev1.Pod, verdict podVerdict, extra []string) string {
	parts := []string{verdict.reason, verdict.message}
	parts = append(parts, extra...)
	if pod != nil {
		parts = append(parts, string(pod.UID))
		for _, cs := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
			parts = append(parts, strconv.Itoa(int(cs.RestartCount)))
		}
	}
	return strings.Join(parts, "\x00")
}

// statusText is what Kubernetes itself says about the Pod: reasons, messages and exit codes, one per line.
func statusText(pod *corev1.Pod, verdict podVerdict, extra []string) string {
	lines := append([]string{verdict.reason + ": " + verdict.message}, extra...)
	if pod != nil {
		lines = append(lines, pod.Status.Reason+": "+pod.Status.Message)
		for _, c := range pod.Status.Conditions {
			if c.Status != corev1.ConditionTrue && c.Message != "" {
				lines = append(lines, c.Reason+": "+c.Message)
			}
		}
		for _, cs := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
			if w := cs.State.Waiting; w != nil {
				lines = append(lines, w.Reason+": "+w.Message)
			}
			for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
				if t != nil {
					lines = append(lines, terminationSummary(t))
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// lastExit finds the container's last exit code, whether its log is worth reading (it ran at all) and whether that log
// belongs to the previous instance (the container is waiting to restart).
func lastExit(pod *corev1.Pod, container string) (exit *int32, previous, ran bool) {
	if pod == nil {
		return nil, false, false
	}
	for _, cs := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if cs.Name != container {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			return &t.ExitCode, false, true
		}
		if t := cs.LastTerminationState.Terminated; t != nil {
			return &t.ExitCode, cs.State.Waiting != nil, true
		}
		return nil, false, cs.State.Running != nil
	}
	return nil, false, false
}

// anyContainerStarted reports a Pod with a container that runs or has run: past scheduling, volumes and image.
func anyContainerStarted(pod *corev1.Pod) bool {
	for _, cs := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
			return true
		}
	}
	return false
}

// runningSince is when the engine container last started, or zero when it is not running. A container that passed its
// startup probe once (it served, then lost readiness) is not loading: zero too, so a failing server never reads as
// "still loading".
func runningSince(pod *corev1.Pod) time.Time {
	if pod == nil {
		return time.Time{}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == engineContainer && cs.State.Running != nil {
			if cs.Started != nil && *cs.Started {
				return time.Time{}
			}
			return cs.State.Running.StartedAt.Time
		}
	}
	return time.Time{}
}

func (r *ServedModelReconciler) cachedDiagnosis(key diagnosisKey, attempt string) (diagnosis, bool) {
	r.diagnosesMu.Lock()
	defer r.diagnosesMu.Unlock()
	c, ok := r.diagnoses[key]
	if !ok || c.attempt != attempt {
		return diagnosis{}, false
	}
	return c.result, true
}

func (r *ServedModelReconciler) rememberDiagnosis(key diagnosisKey, attempt string, d diagnosis) {
	r.diagnosesMu.Lock()
	defer r.diagnosesMu.Unlock()
	if r.diagnoses == nil {
		r.diagnoses = map[diagnosisKey]cachedDiagnosis{}
	}
	r.diagnoses[key] = cachedDiagnosis{attempt: attempt, result: d}
}

func (r *ServedModelReconciler) forgetDiagnoses(nn types.NamespacedName) {
	r.diagnosesMu.Lock()
	defer r.diagnosesMu.Unlock()
	for _, stage := range []string{stageDownload, stageEngine} {
		delete(r.diagnoses, diagnosisKey{NamespacedName: nn, stage: stage})
	}
}

// stillLoading turns a long Loading into StillLoading: the engine runs and has not answered /health for a long time.
// Loading a big model or compiling kernels can take half an hour; the phase stays Loading and nothing is wrong yet.
func (r *ServedModelReconciler) stillLoading(pod *corev1.Pod) (diagnosis, bool) {
	since := runningSince(pod)
	if r.Config.StillLoadingAfter <= 0 || since.IsZero() {
		return diagnosis{}, false
	}
	elapsed := r.now().Sub(since)
	if elapsed < r.Config.StillLoadingAfter {
		return diagnosis{}, false
	}
	return diagnosis{
		reason:  ReasonStillLoading,
		message: fmt.Sprintf("vLLM이 %d분째 모델을 올리는 중이다(가중치 적재·커널 컴파일). 아직 실패가 아니다", int(elapsed.Minutes())),
		hint:    "기다린다. 40분 안에 /health에 답하지 않으면 kubelet이 다시 시작하고 실패 원인이 보인다. 이 시간이 늘 길면 spec.engine.args의 --max-model-len을 줄인다",
	}, true
}
