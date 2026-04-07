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
	// a RuleSource of type Rule. When set to "false", per-source validation is
	// skipped (the aggregated RuleSet validation still runs).
	AnnotationSkipValidation = "coraza.io/validation"
)

// RuleSourceType discriminates between rule text and data file fragments.
//
// +kubebuilder:validation:Enum=Rule;Data
type RuleSourceType string

const (
	// RuleSourceTypeRule indicates the RuleSource contains SecLang rule text.
	RuleSourceTypeRule RuleSourceType = "Rule"

	// RuleSourceTypeData indicates the RuleSource contains data files
	// (e.g. for @pmFromFile).
	RuleSourceTypeData RuleSourceType = "Data"
)

// -----------------------------------------------------------------------------
// RuleSource
// -----------------------------------------------------------------------------

// RuleSource holds WAF rule text or data file content for consumption by
// RuleSet resources. It replaces the previous use of ConfigMaps (for rules)
// and Secrets (for @pmFromFile data) as storage for WAF rule material.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.spec.type == 'Rule' ? has(self.spec.rules) && self.spec.rules != \"\" : true",message="rules must be set and non-empty when type is Rule"
// +kubebuilder:validation:XValidation:rule="self.spec.type == 'Rule' ? !has(self.spec.files) || size(self.spec.files) == 0 : true",message="files must not be set when type is Rule"
// +kubebuilder:validation:XValidation:rule="self.spec.type == 'Data' ? has(self.spec.files) && size(self.spec.files) > 0 : true",message="files must be non-empty when type is Data"
// +kubebuilder:validation:XValidation:rule="self.spec.type == 'Data' ? !has(self.spec.rules) || self.spec.rules == \"\" : true",message="rules must not be set when type is Data"
// +kubebuilder:validation:XValidation:rule="self.spec.type == 'Data' && has(self.spec.files) ? self.spec.files.all(k, k.matches('^[-._a-zA-Z0-9]+$') && size(k) <= 253) : true",message="files keys must be valid data file names (alphanumeric, '-', '_', '.'; max 253 chars)"
type RuleSource struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the rule or data content.
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
	// type discriminates between Rule and Data fragments.
	//
	// +required
	Type RuleSourceType `json:"type,omitempty"`

	// rules contains SecLang rule text. Required when type is Rule;
	// must not be set when type is Data.
	//
	// +optional
	Rules *string `json:"rules,omitempty"`

	// files maps filenames to file content, used for @pmFromFile data.
	// Required when type is Data; must not be set when type is Rule.
	//
	// +optional
	Files map[string]string `json:"files,omitempty"`
}
