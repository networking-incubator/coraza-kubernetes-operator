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
	// ruleSetDegradedReasonRejectedContent marks content this build's delivery
	// contract refuses: host-file directives, unsupported includes, or limits
	// the plugin would NACK. Unlike an ordinary invalid edit, an accepted
	// configuration of this content must not keep serving.
	ruleSetDegradedReasonRejectedContent = "RejectedContent"
	// ruleSetDegradedReasonContentTooLarge marks rule text and RuleData that
	// together exceed the plugin's configuration limit. Like an ordinary
	// invalid edit, it keeps the content accepted before.
	ruleSetDegradedReasonContentTooLarge = "ContentTooLarge"
	ruleSetReadyReasonRulesAccepted      = "RulesAccepted"
)

// errRuleSetRulesInvalid indicates aggregate validation failed and the RuleSet
// status was patched to Degraded. Reconcile should stop without error.
var errRuleSetRulesInvalid = errors.New("aggregated rules failed validation")

// validateAggregatedRules checks the aggregated rule set against what
// envoy-coraza reads differently from Coraza and, for content not accepted
// yet, against the cluster rule policy, then compiles it with Coraza. The
// caller has already run the directive and Include guard
// (validation.ValidateIncludes), which must also cover accepted content.
// Sets Degraded status and emits Warning events on failure.
// The returned duration covers validation only (not status patches).
func (r *RuleSetReconciler) validateAggregatedRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	rejectedContentHash string,
	content assemble.Content,
) (time.Duration, error) {
	start := time.Now()
	if err := validation.CheckGatewayParity(content.Rules, content.DataFiles); err != nil {
		return time.Since(start), r.markParityInvalid(ctx, log, req, ruleset, err)
	}
	// The cluster rule policy applies to content the RuleSet has not accepted
	// yet, so a tightened policy takes effect on the next content change.
	if accepted := ruleset.Status.AcceptedContent; accepted == nil || accepted.ContentHash != rejectedContentHash {
		if err := validation.CheckRulePolicy(content.Rules, content.DataFiles, r.RulePolicy); err != nil {
			return time.Since(start), r.markRulesInvalid(ctx, log, req, ruleset, ruleSetDegradedReasonInvalidRuleSet, rejectedContentHash, err)
		}
	}
	if err := r.compile(req.NamespacedName, rejectedContentHash, content); err != nil {
		return time.Since(start), r.markRulesInvalid(ctx, log, req, ruleset, ruleSetDegradedReasonInvalidRuleSet, rejectedContentHash, err)
	}
	return time.Since(start), nil
}

// compile compiles content with Coraza, once per content: the result only
// depends on the content, and compiling costs time and memory that grow with
// it.
func (r *RuleSetReconciler) compile(key types.NamespacedName, contentHash string, content assemble.Content) error {
	results := r.contentResults(key, contentHash)
	if !results.compiled {
		results.compiled = true
		if waf, err := coraza.NewWAF(validation.BuildWAFConfig(content.Rules, content.DataFiles)); err != nil {
			results.compileErr = truncateMessage(validation.SanitizeErrorMessage(err).Error(), maxResultMessageBytes)
		} else {
			validation.CloseWAF(waf)
		}
		r.storeContentResults(key, results)
	}
	if results.compileErr != "" {
		return errors.New(results.compileErr)
	}
	return nil
}

// markRulesInvalid patches Degraded (unless the condition is already current)
// and returns errRuleSetRulesInvalid so the caller stops reconciling.
func (r *RuleSetReconciler) markRulesInvalid(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	reason string,
	rejectedContentHash string,
	cause error,
) error {
	// Only drop an accepted record for the exact content this build rejects:
	// legacy accepted content that now fails validation. A different record is
	// the last accepted RuleSet content and is kept for status/audit. The Engine
	// separately clears its own accepted configuration when it observes
	// RejectedContent for the current generation.
	clearAccepted := func() {
		if accepted := ruleset.Status.AcceptedContent; accepted != nil && accepted.ContentHash == rejectedContentHash {
			ruleset.Status.AcceptedContent = nil
		}
	}
	if err := r.markDegraded(ctx, log, req, ruleset, reason, invalidRuleSetMessage(cause), clearAccepted); err != nil {
		return err
	}
	return errRuleSetRulesInvalid
}

// markParityInvalid patches Degraded with InvalidRuleSet for content that
// fails the gateway-parity checks and keeps status.acceptedContent, so content
// a RuleSet accepted earlier keeps its record and the Engine its configuration.
func (r *RuleSetReconciler) markParityInvalid(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	cause error,
) error {
	if err := r.markDegraded(ctx, log, req, ruleset, ruleSetDegradedReasonInvalidRuleSet, invalidRuleSetMessage(cause)); err != nil {
		return err
	}
	return errRuleSetRulesInvalid
}

// markDegraded patches Degraded unless the same reason and message are already
// current. It keeps status.acceptedContent unless mutate drops it in the patch.
func (r *RuleSetReconciler) markDegraded(ctx context.Context, log logr.Logger, req ctrl.Request, ruleset *wafv1alpha1.RuleSet, reason, msg string, mutate ...func()) error {
	msg = truncateMessage(msg, maxConditionMessageBytes)
	if isConditionCurrent(ruleset.Status.Conditions, conditionDegraded, reason, ruleset.Generation) &&
		degradedConditionMessage(ruleset, reason) == msg {
		return nil
	}
	return patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, reason, msg, mutate...)
}

// invalidRuleSetMessage is the Degraded message of a RuleSet whose content
// failed validation with cause.
func invalidRuleSetMessage(cause error) string {
	return fmt.Sprintf("Ruleset is invalid\n%v", validation.SanitizeErrorMessage(cause))
}

// degradedConditionMessage returns the current Degraded message for reason, so
// a new distinct error is not hidden by the generation-only idempotency guard.
func degradedConditionMessage(ruleset *wafv1alpha1.RuleSet, reason string) string {
	condition := apimeta.FindStatusCondition(ruleset.Status.Conditions, conditionDegraded)
	if condition == nil || condition.Reason != reason {
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
