/*
Copyright Coraza Kubernetes Operator contributors.

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

package framework

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readyPod() corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func notReadyPod() corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
}

func deletingReadyPod() corev1.Pod {
	pod := readyPod()
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now
	return pod
}

func TestAllPodsStableReady(t *testing.T) {
	tests := []struct {
		name string
		pods []corev1.Pod
		want bool
	}{
		{name: "no pods", pods: nil, want: false},
		{name: "one ready pod", pods: []corev1.Pod{readyPod()}, want: true},
		{name: "two ready pods", pods: []corev1.Pod{readyPod(), readyPod()}, want: true},
		{name: "ready plus not-ready pod", pods: []corev1.Pod{readyPod(), notReadyPod()}, want: false},
		{name: "ready plus deleting pod", pods: []corev1.Pod{readyPod(), deletingReadyPod()}, want: false},
		{name: "single deleting pod", pods: []corev1.Pod{deletingReadyPod()}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := allPodsStableReady(tt.pods); got != tt.want {
				t.Errorf("allPodsStableReady() = %v, want %v", got, tt.want)
			}
		})
	}
}
