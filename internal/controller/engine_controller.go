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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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

// -----------------------------------------------------------------------------
// Engine Controller
// -----------------------------------------------------------------------------

// EngineReconciler reconciles an Engine object
type EngineReconciler struct {
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	client.Client
	ruleSetCacheServerCluster string
}

// SetupWithManager sets up the controller with the Manager.
func (r *EngineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	wasmPlugin := &unstructured.Unstructured{}
	wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "extensions.istio.io",
		Version: "v1alpha1",
		Kind:    "WasmPlugin",
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&wafv1alpha1.Engine{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(wasmPlugin).
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
// Engine Controller - Reconciler
// -----------------------------------------------------------------------------

// Reconcile handles reconciliation of Engine resources
func (r *EngineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	logDebug(log, req, "Engine", "Starting reconciliation")
	var engine wafv1alpha1.Engine
	if err := r.Get(ctx, req.NamespacedName, &engine); err != nil {
		if apierrors.IsNotFound(err) {
			logDebug(log, req, "Engine", "Resource not found")
			return ctrl.Result{Requeue: false}, nil
		}

		logError(log, req, "Engine", err, "Failed to get")
		return ctrl.Result{Requeue: true}, err
	}

	logDebug(log, req, "Engine", "Applying conditions")
	if apimeta.FindStatusCondition(engine.Status.Conditions, "Ready") == nil {
		patch := client.MergeFrom(engine.DeepCopy())
		setStatusProgressing(log, req, "Engine", &engine.Status.Conditions, engine.Generation, "Reconciling", "Starting reconciliation")
		if err := r.Status().Patch(ctx, &engine, patch); err != nil {
			logError(log, req, "Engine", err, "Failed to patch initial status")
			return ctrl.Result{}, err
		}
	}

	logInfo(log, req, "Engine", "Selecting driver and provisioning")
	return r.selectDriver(ctx, log, req, engine)
}

// -----------------------------------------------------------------------------
// Engine Controller - Driver Provisioning
// -----------------------------------------------------------------------------

func (r *EngineReconciler) selectDriver(ctx context.Context, log logr.Logger, req ctrl.Request, engine wafv1alpha1.Engine) (ctrl.Result, error) {
	switch {
	case engine.Spec.Driver.Istio != nil:
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
// Engine Controller - Configuration Issue Handling
// -----------------------------------------------------------------------------

// handleInvalidDriverConfiguration marks the engine as degraded due to invalid
// driver configuration. Currently, only Istio driver with Wasm mode is supported.
func (r *EngineReconciler) handleInvalidDriverConfiguration(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) error {
	err := fmt.Errorf("invalid driver configuration: only Istio driver with Wasm mode is currently supported")
	logError(log, req, "Engine", err, "Invalid driver configuration")

	r.Recorder.Eventf(engine, nil, "Warning", "InvalidConfiguration", "Reconcile", err.Error())
	patch := client.MergeFrom(engine.DeepCopy())
	setStatusConditionDegraded(log, req, "Engine", &engine.Status.Conditions, engine.Generation, "InvalidConfiguration", err.Error())
	if updateErr := r.Status().Patch(ctx, engine, patch); updateErr != nil {
		logError(log, req, "Engine", updateErr, "Failed to patch status after validation error")
		return fmt.Errorf("validation failed: %w (status patch also failed: %v)", err, updateErr)
	}

	return err
}
