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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// Target Validation
// -----------------------------------------------------------------------------

// isTargetNotFound checks whether the Gateway referenced by spec.target.name
// exists in the Engine's namespace. Returns true when the Gateway is not found.
// On transient API errors it returns (false, err) so the caller can retry.
func (r *EngineReconciler) isTargetNotFound(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (bool, error) {
	if !hasGatewayTarget(engine) {
		return false, nil
	}

	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "Gateway",
	})

	err := r.Get(ctx, types.NamespacedName{
		Name:      engine.Spec.Target.Name,
		Namespace: engine.Namespace,
	}, gw)
	if err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Gateway %q not found in namespace %q", engine.Spec.Target.Name, engine.Namespace)
			logInfo(log, req, "Engine", "Target Gateway not found; marking Engine not accepted", "gateway", engine.Spec.Target.Name)
			if patchErr := patchNotAccepted(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "TargetNotFound", msg); patchErr != nil {
				return true, patchErr
			}
			return true, nil
		}
		logAPIError(log, req, "Engine", err, "Failed to get target Gateway", engine)
		return false, fmt.Errorf("failed to get Gateway %s/%s: %w", engine.Namespace, engine.Spec.Target.Name, err)
	}

	return false, nil
}

// hasTargetConflict checks whether another Engine in the same namespace already
// targets the same Gateway. The oldest Engine wins (by creationTimestamp; ties
// broken by lexicographic name). Returns true if this Engine loses the conflict.
func (r *EngineReconciler) hasTargetConflict(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (bool, error) {
	if !hasGatewayTarget(engine) {
		return false, nil
	}

	var engineList wafv1alpha1.EngineList
	if err := r.List(ctx, &engineList, client.InNamespace(engine.Namespace)); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to list Engines for conflict detection", engine)
		return false, fmt.Errorf("failed to list Engines: %w", err)
	}

	// Collect all non-deleting Engines that target the same Gateway.
	var candidates []wafv1alpha1.Engine
	for i := range engineList.Items {
		if !engineList.Items[i].DeletionTimestamp.IsZero() {
			continue
		}
		if engineList.Items[i].Spec.Target.Type == engine.Spec.Target.Type &&
			engineList.Items[i].Spec.Target.Name == engine.Spec.Target.Name {
			candidates = append(candidates, engineList.Items[i])
		}
	}

	if len(candidates) <= 1 {
		return false, nil
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
		return false, nil
	}

	msg := fmt.Sprintf("Target %s %q is already claimed by Engine %q", engine.Spec.Target.Type, engine.Spec.Target.Name, winnerName)
	logInfo(log, req, "Engine", "Target conflict detected; marking Engine not accepted", "winner", winnerName, "gateway", engine.Spec.Target.Name)
	if patchErr := patchNotAccepted(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "TargetConflict", msg); patchErr != nil {
		return true, patchErr
	}

	return true, nil
}
