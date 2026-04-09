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

// DriverConfig defines the driver configuration for the Engine.
//
// Exactly one driver must be specified.
//
// +kubebuilder:validation:XValidation:rule="[has(self.istio)].filter(x, x).size() == 1",message="exactly one driver must be specified"
// +kubebuilder:validation:MinProperties=0
type DriverConfig struct {
	// istio configures the Engine to integrate with Istio service mesh.
	//
	// +optional
	Istio *IstioDriverConfig `json:"istio,omitempty"`
}
