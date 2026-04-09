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
	"time"

	"github.com/corazawaf/coraza/v3"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
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
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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
	return ctrl.NewControllerManagedBy(mgr).
		For(&wafv1alpha1.RuleSet{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			annotationChangedPredicate(wafv1alpha1.AnnotationSkipUnsupportedRulesCheck),
		))).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findRuleSetsForConfigMap),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findRuleSetsForSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
				secret, ok := object.(*corev1.Secret)
				if !ok {
					return false
				}
				return secret.Type == wafv1alpha1.RuleDataSecretType
			})),
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

	logDebug(log, req, "RuleSet", "Loading rule data")
	secretData, done, err := r.loadRuleDataSecret(ctx, log, req, &ruleset)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	logDebug(log, req, "RuleSet", "aggregating rules")
	aggregatedRules, aggregatedErrors, done, err := r.aggregateRulesFromSources(ctx, log, req, &ruleset, secretData)
	if done || err != nil {
		return ctrl.Result{}, err
	}

	logInfo(log, req, "RuleSet", "Validating aggregated rules")
	fsRules := getDataFilesystem(secretData)
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
	return r.cacheRules(ctx, log, req, &ruleset, aggregatedRules, secretData, unsupportedMsg)
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Status Initialization
// -----------------------------------------------------------------------------

// initializeStatus sets the initial Progressing condition if the RuleSet has
// never been reconciled before.
func (r *RuleSetReconciler) initializeStatus(ctx context.Context, log logr.Logger, req ctrl.Request, ruleset *wafv1alpha1.RuleSet) error {
	if apimeta.FindStatusCondition(ruleset.Status.Conditions, "Ready") != nil {
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
