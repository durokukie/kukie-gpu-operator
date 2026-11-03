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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClassifyPod(t *testing.T) {
	cases := []struct {
		name    string
		pod     *corev1.Pod
		ready   bool
		failed  bool
		ended   bool
		reason  string
		message string
	}{
		{name: "no pod", pod: nil, reason: "NoPod"},
		{
			name: "ready",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			}}},
			ready: true, reason: "Healthy",
		},
		{
			name: "pending, scheduled, probes not yet passing",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"},
			}}},
			reason: "Starting",
		},
		{
			name: "unschedulable",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
					Message: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu."},
			}}},
			failed: true, reason: "Unschedulable", message: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu.",
		},
		{
			name: "image pull",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
			}}}},
			failed: true, reason: "ImagePullBackOff", message: "Back-off pulling image",
		},
		{
			name: "crash loop with last termination",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 10s"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1, Reason: "Error", Message: "CUDA out of memory"}},
			}}}},
			failed: true, reason: "CrashLoopBackOff", message: "back-off 10s\n마지막 종료: exit code 1 (Error): CUDA out of memory",
		},
		{
			name: "container exited non-zero, not yet restarted",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
			}}}},
			failed: true, reason: "OOMKilled", message: "마지막 종료: exit code 137 (OOMKilled)",
		},
		{
			name: "download attempt exited non-zero (restartPolicy Never): exit code from the container, not the empty Pod reason",
			pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
			}}}},
			failed: true, ended: true, reason: "Error", message: "마지막 종료: exit code 1 (Error)",
		},
		{
			name:   "pod failed",
			pod:    &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "The node was low on resource: ephemeral-storage."}},
			failed: true, ended: true, reason: "Evicted", message: "The node was low on resource: ephemeral-storage.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := classifyPod(tc.pod)
			if v.ready != tc.ready || v.failed != tc.failed || v.ended != tc.ended || v.reason != tc.reason {
				t.Fatalf("got ready=%v failed=%v ended=%v reason=%q, want ready=%v failed=%v ended=%v reason=%q",
					v.ready, v.failed, v.ended, v.reason, tc.ready, tc.failed, tc.ended, tc.reason)
			}
			if tc.message != "" && v.message != tc.message {
				t.Fatalf("got message %q, want %q", v.message, tc.message)
			}
		})
	}
}

func TestNewestLivePod(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	newer := metav1.NewTime(time.Now())
	deleting := metav1.NewTime(time.Now().Add(time.Minute))
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "old", CreationTimestamp: old}},
		{ObjectMeta: metav1.ObjectMeta{Name: "deleting", CreationTimestamp: deleting, DeletionTimestamp: &deleting}},
		{ObjectMeta: metav1.ObjectMeta{Name: "new", CreationTimestamp: newer}},
	}
	if got := newestLivePod(pods); got == nil || got.Name != "new" {
		t.Fatalf("got %v, want the pod named new", got)
	}
	if got := newestLivePod(nil); got != nil {
		t.Fatalf("got %v for no pods, want nil", got)
	}
}

func TestDownloadJobNameFollowsInputs(t *testing.T) {
	sm := newServedModel("ns", "m", true)
	cfg := Config{}
	a := cfg.downloadJobName(sm)
	if a != cfg.downloadJobName(sm) {
		t.Fatal("name must be stable for the same spec")
	}
	sm.Spec.Source.TokenSecretRef = "hf-token"
	if b := cfg.downloadJobName(sm); b == a {
		t.Fatal("name must change when the token secret changes")
	}
	if c := (Config{DownloadImage: "python:3.13-slim@sha256:0"}).downloadJobName(sm); c == cfg.downloadJobName(sm) {
		t.Fatal("name must change when the download image changes")
	}
}
