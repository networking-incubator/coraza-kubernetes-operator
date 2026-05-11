package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

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

// loadData fetches all RuleData objects referenced by the RuleSet and merges
// their file maps. Last-listed wins on duplicate keys.
func (r *RuleSetReconciler) loadData(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
) (map[string][]byte, bool, error) {
	if len(ruleset.Spec.Data) == 0 {
		return nil, false, nil
	}

	logInfo(log, req, "RuleSet", "Loading data", "dataCount", len(ruleset.Spec.Data))

	dataFiles := make(map[string][]byte)
	for _, ref := range ruleset.Spec.Data {
		var rd wafv1alpha1.RuleData
		if err := r.Get(ctx, types.NamespacedName{
			Name:      ref.Name,
			Namespace: ruleset.Namespace,
		}, &rd); err != nil {
			if apierrors.IsNotFound(err) {
				logInfo(log, req, "RuleSet", "Referenced RuleData not found; waiting for it to appear", "ruleDataName", ref.Name)
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

		for k, v := range rd.Spec.Files {
			dataFiles[k] = []byte(v)
		}
	}

	return dataFiles, false, nil
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Source Loading
// -----------------------------------------------------------------------------

// loadSources fetches all RuleSource objects referenced by the RuleSet and
// concatenates their rules in order. Per-source Coraza validation and
// RuleSource status are owned by RuleSourceReconciler; this function gates on
// RuleSource status and returns a requeue interval while sources are not ready.
func (r *RuleSetReconciler) loadSources(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
) (aggregatedRules string, done bool, requeueAfter time.Duration, err error) {
	logInfo(log, req, "RuleSet", "Loading sources", "sourceCount", len(ruleset.Spec.Sources))

	type ruleFragment struct {
		name  string
		rules string
	}
	ruleFragments := make([]ruleFragment, 0, len(ruleset.Spec.Sources))
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
					return "", true, 0, patchErr
				}
				return "", true, 0, nil
			}
			logError(log, req, "RuleSet", err, "Failed to get RuleSource", "ruleSourceName", src.Name)
			msg := fmt.Sprintf("Failed to access RuleSource %s: %v", src.Name, err)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleSourceAccessError", msg); patchErr != nil {
				return "", true, 0, patchErr
			}
			return "", true, 0, err
		}

		skipFragmentValidation := rs.Annotations[wafv1alpha1.AnnotationSkipValidation] == "false"

		if ruleSourceInvalidForGeneration(rs.Status.Conditions, rs.Generation) {
			msg := fmt.Sprintf("Referenced RuleSource %s has invalid rules (see RuleSource status for details)", rs.Name)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "ReferencedRuleSourceInvalid", msg); patchErr != nil {
				return "", true, 0, patchErr
			}
			return "", true, 0, nil
		}

		if !ruleSourceValidatedForGeneration(rs.Status.Conditions, rs.Generation, skipFragmentValidation) {
			pendingSources = append(pendingSources, src.Name)
		}

		ruleFragments = append(ruleFragments, ruleFragment{
			name:  src.Name,
			rules: rs.Spec.Rules,
		})
	}

	if len(pendingSources) > 0 {
		msg := fmt.Sprintf("Waiting for RuleSource validation: %s", strings.Join(pendingSources, ", "))
		if patchErr := r.patchRuleSetAwaitingRuleSources(ctx, log, req, ruleset, msg); patchErr != nil {
			return "", true, 0, patchErr
		}
		return "", true, ruleSourceValidationRequeue, nil
	}

	var aggregated strings.Builder
	for i, frag := range ruleFragments {
		aggregated.WriteString(frag.rules)
		if i < len(ruleFragments)-1 {
			aggregated.WriteString("\n")
		}
	}

	return aggregated.String(), false, 0, nil
}
