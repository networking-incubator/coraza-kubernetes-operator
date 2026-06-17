//go:build integration

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

package integration

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

const envoyPromPort = 15090

// TestDataplaneMetrics verifies that the operator injects engine/namespace into
// the WasmPlugin pluginConfig and that coraza_waf_* stats appear on the Gateway
// Envoy prometheus port after WAF-evaluated traffic.
//
// Requires a WASM image that implements the coraza_waf_* contract; set
// CORAZA_WASM_IMAGE when running against a custom driver build.
func TestDataplaneMetrics(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	ns := s.GenerateNamespace("dataplane-metrics")

	s.Step("create gateway")
	s.CreateGateway(ns, "metrics-gw")
	s.ExpectGatewayProgrammed(ns, "metrics-gw")

	s.Step("deploy blocking rules and engine")
	s.CreateRuleSource(ns, "base-rules", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block-evil", framework.SimpleBlockRule(3001, "evilpayload"))
	s.CreateRuleSet(ns, "ruleset", []string{"base-rules", "block-evil"}, nil)
	s.ExpectRuleSetReady(ns, "ruleset")

	s.CreateEngine(ns, "engine", framework.EngineOpts{
		RuleSetName: "ruleset",
		GatewayName: "metrics-gw",
	})
	s.ExpectEngineReady(ns, "engine")

	s.Step("verify WasmPlugin pluginConfig tenant labels")
	wasmPlugin := s.ExpectWasmPluginExists(ns, "coraza-engine-engine")
	engineLabel, found, err := unstructured.NestedString(wasmPlugin.Object, "spec", "pluginConfig", "engine")
	require.NoError(t, err)
	require.True(t, found, "WasmPlugin pluginConfig.engine should be set")
	assert.Equal(t, "engine", engineLabel)

	namespaceLabel, found, err := unstructured.NestedString(wasmPlugin.Object, "spec", "pluginConfig", "namespace")
	require.NoError(t, err)
	require.True(t, found, "WasmPlugin pluginConfig.namespace should be set")
	assert.Equal(t, ns, namespaceLabel)

	s.Step("deploy echo backend and route")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "echo-route", "metrics-gw", "echo")

	gw := s.ProxyToGateway(ns, "metrics-gw")
	gatewayPodSelector := fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", "metrics-gw")
	statsProxy := s.ProxyToPod(ns, gatewayPodSelector, envoyPromPort)
	statsURL := fmt.Sprintf("http://localhost:%s/stats/prometheus", statsProxy.LocalPort())

	httpClient := &http.Client{Timeout: 10 * time.Second}

	s.Step("verify Envoy stats endpoint is reachable")
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get(statsURL)
		if err != nil {
			return false
		}
		defer func() {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
		return resp.StatusCode == http.StatusOK
	}, framework.DefaultTimeout, framework.DefaultInterval,
		"Envoy /stats/prometheus not reachable via port-forward at %s", statsURL,
	)

	s.Step("send blocked request through WAF")
	gw.ExpectBlocked("/?test=evilpayload")

	s.Step("verify coraza_waf metrics appear on gateway Envoy stats")
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get(statsURL)
		if err != nil {
			return false
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			return false
		}
		text := string(body)
		return strings.Contains(text, "coraza_waf") &&
			strings.Contains(text, "requests") &&
			(strings.Contains(text, "engine") || strings.Contains(text, "_engine="))
	}, framework.DefaultTimeout, framework.DefaultInterval,
		"expected coraza_waf request metrics on gateway Envoy stats after blocked traffic",
	)
}
