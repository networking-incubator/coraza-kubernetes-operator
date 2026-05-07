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
	"fmt"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// Pod Readiness
// -----------------------------------------------------------------------------

// GatewayReadyTimeout is a longer timeout for gateway pod readiness.
// Gateway pods may restart due to Istio CA certificate signing delays
// when running parallel tests. namespaceMu serialization mitigates
// most contention, so 5 minutes is sufficient.
const GatewayReadyTimeout = 300 * time.Second

// waitForGatewayPodReady waits until at least one pod for the gateway is Ready.
// This prevents port-forward attempts before Istio sidecar is fully initialized.
// Uses a longer timeout than default to handle CA certificate signing delays.
func (s *Scenario) waitForGatewayPodReady(namespace, gatewayName string) {
	s.T.Helper()
	labelSelector := fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", gatewayName)

	require.Eventually(s.T, func() bool {
		pods, err := s.F.KubeClient.CoreV1().Pods(namespace).List(
			s.T.Context(),
			metav1.ListOptions{LabelSelector: labelSelector},
		)
		if err != nil {
			return false
		}
		for _, pod := range pods.Items {
			if isPodReady(&pod) {
				return true
			}
		}
		return false
	}, GatewayReadyTimeout, DefaultInterval,
		"gateway pod %s/%s not ready", namespace, gatewayName,
	)
	s.T.Logf("Gateway pod %s/%s is ready", namespace, gatewayName)
}

// isPodReady returns true if the pod is in Running phase and has Ready condition.
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
