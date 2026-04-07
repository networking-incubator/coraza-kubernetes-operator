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
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// RuleSet Controller - Watch Predicates
// -----------------------------------------------------------------------------

// findRuleSetsForRuleSource maps a RuleSource to the RuleSets that reference it (if any).
func (r *RuleSetReconciler) findRuleSetsForRuleSource(ctx context.Context, ruleSource client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var ruleSetList wafv1alpha1.RuleSetList
	if err := r.List(ctx, &ruleSetList, client.InNamespace(ruleSource.GetNamespace())); err != nil {
		log.Error(err, "RuleSet: Failed to list RuleSets", "namespace", ruleSource.GetNamespace())
		return nil
	}

	return collectRequests(ruleSetList.Items, func(rs *wafv1alpha1.RuleSet) bool {
		for _, src := range rs.Spec.Sources {
			if src.Name == ruleSource.GetName() {
				return true
			}
		}
		return false
	})
}
