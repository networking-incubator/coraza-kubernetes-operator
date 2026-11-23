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
	"flag"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"

	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
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

func TestBuildECDSConfigAdvertiseAddress(t *testing.T) {
	tests := map[string]struct {
		advertise    string
		serviceName  string
		operatorName string
		want         string
		wantErr      string
	}{
		"bare host gets the port":           {advertise: "coraza.example", want: "coraza.example:18080"},
		"host and port are kept":            {advertise: "coraza.example:9443", want: "coraza.example:9443"},
		"bare IPv6 gets brackets":           {advertise: "2001:db8::1", want: "[2001:db8::1]:18080"},
		"bracketed IPv6 is not rebracketed": {advertise: "[2001:db8::1]", want: "[2001:db8::1]:18080"},
		"bracketed IPv6 with port is kept":  {advertise: "[2001:db8::1]:9443", want: "[2001:db8::1]:9443"},
		"derived from the Service name":     {serviceName: "coraza", want: "coraza.default.svc.cluster.local:18080"},
		"derived from the release name":     {operatorName: "coraza-release", want: "coraza-release.default.svc.cluster.local:18080"},
		"no address at all":                 {wantErr: "the xDS advertise address is not configured"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, assetURL, err := buildECDSConfig(config{
				xdsAdvertiseAddress: tc.advertise,
				operatorServiceName: tc.serviceName,
				operatorName:        tc.operatorName,
				xdsPort:             18080,
				// A missing CA is not fatal: Engine reconciliation reports it.
				xdsCAPath: filepath.Join(t.TempDir(), "missing-ca.crt"),
			}, "default", xds.NewCASource(filepath.Join(t.TempDir(), "missing-ca.crt"), logr.Discard()))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.AdvertiseAddress)
			assert.Equal(t, "https://"+tc.want, assetURL)
		})
	}
}

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

// -----------------------------------------------------------------------------
// Cluster rule policy flag Tests
// -----------------------------------------------------------------------------

func TestRuleMaxRulesFlag(t *testing.T) {
	for _, args := range [][]string{nil, {"--rule-max-rules=0"}} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		var cfg config
		addRulePolicyFlags(fs, &cfg)
		require.NoError(t, fs.Parse(args))
		_, err := cfg.rulePolicy()
		if args == nil {
			assert.Equal(t, "20000", fs.Lookup("rule-max-rules").DefValue)
			assert.NoError(t, err)
		} else {
			assert.ErrorContains(t, err, "rule-max-rules 0 must be positive")
		}
	}
}

func TestRulePolicyFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want validation.RulePolicy
	}{
		{name: "defaults deny nothing", want: validation.RulePolicy{}},
		{
			name: "every flag set",
			args: []string{
				"--rule-denied-operators=pmf, RX",
				"--rule-denied-directives=secruleengine,Include",
				"--rule-max-request-body-limit=1024",
				"--rule-response-body-access=Denied",
			},
			want: validation.RulePolicy{
				DeniedOperators:        []string{"pmFromFile", "rx"},
				DeniedDirectives:       []string{"SecRuleEngine", "Include"},
				MaxRequestBodyLimit:    1024,
				DenyResponseBodyAccess: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			var cfg config
			addRulePolicyFlags(fs, &cfg)
			require.NoError(t, fs.Parse(tt.args))
			assert.Equal(t, strconv.Itoa(validation.MaxRequestBodyLimit), fs.Lookup("rule-max-request-body-limit").DefValue)
			got, err := cfg.rulePolicy()
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
