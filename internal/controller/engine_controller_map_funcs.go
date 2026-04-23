package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// EngineReconciler - Map Funcs
// -----------------------------------------------------------------------------

// findEnginesForRuleSet maps a RuleSet to the Engines in the same namespace that reference it.
func (r *EngineReconciler) findEnginesForRuleSet(ctx context.Context, ruleSet client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList, client.InNamespace(ruleSet.GetNamespace())); err != nil {
		log.Error(err, "Engine: Failed to list Engines", "namespace", ruleSet.GetNamespace())
		return nil
	}

	return collectRequests(engineList.Items, func(e *wafv1alpha1.Engine) bool {
		return e.Spec.RuleSet.Name == ruleSet.GetName()
	})
}

// findEnginesForGateway maps a Gateway to the Engines in the same namespace
// that use an Istio Wasm driver.
func (r *EngineReconciler) findEnginesForGateway(ctx context.Context, gateway client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList, client.InNamespace(gateway.GetNamespace())); err != nil {
		log.Error(err, "Engine: Failed to list Engines", "namespace", gateway.GetNamespace())
		return nil
	}

	return collectRequests(engineList.Items, func(e *wafv1alpha1.Engine) bool {
		return hasIstioWasmDriver(e) || hasIstioDynamicModuleDriver(e)
	})
}

// findEnginesForPod maps a Pod to the Engines in the same namespace whose
// workload selector matches the Pod's labels.
func (r *EngineReconciler) findEnginesForPod(ctx context.Context, pod client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList, client.InNamespace(pod.GetNamespace())); err != nil {
		log.Error(err, "Engine: Failed to list Engines", "namespace", pod.GetNamespace())
		return nil
	}

	return collectRequests(engineList.Items, func(e *wafv1alpha1.Engine) bool {
		return engineMatchesLabels(e, pod.GetLabels())
	})
}
