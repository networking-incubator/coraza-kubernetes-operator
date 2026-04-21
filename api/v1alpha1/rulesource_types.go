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
// RuleSource - Schema Registration
// -----------------------------------------------------------------------------

func init() {
	SchemeBuilder.Register(&RuleSource{}, &RuleSourceList{})
}

// -----------------------------------------------------------------------------
// RuleSource - Constants
// -----------------------------------------------------------------------------

const (
	// AnnotationSkipValidation controls per-fragment Coraza rule validation on
	// a RuleSource. When set to "false", per-source validation is skipped
	// (the aggregated RuleSet validation still runs).
	AnnotationSkipValidation = "coraza.io/validation"
)

// -----------------------------------------------------------------------------
// RuleSource
// -----------------------------------------------------------------------------

// RuleSource holds SecLang WAF rule text for consumption by RuleSet resources.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="has(self.spec.rules) && self.spec.rules != \"\"",message="rules must be set and non-empty"
type RuleSource struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the rule content.
	//
	// +required
	Spec RuleSourceSpec `json:"spec,omitzero"`
}

// RuleSourceList contains a list of RuleSource resources.
//
// +kubebuilder:object:root=true
type RuleSourceList struct {
	metav1.TypeMeta `json:",inline"`

	// ListMeta is standard list metadata.
	//
	// +optional
	metav1.ListMeta `json:"metadata,omitzero"`

	// Items is the list of RuleSources.
	//
	// +required
	Items []RuleSource `json:"items"`
}

// -----------------------------------------------------------------------------
// RuleSource - Spec
// -----------------------------------------------------------------------------

// RuleSourceSpec defines the content of a RuleSource.
type RuleSourceSpec struct {
	// rules contains SecLang rule text.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1572864
	Rules string `json:"rules,omitempty"`
}
