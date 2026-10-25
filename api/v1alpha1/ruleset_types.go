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
// RuleSet - Schema Registration
// -----------------------------------------------------------------------------

func init() {
	SchemeBuilder.Register(&RuleSet{}, &RuleSetList{})
}

// -----------------------------------------------------------------------------
// RuleSet - Constants
// -----------------------------------------------------------------------------

const (
	// AnnotationSkipUnsupportedRulesCheck is an annotation to disable the unsupported
	// rules degradation on a RuleSet (it will still log).
	AnnotationSkipUnsupportedRulesCheck = Group + "/skip-unsupported-rules-check"
)

// -----------------------------------------------------------------------------
// RuleSet
// -----------------------------------------------------------------------------

// RuleSet represents a set of Web Application Firewall (WAF) rules.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type RuleSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	//
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RuleSet.
	//
	// +required
	Spec RuleSetSpec `json:"spec,omitzero"`

	// status defines the observed state of RuleSet.
	//
	// +optional
	Status RuleSetStatus `json:"status,omitempty,omitzero"`
}

// RuleSetList contains a list of RuleSet resources.
//
// +kubebuilder:object:root=true
type RuleSetList struct {
	metav1.TypeMeta `json:",inline"`

	// ListMeta is standard list metadata.
	//
	// +optional
	metav1.ListMeta `json:"metadata,omitzero"`

	// Items is the list of RuleSets.
	//
	// +required
	Items []RuleSet `json:"items"`
}

// -----------------------------------------------------------------------------
// RuleSet - Spec
// -----------------------------------------------------------------------------

// RuleSetSpec defines the desired state of RuleSet.
type RuleSetSpec struct {
	// sources is an ordered list of references to RuleSource objects in the
	// same namespace as the RuleSet. Sources are concatenated in list order
	// to form the aggregated SecLang string.
	//
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2048
	// +listType=atomic
	Sources []SourceReference `json:"sources,omitempty"`

	// data is an optional list of references to RuleData objects in the same
	// namespace as the RuleSet. Data entries are merged to provide the
	// filesystem for @pmFromFile directives (last-listed wins on duplicate keys).
	//
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Data []DataReference `json:"data,omitempty"`

	// emptyRulesPolicy controls how an assembled RuleSet with no rules is
	// treated. Reject fails acceptance; Allow accepts and renders an explicit
	// no-op payload. Empty rules are never treated as missing.
	//
	// +optional
	// +default="Reject"
	// +kubebuilder:validation:Enum=Reject;Allow
	EmptyRulesPolicy EmptyRulesPolicy `json:"emptyRulesPolicy,omitempty"`
}

// EmptyRulesPolicy controls how an empty assembled RuleSet is treated.
type EmptyRulesPolicy string

const (
	// EmptyRulesPolicyReject rejects an empty assembled RuleSet.
	EmptyRulesPolicyReject EmptyRulesPolicy = "Reject"

	// EmptyRulesPolicyAllow accepts an empty assembled RuleSet and renders an
	// explicit no-op payload.
	EmptyRulesPolicyAllow EmptyRulesPolicy = "Allow"
)

// -----------------------------------------------------------------------------
// RuleSet - References
// -----------------------------------------------------------------------------

// SourceReference is a reference to a RuleSource object in the same namespace
// as the RuleSet.
type SourceReference struct {
	// name is the name of the RuleSource in the same namespace as the RuleSet.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`
}

// DataReference is a reference to a RuleData object in the same namespace
// as the RuleSet.
type DataReference struct {
	// name is the name of the RuleData in the same namespace as the RuleSet.
	//
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`
}

// -----------------------------------------------------------------------------
// RuleSet - Status
// -----------------------------------------------------------------------------

// RuleSetStatus defines the observed state of RuleSet.
// +kubebuilder:validation:MinProperties=1
type RuleSetStatus struct {
	// conditions represent the current state of the RuleSet resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Ready": the RuleSet has been processed and the rules have been cached
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	//
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`

	// acceptedContent records the last accepted assembled content. It survives
	// desired-input degradation.
	//
	// +optional
	AcceptedContent *RuleSetAcceptedContent `json:"acceptedContent,omitempty"`
}

// RuleSetAcceptedContent records the assembled content accepted for a RuleSet
// generation.
type RuleSetAcceptedContent struct {
	// observedGeneration is the RuleSet generation whose content was accepted.
	ObservedGeneration int64 `json:"observedGeneration"`

	// contentHash is the deterministic hash of the accepted assembled content,
	// in the form sha256:<64 lowercase hexadecimal characters>.
	ContentHash string `json:"contentHash"`
}
