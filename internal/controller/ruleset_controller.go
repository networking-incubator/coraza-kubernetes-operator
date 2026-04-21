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

	"github.com/corazawaf/coraza/v3"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
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
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
)

// -----------------------------------------------------------------------------
// RuleSetReconciler - RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesources,verbs=get;list;watch
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=ruledata,verbs=get;list;watch

// -----------------------------------------------------------------------------
// RuleSetReconciler
// -----------------------------------------------------------------------------

// RuleSetReconciler reconciles a RuleSet object
type RuleSetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	Cache    *cache.RuleSetCache
}

// SetupWithManager sets up the controller with the Manager.
func (r *RuleSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &wafv1alpha1.RuleSet{}, "spec.sources.name", func(obj client.Object) []string {
		rs := obj.(*wafv1alpha1.RuleSet)
		names := make([]string, len(rs.Spec.Sources))
		for i, src := range rs.Spec.Sources {
			names[i] = src.Name
		}
		return names
	}); err != nil {
		return fmt.Errorf("index spec.sources.name: %w", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &wafv1alpha1.RuleSet{}, "spec.data.name", func(obj client.Object) []string {
		rs := obj.(*wafv1alpha1.RuleSet)
		names := make([]string, len(rs.Spec.Data))
		for i, d := range rs.Spec.Data {
			names[i] = d.Name
		}
		return names
	}); err != nil {
		return fmt.Errorf("index spec.data.name: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&wafv1alpha1.RuleSet{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			annotationChangedPredicate(wafv1alpha1.AnnotationSkipUnsupportedRulesCheck),
		))).
		Watches(
			&wafv1alpha1.RuleSource{},
			handler.EnqueueRequestsFromMapFunc(r.findRuleSetsForRuleSource),
		).
		Watches(
			&wafv1alpha1.RuleData{},
			handler.EnqueueRequestsFromMapFunc(r.findRuleSetsForRuleData),
		).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](
				1*time.Second,
				1*time.Minute,
			),
		}).
		Named("ruleset").
		Complete(r)
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Reconcile
// -----------------------------------------------------------------------------

// Reconcile handles reconciliation of RuleSet resources
func (r *RuleSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	logDebug(log, req, "RuleSet", "Starting reconciliation")
	var ruleset wafv1alpha1.RuleSet
	if err := r.Get(ctx, req.NamespacedName, &ruleset); err != nil {
		if apierrors.IsNotFound(err) {
			logDebug(log, req, "RuleSet", "Resource not found")
			return ctrl.Result{}, nil
		}
		logAPIError(log, req, "RuleSet", err, "Failed to GET", nil)
		return ctrl.Result{}, err
	}

	logDebug(log, req, "RuleSet", "Initializing status")
	if err := r.initializeStatus(ctx, log, req, &ruleset); err != nil {
		return ctrl.Result{}, err
	}

	logDebug(log, req, "RuleSet", "Loading RuleData objects")
	dataFiles, done, err := r.loadData(ctx, log, req, &ruleset)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	logDebug(log, req, "RuleSet", "Loading RuleSource objects")
	aggregatedRules, aggregatedErrors, done, err := r.loadSources(ctx, log, req, &ruleset, dataFiles)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	logInfo(log, req, "RuleSet", "Validating aggregated rules")
	fsRules := getDataFilesystem(dataFiles)
	conf := coraza.NewWAFConfig().WithDirectives(aggregatedRules)
	if fsRules != nil {
		conf = conf.WithRootFS(fsRules)
	}
	if err := r.validateAggregatedRules(ctx, log, req, &ruleset, conf, aggregatedErrors); err != nil {
		return ctrl.Result{}, err
	}

	logDebug(log, req, "RuleSet", "Checking for unsupported rules")
	foundUnsupportedRules, unsupportedMsg, err := r.rejectUnsupportedRules(ctx, log, req, &ruleset, aggregatedRules)
	if err != nil {
		return ctrl.Result{}, err
	}
	if foundUnsupportedRules {
		return ctrl.Result{}, nil
	}

	logInfo(log, req, "RuleSet", "Caching rules")
	return r.cacheRules(ctx, log, req, &ruleset, aggregatedRules, dataFiles, unsupportedMsg)
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Status Initialization
// -----------------------------------------------------------------------------

// initializeStatus sets the initial Progressing condition if the RuleSet has
// never been reconciled before.
func (r *RuleSetReconciler) initializeStatus(ctx context.Context, log logr.Logger, req ctrl.Request, ruleset *wafv1alpha1.RuleSet) error {
	if apimeta.FindStatusCondition(ruleset.Status.Conditions, conditionReady) != nil {
		return nil
	}

	patch := client.MergeFrom(ruleset.DeepCopy())
	before := snapshotConditions(ruleset.Status.Conditions)
	applyStatusProgressing(&ruleset.Status.Conditions, ruleset.Generation, "Reconciling", "Starting reconciliation")
	if err := r.Status().Patch(ctx, ruleset, patch); err != nil {
		logAPIError(log, req, "RuleSet", err, "Failed to patch initial status", ruleset)
		return err
	}
	logConditionTransitions(log, req, "RuleSet", before, ruleset.Status.Conditions)
	return nil
}
