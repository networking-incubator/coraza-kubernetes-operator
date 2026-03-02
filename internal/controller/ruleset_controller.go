/*
Copyright 2026 Shane Utt.

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
	"strings"
	"time"

	"github.com/corazawaf/coraza/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
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
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
)

// -----------------------------------------------------------------------------
// RuleSet Controller - RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// -----------------------------------------------------------------------------
// RuleSet Controller
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
		For(&wafv1alpha1.RuleSet{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findRuleSetsForConfigMap),
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

// Reconcile handles reconciliation of RuleSet resources
func (r *RuleSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	logDebug(log, req, "RuleSet", "Starting reconciliation")
	var ruleset wafv1alpha1.RuleSet
	if err := r.Get(ctx, req.NamespacedName, &ruleset); err != nil {
		if errors.IsNotFound(err) {
			logDebug(log, req, "RuleSet", "Resource not found")
			return ctrl.Result{}, nil
		}
		logError(log, req, "RuleSet", err, "Failed to GET")
		return ctrl.Result{}, err
	}

	if apimeta.FindStatusCondition(ruleset.Status.Conditions, "Ready") == nil {
		patch := client.MergeFrom(ruleset.DeepCopy())
		setStatusProgressing(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "Reconciling", "Starting reconciliation")
		if err := r.Status().Patch(ctx, &ruleset, patch); err != nil {
			logError(log, req, "RuleSet", err, "Failed to patch initial status")
			return ctrl.Result{}, err
		}
	}

	logDebug(log, req, "RuleSet", "Aggregating rules from sources", "ruleCount", len(ruleset.Spec.Rules))
	var aggregatedRules strings.Builder
	for i, rule := range ruleset.Spec.Rules {
		logDebug(log, req, "RuleSet", "Processing rule source", "index", i, "configMapName", rule.Name)
		logDebug(log, req, "RuleSet", "Fetching ConfigMap", "configMapName", rule.Name, "configMapNamespace", ruleset.Namespace)
		var cm corev1.ConfigMap
		if err := r.Get(ctx, types.NamespacedName{
			Name:      rule.Name,
			Namespace: ruleset.Namespace,
		}, &cm); err != nil {
			if errors.IsNotFound(err) {
				logInfo(log, req, "RuleSet", "ConfigMap not found", "configMapName", rule.Name)
				patch := client.MergeFrom(ruleset.DeepCopy())
				msg := fmt.Sprintf("Referenced ConfigMap %s does not exist", rule.Name)
				r.Recorder.Eventf(&ruleset, nil, "Warning", "ConfigMapNotFound", "Reconcile", msg)
				setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "ConfigMapNotFound", msg)
				if updateErr := r.Status().Patch(ctx, &ruleset, patch); updateErr != nil {
					logError(log, req, "RuleSet", updateErr, "Failed to patch status")
				}

				return ctrl.Result{Requeue: true}, nil
			}
			logError(log, req, "RuleSet", err, "Failed to get ConfigMap", "configMapName", rule.Name)

			patch := client.MergeFrom(ruleset.DeepCopy())
			msg := fmt.Sprintf("Failed to access ConfigMap %s: %v", rule.Name, err)
			r.Recorder.Eventf(&ruleset, nil, "Warning", "ConfigMapAccessError", "Reconcile", msg)
			setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "ConfigMapAccessError", msg)
			if updateErr := r.Status().Patch(ctx, &ruleset, patch); updateErr != nil {
				logError(log, req, "RuleSet", updateErr, "Failed to patch status")
			}

			return ctrl.Result{}, err
		}

		data, ok := cm.Data["rules"]
		if !ok {
			err := fmt.Errorf("ConfigMap %s missing 'rules' key", rule.Name)
			logError(log, req, "RuleSet", err, "ConfigMap missing 'rules' key", "configMapName", rule.Name)

			patch := client.MergeFrom(ruleset.DeepCopy())
			msg := fmt.Sprintf("ConfigMap %s is missing required 'rules' key", rule.Name)
			r.Recorder.Eventf(&ruleset, nil, "Warning", "InvalidConfigMap", "Reconcile", msg)
			setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "InvalidConfigMap", msg)
			if updateErr := r.Status().Patch(ctx, &ruleset, patch); updateErr != nil {
				logError(log, req, "RuleSet", updateErr, "Failed to patch status")
			}

			return ctrl.Result{}, err
		}

		if cm.Annotations["coraza.io/validation"] != "false" {
			conf := coraza.NewWAFConfig()
			if _, err := coraza.NewWAF(conf.WithDirectives(data)); err != nil {
				patch := client.MergeFrom(ruleset.DeepCopy())
				msg := fmt.Sprintf("ConfigMap %s doesn't contain valid rules:\n%v", rule.Name, err)
				r.Recorder.Eventf(&ruleset, nil, "Warning", "InvalidConfigMap", "Reconcile", msg)
				setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "InvalidConfigMap", msg)
				if updateErr := r.Status().Patch(ctx, &ruleset, patch); updateErr != nil {
					logError(log, req, "RuleSet", updateErr, "Failed to patch status")
				}

				return ctrl.Result{}, err
			}
		}

		aggregatedRules.WriteString(data)
		if i < len(ruleset.Spec.Rules)-1 {
			aggregatedRules.WriteString("\n")
		}
	}

	logDebug(log, req, "RuleSet", "Storing aggregated rules in cache")
	cacheKey := fmt.Sprintf("%s/%s", ruleset.Namespace, ruleset.Name)
	r.Cache.Put(cacheKey, aggregatedRules.String())
	logInfo(log, req, "RuleSet", "Stored rules in cache", "cacheKey", cacheKey)

	patch := client.MergeFrom(ruleset.DeepCopy())
	msg := fmt.Sprintf("Successfully cached rules for %s/%s", ruleset.Namespace, ruleset.Name)
	r.Recorder.Eventf(&ruleset, nil, "Normal", "RulesCached", "Reconcile", msg)
	setStatusReady(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "RulesCached", msg)
	if err := r.Status().Patch(ctx, &ruleset, patch); err != nil {
		logError(log, req, "RuleSet", err, "Failed to patch status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
