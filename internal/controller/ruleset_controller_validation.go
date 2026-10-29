package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/corazawaf/coraza/v3"
	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
)

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
