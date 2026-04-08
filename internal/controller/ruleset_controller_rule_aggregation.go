package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// RuleSetReconciler - Rule Aggregation
// -----------------------------------------------------------------------------

// aggregateRulesFromSources fetches and validates each ConfigMap referenced by
// the RuleSet, building the combined rule string. Returns early with a non-zero
// Result on unrecoverable ConfigMap issues.
func (r *RuleSetReconciler) aggregateRulesFromSources(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	secretData map[string][]byte,
) (string, []error, bool, error) {
	logInfo(log, req, "RuleSet", "Aggregating rules from sources", "sourceCount", len(ruleset.Spec.Rules))

	var aggregatedRules strings.Builder
	aggregatedErrors := make([]error, 0)

	for i, rule := range ruleset.Spec.Rules {
		logDebug(log, req, "RuleSet", "Fetching ConfigMap", "index", i, "configMapName", rule.Name)

		data, skipValidation, done, err := r.fetchConfigMapRules(ctx, log, req, ruleset, rule.Name)
		if done || err != nil {
			return "", nil, done, err
		}

		if !skipValidation {
			if validationErr := validateConfigMapRules(data, rule.Name, secretData); validationErr != nil {
				logDebug(log, req, "RuleSet", "ConfigMap validation issue recorded", "configMapName", rule.Name, "error", validationErr.Error())
				aggregatedErrors = append(aggregatedErrors, validationErr)
			}
		}

		aggregatedRules.WriteString(data)
		if i < len(ruleset.Spec.Rules)-1 {
			aggregatedRules.WriteString("\n")
		}
	}

	return aggregatedRules.String(), aggregatedErrors, false, nil
}

// fetchConfigMapRules fetches a single ConfigMap and extracts its "rules" key.
// Returns the rules data, whether per-ConfigMap validation should be skipped
// (via the coraza.io/validation annotation), a ctrl.Result for early return,
// and any error.
func (r *RuleSetReconciler) fetchConfigMapRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	configMapName string,
) (string, bool, bool, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{
		Name:      configMapName,
		Namespace: ruleset.Namespace,
	}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			logInfo(log, req, "RuleSet", "Referenced ConfigMap not found; waiting for it to appear", "configMapName", configMapName)
			msg := fmt.Sprintf("Referenced ConfigMap %s does not exist", configMapName)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "ConfigMapNotFound", msg); patchErr != nil {
				return "", false, true, patchErr
			}
			return "", false, true, nil
		}
		logAPIError(log, req, "RuleSet", err, "Failed to get ConfigMap", ruleset, "configMapName", configMapName)
		msg := fmt.Sprintf("Failed to access ConfigMap %s: %v", configMapName, err)
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "ConfigMapAccessError", msg); patchErr != nil {
			return "", false, true, patchErr
		}
		return "", false, true, err
	}

	data, ok := cm.Data["rules"]
	if !ok {
		err := fmt.Errorf("ConfigMap %s missing 'rules' key", configMapName)
		logError(log, req, "RuleSet", err, "ConfigMap missing required 'rules' key", "configMapName", configMapName)
		msg := fmt.Sprintf("ConfigMap %s is missing required 'rules' key", configMapName)
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "InvalidConfigMap", msg); patchErr != nil {
			return "", false, true, patchErr
		}
		return "", false, true, err
	}

	skipValidation := cm.Annotations["coraza.io/validation"] == "false"
	if skipValidation {
		logDebug(log, req, "RuleSet", "Per-ConfigMap validation skipped via annotation", "configMapName", configMapName)
	}

	return data, skipValidation, false, nil
}
