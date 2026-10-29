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

package controller

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

const (
	envoyFilterTestEngineName  = "checkout"
	envoyFilterTestNamespace   = "shop"
	envoyFilterTestAssetHost   = "coraza-operator.coraza-system.svc.cluster.local"
	envoyFilterTestBinaryURL   = "https://" + envoyFilterTestAssetHost + ":18080/wasm/cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.wasm"
	envoyFilterTestAdvertise   = envoyFilterTestAssetHost + ":18080"
	envoyFilterTestCA          = "-----BEGIN CERTIFICATE-----\nMIIBtestCa\n-----END CERTIFICATE-----\n"
	envoyFilterTestGeneratedBy = "coraza-kubernetes-operator"
)

// newEnvoyFilterTestEngine returns a Gateway-targeted Engine.
func newEnvoyFilterTestEngine() *wafv1alpha1.Engine {
	return &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envoyFilterTestEngineName,
			Namespace: envoyFilterTestNamespace,
			UID:       types.UID("11111111-1111-4111-8111-111111111111"),
		},
		Spec: wafv1alpha1.EngineSpec{
			RuleSet: wafv1alpha1.RuleSetReference{Name: "base"},
			Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: "gw",
			},
		},
	}
}

// newEnvoyFilterTestArtifact renders a real artifact for the EnvoyFilter test.
func newEnvoyFilterTestArtifact(t *testing.T) (render.Artifact, render.BinaryRef) {
	t.Helper()

	engine := newEnvoyFilterTestEngine()
	content := assemble.Content{Rules: "SecRuleEngine On"}
	contentHash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)

	binary := render.BinaryRef{
		URL:    envoyFilterTestBinaryURL,
		SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
	}
	artifact, err := render.Render(render.EngineInput{
		Namespace: engine.Namespace,
		Name:      engine.Name,
		UID:       engine.UID,
		RuleSet: render.RuleSetAccepted{
			UID:         types.UID("22222222-2222-4222-8222-222222222222"),
			ContentHash: contentHash,
		},
		Content: content,
		Binary:  binary,
	})
	require.NoError(t, err)

	return artifact, binary
}

// socketAddress returns the asset cluster's single endpoint socket address.
func socketAddress(t *testing.T, cluster map[string]any) map[string]any {
	t.Helper()
	endpoints := nestedSlice(t, cluster, "load_assignment", "endpoints")
	require.Len(t, endpoints, 1)
	lbEndpoints := asMap(t, endpoints[0])["lb_endpoints"]
	require.IsType(t, []any{}, lbEndpoints)
	endpoint := asMap(t, lbEndpoints.([]any)[0])
	return nestedMap(t, endpoint, "endpoint", "address", "socket_address")
}

func TestBuildEnvoyFilterShape(t *testing.T) {
	artifact, binary := newEnvoyFilterTestArtifact(t)
	engine := newEnvoyFilterTestEngine()
	cfg := EnvoyFilterConfig{
		AdvertiseAddress: envoyFilterTestAdvertise,
		CAPEM:            []byte(envoyFilterTestCA),
		IstioRevision:    "1-30-3",
	}

	for _, tc := range []struct {
		policy string
		action string
	}{
		{policy: "fail", action: "ALLOW"},
		{policy: "allow", action: "DENY"},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			ef, err := buildEnvoyFilter(engine, artifact, binary, cfg, tc.policy)
			require.NoError(t, err)

			assert.Equal(t, "networking.istio.io/v1alpha3", ef.GetAPIVersion())
			assert.Equal(t, "EnvoyFilter", ef.GetKind())
			assert.Equal(t, "coraza-engine-"+engine.Name, ef.GetName())
			assert.Equal(t, engine.Namespace, ef.GetNamespace())

			labels := ef.GetLabels()
			assert.Equal(t, envoyFilterTestGeneratedBy, labels["app.kubernetes.io/managed-by"])
			assert.Equal(t, engine.Name, labels[engineNameLabel])
			assert.Equal(t, cfg.IstioRevision, labels["istio.io/rev"])

			annotations := ef.GetAnnotations()
			assert.Equal(t, artifact.Version, annotations["waf.k8s.coraza.io/configuration-version"])
			assert.Equal(t, tc.policy, annotations["waf.k8s.coraza.io/bootstrap-policy"])
			caHashAnnotation := annotations["waf.k8s.coraza.io/ca-hash"]
			assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, caHashAnnotation)
			assert.Equal(t, caHash([]byte(envoyFilterTestCA)), caHashAnnotation)

			assert.Equal(t, map[string]any{gatewayNameLabel: "gw"},
				nestedMap(t, ef.Object, "spec", "workloadSelector", "labels"))

			patches := nestedSlice(t, ef.Object, "spec", "configPatches")
			require.Len(t, patches, 2)

			// Patch 1: the WASM asset cluster.
			assetClusterPatch := asMap(t, patches[0])
			assert.Equal(t, "CLUSTER", nestedString(t, assetClusterPatch, "applyTo"))
			assert.Equal(t, "GATEWAY", nestedString(t, assetClusterPatch, "match", "context"))
			assert.Equal(t, "ADD", nestedString(t, assetClusterPatch, "patch", "operation"))

			cluster := nestedMap(t, assetClusterPatch, "patch", "value")
			assert.Equal(t, "coraza_wasm", nestedString(t, cluster, "name"))
			assert.Equal(t, "STRICT_DNS", nestedString(t, cluster, "type"))
			assert.Equal(t, "10s", nestedString(t, cluster, "connect_timeout"))
			assert.Equal(t, "coraza_wasm", nestedString(t, cluster, "load_assignment", "cluster_name"))
			socket := socketAddress(t, cluster)
			assert.Equal(t, envoyFilterTestAssetHost, socket["address"])
			// protoToMap decodes protojson numbers as float64.
			assert.EqualValues(t, 18080, socket["port_value"])
			assert.Equal(t, "envoy.transport_sockets.tls", nestedString(t, cluster, "transport_socket", "name"))
			assert.Equal(t, "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext",
				nestedString(t, cluster, "transport_socket", "typed_config", "@type"))
			assert.Equal(t, envoyFilterTestAssetHost,
				nestedString(t, cluster, "transport_socket", "typed_config", "sni"))
			assert.Equal(t, envoyFilterTestCA,
				nestedString(t, cluster, "transport_socket", "typed_config",
					"common_tls_context", "validation_context", "trusted_ca", "inline_string"))

			// Patch 2: the ECDS HTTP filter, inserted before the router.
			httpFilterPatch := asMap(t, patches[1])
			assert.Equal(t, "HTTP_FILTER", nestedString(t, httpFilterPatch, "applyTo"))
			assert.Equal(t, "GATEWAY", nestedString(t, httpFilterPatch, "match", "context"))
			assert.Equal(t, "envoy.filters.network.http_connection_manager",
				nestedString(t, httpFilterPatch, "match", "listener", "filterChain", "filter", "name"))
			assert.Equal(t, "envoy.filters.http.router",
				nestedString(t, httpFilterPatch, "match", "listener", "filterChain", "filter", "subFilter", "name"))
			assert.Equal(t, "INSERT_BEFORE", nestedString(t, httpFilterPatch, "patch", "operation"))

			filter := nestedMap(t, httpFilterPatch, "patch", "value")
			assert.Equal(t, artifact.ResourceName, nestedString(t, filter, "name"))

			apiConfig := nestedMap(t, filter, "config_discovery", "config_source", "api_config_source")
			assert.Equal(t, "GRPC", nestedString(t, apiConfig, "api_type"))
			assert.Equal(t, "V3", nestedString(t, apiConfig, "transport_api_version"))
			discovery := nestedMap(t, filter, "config_discovery")

			grpcServices := nestedSlice(t, apiConfig, "grpc_services")
			require.Len(t, grpcServices, 1)
			googleGrpc := nestedMap(t, asMap(t, grpcServices[0]), "google_grpc")
			assert.Equal(t, envoyFilterTestAdvertise, nestedString(t, googleGrpc, "target_uri"))
			assert.Equal(t, "coraza_ecds", nestedString(t, googleGrpc, "stat_prefix"))
			assert.Equal(t, "envoy.grpc_credentials.file_based_metadata",
				nestedString(t, googleGrpc, "credentials_factory_name"))
			// proto3 JSON encodes 64-bit integers as strings; Envoy and
			// Istio's proto JSON parsers accept both forms identically.
			assert.Equal(t, "16777216",
				nestedString(t, googleGrpc, "channel_args", "args", "grpc.max_receive_message_length", "int_value"))
			assert.Equal(t, envoyFilterTestCA,
				nestedString(t, googleGrpc, "channel_credentials", "ssl_credentials", "root_certs", "inline_string"))
			assert.Equal(t, "TLSv1_3",
				nestedString(t, assetClusterPatch, "patch", "value", "transport_socket", "typed_config", "common_tls_context", "tls_params", "tls_minimum_protocol_version"))

			callCredentials := nestedSlice(t, googleGrpc, "call_credentials")
			require.Len(t, callCredentials, 1)
			fromPlugin := nestedMap(t, asMap(t, callCredentials[0]), "from_plugin")
			assert.Equal(t, "envoy.grpc_credentials.file_based_metadata", nestedString(t, fromPlugin, "name"))
			assert.Equal(t, "type.googleapis.com/envoy.config.grpc_credential.v3.FileBasedMetadataConfig",
				nestedString(t, fromPlugin, "typed_config", "@type"))
			assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token",
				nestedString(t, fromPlugin, "typed_config", "secret_data", "filename"))
			assert.Equal(t, "authorization", nestedString(t, fromPlugin, "typed_config", "header_key"))
			assert.Equal(t, "Bearer ", nestedString(t, fromPlugin, "typed_config", "header_prefix"))

			typeURLs := nestedSlice(t, discovery, "type_urls")
			assert.Equal(t, []any{
				"type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm",
				"type.googleapis.com/envoy.extensions.filters.http.rbac.v3.RBAC",
			}, typeURLs)

			assert.Equal(t, "V3", nestedString(t, discovery, "config_source", "resource_api_version"))
			assert.True(t, nestedBool(t, discovery, "apply_default_config_without_warming"))
			assert.Equal(t, "type.googleapis.com/envoy.extensions.filters.http.rbac.v3.RBAC",
				nestedString(t, discovery, "default_config", "@type"))
			// proto3 JSON omits the zero-valued ALLOW action and the empty
			// policies map. An absent `rules` object would make Envoy skip the
			// RBAC engine entirely and allow traffic, so its presence is the
			// fail-closed invariant.
			_, found, err := unstructured.NestedFieldNoCopy(discovery, "default_config", "rules")
			require.NoError(t, err)
			require.True(t, found, "default_config must carry a rules object")
			action, found, err := unstructured.NestedString(discovery, "default_config", "rules", "action")
			require.NoError(t, err)
			if tc.action == "ALLOW" {
				assert.False(t, found, "proto3 JSON omits the zero-valued ALLOW action")
			} else {
				require.True(t, found)
				assert.Equal(t, tc.action, action)
			}
		})
	}
}

func TestBuildEnvoyFilterWithoutIstioRevision(t *testing.T) {
	artifact, binary := newEnvoyFilterTestArtifact(t)
	ef, err := buildEnvoyFilter(newEnvoyFilterTestEngine(), artifact, binary,
		EnvoyFilterConfig{AdvertiseAddress: envoyFilterTestAdvertise, CAPEM: []byte(envoyFilterTestCA)}, "fail")
	require.NoError(t, err)
	assert.NotContains(t, ef.GetLabels(), "istio.io/rev")
}

func TestBuildEnvoyFilterDefaultAssetPort(t *testing.T) {
	artifact, _ := newEnvoyFilterTestArtifact(t)
	ef, err := buildEnvoyFilter(newEnvoyFilterTestEngine(), artifact,
		render.BinaryRef{URL: "https://assets.example.com/plugin.wasm"},
		EnvoyFilterConfig{AdvertiseAddress: envoyFilterTestAdvertise, CAPEM: []byte(envoyFilterTestCA)}, "fail")
	require.NoError(t, err)

	cluster := nestedMap(t, asMap(t, nestedSlice(t, ef.Object, "spec", "configPatches")[0]), "patch", "value")
	socket := socketAddress(t, cluster)
	assert.Equal(t, "assets.example.com", socket["address"])
	assert.EqualValues(t, 443, socket["port_value"])
	assert.Equal(t, "assets.example.com", nestedString(t, cluster, "transport_socket", "typed_config", "sni"))
}

func TestBuildEnvoyFilterErrors(t *testing.T) {
	artifact, binary := newEnvoyFilterTestArtifact(t)
	engine := newEnvoyFilterTestEngine()
	valid := EnvoyFilterConfig{AdvertiseAddress: envoyFilterTestAdvertise, CAPEM: []byte(envoyFilterTestCA)}

	t.Run("non-https binary url", func(t *testing.T) {
		_, err := buildEnvoyFilter(engine, artifact,
			render.BinaryRef{URL: "http://" + envoyFilterTestAssetHost + ":18080/plugin.wasm"}, valid, "fail")
		require.Error(t, err)
	})

	t.Run("binary url without host", func(t *testing.T) {
		_, err := buildEnvoyFilter(engine, artifact, render.BinaryRef{URL: "https:///plugin.wasm"}, valid, "fail")
		require.Error(t, err)
	})

	t.Run("binary url with zero port", func(t *testing.T) {
		_, err := buildEnvoyFilter(engine, artifact,
			render.BinaryRef{URL: "https://" + envoyFilterTestAssetHost + ":0/plugin.wasm"}, valid, "fail")
		require.Error(t, err)
	})

	t.Run("binary url with out-of-range port", func(t *testing.T) {
		_, err := buildEnvoyFilter(engine, artifact,
			render.BinaryRef{URL: "https://" + envoyFilterTestAssetHost + ":99999/plugin.wasm"}, valid, "fail")
		require.Error(t, err)
	})

	t.Run("empty CA", func(t *testing.T) {
		cfg := valid
		cfg.CAPEM = nil
		_, err := buildEnvoyFilter(engine, artifact, binary, cfg, "fail")
		require.Error(t, err)
	})

	t.Run("empty advertise address", func(t *testing.T) {
		cfg := valid
		cfg.AdvertiseAddress = ""
		_, err := buildEnvoyFilter(engine, artifact, binary, cfg, "fail")
		require.Error(t, err)
	})

	t.Run("nil workload selector (non-gateway target)", func(t *testing.T) {
		nonGateway := newEnvoyFilterTestEngine()
		nonGateway.Spec.Target.Type = ""
		_, err := buildEnvoyFilter(nonGateway, artifact, binary, valid, "fail")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no workload selector")
	})

	t.Run("nil workload selector (empty gateway name)", func(t *testing.T) {
		unnamed := newEnvoyFilterTestEngine()
		unnamed.Spec.Target.Name = ""
		_, err := buildEnvoyFilter(unnamed, artifact, binary, valid, "fail")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no workload selector")
	})

	t.Run("nil workload selector (invalid gateway name)", func(t *testing.T) {
		invalid := newEnvoyFilterTestEngine()
		invalid.Spec.Target.Name = "Invalid_Gateway"
		_, err := buildEnvoyFilter(invalid, artifact, binary, valid, "fail")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no workload selector")
	})
}

func TestBuildEnvoyFilterDeterminism(t *testing.T) {
	artifact, binary := newEnvoyFilterTestArtifact(t)
	engine := newEnvoyFilterTestEngine()
	cfg := EnvoyFilterConfig{
		AdvertiseAddress: envoyFilterTestAdvertise,
		CAPEM:            []byte(envoyFilterTestCA),
		IstioRevision:    "1-30-3",
	}

	first, err := buildEnvoyFilter(engine, artifact, binary, cfg, "fail")
	require.NoError(t, err)
	second, err := buildEnvoyFilter(engine, artifact, binary, cfg, "fail")
	require.NoError(t, err)

	assert.Equal(t, first.Object, second.Object)
	assert.Empty(t, first.GetResourceVersion())
	assert.True(t, first.GetCreationTimestamp().Time.IsZero())
}

func TestProtoToMap(t *testing.T) {
	tlsContext, err := anypb.New(&tlsv3.UpstreamTlsContext{
		Sni: "assets.example.com",
		CommonTlsContext: &tlsv3.CommonTlsContext{
			TlsParams: &tlsv3.TlsParameters{
				TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
			},
		},
	})
	require.NoError(t, err)

	got, err := protoToMap(&corev3.TransportSocket{
		Name: "envoy.transport_sockets.tls",
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: tlsContext,
		},
	})
	require.NoError(t, err)

	// Proto field names survive as Envoy snake_case keys, enums render as
	// their proto names, and the Any expands to its "@type" URL inline.
	assert.Equal(t, map[string]any{
		"name": "envoy.transport_sockets.tls",
		"typed_config": map[string]any{
			"@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext",
			"sni":   "assets.example.com",
			"common_tls_context": map[string]any{
				"tls_params": map[string]any{
					"tls_minimum_protocol_version": "TLSv1_3",
				},
			},
		},
	}, got)

	// protojson's byte output is deliberately unstable; the decoded map
	// must still be deterministic.
	again, err := protoToMap(&corev3.TransportSocket{
		Name: "envoy.transport_sockets.tls",
		ConfigType: &corev3.TransportSocket_TypedConfig{
			TypedConfig: tlsContext,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

// -----------------------------------------------------------------------------
// Unstructured navigation helpers
// -----------------------------------------------------------------------------

func asMap(t *testing.T, value any) map[string]any {
	t.Helper()
	m, ok := value.(map[string]any)
	require.True(t, ok, "value is not a map: %T", value)
	return m
}

func nestedMap(t *testing.T, obj map[string]any, fields ...string) map[string]any {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "missing field %v", fields)
	return asMap(t, value)
}

func nestedSlice(t *testing.T, obj map[string]any, fields ...string) []any {
	t.Helper()
	value, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "missing field %v", fields)
	s, ok := value.([]any)
	require.True(t, ok, "field %v is not a slice: %T", fields, value)
	return s
}

func nestedString(t *testing.T, obj map[string]any, fields ...string) string {
	t.Helper()
	value, found, err := unstructured.NestedString(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "missing field %v", fields)
	return value
}

func nestedBool(t *testing.T, obj map[string]any, fields ...string) bool {
	t.Helper()
	value, found, err := unstructured.NestedBool(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "missing field %v", fields)
	return value
}
