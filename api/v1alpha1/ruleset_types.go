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
	// RuleDataSecretType is the expected type for a Secret that contains rule data files.
	RuleDataSecretType = "coraza/data"

	// AnnotationSkipUnsupportedRulesCheck is an annotation to disable the unsupported
	// rules degradation on a RuleSet (it will still log).
	AnnotationSkipUnsupportedRulesCheck = "waf.k8s.coraza.io/skip-unsupported-rules-check"
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
	// rules is an ordered list of references to ConfigMaps that contain the
	// firewall rules to be compiled into a complete set.
	//
	// Each entry refers to a ConfigMap by name in the same namespace as
	// the RuleSet. The ConfigMap must contain a "rules" key.
	//
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2048
	// +listType=atomic
	Rules []RuleSourceReference `json:"rules,omitempty"`

	// ruleData contains the name of a secret with the required data for rules.
	// Usually rules that contain the directive '@pmFromFile'.
	// This secret must be created containing the type coraza/data otherwise it will
	// not be watched.
	// Additionally, the secret must contain the name of each file as the key, and the content
	// of the file as the value
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RuleData string `json:"ruleData,omitempty"`
}

// -----------------------------------------------------------------------------
// RuleSet - Cache Server
// -----------------------------------------------------------------------------

// RuleSetCacheServerConfig defines the configuration for the RuleSet cache server.
// +kubebuilder:validation:MinProperties=0
type RuleSetCacheServerConfig struct {
	// pollIntervalSeconds specifies how often the WAF should check for
	// configuration updates. The value is specified in seconds.
	//
	// When omitted, this means the user has no opinion and the platform
	// will choose a reasonable default, which is subject to change over time.
	// The current default is 15 seconds.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +optional
	// +default=15
	PollIntervalSeconds int32 `json:"pollIntervalSeconds,omitempty"`
}

// -----------------------------------------------------------------------------
// RuleSet - References
// -----------------------------------------------------------------------------

// RuleSourceReference is a reference to a ConfigMap that contains WAF rules.
type RuleSourceReference struct {
	// name is the name of the ConfigMap in the same namespace as the RuleSet.
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
}
