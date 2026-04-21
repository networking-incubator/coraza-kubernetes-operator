package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/corazawaf/coraza/v3"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

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

		for k, v := range rd.Spec.Files {
			dataFiles[k] = []byte(v)
		}
	}

	return dataFiles, false, nil
}

// -----------------------------------------------------------------------------
// RuleSetReconciler - Source Loading
// -----------------------------------------------------------------------------

// loadSources fetches all RuleSource objects referenced by the RuleSet,
// concatenates their rules in order, and individually validates each fragment
// against the merged data files.
func (r *RuleSetReconciler) loadSources(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	dataFiles map[string][]byte,
) (string, []error, bool, error) {
	logInfo(log, req, "RuleSet", "Loading sources", "sourceCount", len(ruleset.Spec.Sources))

	type ruleFragment struct {
		name           string
		rules          string
		skipValidation bool
	}
	ruleFragments := make([]ruleFragment, 0, len(ruleset.Spec.Sources))

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
					return "", nil, true, patchErr
				}
				return "", nil, true, nil
			}
			logError(log, req, "RuleSet", err, "Failed to get RuleSource", "ruleSourceName", src.Name)
			msg := fmt.Sprintf("Failed to access RuleSource %s: %v", src.Name, err)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleSourceAccessError", msg); patchErr != nil {
				return "", nil, true, patchErr
			}
			return "", nil, true, err
		}

		skipValidation := rs.Annotations[wafv1alpha1.AnnotationSkipValidation] == "false"
		ruleFragments = append(ruleFragments, ruleFragment{
			name:           src.Name,
			rules:          rs.Spec.Rules,
			skipValidation: skipValidation,
		})
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

	return aggregatedRules.String(), aggregatedErrors, false, nil
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
