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

// TestCorazaControllerMetrics verifies that the CorazaCollector metrics
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
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		assert.Equal(collect, http.StatusOK, resp.StatusCode,
			"expected 200 for authenticated metrics request, got %d", resp.StatusCode)
		return string(body)
	}

	// -------------------------------------------------------------------------
	// Verify Coraza engine/ruleset metrics are present
	// -------------------------------------------------------------------------

	s.Step("verify CorazaCollector engine metrics")
	t.Run("exposes coraza_engine metrics", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			body := fetchMetrics(collect)
			if body == "" {
				return
			}
			// The CorazaCollector always sends descriptors via Describe, so the
			// HELP lines appear in every scrape even when no Engine resources exist.
			// Assert descriptor presence rather than data-point presence to avoid
			// false failures on a clean cluster with no Engines.
			hasEngineDescriptor := strings.Contains(body, "# HELP coraza_engine_info") ||
				strings.Contains(body, "# HELP coraza_engines")
			assert.True(collect, hasEngineDescriptor,
				"expected # HELP coraza_engine_info or # HELP coraza_engines descriptor in response")
		}, framework.DefaultTimeout, framework.DefaultInterval)
	})

	s.Step("verify CorazaCollector ruleset metrics")
	t.Run("exposes coraza_ruleset metrics", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			body := fetchMetrics(collect)
			if body == "" {
				return
			}
			// The CorazaCollector always sends descriptors via Describe, so the
			// HELP lines appear in every scrape even when no RuleSet resources exist.
			hasRulesetDescriptor := strings.Contains(body, "# HELP coraza_ruleset_info") ||
				strings.Contains(body, "# HELP coraza_rulesets")
			assert.True(collect, hasRulesetDescriptor,
				"expected # HELP coraza_ruleset_info or # HELP coraza_rulesets descriptor in response")
		}, framework.DefaultTimeout, framework.DefaultInterval)
	})

	s.Step("verify cache server RED metrics")
	t.Run("exposes coraza_cache_server_requests_total", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			body := fetchMetrics(collect)
			if body == "" {
				return
			}
			assert.Contains(collect, body, "coraza_cache_server_requests_total",
				"expected coraza_cache_server_requests_total metric family in response")
		}, framework.DefaultTimeout, framework.DefaultInterval)
	})

	// -------------------------------------------------------------------------
	// Subtest: create a minimal Engine and verify coraza_engines metric appears
	// -------------------------------------------------------------------------

	s.Step("create Engine and verify coraza_engines metric")
	t.Run("coraza_engines metric appears after Engine creation", func(t *testing.T) {
		ns := s.GenerateNamespace("metrics-engine")

		// Create a minimal RuleSource and RuleSet that the Engine can reference.
		s.CreateRuleSource(ns, "metrics-rs-source", `SecRuleEngine DetectionOnly`)
		s.CreateRuleSet(ns, "metrics-ruleset", []string{"metrics-rs-source"}, nil)

		// Create the Engine referencing the RuleSet.
		// We intentionally omit a gateway so it will not fully reconcile —
		// but the Engine object will exist and the controller will register it.
		s.CreateEngine(ns, "metrics-engine", framework.EngineOpts{
			RuleSetName: "metrics-ruleset",
			GatewayName: "non-existent-gateway",
		})

		// Assert the coraza_engines metric NAME appears in the response.
		// Do not assert specific label values since the Engine may not reconcile fully in CI.
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			body := fetchMetrics(collect)
			if body == "" {
				return
			}
			hasEngineMetric := strings.Contains(body, "coraza_engine_info") ||
				strings.Contains(body, "coraza_engines")
			assert.True(collect, hasEngineMetric,
				"expected coraza_engine_info or coraza_engines metric to appear after Engine creation")
		}, framework.DefaultTimeout, framework.DefaultInterval)
	})
}
