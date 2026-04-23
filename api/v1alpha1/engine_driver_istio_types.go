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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// Engine Driver - Istio Configuration
// -----------------------------------------------------------------------------

// IstioDriverConfig defines Istio-specific integration mechanisms that will be
// used to deploy and manage the Engine with Istio.
//
// Exactly one mode must be specified.
//
// +kubebuilder:validation:XValidation:rule="[has(self.wasm), has(self.dynamicModule)].filter(x, x).size() == 1",message="exactly one integration mechanism (wasm or dynamicModule) must be specified"
// +kubebuilder:validation:MinProperties=0
type IstioDriverConfig struct {
	// wasm configures the Engine to be deployed as a WebAssembly plugin.
	//
	// +optional
	Wasm *IstioWasmConfig `json:"wasm,omitempty,omitzero"`

	// dynamicModule configures the Engine to be deployed as an Envoy dynamic
	// module. This uses the Built On Envoy coraza-waf extension, which is a
	// native shared library (.so) loaded by Envoy at runtime. Rules are
	// embedded inline in an EnvoyFilter resource.
	//
	// +optional
	DynamicModule *IstioDynamicModuleConfig `json:"dynamicModule,omitempty,omitzero"`
}

// -----------------------------------------------------------------------------
// Engine Driver - Istio Wasm Configuration
// -----------------------------------------------------------------------------

// IstioWasmConfig defines configuration for deploying the Engine as a WASM
// plugin with Istio.
//
// +kubebuilder:validation:MinProperties=0
// +kubebuilder:validation:XValidation:rule="self.mode == 'gateway' ? has(self.workloadSelector) : true",message="workloadSelector is required when mode is gateway"
// +kubebuilder:validation:XValidation:rule="!has(self.image) || self.image.matches('^oci://')",message="image must start with oci:// when set"
// +kubebuilder:validation:XValidation:rule="!has(self.image) || size(self.image) <= 1024",message="image must be at most 1024 characters when set"
type IstioWasmConfig struct {
	// mode specifies what mechanism will be used to integrate the WAF with
	// Istio.
	//
	// Currently only supports "Gateway" mode, utilizing Gateway API resources.
	//
	// +optional
	// +default="gateway"
	Mode IstioIntegrationMode `json:"mode,omitempty"`

	// workloadSelector specifies the selection criteria for attaching the WAF to
	// Istio resources.
	//
	// Required when mode is "gateway".
	//
	// +optional
	WorkloadSelector *metav1.LabelSelector `json:"workloadSelector,omitempty,omitzero"`

	// image is the OCI image reference for the Coraza WASM plugin.
	// If omitted the operator uses its configured default WASM OCI reference
	// (--default-wasm-image / CORAZA_DEFAULT_WASM_IMAGE).
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Image string `json:"image,omitempty"`

	// imagePullSecret is the name of a Kubernetes Secret in the same namespace
	// as the Engine that contains Docker registry credentials for pulling the
	// WASM OCI image. This is passed directly to the Istio WasmPlugin resource.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ImagePullSecret string `json:"imagePullSecret,omitempty"`

	// ruleSetCacheServer contains configuration for the ruleset cache server.
	//
	// When omitted, no cache server will be used and no rulesets will be
	// dynamically loaded. This implies that your Engine will be deployed with
	// all rules statically embedded.
	//
	// +optional
	RuleSetCacheServer *RuleSetCacheServerConfig `json:"ruleSetCacheServer,omitempty"`
}

// -----------------------------------------------------------------------------
// Engine Driver - Istio Integration Mode
// -----------------------------------------------------------------------------

// IstioIntegrationMode specifies what mechanism will be used to integrate the
// WAF with Istio.
//
// +kubebuilder:validation:Enum=gateway
type IstioIntegrationMode string

const (
	// IstioIntegrationModeGateway applies the filter at the Gateway level.
	IstioIntegrationModeGateway IstioIntegrationMode = "gateway"

	// MaxImageLen must match the CEL size constraint on IstioWasmConfig.Image.
	MaxImageLen = 1024
)

// -----------------------------------------------------------------------------
// Engine Driver - Istio Dynamic Module Configuration
// -----------------------------------------------------------------------------

// IstioDynamicModuleConfig defines configuration for deploying the Engine as an
// Envoy dynamic module with Istio. The operator creates an EnvoyFilter resource
// that patches the Envoy config to load the dynamic module and passes WAF rules
// inline in the filter configuration.
//
// +kubebuilder:validation:MinProperties=0
// +kubebuilder:validation:XValidation:rule="self.mode == 'gateway' ? has(self.workloadSelector) : true",message="workloadSelector is required when mode is gateway"
type IstioDynamicModuleConfig struct {
	// mode specifies what mechanism will be used to integrate the WAF with
	// Istio.
	//
	// Currently only supports "Gateway" mode, utilizing Gateway API resources.
	//
	// +optional
	// +default="gateway"
	Mode IstioIntegrationMode `json:"mode,omitempty"`

	// workloadSelector specifies the selection criteria for attaching the WAF to
	// Istio resources.
	//
	// Required when mode is "gateway".
	//
	// +optional
	WorkloadSelector *metav1.LabelSelector `json:"workloadSelector,omitempty,omitzero"`

	// moduleName is the Envoy dynamic module name used to identify the loaded
	// shared library. This corresponds to the .so filename without the "lib"
	// prefix and ".so" suffix (e.g., "composer" loads "libcomposer.so").
	//
	// +optional
	// +default="composer"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ModuleName string `json:"moduleName,omitempty"`

	// filterName is the HTTP filter name registered by the dynamic module.
	//
	// +optional
	// +default="coraza-waf"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	FilterName string `json:"filterName,omitempty"`

	// filterMode controls whether the WAF inspects requests, responses, or both.
	//
	// +optional
	// +default="FULL"
	FilterMode DynamicModuleFilterMode `json:"filterMode,omitempty"`

	// moduleImage is the OCI image that contains the dynamic module shared
	// library (libcomposer.so). When set, the operator creates a ConfigMap
	// with a Deployment overlay that injects an init container to copy the
	// .so into the gateway pod. The user must reference this ConfigMap in
	// the Gateway's spec.infrastructure.parametersRef field.
	//
	// This is a workaround until Istio provides a native CRD for dynamic
	// modules (similar to WasmPlugin). The ConfigMap name is
	// "coraza-dm-<engine-name>".
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ModuleImage string `json:"moduleImage,omitempty"`

	// proxyImage overrides the Envoy proxy image used by the gateway pod.
	// The standard Istio proxy image does not include the dynamic modules
	// HTTP filter extension. This field allows specifying an Envoy image
	// that has it compiled in (e.g., "gcr.io/istio-testing/proxyv2:1.31-dev").
	//
	// When set and moduleImage is also set, the proxy image override is
	// included in the operator-managed ConfigMap Deployment overlay.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ProxyImage string `json:"proxyImage,omitempty"`
}

// DynamicModuleFilterMode specifies the WAF inspection mode for the dynamic
// module.
//
// +kubebuilder:validation:Enum=REQUEST_ONLY;RESPONSE_ONLY;FULL
type DynamicModuleFilterMode string

const (
	// DynamicModuleFilterModeRequestOnly inspects request headers and body only.
	DynamicModuleFilterModeRequestOnly DynamicModuleFilterMode = "REQUEST_ONLY"

	// DynamicModuleFilterModeResponseOnly inspects response headers and body only.
	DynamicModuleFilterModeResponseOnly DynamicModuleFilterMode = "RESPONSE_ONLY"

	// DynamicModuleFilterModeFull inspects both request and response phases.
	DynamicModuleFilterModeFull DynamicModuleFilterMode = "FULL"
)
