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

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// RuleSet Controller - Watch Predicates
// -----------------------------------------------------------------------------

// findRuleSetsForRuleSource maps a RuleSource to the RuleSets that reference it
// using the spec.sources.name field index registered in SetupWithManager.
func (r *RuleSetReconciler) findRuleSetsForRuleSource(ctx context.Context, ruleSource client.Object) []reconcile.Request {
	return r.findRuleSetsBy(ctx, ruleSource.GetNamespace(), "spec.sources.name", ruleSource.GetName())
}

// findRuleSetsForRuleData maps a RuleData to the RuleSets that reference it
// using the spec.data.name field index registered in SetupWithManager.
func (r *RuleSetReconciler) findRuleSetsForRuleData(ctx context.Context, ruleData client.Object) []reconcile.Request {
	return r.findRuleSetsBy(ctx, ruleData.GetNamespace(), "spec.data.name", ruleData.GetName())
}

// findRuleSetsBy lists RuleSets matching a field index value and returns
// reconcile requests for each.
func (r *RuleSetReconciler) findRuleSetsBy(ctx context.Context, namespace, indexKey, indexValue string) []reconcile.Request {
	log := logf.FromContext(ctx)

	var ruleSetList wafv1alpha1.RuleSetList
	if err := r.List(ctx, &ruleSetList,
		client.InNamespace(namespace),
		client.MatchingFields{indexKey: indexValue},
	); err != nil {
		log.Error(err, "RuleSet: Failed to list RuleSets", "namespace", namespace, "index", indexKey, "value", indexValue)
		return nil
	}

	return collectRequests(ruleSetList.Items, func(_ *wafv1alpha1.RuleSet) bool { return true })
}

// ruleSourceWatchPredicate enqueues referencing RuleSets when a RuleSource's
// spec changes (generation), validation annotation changes, or status changes
// (e.g. RuleSourceReconciler validation outcome) without a spec bump.
func ruleSourceWatchPredicate() predicate.Predicate {
	statusChanged := predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldRS, ok1 := e.ObjectOld.(*wafv1alpha1.RuleSource)
			newRS, ok2 := e.ObjectNew.(*wafv1alpha1.RuleSource)
			if !ok1 || !ok2 {
				return false
			}
			return !apiequality.Semantic.DeepEqual(oldRS.Status, newRS.Status)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		annotationChangedPredicate(wafv1alpha1.AnnotationSkipValidation),
		statusChanged,
	)
}
