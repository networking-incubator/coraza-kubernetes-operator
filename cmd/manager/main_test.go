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

package main

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
)

// -----------------------------------------------------------------------------
// buildCacheOptions Tests
// -----------------------------------------------------------------------------

func TestBuildCacheOptionsScopesPodsToGatewayDataPlane(t *testing.T) {
	opts := buildCacheOptions("operators")

	var byObject ctrlcache.ByObject
	found := false
	for object, config := range opts.ByObject {
		if _, ok := object.(*corev1.Pod); ok {
			byObject, found = config, true
		}
	}
	require.True(t, found, "the Pod informer must be scoped")
	require.NotNil(t, byObject.Transform, "the Pod informer must strip PodStatus churn")
	assert.True(t, byObject.Label.Matches(labels.Set{xds.GatewayNameLabel: "gw"}),
		"the Pod informer must only cache pods carrying the gateway-name label")
	assert.False(t, byObject.Label.Matches(labels.Set{"app": "gw"}),
		"the Pod informer must not cache non-gateway pods")
}

// -----------------------------------------------------------------------------
// Listener address Tests
// -----------------------------------------------------------------------------

func TestListenPort(t *testing.T) {
	tests := map[string]int32{
		":19090":      19090,
		"0.0.0.0:443": 443,
		":0":          0,
		"invalid":     0,
		":99999":      0,
	}
	for address, want := range tests {
		if got := listenPort(address); got != want {
			t.Errorf("listenPort(%q) = %d, want %d", address, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// buildTLSOpts Tests
// -----------------------------------------------------------------------------

func TestBuildTLSOpts_EnforcesTLS13AndDisablesHTTP2(t *testing.T) {
	opts := buildTLSOpts()
	require.Len(t, opts, 1)

	tlsCfg := &tls.Config{}
	opts[0](tlsCfg)
	assert.Equal(t, uint16(tls.VersionTLS13), tlsCfg.MinVersion)
	assert.Equal(t, []string{"http/1.1"}, tlsCfg.NextProtos,
		"HTTP/2 should be disabled to mitigate Rapid Reset (CVE-2023-44487)")
}

// -----------------------------------------------------------------------------
// buildMetricsServerOptions Tests
// -----------------------------------------------------------------------------

func TestBuildMetricsServerOptions_AlwaysSecure(t *testing.T) {
	cfg := config{
		metricsAddr: ":8443",
	}

	opts := buildMetricsServerOptions(cfg, nil)

	assert.Equal(t, ":8443", opts.BindAddress)
	assert.True(t, opts.SecureServing)
	assert.NotNil(t, opts.FilterProvider)
	assert.Empty(t, opts.CertDir)
}

func TestBuildMetricsServerOptions_WithCertPath(t *testing.T) {
	cfg := config{
		metricsAddr:     ":8443",
		metricsCertPath: "/certs",
		metricsCertName: "server.crt",
		metricsCertKey:  "server.key",
	}

	opts := buildMetricsServerOptions(cfg, nil)

	assert.True(t, opts.SecureServing)
	assert.NotNil(t, opts.FilterProvider)
	assert.Equal(t, "/certs", opts.CertDir)
	assert.Equal(t, "server.crt", opts.CertName)
	assert.Equal(t, "server.key", opts.KeyName)
}

func TestBuildMetricsServerOptions_SelfSignedWhenNoCert(t *testing.T) {
	cfg := config{
		metricsAddr: ":8443",
	}

	opts := buildMetricsServerOptions(cfg, nil)

	assert.True(t, opts.SecureServing)
	assert.Empty(t, opts.CertDir)
	assert.Empty(t, opts.CertName)
	assert.Empty(t, opts.KeyName)
}
