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
}

// -----------------------------------------------------------------------------
// Engine - Driver Type
// -----------------------------------------------------------------------------

// DriverType specifies the mechanism used to deploy the WAF filter.
//
// +kubebuilder:validation:Enum=wasm
type DriverType string

const (
	// DriverTypeWasm deploys the WAF as a WebAssembly plugin.
	DriverTypeWasm DriverType = "wasm"
)

// -----------------------------------------------------------------------------
// Engine - WASM Driver Config
// -----------------------------------------------------------------------------

// WasmDriverConfig defines configuration for deploying the Engine as a WASM
// plugin.
//
// +kubebuilder:validation:MinProperties=0
// +kubebuilder:validation:XValidation:rule="(has(self.url) && has(self.sha256)) || (!has(self.url) && !has(self.sha256))",message="url and sha256 must be set together; omit both to use the bundled module"
// +kubebuilder:validation:XValidation:rule="!has(self.url) || (self.url.matches('^https://') && !self.url.contains('?') && !self.url.contains('#'))",message="url must be an https URL without query or fragment; userinfo is rejected by Go URL parsing"
// +kubebuilder:validation:XValidation:rule="!has(self.sha256) || self.sha256.matches('^[0-9a-f]{64}$')",message="sha256 must be 64 lowercase hexadecimal characters"
// +kubebuilder:validation:XValidation:rule="!has(self.image) && !has(self.imagePullSecret)",message="spec.driver.wasm.image and imagePullSecret are removed; set url and sha256, or omit both to use the bundled module"
type WasmDriverConfig struct {
	// url is an HTTPS URL serving raw WASM bytes. It must be set together
	// with sha256. When both url and sha256 are omitted, the operator uses its
	// bundled module.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url,omitempty"`

	// sha256 is the lowercase hexadecimal SHA-256 of the raw WASM bytes
	// served at url. It must be set together with url.
	//
	// +optional
	SHA256 string `json:"sha256,omitempty"`

	// Deprecated: removed. Retained only to reject legacy manifests with a clear
	// error and to let the startup preflight detect stored legacy objects.
	//
	// image is the OCI image reference for the Coraza WASM plugin.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Image string `json:"image,omitempty"`

	// Deprecated: removed. See Image.
	//
	// imagePullSecret is the name of a Kubernetes Secret in the same namespace
	// as the Engine that contains Docker registry credentials for pulling the
	// WASM OCI image.
	//
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ImagePullSecret string `json:"imagePullSecret,omitempty"`
}
