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
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Envoy Admin - Consts + Types
// -----------------------------------------------------------------------------

// envoyAdminPort is the Istio sidecar's Envoy admin port.
const envoyAdminPort = 15000

// EnvoyAdminProxy manages a port-forward to a Gateway pod's Envoy admin
// interface and exposes its config dump, clusters and stats endpoints.
type EnvoyAdminProxy struct {
	s       *Scenario
	baseURL string
	httpc   *http.Client
}

// envoyStatsResponse is the shape of /stats?format=json.
type envoyStatsResponse struct {
	Stats []struct {
		Name  string `json:"name"`
		Value int64  `json:"value"`
	} `json:"stats"`
}

// -----------------------------------------------------------------------------
// Envoy Admin - Proxy Setup
// -----------------------------------------------------------------------------

// ProxyToEnvoyAdmin sets up a port-forward to the named Gateway pod's Envoy
// admin port 15000 and returns a proxy for the admin endpoints. The
// port-forward is cleaned up when the scenario ends.
func (s *Scenario) ProxyToEnvoyAdmin(namespace, gatewayName string) *EnvoyAdminProxy {
	s.T.Helper()

	proxy := &EnvoyAdminProxy{
		s:     s,
		httpc: &http.Client{Timeout: DefaultTimeout},
	}

	selector := fmt.Sprintf("%s=%s", gatewayNameLabel, gatewayName)
	pod := s.ProxyToPod(namespace, selector, envoyAdminPort)
	proxy.baseURL = fmt.Sprintf("http://localhost:%s", pod.LocalPort())

	// /ready returns 200 with "LIVE" only after the proxy has initialized.
	require.EventuallyWithT(s.T, func(collect *assert.CollectT) {
		res := proxy.Get("/ready")
		if !assert.NoError(collect, res.Err, "Envoy admin not reachable") {
			return
		}
		assert.Equal(collect, http.StatusOK, res.StatusCode, "Envoy admin is not live")
	}, DefaultTimeout, DefaultInterval, "Envoy admin for %s/%s not ready", namespace, gatewayName)

	s.T.Logf("Port-forwarding Envoy admin for %s/%s -> localhost:%s", namespace, gatewayName, pod.LocalPort())
	return proxy
}

// URL returns the full URL for a given admin path.
func (p *EnvoyAdminProxy) URL(path string) string {
	return p.baseURL + path
}

// Get makes a GET request to the Envoy admin endpoint.
func (p *EnvoyAdminProxy) Get(path string) *HTTPResult {
	req, err := http.NewRequestWithContext(p.s.T.Context(), http.MethodGet, p.URL(path), nil)
	if err != nil {
		return &HTTPResult{Err: err}
	}
	resp, err := p.httpc.Do(req)
	if err != nil {
		return &HTTPResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return &HTTPResult{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Body:       body,
	}
}

// -----------------------------------------------------------------------------
// Envoy Admin - Endpoints
// -----------------------------------------------------------------------------

// ConfigDump returns the raw /config_dump JSON.
func (p *EnvoyAdminProxy) ConfigDump() *HTTPResult {
	return p.Get("/config_dump")
}

// Clusters returns the raw /clusters JSON.
func (p *EnvoyAdminProxy) Clusters() *HTTPResult {
	return p.Get("/clusters?format=json")
}

// Stats fetches /stats?format=json and returns a name -> value map.
func (p *EnvoyAdminProxy) Stats() (map[string]int64, error) {
	res := p.Get("/stats?format=json")
	if res.Err != nil {
		return nil, res.Err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("envoy admin /stats returned %d: %s", res.StatusCode, string(res.Body))
	}
	return parseEnvoyStats(res.Body)
}

func parseEnvoyStats(body []byte) (map[string]int64, error) {
	var parsed envoyStatsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode Envoy admin /stats: %w", err)
	}
	stats := make(map[string]int64, len(parsed.Stats))
	for _, stat := range parsed.Stats {
		stats[stat.Name] = stat.Value
	}
	return stats, nil
}

// ExpectConfigDumpContains polls until the raw /config_dump body contains substr.
// Use it to assert the ECDS HTTP filter and clusters are attached without
// depending on the nested dump structure.
func (p *EnvoyAdminProxy) ExpectConfigDumpContains(substr string) {
	p.s.T.Helper()
	require.EventuallyWithT(p.s.T, func(collect *assert.CollectT) {
		res := p.ConfigDump()
		if !assert.NoError(collect, res.Err) {
			return
		}
		if !assert.Equal(collect, http.StatusOK, res.StatusCode) {
			return
		}
		assert.Contains(collect, string(res.Body), substr,
			"Envoy config dump must contain %q", substr)
	}, DefaultTimeout, DefaultInterval)
}
