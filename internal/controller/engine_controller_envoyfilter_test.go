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
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
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

func TestEnvoyFilterMatchesRevisionAndRetainsForeignMetadata(t *testing.T) {
	artifact, binary := newEnvoyFilterTestArtifact(t)
	desired, err := buildEnvoyFilter(newEnvoyFilterTestEngine(), artifact, binary, EnvoyFilterConfig{
		AdvertiseAddress: envoyFilterTestAdvertise,
		CAPEM:            []byte(envoyFilterTestCA),
		IstioRevision:    "revision-2",
	}, "fail")
	require.NoError(t, err)

	existing := desired.DeepCopy()
	labels := existing.GetLabels()
	labels["example.com/retained"] = "label"
	existing.SetLabels(labels)
	annotations := existing.GetAnnotations()
	annotations["example.com/retained"] = "annotation"
	existing.SetAnnotations(annotations)

	assert.True(t, envoyFilterMatches(existing, desired), "foreign metadata should not trigger an apply")
	assert.Equal(t, "label", existing.GetLabels()["example.com/retained"])
	assert.Equal(t, "annotation", existing.GetAnnotations()["example.com/retained"])

	staleRevision := existing.DeepCopy()
	labels = staleRevision.GetLabels()
	labels["istio.io/rev"] = "revision-1"
	staleRevision.SetLabels(labels)
	assert.False(t, envoyFilterMatches(staleRevision, desired), "a stale revision must be updated")

	desiredWithoutRevision := desired.DeepCopy()
	labels = desiredWithoutRevision.GetLabels()
	delete(labels, "istio.io/rev")
	desiredWithoutRevision.SetLabels(labels)
	assert.False(t, envoyFilterMatches(existing, desiredWithoutRevision), "a removed desired revision must be removed from the object")

	existingWithoutRevision := existing.DeepCopy()
	labels = existingWithoutRevision.GetLabels()
	delete(labels, "istio.io/rev")
	existingWithoutRevision.SetLabels(labels)
	assert.True(t, envoyFilterMatches(existingWithoutRevision, desiredWithoutRevision))

	deleting := existing.DeepCopy()
	deletionTimestamp := metav1.Now()
	deleting.SetDeletionTimestamp(&deletionTimestamp)
	assert.False(t, envoyFilterMatches(deleting, desired), "a deleting EnvoyFilter must be recreated")
}

func TestEnvoyFilterMatchesOmittedRevisionByManagedFields(t *testing.T) {
	artifact, binary := newEnvoyFilterTestArtifact(t)
	desired, err := buildEnvoyFilter(newEnvoyFilterTestEngine(), artifact, binary, EnvoyFilterConfig{
		AdvertiseAddress: envoyFilterTestAdvertise,
		CAPEM:            []byte(envoyFilterTestCA),
	}, "fail")
	require.NoError(t, err)

	const revisionFieldSet = `{"f:metadata":{"f:labels":{"f:istio.io/rev":{}}}}`
	const foreignManager = "foreign-revision-controller"
	managedBy := func(manager string, operation metav1.ManagedFieldsOperationType) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{
			Manager:    manager,
			Operation:  operation,
			APIVersion: envoyFilterGVK.GroupVersion().String(),
			FieldsType: "FieldsV1",
			FieldsV1:   &metav1.FieldsV1{Raw: []byte(revisionFieldSet)},
		}
	}
	operatorOwner := managedBy(fieldManager, metav1.ManagedFieldsOperationApply)
	foreignOwner := managedBy(foreignManager, metav1.ManagedFieldsOperationApply)
	statusOwner := operatorOwner
	statusOwner.Subresource = "status"

	tests := []struct {
		name      string
		managed   []metav1.ManagedFieldsEntry
		wantMatch bool
	}{
		{name: "missing managed fields", wantMatch: false},
		{name: "empty managed fields", managed: []metav1.ManagedFieldsEntry{}, wantMatch: false},
		{
			name: "missing fieldsV1",
			managed: []metav1.ManagedFieldsEntry{{
				Manager:    foreignManager,
				Operation:  metav1.ManagedFieldsOperationApply,
				APIVersion: envoyFilterGVK.GroupVersion().String(),
				FieldsType: "FieldsV1",
			}},
			wantMatch: false,
		},
		{
			name: "invalid fieldsV1",
			managed: []metav1.ManagedFieldsEntry{{
				Manager:    foreignManager,
				Operation:  metav1.ManagedFieldsOperationApply,
				APIVersion: envoyFilterGVK.GroupVersion().String(),
				FieldsType: "FieldsV1",
				FieldsV1:   &metav1.FieldsV1{Raw: []byte("[]")},
			}},
			wantMatch: false,
		},
		{name: "foreign owner", managed: []metav1.ManagedFieldsEntry{foreignOwner}, wantMatch: true},
		{name: "operator owner", managed: []metav1.ManagedFieldsEntry{operatorOwner}, wantMatch: false},
		{name: "shared owners", managed: []metav1.ManagedFieldsEntry{foreignOwner, operatorOwner}, wantMatch: false},
		{
			name: "operator update manager is distinct from apply manager",
			managed: []metav1.ManagedFieldsEntry{
				managedBy(fieldManager, metav1.ManagedFieldsOperationUpdate),
			},
			wantMatch: true,
		},
		{
			name:      "operator status subresource is distinct from main resource",
			managed:   []metav1.ManagedFieldsEntry{statusOwner},
			wantMatch: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			existing := desired.DeepCopy()
			labels := existing.GetLabels()
			labels[istioRevisionLabel] = "foreign-revision"
			existing.SetLabels(labels)
			existing.SetManagedFields(tc.managed)

			assert.Equal(t, tc.wantMatch, envoyFilterMatches(existing, desired))
		})
	}
}

func TestRenderEngineArtifactReturnsMetadataOnlyAndRechecksSizes(t *testing.T) {
	engine := newEnvoyFilterTestEngine()
	content := assemble.Content{Rules: "SecRuleEngine On"}
	contentHash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)
	input := render.EngineInput{
		Namespace: engine.Namespace,
		Name:      engine.Name,
		UID:       engine.UID,
		RuleSet: render.RuleSetAccepted{
			UID:         types.UID("22222222-2222-4222-8222-222222222222"),
			ContentHash: contentHash,
		},
		Content: content,
		Binary: render.BinaryRef{
			URL:    envoyFilterTestBinaryURL,
			SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
	}
	engineKey := types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}
	reconciler := &EngineReconciler{Limits: xds.Limits{MaxMessageBytes: 1}}

	_, failureMessage, err := reconciler.renderEngineArtifact(engineKey, input)
	require.Error(t, err)
	assert.Contains(t, failureMessage, "ECDS response size")
	_, cached := reconciler.renderCache.get(engineKey, engineRenderCacheKeyFor(input))
	assert.False(t, cached, "an oversized miss must not populate the cache")

	reconciler.Limits = xds.Limits{}
	first, failureMessage, err := reconciler.renderEngineArtifact(engineKey, input)
	require.NoError(t, err, failureMessage)
	assert.Nil(t, first.Payload)
	assert.Nil(t, first.Resource)

	entry, cached := reconciler.renderCache.get(engineKey, engineRenderCacheKeyFor(input))
	require.True(t, cached)
	assert.Greater(t, entry.payloadSize, 0)
	assert.Greater(t, entry.resourceSize, 0)
	assert.Nil(t, entry.artifact.Payload)
	assert.Nil(t, entry.artifact.Resource)

	reconciler.Limits = xds.Limits{MaxMessageBytes: 1}
	_, failureMessage, err = reconciler.renderEngineArtifact(engineKey, input)
	require.Error(t, err)
	assert.Contains(t, failureMessage, "ECDS response size")

	reconciler.Limits = xds.Limits{}
	cachedArtifact, failureMessage, err := reconciler.renderEngineArtifact(engineKey, input)
	require.NoError(t, err, failureMessage)
	assert.Nil(t, cachedArtifact.Payload)
	assert.Nil(t, cachedArtifact.Resource)
	assert.Equal(t, first.Version, cachedArtifact.Version)
}

func TestEngineRenderCacheKeyInvalidatesRenderingInputs(t *testing.T) {
	engine := newEnvoyFilterTestEngine()
	input := render.EngineInput{
		Namespace: engine.Namespace,
		Name:      engine.Name,
		UID:       engine.UID,
		RuleSet: render.RuleSetAccepted{
			UID:         types.UID("22222222-2222-4222-8222-222222222222"),
			ContentHash: "sha256:" + strings.Repeat("a", 64),
		},
		Content:               assemble.Content{Rules: "SecRuleEngine On"},
		EmptyRulesPolicy:      "Reject",
		FailurePolicy:         "fail",
		DriverType:            "wasm",
		Binary:                render.BinaryRef{URL: envoyFilterTestBinaryURL, SHA256: strings.Repeat("c", 64), CRSVersion: "v4.30.0"},
		EnableFilterStateLogs: false,
	}
	engineKey := types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}
	cache := engineRenderCache{}
	cache.set(engineKey, engineRenderCacheEntry{key: engineRenderCacheKeyFor(input)})
	if _, found := cache.get(engineKey, engineRenderCacheKeyFor(input)); !found {
		t.Fatal("the original rendering inputs should hit the cache")
	}

	tests := []struct {
		name   string
		mutate func(*render.EngineInput)
	}{
		{name: "namespace", mutate: func(in *render.EngineInput) { in.Namespace = "different-namespace" }},
		{name: "name", mutate: func(in *render.EngineInput) { in.Name = "different-engine" }},
		{name: "Engine UID", mutate: func(in *render.EngineInput) { in.UID = types.UID("44444444-4444-4444-8444-444444444444") }},
		{name: "binary URL", mutate: func(in *render.EngineInput) { in.Binary.URL += "/other" }},
		{name: "binary digest", mutate: func(in *render.EngineInput) { in.Binary.SHA256 = strings.Repeat("d", 64) }},
		{name: "CRS capability", mutate: func(in *render.EngineInput) { in.Binary.CRSVersion = "none" }},
		{name: "empty rules policy", mutate: func(in *render.EngineInput) { in.EmptyRulesPolicy = "Allow" }},
		{name: "failure policy", mutate: func(in *render.EngineInput) { in.FailurePolicy = "allow" }},
		{name: "observability", mutate: func(in *render.EngineInput) { in.EnableFilterStateLogs = true }},
		{name: "driver type", mutate: func(in *render.EngineInput) { in.DriverType = "other" }},
		{
			name: "VM settings",
			mutate: func(in *render.EngineInput) {
				in.VMSettings.Runtime = "envoy.wasm.runtime.wasmtime"
			},
		},
		{name: "RuleSet UID", mutate: func(in *render.EngineInput) { in.RuleSet.UID = types.UID("33333333-3333-4333-8333-333333333333") }},
		{name: "accepted content hash", mutate: func(in *render.EngineInput) { in.RuleSet.ContentHash = "sha256:" + strings.Repeat("b", 64) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := input
			tc.mutate(&changed)
			if _, found := cache.get(engineKey, engineRenderCacheKeyFor(changed)); found {
				t.Fatal("changed rendering inputs must invalidate the cached artifact")
			}
		})
	}
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

			assert.Equal(t, []any{
				map[string]any{
					"group": "gateway.networking.k8s.io",
					"kind":  "Gateway",
					"name":  "gw",
				},
			}, nestedSlice(t, ef.Object, "spec", "targetRefs"))
			assert.NotContains(t, asMap(t, ef.Object["spec"]), "workloadSelector")

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
			// The asset channel must not accept any CA-signed certificate:
			// the serving certificate has to carry the asset host as a SAN.
			// DNS uses an anchored regex, not an exact StringMatcher: Envoy
			// resolves DNS exact matches with RFC 6125 semantics, where a
			// wildcard SAN covers the configured host.
			sanMatchers := nestedSlice(t, cluster, "transport_socket", "typed_config",
				"common_tls_context", "validation_context", "match_typed_subject_alt_names")
			require.Len(t, sanMatchers, 1)
			sanMatcher := asMap(t, sanMatchers[0])
			assert.Equal(t, "DNS", nestedString(t, sanMatcher, "san_type"))
			regex := nestedString(t, sanMatcher, "matcher", "safe_regex", "regex")
			assert.Equal(t, "^(?i:"+regexp.QuoteMeta(envoyFilterTestAssetHost)+")$", regex)
			// The anchored literal must not match a wildcard SAN that would
			// cover the host under RFC 6125.
			mustNotMatch := []string{
				"*." + strings.Join(strings.Split(envoyFilterTestAssetHost, ".")[1:], "."),
				"*." + envoyFilterTestAssetHost,
				envoyFilterTestAssetHost + ".evil.example",
			}
			compiled, err := regexp.Compile(regex)
			require.NoError(t, err)
			for _, san := range mustNotMatch {
				assert.False(t, compiled.MatchString(san), "wildcard or suffix SAN %q must not satisfy the asset host matcher", san)
			}
			// DNS names are case-insensitive: a mixed-case SAN must match.
			assert.True(t, compiled.MatchString(strings.ToUpper(envoyFilterTestAssetHost)),
				"a mixed-case SAN must satisfy the asset host matcher")

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
			assert.Equal(t, true, apiConfig["set_node_on_first_message_only"])
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

// An IP-literal asset host must use the IP SAN type, which matches the
// address text instead of a DNS entry.
func TestBuildEnvoyFilterAssetHostIPUsesIPSAN(t *testing.T) {
	artifact, _ := newEnvoyFilterTestArtifact(t)
	ef, err := buildEnvoyFilter(newEnvoyFilterTestEngine(), artifact,
		render.BinaryRef{URL: "https://192.0.2.10/plugin.wasm"},
		EnvoyFilterConfig{AdvertiseAddress: envoyFilterTestAdvertise, CAPEM: []byte(envoyFilterTestCA)}, "fail")
	require.NoError(t, err)

	cluster := nestedMap(t, asMap(t, nestedSlice(t, ef.Object, "spec", "configPatches")[0]), "patch", "value")
	sanMatchers := nestedSlice(t, cluster, "transport_socket", "typed_config",
		"common_tls_context", "validation_context", "match_typed_subject_alt_names")
	require.Len(t, sanMatchers, 1)
	sanMatcher := asMap(t, sanMatchers[0])
	assert.Equal(t, "IP_ADDRESS", nestedString(t, sanMatcher, "san_type"))
	assert.Equal(t, "192.0.2.10", nestedString(t, sanMatcher, "matcher", "exact"))
}

// A non-canonical IP literal in the URL must match the canonical form the
// certificate's IP SAN carries.
func TestBuildEnvoyFilterAssetHostIPv6NormalizesSAN(t *testing.T) {
	artifact, _ := newEnvoyFilterTestArtifact(t)
	ef, err := buildEnvoyFilter(newEnvoyFilterTestEngine(), artifact,
		render.BinaryRef{URL: "https://[0:0:0:0:0:0:0:1]/plugin.wasm"},
		EnvoyFilterConfig{AdvertiseAddress: envoyFilterTestAdvertise, CAPEM: []byte(envoyFilterTestCA)}, "fail")
	require.NoError(t, err)

	cluster := nestedMap(t, asMap(t, nestedSlice(t, ef.Object, "spec", "configPatches")[0]), "patch", "value")
	sanMatchers := nestedSlice(t, cluster, "transport_socket", "typed_config",
		"common_tls_context", "validation_context", "match_typed_subject_alt_names")
	require.Len(t, sanMatchers, 1)
	sanMatcher := asMap(t, sanMatchers[0])
	assert.Equal(t, "IP_ADDRESS", nestedString(t, sanMatcher, "san_type"))
	assert.Equal(t, "::1", nestedString(t, sanMatcher, "matcher", "exact"))
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

	t.Run("invalid Gateway target type", func(t *testing.T) {
		nonGateway := newEnvoyFilterTestEngine()
		nonGateway.Spec.Target.Type = ""
		_, err := buildEnvoyFilter(nonGateway, artifact, binary, valid, "fail")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no Gateway target reference")
	})

	t.Run("empty Gateway name", func(t *testing.T) {
		unnamed := newEnvoyFilterTestEngine()
		unnamed.Spec.Target.Name = ""
		_, err := buildEnvoyFilter(unnamed, artifact, binary, valid, "fail")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no Gateway target reference")
	})

	t.Run("invalid Gateway name", func(t *testing.T) {
		invalid := newEnvoyFilterTestEngine()
		invalid.Spec.Target.Name = "Invalid_Gateway"
		_, err := buildEnvoyFilter(invalid, artifact, binary, valid, "fail")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no Gateway target reference")
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

func TestBootstrapPolicy(t *testing.T) {
	tests := []struct {
		name           string
		previous       string
		desired        string
		acceptedActive bool
		want           string
	}{
		{name: "fail stays fail", previous: "fail", desired: "fail", acceptedActive: false, want: "fail"},
		{name: "fail stays fail when accepted", previous: "fail", desired: "fail", acceptedActive: true, want: "fail"},
		{name: "tighten applies immediately", previous: "allow", desired: "fail", acceptedActive: false, want: "fail"},
		{name: "tighten applies immediately when accepted", previous: "allow", desired: "fail", acceptedActive: true, want: "fail"},
		{name: "weaken is deferred without acceptance", previous: "fail", desired: "allow", acceptedActive: false, want: "fail"},
		{name: "weaken applies after acceptance", previous: "fail", desired: "allow", acceptedActive: true, want: "allow"},
		{name: "allow stays allow", previous: "allow", desired: "allow", acceptedActive: false, want: "allow"},
		{name: "allow stays allow when accepted", previous: "allow", desired: "allow", acceptedActive: true, want: "allow"},
		{name: "unknown previous fails closed", previous: "", desired: "allow", acceptedActive: false, want: "fail"},
		{name: "missing previous stays fail-closed when accepted", previous: "", desired: "allow", acceptedActive: true, want: "fail"},
		{name: "unknown desired fails closed", previous: "allow", desired: "", acceptedActive: true, want: "fail"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bootstrapPolicy(tc.previous, tc.desired, tc.acceptedActive))
		})
	}
}

func TestBootstrapRBACAction(t *testing.T) {
	assert.Equal(t, rbacv3.RBAC_ALLOW, bootstrapRBACActionEnum("fail"))
	assert.Equal(t, rbacv3.RBAC_DENY, bootstrapRBACActionEnum("allow"))
	assert.Equal(t, rbacv3.RBAC_ALLOW, bootstrapRBACActionEnum(""), "unknown policies must fail closed")
}

// TestECDSConfigFallsBackToStaticCA pins the precedence rule: a source with no
// loadable bundle must not mask the startup value, and an empty source yields
// nothing so the reconciler reports TLSNotConfigured.
func TestECDSConfigFallsBackToStaticCA(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.crt")

	withStatic := ECDSConfig{
		CAPEM:    []byte(envoyFilterTestCA),
		CASource: xds.NewCASource(missing, logr.Discard()),
	}
	assert.Equal(t, []byte(envoyFilterTestCA), withStatic.caPEM())

	empty := ECDSConfig{CASource: xds.NewCASource(missing, logr.Discard())}
	assert.Nil(t, empty.caPEM())
}
