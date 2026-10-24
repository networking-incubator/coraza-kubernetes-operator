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
	"errors"
	"fmt"
	"net/url"
	"strconv"

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
	assetFetchTimeout = "10s"
	// gatewayServiceAccountTokenPath is the projected token Envoy sends to
	// authenticate ECDS calls.
	gatewayServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// googleGrpcChannelReceiveLimit is the gRPC receive limit for ECDS
	// responses carrying a full rule payload.
	googleGrpcChannelReceiveLimit = 16777216

	upstreamTLSContextTypeURL = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext"
	wasmFilterTypeURL         = "type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm"
	rbacFilterTypeURL         = "type.googleapis.com/envoy.extensions.filters.http.rbac.v3.RBAC"
	fileBasedMetadataTypeURL  = "type.googleapis.com/envoy.config.grpc_credential.v3.FileBasedMetadataConfig"
)

// envoyFilterGVK is the group/version/kind of the ECDS EnvoyFilter.
var envoyFilterGVK = schema.GroupVersionKind{
	Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter",
}

// ECDSConfig carries the operator-side ECDS endpoint settings.
type ECDSConfig struct {
	// AdvertiseAddress is the operator Service address Envoy dials for ECDS.
	AdvertiseAddress string
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
	portValue, err := strconv.ParseInt(assetPort, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("parse wasm binary port %q: %w", assetPort, err)
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

	matchLabels := map[string]string{}
	if ws := targetLabelSelector(engine); ws != nil && ws.MatchLabels != nil {
		matchLabels = ws.MatchLabels
	}

	caPEM := string(cfg.CAPEM)

	// Patch 1: the WASM asset cluster.
	// shortcut: EnvoyGrpc fallback cluster omitted until spike S2 shows GoogleGrpc is unavailable; add coraza_ecds here if it does.
	assetClusterPatch := map[string]any{
		"applyTo": "CLUSTER",
		"match": map[string]any{
			"context": "GATEWAY",
		},
		"patch": map[string]any{
			"operation": "ADD",
			"value": map[string]any{
				"name":            wasmAssetCluster,
				"type":            "STRICT_DNS",
				"connect_timeout": assetFetchTimeout,
				"load_assignment": map[string]any{
					"cluster_name": wasmAssetCluster,
					"endpoints": []any{
						map[string]any{
							"lb_endpoints": []any{
								map[string]any{
									"endpoint": map[string]any{
										"address": map[string]any{
											"socket_address": map[string]any{
												"address":    assetHost,
												"port_value": portValue,
											},
										},
									},
								},
							},
						},
					},
				},
				"transport_socket": map[string]any{
					"name": "envoy.transport_sockets.tls",
					"typed_config": map[string]any{
						"@type": upstreamTLSContextTypeURL,
						"sni":   assetHost,
						"common_tls_context": map[string]any{
							"validation_context": map[string]any{
								"trusted_ca": map[string]any{
									"inline_string": caPEM,
								},
							},
						},
					},
				},
			},
		},
	}

	// Patch 2: the ECDS HTTP filter, inserted before the router. The filter
	// name is the ECDS resource name and must match the served resource.
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
			"value": map[string]any{
				"name": artifact.ResourceName,
				"config_discovery": map[string]any{
					"config_source": map[string]any{
						"api_config_source": map[string]any{
							"api_type":              "GRPC",
							"transport_api_version": "V3",
							"grpc_services": []any{
								map[string]any{
									"google_grpc": map[string]any{
										"target_uri":  cfg.AdvertiseAddress,
										"stat_prefix": ecdsStatPrefix,
										"channel_args": map[string]any{
											"args": map[string]any{
												"grpc.max_receive_message_length": map[string]any{
													"int_value": int64(googleGrpcChannelReceiveLimit),
												},
											},
										},
										"channel_credentials": map[string]any{
											"ssl_credentials": map[string]any{
												"root_certs": map[string]any{
													"inline_string": caPEM,
												},
											},
										},
										"call_credentials": []any{
											map[string]any{
												"from_plugin": map[string]any{
													"name": "envoy.grpc_credentials.file_based_metadata",
													"typed_config": map[string]any{
														"@type":         fileBasedMetadataTypeURL,
														"secret_data":   map[string]any{"filename": gatewayServiceAccountTokenPath},
														"header_key":    "authorization",
														"header_prefix": "Bearer ",
													},
												},
											},
										},
									},
								},
							},
						},
						"resource_api_version": "V3",
					},
					"type_urls": []any{
						wasmFilterTypeURL,
						rbacFilterTypeURL,
					},
					"apply_default_config_without_warming": true,
					"default_config": map[string]any{
						"@type": rbacFilterTypeURL,
						"rules": map[string]any{
							"action":   bootstrapRBACAction(bootstrapPolicy),
							"policies": map[string]any{},
						},
					},
				},
			},
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
