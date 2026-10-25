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
	"crypto/tls"
	"fmt"
	"io"
	"net/http"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// -----------------------------------------------------------------------------
// ECDS - GVRs, Labels and Names
// -----------------------------------------------------------------------------

const (
	// EnvoyFilterNamePrefix is the name prefix of the EnvoyFilter owned by an Engine.
	EnvoyFilterNamePrefix = "coraza-engine-"

	// gatewayNameLabel is the label Istio applies to Gateway pods and Deployments.
	gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"

	// operatorPodSelector matches the operator manager pod.
	operatorPodSelector = "control-plane=coraza-controller-manager"

	// operatorXDSPort is the operator port that serves ECDS (gRPC) and assets (HTTP).
	operatorXDSPort = 18080

	// legacyRuleProbePath is a path served by the pre-ECDS rule cache server.
	legacyRuleProbePath = "/rules/probe/latest"

	// engineNameLabel and engineNamespaceLabel are set on operator-managed
	// resources by internal/controller.
	engineNameLabel      = "waf.k8s.coraza.io/engine-name"
	engineNamespaceLabel = "waf.k8s.coraza.io/engine-namespace"
)

var (
	// EnvoyFilterGVR is the GroupVersionResource for Istio EnvoyFilter resources.
	EnvoyFilterGVR = schema.GroupVersionResource{
		Group: "networking.istio.io", Version: "v1alpha3", Resource: "envoyfilters",
	}

	// NetworkPolicyGVR is the GroupVersionResource for Kubernetes NetworkPolicy resources.
	NetworkPolicyGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "networkpolicies",
	}
)

// EnvoyFilterName returns the deterministic EnvoyFilter name for an Engine.
func EnvoyFilterName(engineName string) string {
	return EnvoyFilterNamePrefix + engineName
}

// -----------------------------------------------------------------------------
// EnvoyFilter / NetworkPolicy Assertions
// -----------------------------------------------------------------------------

// ExpectEnvoyFilter polls until the Engine's EnvoyFilter exists and returns it.
func (s *Scenario) ExpectEnvoyFilter(namespace, engineName string) *unstructured.Unstructured {
	s.T.Helper()
	name := EnvoyFilterName(engineName)
	s.T.Logf("Waiting for EnvoyFilter %s/%s to exist", namespace, name)

	output := &unstructured.Unstructured{}
	require.Eventually(s.T, func() bool {
		obj, err := s.F.DynamicClient.Resource(EnvoyFilterGVR).Namespace(namespace).Get(
			s.T.Context(), name, metav1.GetOptions{},
		)
		if err != nil {
			return false
		}
		output = obj
		return true
	}, DefaultTimeout, DefaultInterval, "EnvoyFilter %s/%s should exist", namespace, name)
	return output
}

// ExpectEngineNetworkPolicy polls until the NetworkPolicy that authorizes the
// Engine's Gateway pods is present in the operator namespace.
func (s *Scenario) ExpectEngineNetworkPolicy(operatorNamespace, engineNamespace, engineName string) *networkingv1.NetworkPolicy {
	s.T.Helper()
	selector := fmt.Sprintf("%s=%s,%s=%s",
		engineNameLabel, engineName, engineNamespaceLabel, engineNamespace,
	)
	s.T.Logf("Waiting for NetworkPolicy for Engine %s/%s", engineNamespace, engineName)

	var policy *networkingv1.NetworkPolicy
	require.Eventually(s.T, func() bool {
		list, err := s.F.KubeClient.NetworkingV1().NetworkPolicies(operatorNamespace).List(
			s.T.Context(), metav1.ListOptions{LabelSelector: selector},
		)
		if err != nil || len(list.Items) == 0 {
			return false
		}
		policy = &list.Items[0]
		return true
	}, DefaultTimeout, DefaultInterval,
		"NetworkPolicy for Engine %s/%s should exist in %s", engineNamespace, engineName, operatorNamespace,
	)
	return policy
}

// -----------------------------------------------------------------------------
// Gateway Deployment Assertions
// -----------------------------------------------------------------------------

// GetGatewayDeploymentGeneration returns the generation of the Deployment that
// runs the named Gateway. Scenario 2 asserts it does not change when rules,
// tokens, or certificates change: the data plane must not roll out.
func (s *Scenario) GetGatewayDeploymentGeneration(namespace, gatewayName string) int64 {
	s.T.Helper()
	selector := fmt.Sprintf("%s=%s", gatewayNameLabel, gatewayName)

	var generation int64
	require.Eventually(s.T, func() bool {
		deployments, err := s.F.KubeClient.AppsV1().Deployments(namespace).List(
			s.T.Context(), metav1.ListOptions{LabelSelector: selector},
		)
		if err != nil || len(deployments.Items) != 1 {
			return false
		}
		generation = deployments.Items[0].Generation
		return true
	}, DefaultTimeout, DefaultInterval,
		"expected exactly one Deployment for Gateway %s/%s", namespace, gatewayName,
	)
	return generation
}

// -----------------------------------------------------------------------------
// Legacy Rule Endpoint Assertion
// -----------------------------------------------------------------------------

// ExpectNoRuleEndpoint polls until the operator's xDS listener no longer serves
// the legacy /rules route. It speaks TLS with HTTP/1.1 (the asset side of the
// ALPN listener) and accepts only a 404.
//
// Depends on Phase 4 (Lane C): until the TLS/ALPN listener exists this helper
// fails. TLS verification is skipped because the assertion is route absence,
// not server identity.
func (s *Scenario) ExpectNoRuleEndpoint(operatorNamespace string) {
	s.T.Helper()
	proxy := s.ProxyToPod(operatorNamespace, operatorPodSelector, operatorXDSPort)

	httpc := &http.Client{
		Timeout: DefaultTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // route-absence probe against a test cluster
				NextProtos:         []string{"http/1.1"},
			},
		},
	}
	url := fmt.Sprintf("https://localhost:%s%s", proxy.LocalPort(), legacyRuleProbePath)

	s.T.Logf("Waiting for legacy endpoint %s to return 404", legacyRuleProbePath)
	require.EventuallyWithT(s.T, func(collect *assert.CollectT) {
		req, err := http.NewRequestWithContext(s.T.Context(), http.MethodGet, url, nil)
		if !assert.NoError(collect, err, "build legacy endpoint request") {
			return
		}
		resp, err := httpc.Do(req)
		if !assert.NoError(collect, err, "request legacy endpoint") {
			return
		}
		defer func() {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
		assert.Equal(collect, http.StatusNotFound, resp.StatusCode,
			"legacy /rules route must not be served")
	}, DefaultTimeout, DefaultInterval,
		"legacy /rules route is still reachable at %s", url,
	)
}
