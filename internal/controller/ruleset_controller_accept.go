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

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

// -----------------------------------------------------------------------------
// RuleSetReconciler - Content Acceptance
// -----------------------------------------------------------------------------

// acceptRules records the content hash of the assembled RuleSet and marks it
// Ready in one status patch. The inputs are re-read first: if the RuleSet, any
// RuleSource, or any RuleData changed since they were loaded, nothing is
// accepted and the RuleSet is requeued.
func (r *RuleSetReconciler) acceptRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	content assemble.Content,
	inputs assemble.Inputs,
	unsupportedMsg string,
) (ctrl.Result, error) {
	hash, err := render.RuleSetContentHash(content)
	if err != nil {
		return ctrl.Result{}, err
	}

	changed, err := r.inputsChanged(ctx, ruleset, inputs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		logInfo(log, req, "RuleSet", "Inputs changed before acceptance; requeueing")
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}

	statusMsg := buildAcceptReadyMessage(ruleset.Namespace, ruleset.Name, unsupportedMsg)
	if err := patchRuleSetAccepted(ctx, r.Status(), r.Recorder, log, req, ruleset, hash, statusMsg); err != nil {
		return ctrl.Result{}, err
	}
	logInfo(log, req, "RuleSet", "Accepted rules", "contentHash", hash)
	return ctrl.Result{}, nil
}

// inputsChanged reports whether the RuleSet or any of its resolved inputs
// changed after they were read into inputs.
func (r *RuleSetReconciler) inputsChanged(ctx context.Context, ruleset *wafv1alpha1.RuleSet, inputs assemble.Inputs) (bool, error) {
	var current wafv1alpha1.RuleSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ruleset.Name}, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if current.UID != ruleset.UID || current.Generation != ruleset.Generation {
		return true, nil
	}

	for _, input := range inputs.Sources {
		var source wafv1alpha1.RuleSource
		if err := r.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: input.Name}, &source); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		if source.UID != input.UID || source.Generation != input.Generation {
			return true, nil
		}
	}

	for _, input := range inputs.Data {
		var data wafv1alpha1.RuleData
		if err := r.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: input.Name}, &data); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		if data.UID != input.UID || data.Generation != input.Generation {
			return true, nil
		}
	}

	return false, nil
}

// isRuleSetContentAccepted reports whether status.acceptedContent already
// records contentHash for the RuleSet's current generation.
func isRuleSetContentAccepted(ruleset *wafv1alpha1.RuleSet, contentHash string) bool {
	accepted := ruleset.Status.AcceptedContent
	return accepted != nil &&
		accepted.ObservedGeneration == ruleset.Generation &&
		accepted.ContentHash == contentHash
}

// patchRuleSetAccepted records the accepted content and marks the RuleSet
// Ready in a single status patch, so a Ready condition is never observed
// without the matching accepted record.
func patchRuleSetAccepted(
	ctx context.Context,
	statusWriter client.StatusWriter,
	recorder events.EventRecorder,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	contentHash, message string,
) error {
	recorder.Eventf(ruleset, nil, "Normal", ruleSetReadyReasonRulesAccepted, "Reconcile", truncateEventNote(message))
	patch := client.MergeFrom(ruleset.DeepCopy())
	before := snapshotConditions(ruleset.Status.Conditions)
	ruleset.Status.AcceptedContent = &wafv1alpha1.RuleSetAcceptedContent{
		ObservedGeneration: ruleset.Generation,
		ContentHash:        contentHash,
	}
	applyStatusReady(&ruleset.Status.Conditions, ruleset.Generation, ruleSetReadyReasonRulesAccepted, message)
	if err := statusWriter.Patch(ctx, ruleset, patch); err != nil {
		logAPIError(log, req, "RuleSet", err, "Failed to patch status", ruleset)
		return err
	}
	logConditionTransitions(log, req, "RuleSet", before, ruleset.Status.Conditions)
	return nil
}
