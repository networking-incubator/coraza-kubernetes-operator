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
// +kubebuilder:validation:XValidation:rule="[has(self.wasm)].filter(x, x).size() == 1",message="exactly one integration mechanism (Wasm, etc) must be specified"
type IstioDriverConfig struct {
	// wasm configures the Engine to be deployed as a WebAssembly plugin.
	//
	// +optional
	Wasm *IstioWasmConfig `json:"wasm,omitempty,omitzero"`
}

// -----------------------------------------------------------------------------
// Engine Driver - Istio Wasm Configuration
// -----------------------------------------------------------------------------

// IstioWasmConfig defines configuration for deploying the Engine as a WASM
// plugin with Istio.
//
// +kubebuilder:validation:XValidation:rule="self.mode == 'gateway' ? has(self.workloadSelector) : true",message="workloadSelector is required when mode is gateway"
type IstioWasmConfig struct {
	// mode specifies what mechanism will be used to integrate the WAF with
	// Istio.
	//
	// Currently only supports "Gateway" mode, utilizing Gateway API resources.
	//
	// +optional
	// +default="gateway"
	Mode *IstioIntegrationMode `json:"mode,omitempty"`

	// workloadSelector specifies the selection criteria for attaching the WAF to
	// Istio resources.
	//
	// Required when mode is "gateway".
	//
	// +optional
	WorkloadSelector *metav1.LabelSelector `json:"workloadSelector,omitempty,omitzero"`

	// image is the OCI image reference for the Coraza WASM plugin.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^oci://`
	Image string `json:"image,omitempty"`

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
// Engine Driver - Istio Integration Configuration
// -----------------------------------------------------------------------------

// IstioIntegrationConfig defines Istio-specific integration options for the
// Engine.
type IstioIntegrationConfig struct {
	// mode specifies what mechanism will be used to integrate the WAF with
	// Istio.
	//
	// Currently only supports "Gateway" mode, utilizing Gateway API resources.
	//
	// +required
	Mode IstioIntegrationMode `json:"mode,omitempty"`

	// workloadSelector specifies the selection criteria for attaching the WAF.
	//
	// When mode is "gateway", this selector is used to identify the Gateway
	// Pods to which the WAF should be attached.
	//
	// +required
	WorkloadSelector metav1.LabelSelector `json:"workloadSelector,omitempty"`
}

// IstioIntegrationMode specifies what mechanism will be used to integrate the
// WAF with Istio.
//
// +kubebuilder:validation:Enum=gateway
type IstioIntegrationMode string

const (
	// IstioIntegrationModeGateway applies the filter at the Gateway level.
	IstioIntegrationModeGateway IstioIntegrationMode = "gateway"
)
