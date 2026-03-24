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

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// RuleSet Controller - Watch Predicates
// -----------------------------------------------------------------------------

// TODO: the functions here should probably be an index on the RuleSet containing
// the referred resources

// findRuleSetsForConfigMap maps a ConfigMap to the RuleSets that reference it (if any).
func (r *RuleSetReconciler) findRuleSetsForConfigMap(ctx context.Context, configMap client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var ruleSetList wafv1alpha1.RuleSetList
	if err := r.List(ctx, &ruleSetList, client.InNamespace(configMap.GetNamespace())); err != nil {
		log.Error(err, "RuleSet: Failed to list RuleSets", "namespace", configMap.GetNamespace())
		return nil
	}

	var requests []reconcile.Request
	for _, ruleSet := range ruleSetList.Items {
		for _, rule := range ruleSet.Spec.Rules {
			if rule.Name == configMap.GetName() {
				req := ctrl.Request{
					NamespacedName: types.NamespacedName{
						Name:      ruleSet.Name,
						Namespace: ruleSet.Namespace,
					},
				}
				requests = append(requests, req)

				logInfo(log, req, "RuleSet", "Enqueuing for reconciliation due to ConfigMap change", "configMapName", configMap.GetName())
				break
			}
		}
	}

	return requests
}

// findRuleSetsForSecret maps a Secret to the RuleSets that reference it (if any).
func (r *RuleSetReconciler) findRuleSetsForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var ruleSetList wafv1alpha1.RuleSetList
	if err := r.List(ctx, &ruleSetList, client.InNamespace(secret.GetNamespace())); err != nil {
		log.Error(err, "RuleSet: Failed to list RuleSets", "namespace", secret.GetNamespace())
		return nil
	}

	var requests []reconcile.Request
	for _, ruleSet := range ruleSetList.Items {
		if ruleSet.Spec.RuleData != nil && *ruleSet.Spec.RuleData == secret.GetName() {
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      ruleSet.Name,
					Namespace: ruleSet.Namespace,
				},
			}
			requests = append(requests, req)
			logInfo(log, req, "RuleSet", "Enqueuing for reconciliation due to Secret change", "secretName", secret.GetName())
		}
	}

	return requests
}
