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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

// -----------------------------------------------------------------------------
// RuleSourceReconciler - RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesources,verbs=get;list;watch
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesources/status,verbs=get;update;patch

// -----------------------------------------------------------------------------
// RuleSourceReconciler
// -----------------------------------------------------------------------------

// RuleSourceReconciler validates RuleSource spec.rules via Coraza and updates
// RuleSource status. RuleData (@pmFromFile) is not available here; aggregate
// validation on the RuleSet remains authoritative for file-backed rules.
type RuleSourceReconciler struct {
	client.Client
	Recorder events.EventRecorder
}

// SetupWithManager sets up the RuleSource controller.
func (r *RuleSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wafv1alpha1.RuleSource{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			annotationChangedPredicate(wafv1alpha1.AnnotationSkipValidation),
		))).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](
				1*time.Second,
				1*time.Minute,
			),
		}).
		Named("rulesource").
		Complete(r)
}

// Reconcile validates the RuleSource and patches its status.
func (r *RuleSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var rs wafv1alpha1.RuleSource
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logAPIError(log, req, "RuleSource", err, "Failed to GET", nil)
		return ctrl.Result{}, err
	}

	skipValidation := rs.Annotations[wafv1alpha1.AnnotationSkipValidation] == "false"
	if skipValidation {
		if isConditionCurrent(rs.Status.Conditions, conditionReady, ruleSourceReadyReasonValidationSkipped, rs.Generation) {
			return ctrl.Result{}, nil
		}
		if patchErr := patchReady(ctx, r.Status(), r.Recorder, log, req, "RuleSource", &rs, &rs.Status.Conditions, rs.Generation, ruleSourceReadyReasonValidationSkipped, "Per-fragment validation skipped by annotation"); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, nil
	}

	if err := validation.ValidateRuleSourceRules(rs.Spec.Rules, rs.Name, nil); err != nil {
		if !isConditionCurrent(rs.Status.Conditions, conditionDegraded, ruleSourceDegradedReasonInvalidRules, rs.Generation) {
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSource", &rs, &rs.Status.Conditions, rs.Generation, ruleSourceDegradedReasonInvalidRules, err.Error()); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
		}
		return ctrl.Result{}, nil
	}

	if isConditionCurrent(rs.Status.Conditions, conditionReady, ruleSourceReadyReasonValidated, rs.Generation) {
		return ctrl.Result{}, nil
	}
	if patchErr := patchReady(ctx, r.Status(), r.Recorder, log, req, "RuleSource", &rs, &rs.Status.Conditions, rs.Generation, ruleSourceReadyReasonValidated, "Rules validated successfully"); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	return ctrl.Result{}, nil
}
