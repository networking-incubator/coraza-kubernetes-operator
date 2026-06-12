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
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

// TestCorazaControllerMetrics verifies that the operator's coraza_* metrics
// (engine, ruleset, and cache server counters) are exposed on the operator's
// authenticated HTTPS metrics endpoint.
func TestCorazaControllerMetrics(t *testing.T) {
	t.Parallel()
	s := fw.NewScenario(t)

	// -------------------------------------------------------------------------
	// Port-forward to the operator pod metrics port
	// -------------------------------------------------------------------------

	s.Step("port-forward to operator metrics")
	proxy := s.ProxyToPod(operatorNamespace, operatorSelector, metricsPort)
	metricsURL := fmt.Sprintf("https://localhost:%s/metrics", proxy.LocalPort())

	// Wait for the TLS endpoint to accept connections through the port-forward.
	tlsClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // test against self-signed cert
			},
		},
	}
	require.Eventually(t, func() bool {
		resp, err := tlsClient.Get(metricsURL)
		if err != nil {
			return false
		}
		defer func() {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}()
		return true
	}, framework.DefaultTimeout, framework.DefaultInterval,
		"metrics endpoint not reachable via port-forward at %s", metricsURL,
	)

	// -------------------------------------------------------------------------
	// Obtain a service account token for authentication
	// -------------------------------------------------------------------------

	s.Step("obtain service account token")
	cmd := fw.Kubectl(operatorNamespace, "create", "token",
		"coraza-kubernetes-operator", "--duration=10m")
	tokenBytes, err := cmd.Output()
	require.NoError(t, err, "create service account token")
	token := strings.TrimSpace(string(tokenBytes))
	require.NotEmpty(t, token, "token should not be empty")

	// -------------------------------------------------------------------------
	// Helper: fetch metrics with authentication
	// -------------------------------------------------------------------------

	fetchMetrics := func(collect *assert.CollectT) string {
		req, err := http.NewRequest(http.MethodGet, metricsURL, nil)
		if !assert.NoError(collect, err) {
			return ""
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := tlsClient.Do(req)
		if !assert.NoError(collect, err) {
			return ""
		}
		// Always drain then close the body so the keep-alive connection is reused
		// across EventuallyWithT iterations. Without draining, each iteration opens
		// a new TCP connection and the port-forward connection backlog can be exhausted.
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		assert.Equal(collect, http.StatusOK, resp.StatusCode,
			"expected 200 for authenticated metrics request, got %d", resp.StatusCode)
		return string(body)
	}

	// -------------------------------------------------------------------------
	// Verify cache server metrics are present (always emitted by the server)
	// -------------------------------------------------------------------------

	s.Step("verify cache server metrics")
	t.Run("exposes coraza_cache metrics", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			body := fetchMetrics(collect)
			if body == "" {
				return
			}
			assert.Contains(collect, body, "coraza_cache_size_bytes",
				"expected coraza_cache_size_bytes gauge in response")
		}, framework.DefaultTimeout, framework.DefaultInterval)
	})

	// -------------------------------------------------------------------------
	// Create resources and verify Engine + RuleSet metrics appear
	// -------------------------------------------------------------------------

	s.Step("create Engine and RuleSet, verify metrics appear")
	t.Run("coraza_engine and coraza_ruleset metrics appear after resource creation", func(t *testing.T) {
		ns := s.GenerateNamespace("metrics-engine")

		s.CreateRuleSource(ns, "metrics-rs-source", `SecRuleEngine DetectionOnly`)
		s.CreateRuleSet(ns, "metrics-ruleset", []string{"metrics-rs-source"}, nil)

		s.CreateEngine(ns, "metrics-engine", framework.EngineOpts{
			RuleSetName: "metrics-ruleset",
			GatewayName: "non-existent-gateway",
		})

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			body := fetchMetrics(collect)
			if body == "" {
				return
			}
			hasEngineMetric := strings.Contains(body, "coraza_engine_info") ||
				strings.Contains(body, "coraza_engines")
			assert.True(collect, hasEngineMetric,
				"expected coraza_engine_info or coraza_engines metric after Engine creation")

			hasRulesetMetric := strings.Contains(body, "coraza_ruleset_info") ||
				strings.Contains(body, "coraza_rulesets")
			assert.True(collect, hasRulesetMetric,
				"expected coraza_ruleset_info or coraza_rulesets metric after RuleSet creation")
		}, framework.DefaultTimeout, framework.DefaultInterval)
	})
}
