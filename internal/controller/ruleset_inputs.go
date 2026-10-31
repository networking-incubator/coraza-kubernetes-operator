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
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/corazawaf/coraza/v3"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

// loadRuleSetInputs resolves a RuleSet's RuleSources and RuleData into
// assembler inputs, preserving spec order and UID/generation. It performs plain
// Gets and has no status or requeue semantics.
func loadRuleSetInputs(ctx context.Context, reader client.Reader, ruleset *wafv1alpha1.RuleSet) (assemble.Inputs, error) {
	inputs := assemble.Inputs{
		RuleSetUID:       ruleset.UID,
		RuleSetName:      ruleset.Name,
		Namespace:        ruleset.Namespace,
		EmptyRulesPolicy: assemble.EmptyRulesPolicy(ruleset.Spec.EmptyRulesPolicy),
	}

	for _, ref := range ruleset.Spec.Sources {
		var source wafv1alpha1.RuleSource
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ref.Name}, &source); err != nil {
			return assemble.Inputs{}, fmt.Errorf("get RuleSource %s: %w", ref.Name, err)
		}
		inputs.Sources = append(inputs.Sources, assemble.SourceInput{
			Name:       source.Name,
			UID:        source.UID,
			Generation: source.Generation,
			Rules:      source.Spec.Rules,
		})
	}

	for _, ref := range ruleset.Spec.Data {
		var data wafv1alpha1.RuleData
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ref.Name}, &data); err != nil {
			return assemble.Inputs{}, fmt.Errorf("get RuleData %s: %w", ref.Name, err)
		}
		inputs.Data = append(inputs.Data, assemble.DataInput{
			Name:       data.Name,
			UID:        data.UID,
			Generation: data.Generation,
			Files:      data.Spec.Files,
		})
	}

	return inputs, nil
}

// engineInputsChanged reports whether the Engine, its RuleSet, or any resolved
// RuleSet input changed after they were read. A changed read must never be
// accepted as if it were one consistent configuration.
func (r *EngineReconciler) engineInputsChanged(ctx context.Context, engine *wafv1alpha1.Engine, ruleset *wafv1alpha1.RuleSet, inputs assemble.Inputs) (bool, error) {
	var currentEngine wafv1alpha1.Engine
	if err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}, &currentEngine); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if currentEngine.UID != engine.UID || currentEngine.Generation != engine.Generation {
		return true, nil
	}

	var currentRuleSet wafv1alpha1.RuleSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ruleset.Name}, &currentRuleSet); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if currentRuleSet.UID != ruleset.UID || currentRuleSet.Generation != ruleset.Generation {
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

// -----------------------------------------------------------------------------
// RuleSet Validation
// -----------------------------------------------------------------------------

const (
	ruleSetDegradedReasonInvalidRuleSet = "InvalidRuleSet"
	ruleSetReadyReasonRulesAccepted     = "RulesAccepted"
)

// errRuleSetRulesInvalid indicates aggregate Coraza validation failed and the
// RuleSet status was patched to Degraded. Reconcile should stop without error.
var errRuleSetRulesInvalid = errors.New("aggregated rules failed Coraza validation")

// validateAggregatedRules compiles the aggregated rule set with Coraza. The
// caller has already run the directive and Include guard
// (validation.ValidateIncludes), which must also cover accepted content.
// Sets Degraded status and emits Warning events on failure.
// The returned duration covers Coraza parsing only (not status patches).
func (r *RuleSetReconciler) validateAggregatedRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	rejectedContentHash string,
	conf coraza.WAFConfig,
) (time.Duration, error) {
	start := time.Now()
	waf, err := coraza.NewWAF(conf)
	if err != nil {
		return time.Since(start), r.markRulesInvalid(ctx, log, req, ruleset, rejectedContentHash, err)
	}
	validation.CloseWAF(waf)
	return time.Since(start), nil
}

// markRulesInvalid patches Degraded (unless the condition is already current)
// and returns errRuleSetRulesInvalid so the caller stops reconciling.
func (r *RuleSetReconciler) markRulesInvalid(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	rejectedContentHash string,
	cause error,
) error {
	msg := fmt.Sprintf("Ruleset is invalid\n%v", validation.SanitizeErrorMessage(cause))
	if isConditionCurrent(ruleset.Status.Conditions, conditionDegraded, ruleSetDegradedReasonInvalidRuleSet, ruleset.Generation) &&
		invalidRuleSetConditionMessage(ruleset) == msg {
		return errRuleSetRulesInvalid
	}
	// Only drop an accepted record for the exact content this build rejects:
	// legacy accepted content that now fails validation. A different record is
	// the last accepted RuleSet content and is kept for status/audit. The Engine
	// separately clears its own accepted configuration when it observes
	// InvalidRuleSet for the current generation.
	clearAccepted := func() {
		if accepted := ruleset.Status.AcceptedContent; accepted != nil && accepted.ContentHash == rejectedContentHash {
			ruleset.Status.AcceptedContent = nil
		}
	}
	if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, ruleSetDegradedReasonInvalidRuleSet, msg, clearAccepted); patchErr != nil {
		return patchErr
	}
	return errRuleSetRulesInvalid
}

// invalidRuleSetConditionMessage returns the current InvalidRuleSet message, so
// a new distinct error is not hidden by the generation-only idempotency guard.
func invalidRuleSetConditionMessage(ruleset *wafv1alpha1.RuleSet) string {
	condition := apimeta.FindStatusCondition(ruleset.Status.Conditions, conditionDegraded)
	if condition == nil || condition.Reason != ruleSetDegradedReasonInvalidRuleSet {
		return ""
	}
	return condition.Message
}

// rejectUnsupportedRules checks rules for IDs unsupported in WASM mode.
// Always emits a Warning event when unsupported rules are detected.
//
// When the AnnotationSkipUnsupportedRulesCheck annotation is "true", degradation
// is suppressed: returns (false, message, nil) so the caller can surface the
// detected rules in the Ready status without blocking reconciliation.
//
// Without the annotation, sets Degraded status and returns (true, "", nil).
func (r *RuleSetReconciler) rejectUnsupportedRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	rules string,
) (bool, string, error) {
	unsupported := rulesets.CheckUnsupportedRules(rules)
	if len(unsupported) == 0 {
		return false, "", nil
	}

	msg := rulesets.FormatUnsupportedMessage(unsupported)
	logInfo(log, req, "RuleSet", "RuleSet contains unsupported rules", "count", len(unsupported))

	if ruleset.Annotations[wafv1alpha1.AnnotationSkipUnsupportedRulesCheck] == "true" {
		logDebug(log, req, "RuleSet", "Unsupported rules check overridden by annotation; not degrading")
		r.Recorder.Eventf(ruleset, nil, "Warning", "UnsupportedRules", "Reconcile", truncateEventNote(msg))
		return false, msg, nil
	}

	if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "UnsupportedRules", msg); patchErr != nil {
		return true, "", patchErr
	}

	return true, "", nil
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Reference Validation
// -----------------------------------------------------------------------------

// findDuplicateReferences checks for duplicate RuleSource names in spec.sources
// and duplicate RuleData names in spec.data. Returns a descriptive message if
// any duplicates are found, or empty string if all references are unique.
func findDuplicateReferences(ruleset *wafv1alpha1.RuleSet) string {
	var msgs []string

	if dups := findDuplicateNames(ruleset.Spec.Sources, func(s wafv1alpha1.SourceReference) string { return s.Name }); len(dups) > 0 {
		msgs = append(msgs, fmt.Sprintf("spec.sources contains duplicate RuleSource name(s): %s", strings.Join(dups, ", ")))
	}

	if dups := findDuplicateNames(ruleset.Spec.Data, func(d wafv1alpha1.DataReference) string { return d.Name }); len(dups) > 0 {
		msgs = append(msgs, fmt.Sprintf("spec.data contains duplicate RuleData name(s): %s", strings.Join(dups, ", ")))
	}

	return strings.Join(msgs, "; ")
}

// findDuplicateNames returns the names that appear more than once in items.
func findDuplicateNames[T any](items []T, name func(T) string) []string {
	seen := make(map[string]int, len(items))
	for _, item := range items {
		seen[name(item)]++
	}

	var dups []string
	for n, count := range seen {
		if count > 1 {
			dups = append(dups, n)
		}
	}
	sort.Strings(dups)
	return dups
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Data Loading
// -----------------------------------------------------------------------------

// loadData fetches all RuleData objects referenced by the RuleSet and returns
// them in spec order. merge/validation is performed by assemble.Assemble.
func (r *RuleSetReconciler) loadData(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
) ([]assemble.DataInput, bool, error) {
	if len(ruleset.Spec.Data) == 0 {
		return nil, false, nil
	}

	logInfo(log, req, "RuleSet", "Loading data", "dataCount", len(ruleset.Spec.Data))

	dataInputs := make([]assemble.DataInput, 0, len(ruleset.Spec.Data))
	for _, ref := range ruleset.Spec.Data {
		var rd wafv1alpha1.RuleData
		if err := r.Get(ctx, types.NamespacedName{
			Name:      ref.Name,
			Namespace: ruleset.Namespace,
		}, &rd); err != nil {
			if apierrors.IsNotFound(err) {
				logInfo(log, req, "RuleSet", "Referenced RuleData not found; waiting for it to appear", "ruleDataName", ref.Name)
				r.Metrics.ForgetRuleData(ruleset.Namespace, ref.Name)
				msg := fmt.Sprintf("Referenced RuleData %s does not exist", ref.Name)
				if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleDataNotFound", msg); patchErr != nil {
					return nil, true, patchErr
				}
				return nil, true, nil
			}
			logError(log, req, "RuleSet", err, "Failed to get RuleData", "ruleDataName", ref.Name)
			msg := fmt.Sprintf("Failed to access RuleData %s: %v", ref.Name, err)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleDataAccessError", msg); patchErr != nil {
				return nil, true, patchErr
			}
			return nil, true, err
		}

		if !isConditionCurrent(rd.Status.Conditions, conditionReady, "Loaded", rd.Generation) {
			rdReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: rd.Name, Namespace: rd.Namespace}}
			if patchErr := patchReady(ctx, r.Status(), r.Recorder, log, rdReq, "RuleData", &rd, &rd.Status.Conditions, rd.Generation, "Loaded", "Data loaded successfully"); patchErr != nil {
				return nil, true, patchErr
			}
		}
		r.Metrics.RecordRuleData(&rd)

		dataInputs = append(dataInputs, assemble.DataInput{
			Name:       rd.Name,
			UID:        rd.UID,
			Generation: rd.Generation,
			Files:      rd.Spec.Files,
		})
	}

	return dataInputs, false, nil
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Source Loading
// -----------------------------------------------------------------------------

// loadSources fetches all RuleSource objects referenced by the RuleSet and
// returns them in spec order. Per-source Coraza validation and RuleSource
// status are owned by RuleSourceReconciler; this function gates on RuleSource
// status and returns a requeue interval while sources are not ready.
func (r *RuleSetReconciler) loadSources(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
) (sources []assemble.SourceInput, done bool, requeueAfter time.Duration, err error) {
	logInfo(log, req, "RuleSet", "Loading sources", "sourceCount", len(ruleset.Spec.Sources))

	sources = make([]assemble.SourceInput, 0, len(ruleset.Spec.Sources))
	var pendingSources []string

	for _, src := range ruleset.Spec.Sources {
		var rs wafv1alpha1.RuleSource
		if err := r.Get(ctx, types.NamespacedName{
			Name:      src.Name,
			Namespace: ruleset.Namespace,
		}, &rs); err != nil {
			if apierrors.IsNotFound(err) {
				logInfo(log, req, "RuleSet", "Referenced RuleSource not found; waiting for it to appear", "ruleSourceName", src.Name)
				msg := fmt.Sprintf("Referenced RuleSource %s does not exist", src.Name)
				if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleSourceNotFound", msg); patchErr != nil {
					return nil, true, 0, patchErr
				}
				return nil, true, 0, nil
			}
			logError(log, req, "RuleSet", err, "Failed to get RuleSource", "ruleSourceName", src.Name)
			msg := fmt.Sprintf("Failed to access RuleSource %s: %v", src.Name, err)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleSourceAccessError", msg); patchErr != nil {
				return nil, true, 0, patchErr
			}
			return nil, true, 0, err
		}

		skipFragmentValidation := rs.Annotations[wafv1alpha1.AnnotationSkipValidation] == "false"

		if ruleSourceInvalidForGeneration(rs.Status.Conditions, rs.Generation) {
			msg := fmt.Sprintf("Referenced RuleSource %s has invalid rules (see RuleSource status for details)", rs.Name)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "ReferencedRuleSourceInvalid", msg); patchErr != nil {
				return nil, true, 0, patchErr
			}
			return nil, true, 0, nil
		}

		if !ruleSourceValidatedForGeneration(rs.Status.Conditions, rs.Generation, skipFragmentValidation) {
			pendingSources = append(pendingSources, src.Name)
		}

		sources = append(sources, assemble.SourceInput{
			Name:       rs.Name,
			UID:        rs.UID,
			Generation: rs.Generation,
			Rules:      rs.Spec.Rules,
		})
	}

	if len(pendingSources) > 0 {
		msg := fmt.Sprintf("Waiting for RuleSource validation: %s", strings.Join(pendingSources, ", "))
		if patchErr := r.patchRuleSetAwaitingRuleSources(ctx, log, req, ruleset, msg); patchErr != nil {
			return nil, true, 0, patchErr
		}
		return nil, true, ruleSourceValidationRequeue, nil
	}

	return sources, false, 0, nil
}
