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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/defaults"
)

// -----------------------------------------------------------------------------
// validateDefaultWasmImage Tests
// -----------------------------------------------------------------------------

func TestValidateDefaultWasmImage(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateDefaultWasmImage(""))
	})

	t.Run("not_oci", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateDefaultWasmImage("docker.io/foo:latest"))
		assert.Error(t, validateDefaultWasmImage("http://example/wasm"))
	})

	t.Run("too_long", func(t *testing.T) {
		t.Parallel()
		long := "oci://" + strings.Repeat("a", wafv1alpha1.MaxImageLen+1-len("oci://"))
		require.Len(t, long, wafv1alpha1.MaxImageLen+1)
		assert.Error(t, validateDefaultWasmImage(long))
	})

	t.Run("max_len_ok", func(t *testing.T) {
		t.Parallel()
		s := "oci://" + strings.Repeat("a", wafv1alpha1.MaxImageLen-len("oci://"))
		require.Len(t, s, wafv1alpha1.MaxImageLen)
		assert.NoError(t, validateDefaultWasmImage(s))
	})

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateDefaultWasmImage("oci://ghcr.io/org/coraza-proxy-wasm:tag"))
	})
}

// -----------------------------------------------------------------------------
// resolveDefaultWasmImage Tests
// -----------------------------------------------------------------------------

func TestResolveDefaultWasmImage(t *testing.T) {
	t.Run("env var overrides hardcoded default", func(t *testing.T) {
		t.Setenv("CORAZA_DEFAULT_WASM_IMAGE", "oci://custom/img:v1")
		assert.Equal(t, "oci://custom/img:v1", resolveDefaultWasmImage())
	})

	t.Run("falls back to hardcoded default when env var unset", func(t *testing.T) {
		assert.Equal(t, defaults.DefaultCorazaWasmOCIReference, resolveDefaultWasmImage())
	})
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
