package controller

import (
	"context"

	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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
// that target this specific Gateway by name.
func (r *EngineReconciler) findEnginesForGateway(ctx context.Context, gateway client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList, client.InNamespace(gateway.GetNamespace())); err != nil {
		log.Error(err, "Engine: Failed to list Engines", "namespace", gateway.GetNamespace())
		return nil
	}

	return collectRequests(engineList.Items, func(e *wafv1alpha1.Engine) bool {
		return hasGatewayTarget(e) && e.Spec.Target.Name == gateway.GetName()
	})
}

// findCompetingEngines maps an Engine to all other Engines in the same
// namespace that target the same Gateway. Called by competingEngineHandler on
// create, delete, and generation-changing updates.
func (r *EngineReconciler) findCompetingEngines(ctx context.Context, obj client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	engine, ok := obj.(*wafv1alpha1.Engine)
	if !ok {
		return nil
	}
	if !hasGatewayTarget(engine) {
		return nil
	}

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList, client.InNamespace(engine.GetNamespace())); err != nil {
		log.Error(err, "Engine: Failed to list Engines for conflict mapping", "namespace", engine.GetNamespace())
		return nil
	}

	return collectRequests(engineList.Items, func(e *wafv1alpha1.Engine) bool {
		// hasGatewayTarget is redundant with the Type/Name checks but kept as
		// defense-in-depth to reject candidates with empty Target.Name.
		return e.Name != engine.Name &&
			hasGatewayTarget(e) &&
			e.Spec.Target.Type == engine.Spec.Target.Type &&
			e.Spec.Target.Name == engine.Spec.Target.Name
	})
}

// competingEngineHandler returns an EventHandler that requeues all competing
// Engines when an Engine is created, deleted, or has its target changed.
//
// On Update events (filtered by the caller to generation-changing updates),
// competitors for both the old and new targets are enqueued so that Engines
// targeting the old Gateway can clear a stale TargetConflict.
func (r *EngineReconciler) competingEngineHandler() handler.EventHandler {
	enqueue := func(ctx context.Context, obj client.Object, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
		for _, req := range r.findCompetingEngines(ctx, obj) {
			q.Add(req)
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(ctx, e.Object, q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(ctx, e.Object, q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(ctx, e.ObjectOld, q)
			enqueue(ctx, e.ObjectNew, q)
		},
	}
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
