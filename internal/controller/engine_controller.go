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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Controller - RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=engines,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=engines/finalizers,verbs=update
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=engines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets,verbs=get;list;watch
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets/status,verbs=get
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=list;watch

// -----------------------------------------------------------------------------
// EngineReconciler
// -----------------------------------------------------------------------------

// EngineReconciler reconciles an Engine object
type EngineReconciler struct {
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	client.Client
	ruleSetCacheServerCluster string
	istioRevision             string
	// defaultWasmImage is the OCI URL used for Istio WasmPlugin spec.url when the
	// Engine omits spec.driver.istio.wasm.image.
	defaultWasmImage  string
	operatorNamespace string
}

// SetupWithManager sets up the controller with the Manager.
func (r *EngineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	wasmPlugin := &unstructured.Unstructured{}
	wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "extensions.istio.io",
		Version: "v1alpha1",
		Kind:    "WasmPlugin",
	})

	gateway := &unstructured.Unstructured{}
	gateway.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "Gateway",
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&wafv1alpha1.Engine{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(wasmPlugin).
		Watches(gateway, handler.EnqueueRequestsFromMapFunc(r.findEnginesForGateway)).
		Watches(&wafv1alpha1.RuleSet{}, handler.EnqueueRequestsFromMapFunc(r.findEnginesForRuleSet)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.findEnginesForPod), builder.WithPredicates(
			predicate.NewPredicateFuncs(func(object client.Object) bool {
				_, hasGWAPI := object.GetLabels()[gatewayNameLabel]
				return hasGWAPI
			}),
		)).
		Watches(&networkingv1.NetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(r.findEnginesForNetworkPolicy), builder.WithPredicates(
			networkPolicyPredicate(),
		)).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](
				1*time.Second,
				1*time.Minute,
			),
		}).
		Named("engine").
		Complete(r)
}

// -----------------------------------------------------------------------------
// EngineReconciler - Reconciler
// -----------------------------------------------------------------------------

// Reconcile handles reconciliation of Engine resources
func (r *EngineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	logDebug(log, req, "Engine", "Starting reconciliation")
	var engine wafv1alpha1.Engine
	if err := r.Get(ctx, req.NamespacedName, &engine); err != nil {
		if apierrors.IsNotFound(err) {
			logDebug(log, req, "Engine", "Resource not found")
			// Best-effort cleanup: remove any orphaned NetworkPolicy that may
			// remain if the Engine was deleted before the finalizer was added
			// (e.g., race during upgrade or legacy Engine without finalizer).
			if cleanupErr := r.cleanupNetworkPolicy(ctx, log, req); cleanupErr != nil {
				return ctrl.Result{}, cleanupErr
			}
			return ctrl.Result{}, nil
		}

		logAPIError(log, req, "Engine", err, "Failed to get", nil)
		return ctrl.Result{}, err
	}

	// Handle deletion: clean up cross-namespace NetworkPolicy before removing
	// the finalizer so the Engine can be garbage-collected.
	if deleting, err := r.handleNetworkPolicyDeletion(ctx, log, req, &engine); deleting || err != nil {
		return ctrl.Result{}, err
	}

	// Ensure the finalizer is present so we get a chance to clean up
	// the cross-namespace NetworkPolicy before the Engine is deleted.
	if added, err := r.ensureNetworkPolicyFinalizer(ctx, log, req, &engine); err != nil {
		return ctrl.Result{}, err
	} else if added {
		// The finalizer patch updated the Engine on the API server. Because
		// the Engine watch uses GenerationChangedPredicate (metadata-only
		// changes don't bump generation), we must explicitly requeue rather
		// than relying on the update event to trigger a fresh reconcile.
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}

	logDebug(log, req, "Engine", "Applying conditions")
	if engine.Status == nil {
		engine.Status = &wafv1alpha1.EngineStatus{}
	}
	if apimeta.FindStatusCondition(engine.Status.Conditions, "Ready") == nil {
		patch := client.MergeFrom(engine.DeepCopy())
		before := snapshotConditions(engine.Status.Conditions)
		applyStatusProgressing(&engine.Status.Conditions, engine.Generation, "Reconciling", "Starting reconciliation")
		if err := r.Status().Patch(ctx, &engine, patch); err != nil {
			logAPIError(log, req, "Engine", err, "Failed to patch initial status", &engine)
			return ctrl.Result{}, err
		}
		logConditionTransitions(log, req, "Engine", before, engine.Status.Conditions)
	}

	logDebug(log, req, "Engine", "Checking referenced RuleSet status")
	if degraded, err := r.isRuleSetDegraded(ctx, log, req, &engine); err != nil {
		return ctrl.Result{}, err
	} else if degraded {
		return ctrl.Result{}, nil
	}

	logInfo(log, req, "Engine", "Selecting driver and provisioning")
	return r.selectDriver(ctx, log, req, engine)
}

// -----------------------------------------------------------------------------
// EngineReconciler - Driver Configuration
// -----------------------------------------------------------------------------

// handleInvalidDriverConfiguration marks the engine as degraded due to invalid
// driver configuration. Currently, only Istio driver with Wasm mode is supported.
func (r *EngineReconciler) handleInvalidDriverConfiguration(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) error {
	err := fmt.Errorf("invalid driver configuration: only Istio driver with Wasm mode is currently supported")
	logError(log, req, "Engine", err, "Invalid driver configuration")

	if engine.Status == nil {
		engine.Status = &wafv1alpha1.EngineStatus{}
	}
	if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "InvalidConfiguration", err.Error()); patchErr != nil {
		return fmt.Errorf("validation failed: %w (status patch also failed: %v)", err, patchErr)
	}

	return err
}

// -----------------------------------------------------------------------------
// EngineReconciler - Driver Provisioning
// -----------------------------------------------------------------------------

func (r *EngineReconciler) selectDriver(ctx context.Context, log logr.Logger, req ctrl.Request, engine wafv1alpha1.Engine) (ctrl.Result, error) {
	switch {
	case engine.Spec.Driver != nil && engine.Spec.Driver.Istio != nil:
		switch {
		case engine.Spec.Driver.Istio.Wasm != nil:
			logDebug(log, req, "Engine", "Using Istio driver with WASM mode")
			return r.provisionIstioEngineWithWasm(ctx, log, req, engine)
		default:
			return ctrl.Result{}, r.handleInvalidDriverConfiguration(ctx, log, req, &engine)
		}
	default:
		return ctrl.Result{}, r.handleInvalidDriverConfiguration(ctx, log, req, &engine)
	}
}

// -----------------------------------------------------------------------------
// EngineReconciler - RuleSet Status Check
// -----------------------------------------------------------------------------

// isRuleSetDegraded fetches the Engine's referenced RuleSet and returns true if
// it is currently Degraded. When degraded, it marks the Engine Degraded and
// returns (true, nil). A retriable system error returns (false, err).
func (r *EngineReconciler) isRuleSetDegraded(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (bool, error) {
	var ruleSet wafv1alpha1.RuleSet
	if err := r.Get(ctx, types.NamespacedName{Name: engine.Spec.RuleSet.Name, Namespace: engine.Namespace}, &ruleSet); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("RuleSet %s not found", engine.Spec.RuleSet.Name)
			logInfo(log, req, "Engine", "RuleSet not found; marking Engine degraded", "ruleSet", engine.Spec.RuleSet.Name)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RuleSetNotFound", msg); patchErr != nil {
				return true, patchErr
			}
			return true, nil
		}
		logAPIError(log, req, "Engine", err, "Failed to get RuleSet", nil)
		return false, fmt.Errorf("failed to get RuleSet %s: %w", engine.Spec.RuleSet.Name, err)
	}

	degradedCond := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Degraded")
	if degradedCond == nil || degradedCond.Status != metav1.ConditionTrue {
		return false, nil
	}

	msg := fmt.Sprintf("RuleSet %s is degraded: %s", engine.Spec.RuleSet.Name, degradedCond.Message)
	logInfo(log, req, "Engine", "RuleSet is degraded; marking Engine degraded", "ruleSet", engine.Spec.RuleSet.Name)
	if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RuleSetDegraded", msg); patchErr != nil {
		return true, patchErr
	}

	return true, nil
}
