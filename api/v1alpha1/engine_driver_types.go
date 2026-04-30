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

// -----------------------------------------------------------------------------
// Engine - Driver Config
// -----------------------------------------------------------------------------

// DriverConfig configures how the WAF filter is deployed into the target.
// When omitted from the Engine spec, the operator uses a default driver
// (currently wasm for Istio).
//
// TODO: When using a Gateway resource, the engine reconciler MUST recognize
// what GatewayAPI controller was used and set the better default driver.
//
// Exactly one driver-specific configuration must match the selected type.
//
// +kubebuilder:validation:XValidation:rule="self.type == 'wasm' ? has(self.wasm) : true",message="wasm config is required when type is wasm"
// +kubebuilder:validation:XValidation:rule="self.type == 'dynamicModule' ? has(self.dynamicModule) : true",message="dynamicModule config is required when type is dynamicModule"
// +kubebuilder:validation:XValidation:rule="!has(self.wasm) || !has(self.dynamicModule)",message="only one driver config may be set"
// +kubebuilder:validation:MinProperties=0
type DriverConfig struct {
	// type selects the driver mechanism used to deploy the WAF filter.
	//
	// +required
	Type DriverType `json:"type,omitempty"`

	// wasm contains configuration specific to the WASM driver.
	//
	// +optional
	Wasm *WasmDriverConfig `json:"wasm,omitempty"`

	// dynamicModule contains configuration specific to the Envoy dynamic
	// module driver.
	//
	// +optional
	DynamicModule *DynamicModuleDriverConfig `json:"dynamicModule,omitempty"`
}

// -----------------------------------------------------------------------------
// Engine - Driver Type
// -----------------------------------------------------------------------------

// DriverType specifies the mechanism used to deploy the WAF filter.
//
// +kubebuilder:validation:Enum=wasm;dynamicModule
type DriverType string

const (
	// DriverTypeWasm deploys the WAF as a WebAssembly plugin.
	DriverTypeWasm DriverType = "wasm"

	// DriverTypeDynamicModule deploys the WAF as an Envoy dynamic module.
	DriverTypeDynamicModule DriverType = "dynamicModule"
)

// -----------------------------------------------------------------------------
// Engine - WASM Driver Config
// -----------------------------------------------------------------------------

// WasmDriverConfig defines configuration for deploying the Engine as a WASM
// plugin.
//
// +kubebuilder:validation:MinProperties=0
// +kubebuilder:validation:XValidation:rule="!has(self.image) || self.image.matches('^oci://')",message="image must start with oci:// when set"
// +kubebuilder:validation:XValidation:rule="!has(self.image) || size(self.image) <= 1024",message="image must be at most 1024 characters when set"
type WasmDriverConfig struct {
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
	// WASM OCI image.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ImagePullSecret string `json:"imagePullSecret,omitempty"`
}

// MaxImageLen must match the CEL size constraint on WasmDriverConfig.Image.
const MaxImageLen = 1024

// -----------------------------------------------------------------------------
// Engine - Dynamic Module Driver Config
// -----------------------------------------------------------------------------

// DynamicModuleDriverConfig defines configuration for deploying the Engine as
// an Envoy dynamic module. The operator creates an EnvoyFilter resource that
// patches the Envoy config to load the dynamic module and passes WAF rules
// inline in the filter configuration.
//
// +kubebuilder:validation:MinProperties=0
// +kubebuilder:validation:XValidation:rule="!has(self.proxyImage) || has(self.moduleImage)",message="moduleImage is required when proxyImage is set"
type DynamicModuleDriverConfig struct {
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

// -----------------------------------------------------------------------------
// Engine - Dynamic Module Filter Mode
// -----------------------------------------------------------------------------

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
