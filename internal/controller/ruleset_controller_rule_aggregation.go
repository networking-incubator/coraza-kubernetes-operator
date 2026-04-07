package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/corazawaf/coraza/v3"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// RuleSetReconciler - Source Loading
// -----------------------------------------------------------------------------

// loadSources fetches all RuleSource objects referenced by the RuleSet and
// separates them into data files and aggregated rule text. Data sources are
// merged first (last-listed wins on duplicate keys), then Rule sources are
// concatenated in order and individually validated against the merged data.
func (r *RuleSetReconciler) loadSources(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
) (map[string][]byte, string, []error, bool, error) {
	logInfo(log, req, "RuleSet", "Loading sources", "sourceCount", len(ruleset.Spec.Sources))

	dataFiles := make(map[string][]byte)
	type ruleFragment struct {
		name           string
		rules          string
		skipValidation bool
	}
	var ruleFragments []ruleFragment

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
					return nil, "", nil, true, patchErr
				}
				return nil, "", nil, true, nil
			}
			logError(log, req, "RuleSet", err, "Failed to get RuleSource", "ruleSourceName", src.Name)
			msg := fmt.Sprintf("Failed to access RuleSource %s: %v", src.Name, err)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleSourceAccessError", msg); patchErr != nil {
				return nil, "", nil, true, patchErr
			}
			return nil, "", nil, true, err
		}

		switch rs.Spec.Type {
		case wafv1alpha1.RuleSourceTypeData:
			for k, v := range rs.Spec.Files {
				dataFiles[k] = []byte(v)
			}
		case wafv1alpha1.RuleSourceTypeRule:
			skipValidation := rs.Annotations[wafv1alpha1.AnnotationSkipValidation] == "false"
			ruleFragments = append(ruleFragments, ruleFragment{
				name:           src.Name,
				rules:          ptr.Deref(rs.Spec.Rules, ""),
				skipValidation: skipValidation,
			})
		default:
			msg := fmt.Sprintf("RuleSource %s has unknown type %q", src.Name, rs.Spec.Type)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "InvalidRuleSource", msg); patchErr != nil {
				return nil, "", nil, true, patchErr
			}
			return nil, "", nil, true, fmt.Errorf("%s", msg)
		}
	}

	if len(ruleFragments) == 0 {
		msg := "RuleSet must reference at least one RuleSource of type Rule"
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "NoRuleSources", msg); patchErr != nil {
			return nil, "", nil, true, patchErr
		}
		return nil, "", nil, true, nil
	}

	var dataMap map[string][]byte
	if len(dataFiles) > 0 {
		dataMap = dataFiles
	}

	var aggregatedRules strings.Builder
	aggregatedErrors := make([]error, 0)

	for i, frag := range ruleFragments {
		if !frag.skipValidation {
			if validationErr := validateRuleSourceRules(frag.rules, frag.name, dataMap); validationErr != nil {
				logDebug(log, req, "RuleSet", "RuleSource validation issue recorded", "ruleSourceName", frag.name, "error", validationErr.Error())
				aggregatedErrors = append(aggregatedErrors, validationErr)
			}
		}

		aggregatedRules.WriteString(frag.rules)
		if i < len(ruleFragments)-1 {
			aggregatedRules.WriteString("\n")
		}
	}

	return dataMap, aggregatedRules.String(), aggregatedErrors, false, nil
}

// validateRuleSourceRules validates a single RuleSource's rules via Coraza.
func validateRuleSourceRules(data, ruleSourceName string, dataFiles map[string][]byte) error {
	conf := coraza.NewWAFConfig().WithDirectives(data)
	if _, err := coraza.NewWAF(conf); err != nil {
		if shouldSkipMissingFileError(err, dataFiles) {
			return nil
		}
		return fmt.Errorf("RuleSource %s doesn't contain valid rules: %w", ruleSourceName, sanitizeErrorMessage(err))
	}
	return nil
}
