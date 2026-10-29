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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	grpccredentialv3 "github.com/envoyproxy/go-control-plane/envoy/config/grpc_credential/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	rbachttpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
)

// -----------------------------------------------------------------------------
// Engine Controller - ECDS EnvoyFilter
// -----------------------------------------------------------------------------

const (
	// wasmAssetCluster is the Envoy cluster that serves the raw WASM binary.
	wasmAssetCluster = "coraza_wasm"
	// ecdsStatPrefix is the gRPC stat prefix for the ECDS config source.
	ecdsStatPrefix = "coraza_ecds"
	// assetFetchTimeout bounds the WASM binary download.
	assetFetchTimeout = 10 * time.Second
	// gatewayServiceAccountTokenPath is the projected token Envoy sends to
	// authenticate ECDS calls.
	gatewayServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // path, not a credential
	// googleGrpcChannelReceiveLimit is the gRPC receive limit for ECDS
	// responses carrying a full rule payload.
	googleGrpcChannelReceiveLimit = 16777216

	wasmFilterTypeURL = "type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm"
	rbacFilterTypeURL = "type.googleapis.com/envoy.extensions.filters.http.rbac.v3.RBAC"
	// fileBasedMetadataFactory selects the GoogleGrpc credentials factory that
	// understands from_plugin call credentials; the default factory ignores them.
	fileBasedMetadataFactory = "envoy.grpc_credentials.file_based_metadata"
)

// envoyFilterGVK is the group/version/kind of the ECDS EnvoyFilter.
var envoyFilterGVK = schema.GroupVersionKind{
	Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter",
}

// ECDSConfig carries the operator-side ECDS endpoint settings.
type ECDSConfig struct {
	// AdvertiseAddress is the operator Service address Envoy dials for ECDS.
	AdvertiseAddress string
	// ListenPort is the effective TLS listener port used by the Engine
	// NetworkPolicy. Zero means the default.
	ListenPort int32
	// CAPEM is the public CA PEM the Gateway uses to verify the operator
	// certificate.
	CAPEM []byte
}

// EnvoyFilterConfig carries the operator-side settings of the ECDS attachment.
type EnvoyFilterConfig struct {
	// AdvertiseAddress is "<operator-service>.<ns>.svc.cluster.local:18080" and
	// becomes the GoogleGrpc target_uri.
	AdvertiseAddress string
	// CAPEM is the public CA PEM injected inline for server verification.
	CAPEM []byte
	// IstioRevision is the optional istio.io/rev label value.
	IstioRevision string
}

// envoyFilterName returns the EnvoyFilter name for an Engine.
func envoyFilterName(engineName string) string {
	return "coraza-engine-" + engineName
}

// buildEnvoyFilter returns the deterministic desired EnvoyFilter for one
// Engine. It is pure: no client, no reads, no timestamps.
func buildEnvoyFilter(
	engine *wafv1alpha1.Engine,
	artifact render.Artifact,
	binary render.BinaryRef,
	cfg EnvoyFilterConfig,
	bootstrapPolicy string,
) (*unstructured.Unstructured, error) {
	if engine == nil {
		return nil, errors.New("engine is required")
	}

	assetURL, err := url.Parse(binary.URL)
	if err != nil {
		return nil, fmt.Errorf("parse wasm binary url: %w", err)
	}
	if assetURL.Scheme != "https" || assetURL.Host == "" {
		return nil, fmt.Errorf("wasm binary url %q must be an absolute https URL with a host", binary.URL)
	}
	if len(cfg.CAPEM) == 0 {
		return nil, errors.New("CA PEM is required")
	}
	if cfg.AdvertiseAddress == "" {
		return nil, errors.New("advertise address is required")
	}

	assetHost := assetURL.Hostname()
	assetPort := assetURL.Port()
	if assetPort == "" {
		assetPort = "443"
	}
	portValue, err := strconv.ParseUint(assetPort, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("parse wasm binary port %q: %w", assetPort, err)
	}
	if portValue == 0 {
		return nil, fmt.Errorf("parse wasm binary port %q: port must be non-zero", assetPort)
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "coraza-kubernetes-operator",
		engineNameLabel:                engine.Name,
	}
	if cfg.IstioRevision != "" {
		labels["istio.io/rev"] = cfg.IstioRevision
	}

	annotations := map[string]string{
		"waf.k8s.coraza.io/configuration-version": artifact.Version,
		"waf.k8s.coraza.io/bootstrap-policy":      bootstrapPolicy,
		"waf.k8s.coraza.io/ca-hash":               caHash(cfg.CAPEM),
	}

	// Fail closed: Istio treats an empty or absent workload selector as
	// applying to every workload in the namespace.
	ws := targetLabelSelector(engine)
	if ws == nil || len(ws.MatchLabels) == 0 {
		return nil, fmt.Errorf("target type %q name %q yields no workload selector", engine.Spec.Target.Type, engine.Spec.Target.Name)
	}
	matchLabels := ws.MatchLabels

	caPEM := string(cfg.CAPEM)

	// Patch 1: the WASM asset cluster.
	// shortcut: EnvoyGrpc fallback cluster omitted until spike S2 shows GoogleGrpc is unavailable; add coraza_ecds here if it does.
	assetCluster, err := assetClusterValue(assetHost, portValue, caPEM)
	if err != nil {
		return nil, fmt.Errorf("render asset cluster: %w", err)
	}
	assetClusterPatch := map[string]any{
		"applyTo": "CLUSTER",
		"match": map[string]any{
			"context": "GATEWAY",
		},
		"patch": map[string]any{
			"operation": "ADD",
			"value":     assetCluster,
		},
	}

	// Patch 2: the ECDS HTTP filter, inserted before the router. The filter
	// name is the ECDS resource name and must match the served resource.
	httpFilter, err := httpFilterValue(artifact.ResourceName, cfg.AdvertiseAddress, bootstrapPolicy, caPEM)
	if err != nil {
		return nil, fmt.Errorf("render ECDS HTTP filter: %w", err)
	}
	httpFilterPatch := map[string]any{
		"applyTo": "HTTP_FILTER",
		"match": map[string]any{
			"context": "GATEWAY",
			"listener": map[string]any{
				"filterChain": map[string]any{
					"filter": map[string]any{
						"name": "envoy.filters.network.http_connection_manager",
						"subFilter": map[string]any{
							"name": "envoy.filters.http.router",
						},
					},
				},
			},
		},
		"patch": map[string]any{
			"operation": "INSERT_BEFORE",
			"value":     httpFilter,
		},
	}

	envoyFilter := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.istio.io/v1alpha3",
			"kind":       "EnvoyFilter",
			"metadata": map[string]any{
				"name":      envoyFilterName(engine.Name),
				"namespace": engine.Namespace,
			},
			"spec": map[string]any{
				"workloadSelector": map[string]any{
					"labels": toAnyMap(matchLabels),
				},
				"configPatches": []any{
					assetClusterPatch,
					httpFilterPatch,
				},
			},
		},
	}

	envoyFilter.SetLabels(labels)
	envoyFilter.SetAnnotations(annotations)
	envoyFilter.SetGroupVersionKind(envoyFilterGVK)

	return envoyFilter, nil
}

// assetClusterValue returns the patch value of the STRICT_DNS cluster that
// serves the raw WASM binary, dialed over TLS with the operator CA.
func assetClusterValue(assetHost string, portValue uint64, caPEM string) (map[string]any, error) {
	// Envoy's upstream TLS defaults to a TLS 1.2 maximum; the operator
	// listener requires TLS 1.3.
	tlsContext, err := anypb.New(&tlsv3.UpstreamTlsContext{
		Sni: assetHost,
		CommonTlsContext: &tlsv3.CommonTlsContext{
			TlsParams: &tlsv3.TlsParameters{
				TlsMinimumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
				TlsMaximumProtocolVersion: tlsv3.TlsParameters_TLSv1_3,
			},
			ValidationContextType: &tlsv3.CommonTlsContext_ValidationContext{
				ValidationContext: &tlsv3.CertificateValidationContext{
					TrustedCa: &corev3.DataSource{
						Specifier: &corev3.DataSource_InlineString{InlineString: caPEM},
					},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("wrap upstream TLS context: %w", err)
	}

	return protoToMap(&clusterv3.Cluster{
		Name:                 wasmAssetCluster,
		ClusterDiscoveryType: &clusterv3.Cluster_Type{Type: clusterv3.Cluster_STRICT_DNS},
		ConnectTimeout:       durationpb.New(assetFetchTimeout),
		LoadAssignment: &endpointv3.ClusterLoadAssignment{
			ClusterName: wasmAssetCluster,
			Endpoints: []*endpointv3.LocalityLbEndpoints{{
				LbEndpoints: []*endpointv3.LbEndpoint{{
					HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
						Endpoint: &endpointv3.Endpoint{
							Address: &corev3.Address{
								Address: &corev3.Address_SocketAddress{
									SocketAddress: &corev3.SocketAddress{
										Address: assetHost,
										PortSpecifier: &corev3.SocketAddress_PortValue{
											PortValue: uint32(portValue), //nolint:gosec // bounded by ParseUint(_, 10, 16) above
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
		TransportSocket: &corev3.TransportSocket{
			Name: "envoy.transport_sockets.tls",
			ConfigType: &corev3.TransportSocket_TypedConfig{
				TypedConfig: tlsContext,
			},
		},
	})
}

// httpFilterValue returns the patch value of the ECDS HTTP filter: a config
// discovery against the operator's GoogleGrpc endpoint with the bootstrap RBAC
// default, inserted before the router.
func httpFilterValue(resourceName, advertiseAddress, bootstrapPolicy, caPEM string) (map[string]any, error) {
	tokenCredential, err := anypb.New(&grpccredentialv3.FileBasedMetadataConfig{
		SecretData: &corev3.DataSource{
			Specifier: &corev3.DataSource_Filename{Filename: gatewayServiceAccountTokenPath},
		},
		HeaderKey:    "authorization",
		HeaderPrefix: "Bearer ",
	})
	if err != nil {
		return nil, fmt.Errorf("wrap token credential: %w", err)
	}

	bootstrapDefault, err := anypb.New(&rbachttpv3.RBAC{
		Rules: &rbacv3.RBAC{
			// Fail closed: ALLOW/DENY with no policies denies (respectively
			// allows) every request until the first ECDS push lands. proto3
			// JSON omits the zero-valued ALLOW action and the empty policies
			// map; both parse back identically in Envoy.
			Action:   bootstrapRBACActionEnum(bootstrapPolicy),
			Policies: map[string]*rbacv3.Policy{},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("wrap bootstrap RBAC default: %w", err)
	}

	return protoToMap(&hcmv3.HttpFilter{
		Name: resourceName,
		ConfigType: &hcmv3.HttpFilter_ConfigDiscovery{
			ConfigDiscovery: &corev3.ExtensionConfigSource{
				ConfigSource: &corev3.ConfigSource{
					ConfigSourceSpecifier: &corev3.ConfigSource_ApiConfigSource{
						ApiConfigSource: &corev3.ApiConfigSource{
							ApiType:             corev3.ApiConfigSource_GRPC,
							TransportApiVersion: corev3.ApiVersion_V3,
							GrpcServices: []*corev3.GrpcService{{
								TargetSpecifier: &corev3.GrpcService_GoogleGrpc_{
									GoogleGrpc: ecdsGoogleGrpc(advertiseAddress, caPEM, tokenCredential),
								},
							}},
						},
					},
					ResourceApiVersion: corev3.ApiVersion_V3,
				},
				TypeUrls:                         []string{wasmFilterTypeURL, rbacFilterTypeURL},
				ApplyDefaultConfigWithoutWarming: true,
				DefaultConfig:                    bootstrapDefault,
			},
		},
	})
}

// ecdsGoogleGrpc returns the GoogleGrpc service Envoy uses to reach the
// operator: TLS to the advertised address trusting the inline CA, plus the
// projected service-account token as from_plugin call credentials.
func ecdsGoogleGrpc(advertiseAddress, caPEM string, tokenCredential *anypb.Any) *corev3.GrpcService_GoogleGrpc {
	return &corev3.GrpcService_GoogleGrpc{
		TargetUri:  advertiseAddress,
		StatPrefix: ecdsStatPrefix,
		// Without the file-based-metadata factory the default credentials
		// factory silently drops the from_plugin token.
		CredentialsFactoryName: fileBasedMetadataFactory,
		ChannelArgs: &corev3.GrpcService_GoogleGrpc_ChannelArgs{
			Args: map[string]*corev3.GrpcService_GoogleGrpc_ChannelArgs_Value{
				"grpc.max_receive_message_length": {
					ValueSpecifier: &corev3.GrpcService_GoogleGrpc_ChannelArgs_Value_IntValue{
						IntValue: googleGrpcChannelReceiveLimit,
					},
				},
			},
		},
		ChannelCredentials: &corev3.GrpcService_GoogleGrpc_ChannelCredentials{
			CredentialSpecifier: &corev3.GrpcService_GoogleGrpc_ChannelCredentials_SslCredentials{
				SslCredentials: &corev3.GrpcService_GoogleGrpc_SslCredentials{
					RootCerts: &corev3.DataSource{
						Specifier: &corev3.DataSource_InlineString{InlineString: caPEM},
					},
				},
			},
		},
		CallCredentials: []*corev3.GrpcService_GoogleGrpc_CallCredentials{{
			CredentialSpecifier: &corev3.GrpcService_GoogleGrpc_CallCredentials_FromPlugin{
				FromPlugin: &corev3.GrpcService_GoogleGrpc_CallCredentials_MetadataCredentialsFromPlugin{
					Name: fileBasedMetadataFactory,
					ConfigType: &corev3.GrpcService_GoogleGrpc_CallCredentials_MetadataCredentialsFromPlugin_TypedConfig{
						TypedConfig: tokenCredential,
					},
				},
			},
		}},
	}
}

// protoToMap converts a typed Envoy message into the map form Istio expects
// in an EnvoyFilter patch value. protojson with proto field names keeps the
// Envoy snake_case keys and expands Any values to their "@type" form. The
// decoded map, not protojson's byte output (which is deliberately unstable),
// is what makes the result deterministic.
func protoToMap(m proto.Message) (map[string]any, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// caHash returns the sha256:<lowercase-hex> digest of the CA PEM.
func caHash(caPEM []byte) string {
	sum := sha256.Sum256(caPEM)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// toAnyMap converts a string map into the map[string]any form the
// unstructured object expects.
func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
