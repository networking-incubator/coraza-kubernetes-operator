package controller

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Target Helpers
// -----------------------------------------------------------------------------

// hasGatewayTarget reports whether the Engine targets a Gateway resource.
func hasGatewayTarget(engine *wafv1alpha1.Engine) bool {
	if engine == nil {
		return false
	}
	return engine.Spec.Target.Type == wafv1alpha1.EngineTargetTypeGateway &&
		engine.Spec.Target.Name != ""
}

// targetLabelSelector returns the workload label selector derived from the
// Engine's target reference. For Gateway targets, the GEP-1762
// gateway.networking.k8s.io/gateway-name label is used.
//
// Returns nil if the name is empty or not a valid DNS-1035 label,
// preventing silent selector mismatches.
func targetLabelSelector(engine *wafv1alpha1.Engine) *metav1.LabelSelector {
	if engine == nil {
		return nil
	}
	switch engine.Spec.Target.Type {
	case wafv1alpha1.EngineTargetTypeGateway:
		name := engine.Spec.Target.Name
		if name == "" || len(validation.IsDNS1035Label(name)) > 0 {
			return nil
		}
		return &metav1.LabelSelector{
			MatchLabels: map[string]string{
				gatewayNameLabel: name,
			},
		}
	default:
		return nil
	}
}

// needsAcceptedUpdate reports whether the Accepted condition must be (re-)set
// to True. Returns true when the condition is absent, not True, or has a stale
// ObservedGeneration.
func needsAcceptedUpdate(conditions []metav1.Condition, generation int64) bool {
	cond := apimeta.FindStatusCondition(conditions, conditionAccepted)
	return cond == nil || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != generation
}

// -----------------------------------------------------------------------------
// Target Rejection Cleanup
// -----------------------------------------------------------------------------

// rejectTarget cleans up child resources and then patches the Engine status to
// Accepted=False. Cleanup runs before the status patch so that a cleanup
// failure causes a retry with the status still reflecting the previous state
// (avoiding the case where the status is patched but cleanup never completes).
func (r *EngineReconciler) rejectTarget(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine, reason, message string) error {
	if err := r.cleanupNotAccepted(ctx, log, req, engine); err != nil {
		return err
	}
	return patchNotAccepted(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, reason, message)
}

// cleanupNotAccepted removes child resources that were created when the Engine
// was previously accepted (EnvoyFilter, Telemetry, NetworkPolicy). This
// prevents a stale EnvoyFilter from enforcing rules for an Engine that is no
// longer accepted due to TargetNotFound or TargetConflict.
func (r *EngineReconciler) cleanupNotAccepted(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) error {
	telemetry := &unstructured.Unstructured{}
	telemetry.SetGroupVersionKind(telemetryGVK)
	telemetry.SetNamespace(engine.Namespace)
	telemetry.SetName(telemetryName(engine.Name))
	if err := r.deleteTelemetry(ctx, telemetry); err != nil {
		return err
	}

	envoyFilter := &unstructured.Unstructured{}
	envoyFilter.SetGroupVersionKind(envoyFilterGVK)
	efName := envoyFilterName(engine.Name)
	err := r.Get(ctx, types.NamespacedName{Name: efName, Namespace: engine.Namespace}, envoyFilter)
	if err == nil {
		if delErr := r.Delete(ctx, envoyFilter); client.IgnoreNotFound(delErr) != nil {
			logAPIError(log, req, "Engine", delErr, "Failed to delete EnvoyFilter for not-accepted Engine", envoyFilter)
			return delErr
		}
		logInfo(log, req, "Engine", "Deleted EnvoyFilter for not-accepted Engine", "envoyFilter", efName)
	} else if !apierrors.IsNotFound(err) {
		logAPIError(log, req, "Engine", err, "Failed to get EnvoyFilter for cleanup", nil)
		return err
	}

	// A rejected Engine may still own the retired WasmPlugin from a previous
	// release. It must not keep polling the removed cache server.
	if err := r.deleteLegacyWasmPlugin(ctx, log, req, engine); err != nil {
		return err
	}

	if err := r.cleanupNetworkPolicy(ctx, log, req); err != nil {
		return err
	}

	return nil
}

// -----------------------------------------------------------------------------
// Target Validation
// -----------------------------------------------------------------------------

// isTargetNotFound checks whether the Gateway referenced by spec.target.name
// exists in the Engine's namespace. Returns true when the Gateway is not found.
// On transient API errors it returns (false, err) so the caller can retry.
// This function only detects the condition — it does not patch status.
func (r *EngineReconciler) isTargetNotFound(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (bool, error) {
	if !hasGatewayTarget(engine) {
		return false, nil
	}

	err := r.Get(ctx, types.NamespacedName{
		Name:      engine.Spec.Target.Name,
		Namespace: engine.Namespace,
	}, &gatewayv1.Gateway{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			logInfo(log, req, "Engine", "Target Gateway not found", "gateway", engine.Spec.Target.Name)
			return true, nil
		}
		logAPIError(log, req, "Engine", err, "Failed to get target Gateway", engine)
		return false, fmt.Errorf("failed to get Gateway %s/%s: %w", engine.Namespace, engine.Spec.Target.Name, err)
	}

	return false, nil
}

// hasTargetConflict checks whether another Engine in the same namespace already
// targets the same Gateway. The oldest Engine wins (by creationTimestamp; ties
// broken by lexicographic name). Returns (true, winnerName, nil) if this Engine
// loses the conflict. This function only detects the condition — it does not
// patch status.
func (r *EngineReconciler) hasTargetConflict(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (bool, string, error) {
	if !hasGatewayTarget(engine) {
		return false, "", nil
	}

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList,
		client.InNamespace(engine.Namespace),
		client.MatchingFields{engineTargetIndex: engineTargetKey(engine.Spec.Target.Type, engine.Spec.Target.Name)},
	); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to list Engines for conflict detection", engine)
		return false, "", fmt.Errorf("failed to list Engines: %w", err)
	}

	// Filter out Engines that are being deleted; the index does not
	// account for DeletionTimestamp.
	var candidates []wafv1alpha1.Engine
	for i := range engineList.Items {
		if engineList.Items[i].DeletionTimestamp.IsZero() {
			candidates = append(candidates, engineList.Items[i])
		}
	}

	if len(candidates) <= 1 {
		return false, "", nil
	}

	// Sort: oldest first, tiebreak by name.
	sort.Slice(candidates, func(i, j int) bool {
		ti := candidates[i].CreationTimestamp.Time
		tj := candidates[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return candidates[i].Name < candidates[j].Name
	})

	winnerName := candidates[0].Name
	if winnerName == engine.Name {
		return false, "", nil
	}

	logInfo(log, req, "Engine", "Target conflict detected", "winner", winnerName, "gateway", engine.Spec.Target.Name)
	return true, winnerName, nil
}

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

// findEnginesForGatewayClass maps a GatewayClass change to enabled Engines
// whose target Gateway references that class. GatewayClasses are cluster scoped,
// so Engines must be considered across namespaces.
func (r *EngineReconciler) findEnginesForGatewayClass(ctx context.Context, gatewayClass client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)
	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList); err != nil {
		log.Error(err, "Engine: Failed to list Engines for GatewayClass", "gatewayClass", gatewayClass.GetName())
		return nil
	}

	return collectRequests(engineList.Items, func(engine *wafv1alpha1.Engine) bool {
		if !observabilityEnabled(engine) || !hasGatewayTarget(engine) {
			return false
		}
		var gateway gatewayv1.Gateway
		if err := r.Get(ctx, client.ObjectKey{Namespace: engine.Namespace, Name: engine.Spec.Target.Name}, &gateway); err != nil {
			log.Error(err, "Engine: Failed to get Gateway for GatewayClass", "engine", engine.Name, "namespace", engine.Namespace, "gateway", engine.Spec.Target.Name)
			return true
		}
		return string(gateway.Spec.GatewayClassName) == gatewayClass.GetName()
	})
}

// findEnginesForGateway maps a Gateway to the Engines in the same namespace
// that target this specific Gateway by name. Uses the spec.target index.
func (r *EngineReconciler) findEnginesForGateway(ctx context.Context, gateway client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList,
		client.InNamespace(gateway.GetNamespace()),
		client.MatchingFields{engineTargetIndex: engineTargetKey(wafv1alpha1.EngineTargetTypeGateway, gateway.GetName())},
	); err != nil {
		log.Error(err, "Engine: Failed to list Engines", "namespace", gateway.GetNamespace())
		return nil
	}

	return collectRequests(engineList.Items, func(e *wafv1alpha1.Engine) bool {
		// we collect all of the items, given we are already matching them using the index of 'engineTargetIndex'
		return true
	})
}

// findCompetingEngines maps an Engine to all other Engines in the same
// namespace that target the same Gateway. Called by competingEngineHandler on
// create, delete, and generation-changing updates. Uses the spec.target index.
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
	if err := r.List(ctx, &engineList,
		client.InNamespace(engine.GetNamespace()),
		client.MatchingFields{engineTargetIndex: engineTargetKey(engine.Spec.Target.Type, engine.Spec.Target.Name)},
	); err != nil {
		log.Error(err, "Engine: Failed to list Engines for conflict mapping", "namespace", engine.GetNamespace())
		return nil
	}

	return collectRequests(engineList.Items, func(e *wafv1alpha1.Engine) bool {
		return e.Name != engine.Name
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
