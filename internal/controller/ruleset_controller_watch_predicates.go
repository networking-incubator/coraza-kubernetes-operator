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

// findRuleSetsForConfigMap maps a ConfigMap to the RuleSets that reference it
// via the "spec.rules.name" field index.
func (r *RuleSetReconciler) findRuleSetsForConfigMap(ctx context.Context, configMap client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var ruleSetList wafv1alpha1.RuleSetList
	if err := r.List(ctx, &ruleSetList,
		client.InNamespace(configMap.GetNamespace()),
		client.MatchingFields{"spec.rules.name": configMap.GetName()},
	); err != nil {
		log.Error(err, "RuleSet: Failed to list RuleSets by index", "namespace", configMap.GetNamespace())
		return nil
	}

	return collectRequests(ruleSetList.Items, func(rs *wafv1alpha1.RuleSet) bool { return true })
}

// findRuleSetsForSecret maps a Secret to the RuleSets that reference it
// via the "spec.ruleData" field index.
func (r *RuleSetReconciler) findRuleSetsForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var ruleSetList wafv1alpha1.RuleSetList
	if err := r.List(ctx, &ruleSetList,
		client.InNamespace(secret.GetNamespace()),
		client.MatchingFields{"spec.ruleData": secret.GetName()},
	); err != nil {
		log.Error(err, "RuleSet: Failed to list RuleSets by index", "namespace", secret.GetNamespace())
		return nil
	}

	return collectRequests(ruleSetList.Items, func(rs *wafv1alpha1.RuleSet) bool { return true })
}
