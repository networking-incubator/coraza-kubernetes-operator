package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

func TestParseCommaSeparatedLabels(t *testing.T) {
	t.Parallel()

	assert.Nil(t, ParseCommaSeparatedLabels(""))
	assert.Equal(t, map[string]string{"release": "kube-prometheus-stack"}, ParseCommaSeparatedLabels("release=kube-prometheus-stack"))
	assert.Equal(t, map[string]string{
		"release": "kube-prometheus-stack",
		"team":    "waf",
	}, ParseCommaSeparatedLabels("release=kube-prometheus-stack,team=waf"))
}

func TestBuildPodMonitorNamedPort(t *testing.T) {
	t.Parallel()

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "coraza",
		Namespace:   "integration-tests",
		GatewayName: "coraza-gateway",
	})

	r := &EngineReconciler{
		dataplanePodMonitorEnabled: true,
		dataplanePodMonitorLabels: map[string]string{
			"release": "kube-prometheus-stack",
		},
		dataplanePodMonitorInterval:      "30s",
		dataplanePodMonitorScrapeTimeout: "10s",
		dataplanePodMonitorPortName:      "http-envoy-prom",
	}

	pm := r.buildPodMonitor(engine, map[string]string{
		gatewayNameLabel: "coraza-gateway",
	})
	require.NotNil(t, pm)

	assert.Equal(t, "coraza-engine-coraza-dataplane", pm.GetName())
	assert.Equal(t, "integration-tests", pm.GetNamespace())
	assert.Equal(t, "coraza", pm.GetLabels()[networkPolicyEngineLabelName])
	assert.Equal(t, "kube-prometheus-stack", pm.GetLabels()["release"])

	matchLabels, found, err := unstructured.NestedStringMap(pm.Object, "spec", "selector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "coraza-gateway", matchLabels[gatewayNameLabel])

	endpoints, found, err := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, endpoints, 1)

	endpoint, ok := endpoints[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "http-envoy-prom", endpoint["port"])
	assert.Nil(t, endpoint["targetPort"])
	assert.Equal(t, "/stats/prometheus", endpoint["path"])

	relabelings, ok := endpoint["metricRelabelings"].([]any)
	require.True(t, ok)
	require.GreaterOrEqual(t, len(relabelings), 43)
}

func TestBuildPodMonitorNumericPort(t *testing.T) {
	t.Parallel()

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "coraza",
		Namespace:   "integration-tests",
		GatewayName: "coraza-gateway",
	})

	r := &EngineReconciler{
		dataplanePodMonitorEnabled:       true,
		dataplanePodMonitorInterval:      "30s",
		dataplanePodMonitorScrapeTimeout: "10s",
		dataplanePodMonitorPortName:      "15090",
	}

	pm := r.buildPodMonitor(engine, map[string]string{
		gatewayNameLabel: "coraza-gateway",
	})
	require.NotNil(t, pm)

	endpoints, found, err := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, endpoints, 1)

	endpoint, ok := endpoints[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, int64(15090), endpoint["targetPort"])
	assert.Nil(t, endpoint["port"])
	assert.Equal(t, "/stats/prometheus", endpoint["path"])
}
