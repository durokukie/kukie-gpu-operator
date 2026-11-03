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
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// podVerdict is what one engine Pod tells us: ready, still starting, or broken (with the message as the kubelet
// or scheduler wrote it). Translating the message into a cause and a hint is DURO-171.
type podVerdict struct {
	ready  bool
	failed bool
	// ended is true when the Pod itself is over (phase Failed): with restartPolicy Never that is one finished
	// attempt, which a Job may still retry.
	ended   bool
	reason  string
	message string
}

// waitingReasonsThatMeanFailure are container waiting reasons that do not resolve by waiting.
var waitingReasonsThatMeanFailure = []string{
	"CrashLoopBackOff",
	"ImagePullBackOff",
	"ErrImagePull",
	"ErrImageNeverPull",
	"InvalidImageName",
	"CreateContainerConfigError",
	"CreateContainerError",
	"RunContainerError",
}

// newestLivePod picks the Pod the status should follow: the youngest one that is not being deleted
// (with the Recreate strategy an old Pod may still be terminating while the new one starts).
func newestLivePod(pods []corev1.Pod) *corev1.Pod {
	var pick *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if pick == nil || p.CreationTimestamp.After(pick.CreationTimestamp.Time) {
			pick = p
		}
	}
	return pick
}

// classifyPod reads Pod status only (no Events, no logs) and keeps the Kubernetes message as it is.
func classifyPod(pod *corev1.Pod) podVerdict {
	if pod == nil {
		return podVerdict{reason: "NoPod", message: "vLLM Pod가 아직 없다"}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return podVerdict{ready: true, reason: "Healthy", message: "vLLM /health 응답 확인"}
		}
	}
	ended := pod.Status.Phase == corev1.PodFailed
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return podVerdict{failed: true, ended: ended, reason: c.Reason, message: c.Message}
		}
	}
	// Container states come before the Pod phase: when a container exits non-zero the kubelet leaves the Pod-level
	// reason and message empty, and the exit code lives only in the container's terminated state.
	statuses := slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && slices.Contains(waitingReasonsThatMeanFailure, w.Reason) {
			msg := w.Message
			if t := cs.LastTerminationState.Terminated; t != nil {
				msg = strings.TrimSpace(msg + "\n" + terminationSummary(t))
			}
			return podVerdict{failed: true, ended: ended, reason: w.Reason, message: msg}
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return podVerdict{failed: true, ended: ended, reason: orDefault(t.Reason, "ContainerExited"), message: terminationSummary(t)}
		}
	}
	if ended {
		// Pod-level failure such as Evicted, NodeAffinity or DisruptionTarget.
		return podVerdict{failed: true, ended: true, reason: orDefault(pod.Status.Reason, "PodFailed"), message: pod.Status.Message}
	}
	return podVerdict{reason: "Starting", message: fmt.Sprintf("Pod %s: %s, /health 대기 중", pod.Name, pod.Status.Phase)}
}

func terminationSummary(t *corev1.ContainerStateTerminated) string {
	s := fmt.Sprintf("마지막 종료: exit code %d", t.ExitCode)
	if t.Reason != "" {
		s += " (" + t.Reason + ")"
	}
	if t.Message != "" {
		s += ": " + strings.TrimSpace(t.Message)
	}
	return s
}

// jobOutcome reports whether the download Job has finished, and how.
func jobOutcome(job *batchv1.Job) (done bool, failed bool, message string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false, ""
		case batchv1.JobFailed:
			msg := c.Message
			if c.Reason != "" {
				msg = c.Reason + ": " + msg
			}
			return true, true, strings.TrimSuffix(msg, ": ")
		}
	}
	return false, false, ""
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
