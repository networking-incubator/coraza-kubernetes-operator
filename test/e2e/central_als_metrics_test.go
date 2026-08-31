//go:build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/framework"
)

var centralALSMeshConfigMu sync.Mutex

// defaultFilterStateWasmImage is coraza-proxy-wasm PR #20+ (Envoy filter state for CIO ALS).
const defaultFilterStateWasmImage = "oci://ghcr.io/networking-incubator/coraza-proxy-wasm:e20b40ca25e3c50f212999e3decfde5503e630c3"

const (
	centralALSCollectorNamespace = "coraza-central-waf-telemetry"
	centralALSCollectorService   = "central-waf-als-collector.coraza-central-waf-telemetry.svc.cluster.local"
	centralALSProviderName       = "central-als-provider"
)

func centralALSTestdata(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(file), "testdata", "central-als")
}

func otelCollectorCRDPresent(t *testing.T) bool {
	t.Helper()
	out, err := fw.Kubectl("", "api-resources", "--api-group=opentelemetry.io").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "opentelemetrycollectors")
}

// assertMetricLineContains asserts that the metrics response contains a line
// with substr, failing fast on a read or close error instead of masking it as
// a polling timeout.
func assertMetricLineContains(t *testing.T, httpc *http.Client, url, substr string) {
	t.Helper()
	assertMetricLineContainsAll(t, httpc, url, substr)
}

// assertMetricLineContainsAll asserts that a single line in the metrics
// response contains every substring in substrs together, so a test can't
// pass because the required labels merely appear somewhere in the response
// scattered across unrelated series.
func assertMetricLineContainsAll(t *testing.T, httpc *http.Client, url string, substrs ...string) {
	t.Helper()
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		resp, err := httpc.Get(url)
		if !assert.NoError(collect, err) {
			return
		}
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if !assert.NoError(collect, readErr, "read metrics response body") {
			return
		}
		if !assert.NoError(collect, closeErr, "close metrics response body") {
			return
		}
		if !assert.Equal(collect, http.StatusOK, resp.StatusCode) {
			return
		}
		for _, line := range strings.Split(string(body), "\n") {
			allMatch := true
			for _, substr := range substrs {
				if !strings.Contains(line, substr) {
					allMatch = false
					break
				}
			}
			if allMatch {
				return
			}
		}
		assert.Fail(collect, fmt.Sprintf("no single metric line contains all of %v", substrs))
	}, framework.WasmEnforcementTimeout, framework.DefaultInterval)
}

// meshConfigExtensionProviders returns the Sail Istio CR's current
// spec.values.meshConfig.extensionProviders as a JSON array string ("[]" if
// unset), so callers can restore it later with a merge patch.
func meshConfigExtensionProviders(t *testing.T, istioName string) string {
	t.Helper()
	out, err := fw.Kubectl("", "get", "istio", istioName,
		"-o", "jsonpath={.spec.values.meshConfig.extensionProviders}").CombinedOutput()
	require.NoError(t, err, "read meshConfig.extensionProviders: %s", string(out))
	providers := strings.TrimSpace(string(out))
	if providers == "" || providers == "null" {
		return "[]"
	}
	return providers
}

func configureCentralALSProvider(t *testing.T, istioName string) {
	t.Helper()

	centralALSMeshConfigMu.Lock()
	t.Cleanup(centralALSMeshConfigMu.Unlock)

	priorProviders := meshConfigExtensionProviders(t, istioName)
	t.Cleanup(func() {
		patch := fmt.Sprintf(`{"spec":{"values":{"meshConfig":{"extensionProviders":%s}}}}`, priorProviders)
		out, err := fw.Kubectl("", "patch", "istio", istioName, "--type=merge", "-p", patch).CombinedOutput()
		if err != nil {
			t.Errorf("cleanup: failed to restore meshConfig.extensionProviders: %v: %s", err, string(out))
		}
	})

	var providers []map[string]any
	require.NoError(t, json.Unmarshal([]byte(priorProviders), &providers), "decode meshConfig.extensionProviders")

	provider := map[string]any{
		"name": centralALSProviderName,
		"envoyOtelAls": map[string]any{
			"service": centralALSCollectorService,
			"port":    4317,
			"logFormat": map[string]any{
				"labels": map[string]string{
					"start_time":     "%START_TIME%",
					"namespace":      "%ENVIRONMENT(POD_NAMESPACE)%",
					"gateway":        "%ENVIRONMENT(ISTIO_META_WORKLOAD_NAME)%",
					"pod_name":       "%ENVIRONMENT(POD_NAME)%",
					"method":         "%REQ(:METHOD)%",
					"path":           "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%",
					"response_code":  "%RESPONSE_CODE%",
					"response_flags": "%RESPONSE_FLAGS%",
					"route_name":     "%ROUTE_NAME%",
					"authority":      "%REQ(:AUTHORITY)%",
					"duration":       "%DURATION%",
					"client_ip":      "%REQ(X-FORWARDED-FOR)%",
					"request_id":     "%REQ(X-REQUEST-ID)%",
					"sni_hostname":   "%REQUESTED_SERVER_NAME%",
					"waf_event":      "%FILTER_STATE(wasm.io.coraza.waf.event:PLAIN)%",
					"rule_id":        "%FILTER_STATE(wasm.io.coraza.waf.rule_id:PLAIN)%",
					"action":         "%FILTER_STATE(wasm.io.coraza.waf.action:PLAIN)%",
					"phase":          "%FILTER_STATE(wasm.io.coraza.waf.phase:PLAIN)%",
					"waf_status":     "%FILTER_STATE(wasm.io.coraza.waf.status:PLAIN)%",
					"severity":       "%FILTER_STATE(wasm.io.coraza.waf.severity:PLAIN)%",
					"category":       "%FILTER_STATE(wasm.io.coraza.waf.category:PLAIN)%",
				},
			},
		},
	}

	updated := make([]map[string]any, 0, len(providers)+1)
	for _, existing := range providers {
		if existing["name"] != centralALSProviderName {
			updated = append(updated, existing)
		}
	}
	updated = append(updated, provider)

	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"values": map[string]any{
				"meshConfig": map[string]any{"extensionProviders": updated},
			},
		},
	})
	require.NoError(t, err, "encode MeshConfig provider patch")

	out, err := fw.Kubectl("", "patch", "istio", istioName, "--type=merge", "-p", string(patch)).CombinedOutput()
	require.NoError(t, err, "patch MeshConfig ALS provider: %s", string(out))
}

// TestCentralALSMetricsPipeline checks Gateway traffic reaches central OTC /metrics
// after sidecar removal (#65). Needs OpenTelemetry Operator CRDs (cluster.kind.otel).
func TestCentralALSMetricsPipeline(t *testing.T) {
	if fw.ClusterName == "external" {
		t.Skip("skipping: this test mutates shared cluster state (MeshConfig, collector scale); requires a KIND cluster (KIND_CLUSTER_NAME)")
	}
	if !otelCollectorCRDPresent(t) {
		t.Skip("skipping: OpenTelemetryCollector CRD absent (use make cluster.kind.otel)")
	}

	s := fw.NewScenario(t)
	testdata := centralALSTestdata(t)

	ns := s.GenerateNamespace("central-als")
	gwName := "gw"
	engineName := "engine"

	// FILTER_STATE WASM (coraza-proxy-wasm PR #20+). Override with CORAZA_WASM_IMAGE.
	wasmImage := os.Getenv("CORAZA_WASM_IMAGE")
	if wasmImage == "" {
		wasmImage = defaultFilterStateWasmImage
	}

	s.Step("apply central ALS test fixture (no hostPath)")
	s.ApplyManifest("", filepath.Join(testdata, "00-namespace.yaml"))
	s.ApplyManifest("", filepath.Join(testdata, "10-otel-collector.yaml"))
	require.Eventually(t, func() bool {
		out, err := fw.Kubectl(centralALSCollectorNamespace, "get", "pods",
			"-l", "app.kubernetes.io/name=central-waf-als-collector",
			"-o", "jsonpath={.items[0].status.conditions[?(@.type==\"Ready\")].status}").CombinedOutput()
		return err == nil && strings.TrimSpace(string(out)) == "True"
	}, framework.DefaultTimeout, framework.DefaultInterval, "central collector Ready")

	s.Step("patch MeshConfig ALS provider (serialized, save+restore)")
	istioName := envOr("ISTIO_NAME", "coraza")
	configureCentralALSProvider(t, istioName)

	s.Step("declare central collector on the GatewayClass")
	gatewayClass, err := fw.DynamicClient.Resource(framework.GatewayClassGVR).Get(t.Context(), "istio", metav1.GetOptions{})
	require.NoError(t, err)
	annotations := gatewayClass.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["internal.do-not-use.openshift.io/waf-otel-collector"] = centralALSProviderName
	gatewayClass.SetAnnotations(annotations)
	_, err = fw.DynamicClient.Resource(framework.GatewayClassGVR).Update(t.Context(), gatewayClass, metav1.UpdateOptions{})
	require.NoError(t, err)

	s.Step("create gateway + WAF")
	s.CreateGateway(ns, gwName)
	s.ExpectGatewayProgrammed(ns, gwName)
	s.CreateRuleSource(ns, "base", `SecRuleEngine On`)
	s.CreateRuleSource(ns, "block", `SecRule ARGS_GET:q "@contains evilmonkey" "id:3001,phase:1,deny,status:403,severity:'CRITICAL',tag:'attack-other',msg:'spike block'"`)
	s.CreateRuleSet(ns, "ruleset", []string{"base", "block"}, nil)
	s.CreateEngine(ns, engineName, framework.EngineOpts{
		RuleSetName:         "ruleset",
		GatewayName:         gwName,
		WasmImage:           wasmImage,
		EnableObservability: true,
	})
	s.ExpectEngineReady(ns, engineName)
	s.Step("operator creates Gateway-scoped Telemetry")
	telemetry, err := fw.DynamicClient.Resource(framework.TelemetryGVR).Namespace(ns).Get(t.Context(), "coraza-engine-"+engineName+"-telemetry", metav1.GetOptions{})
	require.NoError(t, err)
	accessLogging, found, err := unstructured.NestedSlice(telemetry.Object, "spec", "accessLogging")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, accessLogging, 1)
	accessLog := accessLogging[0].(map[string]any)
	providers := accessLog["providers"].([]any)
	require.Len(t, providers, 1)
	assert.Equal(t, centralALSProviderName, providers[0].(map[string]any)["name"])
	s.WaitForGatewayPodStable(ns, gwName)

	s.Step("architectural asserts: no OTC sidecar / inject / hostPath volume on Gateway path")
	otcGVR := schema.GroupVersionResource{Group: "opentelemetry.io", Version: "v1beta1", Resource: "opentelemetrycollectors"}
	list, err := fw.DynamicClient.Resource(otcGVR).Namespace(ns).List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, item := range list.Items {
		assert.False(t, strings.HasPrefix(item.GetName(), "coraza-otc-"),
			"operator must not create per-Engine OTC %s/%s", ns, item.GetName())
	}
	gwObj, err := fw.DynamicClient.Resource(framework.GatewayGVR).Namespace(ns).Get(t.Context(), gwName, metav1.GetOptions{})
	require.NoError(t, err)
	ann, _ := unstructuredNestedString(gwObj.Object, "spec", "infrastructure", "annotations", "sidecar.opentelemetry.io/inject")
	assert.Empty(t, ann, "Gateway must not have OTC inject annotation")

	gwPods, err := fw.KubeClient.CoreV1().Pods(ns).List(t.Context(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", gwName),
	})
	require.NoError(t, err)
	require.NotEmpty(t, gwPods.Items, "expected at least one Gateway pod")
	for _, pod := range gwPods.Items {
		for _, vol := range pod.Spec.Volumes {
			assert.Nil(t, vol.HostPath, "Gateway pod %s/%s must not have hostPath volume %q", ns, pod.Name, vol.Name)
		}
	}

	s.Step("deploy backend and route")
	s.CreateEchoBackend(ns, "echo")
	s.CreateHTTPRoute(ns, "route", gwName, "echo")

	proxy := s.ProxyToGateway(ns, gwName)
	s.Step("allow + block traffic")
	proxy.ExpectAllowed("/spike/")
	for range 8 {
		_ = proxy.Get("/spike/")
	}
	proxy.ExpectStatus("/spike/?q=evilmonkey", http.StatusForbidden)
	for range 3 {
		_ = proxy.Get("/spike/?q=evilmonkey")
	}

	s.Step("scrape central collector metrics")
	collectorProxy := s.ProxyToPod(centralALSCollectorNamespace, "app.kubernetes.io/name=central-waf-als-collector", 9090)
	metricsURL := fmt.Sprintf("http://localhost:%s/metrics", collectorProxy.LocalPort())
	httpc := &http.Client{Timeout: framework.DefaultTimeout}
	// Filter state is interruption-only today: block path is required; pass may be absent.
	assertMetricLineContainsAll(t, httpc, metricsURL,
		`coraza_waf_requests_total{`, `outcome="block"`, `namespace="`+ns+`"`, `driver_type="wasm"`)
	assertMetricLineContains(t, httpc, metricsURL, `coraza_waf_blocked_requests_total{`)

	s.Step("collector outage does not break WAF traffic")
	scaleOut, err := fw.Kubectl(centralALSCollectorNamespace, "scale", "deploy/central-waf-als-collector", "--replicas=0").CombinedOutput()
	require.NoError(t, err, string(scaleOut))
	s.OnCleanup(func() {
		out, err := fw.Kubectl(centralALSCollectorNamespace, "scale", "deploy/central-waf-als-collector", "--replicas=1").CombinedOutput()
		if err != nil {
			// One cheap retry (scale is idempotent) before failing loudly.
			out, err = fw.Kubectl(centralALSCollectorNamespace, "scale", "deploy/central-waf-als-collector", "--replicas=1").CombinedOutput()
		}
		if err != nil {
			t.Errorf("cleanup: failed to restore central-waf-als-collector replicas: %v: %s", err, string(out))
		}
	})
	require.Eventually(t, func() bool {
		out, _ := fw.Kubectl(centralALSCollectorNamespace, "get", "deploy", "central-waf-als-collector",
			"-o", "jsonpath={.status.availableReplicas}").CombinedOutput()
		return strings.TrimSpace(string(out)) == "" || strings.TrimSpace(string(out)) == "0"
	}, framework.DefaultTimeout, framework.DefaultInterval)
	proxy.ExpectAllowed("/spike/")
	proxy.ExpectStatus("/spike/?q=evilmonkey", http.StatusForbidden)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func unstructuredNestedString(obj map[string]any, fields ...string) (string, bool) {
	cur := any(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[f]
		if !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	return s, ok
}
