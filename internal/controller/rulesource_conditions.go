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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuleSource status condition reasons (RuleSourceReconciler and RuleSet gating).
const (
	ruleSourceReadyReasonValidated         = "Validated"
	ruleSourceReadyReasonValidationSkipped = "ValidationSkipped"
	ruleSourceDegradedReasonInvalidRules   = "InvalidRules"
)

// ruleSourceInvalidForGeneration reports whether the RuleSource is degraded
// for invalid rules at the given spec generation.
func ruleSourceInvalidForGeneration(conditions []metav1.Condition, generation int64) bool {
	c := apimeta.FindStatusCondition(conditions, conditionDegraded)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == ruleSourceDegradedReasonInvalidRules && c.ObservedGeneration == generation
}

// ruleSourceValidatedForGeneration reports whether RuleSource validation (or
// explicit skip) has completed for the current spec generation.
func ruleSourceValidatedForGeneration(conditions []metav1.Condition, generation int64, skipFragmentValidation bool) bool {
	if skipFragmentValidation {
		return isConditionCurrent(conditions, conditionReady, ruleSourceReadyReasonValidationSkipped, generation)
	}
	return isConditionCurrent(conditions, conditionReady, ruleSourceReadyReasonValidated, generation)
}
