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
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/corazawaf/coraza/v3"
	"github.com/go-logr/logr"
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
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/pkg/utils"
)

var (
	sanitizeFilePath = regexp.MustCompile(`open (.+): no such file or directory`)
)

// -----------------------------------------------------------------------------
// RuleSet Controller - RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=waf.k8s.coraza.io,resources=rulesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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
		For(&wafv1alpha1.RuleSet{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			annotationChangedPredicate(wafv1alpha1.AnnotationSkipUnsupportedRulesCheck),
		))).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findRuleSetsForConfigMap),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findRuleSetsForSecret),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
				secret, ok := object.(*corev1.Secret)
				if !ok {
					return false
				}
				return secret.Type == wafv1alpha1.RuleDataSecretType
			})),
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

	if ruleset.Status == nil {
		ruleset.Status = &wafv1alpha1.RuleSetStatus{}
	}
	if apimeta.FindStatusCondition(ruleset.Status.Conditions, "Ready") == nil {
		patch := client.MergeFrom(ruleset.DeepCopy())
		setStatusProgressing(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "Reconciling", "Starting reconciliation")
		if err := r.Status().Patch(ctx, &ruleset, patch); err != nil {
			logError(log, req, "RuleSet", err, "Failed to patch initial status")
			return ctrl.Result{}, err
		}
	}

	// Load RuleData Secret first if configured, so we can check if missing data files
	// actually exist in it during per-ConfigMap validation
	var ruleData string
	if ruleset.Spec.RuleData != nil {
		ruleData = *ruleset.Spec.RuleData
	}
	var secretData map[string][]byte
	if ruleData != "" {
		var found bool
		var err error
		logDebug(log, req, "RuleSet", "Fetching data secret", "secretName", ruleData, "secretNamespace", ruleset.Namespace)
		secretData, found, err = r.getDataSecret(ctx, ruleData, ruleset.Namespace)
		if err != nil {
			if found {
				// Secret was found but is of wrong type
				logError(log, req, "RuleSet", err, "Failed to get RuleData", "secretName", ruleData)
				patch := client.MergeFrom(ruleset.DeepCopy())
				msg := fmt.Sprintf("Failed to use RuleData secret %s: %v", ruleData, err)
				r.Recorder.Eventf(&ruleset, nil, "Warning", "RuleDataSecretTypeMismatch", "Reconcile", msg)
				setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "RuleDataSecretTypeMismatch", msg)
				if updateErr := r.Status().Patch(ctx, &ruleset, patch); updateErr != nil {
					logError(log, req, "RuleSet", updateErr, "Failed to patch status")
				}
				return ctrl.Result{}, nil
			}
			logError(log, req, "RuleSet", err, "Failed to get RuleData", "secretName", ruleData)
			patch := client.MergeFrom(ruleset.DeepCopy())
			msg := fmt.Sprintf("Failed to access RuleData secret %s: %v", ruleData, err)
			r.Recorder.Eventf(&ruleset, nil, "Warning", "SecretAccessError", "Reconcile", msg)
			setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "SecretAccessError", msg)
			if updateErr := r.Status().Patch(ctx, &ruleset, patch); updateErr != nil {
				logError(log, req, "RuleSet", updateErr, "Failed to patch status")
			}
			return ctrl.Result{}, err
		}
		if !found {
			logInfo(log, req, "RuleSet", "Secret not found", "secretName", ruleData)
			patch := client.MergeFrom(ruleset.DeepCopy())
			msg := fmt.Sprintf("Referenced Secret %s does not exist", ruleData)
			r.Recorder.Eventf(&ruleset, nil, "Warning", "SecretNotFound", "Reconcile", msg)
			setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "SecretNotFound", msg)
			if updateErr := r.Status().Patch(ctx, &ruleset, patch); updateErr != nil {
				logError(log, req, "RuleSet", updateErr, "Failed to patch status")
			}
			// Do not requeue; rely on future Secret events to trigger reconciliation when it appears
			return ctrl.Result{}, nil
		}
	}

	logDebug(log, req, "RuleSet", "Aggregating rules from sources", "ruleCount", len(ruleset.Spec.Rules))
	var aggregatedRules strings.Builder
	aggregatedErrors := make([]error, 0)

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
				// Do not try to reconcile, wait a configmap to appear again
				return ctrl.Result{}, nil
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
			conf := coraza.NewWAFConfig().WithDirectives(data)
			if _, err := coraza.NewWAF(conf); err != nil {
				// If the validation error is due to a missing data file, check if that file
				// is actually present in the RuleData Secret. Only skip this per-ConfigMap
				// validation error if the file will be available during aggregated validation.
				if shouldSkipMissingFileError(err, secretData) {
					logDebug(log, req, "RuleSet", "Skipping per-ConfigMap validation error for missing data file present in RuleData", "configMapName", rule.Name, "error", err.Error())
				} else {
					aggregatedErrors = append(aggregatedErrors, fmt.Errorf("ConfigMap %s doesn't contain valid rules: %w", rule.Name, sanitizeErrorMessage(err)))
				}
			}
		}

		// Write the rules anyway to the buffer, so we can validate it as a single RuleSet
		aggregatedRules.WriteString(data)
		if i < len(ruleset.Spec.Rules)-1 {
			aggregatedRules.WriteString("\n")
		}
	}

	fsRules := getDataFilesystem(secretData)

	conf := coraza.NewWAFConfig().WithDirectives(aggregatedRules.String())
	if fsRules != nil {
		conf = conf.WithRootFS(fsRules)
	}
	if err := r.validateAggregatedRules(ctx, log, req, &ruleset, conf, aggregatedErrors); err != nil {
		return ctrl.Result{}, err
	}

	// Check for rules known to be unsupported in the current WASM engine mode.
	// This runs after Coraza syntax validation succeeds so that syntax errors
	// are reported first; incompatible rules are a separate concern.
	foundUnsupportedRules, unsupportedMsg, err := r.rejectUnsupportedRules(ctx, log, req, &ruleset, aggregatedRules.String())
	if err != nil {
		return ctrl.Result{}, err
	}
	if foundUnsupportedRules {
		return ctrl.Result{}, nil // Do not proceed with caching if unsupported rules are present
	}

	logDebug(log, req, "RuleSet", "Storing aggregated rules in cache")

	cacheKey := fmt.Sprintf("%s/%s", ruleset.Namespace, ruleset.Name)
	// NOTE: The data stored in the cache (including any RuleData sourced from a Secret)
	// is served by the cache HTTP server for consumption by the WASM plugin and must
	// therefore not contain sensitive or credential material. Treat the cache server
	// endpoint as internal / trusted-only in deployments.
	r.Cache.Put(cacheKey, aggregatedRules.String(), secretData)
	logInfo(log, req, "RuleSet", "Stored rules in cache", "cacheKey", cacheKey)

	patch := client.MergeFrom(ruleset.DeepCopy())
	eventMsg := fmt.Sprintf("Successfully cached rules for %s/%s", ruleset.Namespace, ruleset.Name)
	statusMsg := buildCacheReadyMessage(ruleset.Namespace, ruleset.Name, unsupportedMsg)
	r.Recorder.Eventf(&ruleset, nil, "Normal", "RulesCached", "Reconcile", eventMsg)
	setStatusReady(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "RulesCached", statusMsg)
	if err := r.Status().Patch(ctx, &ruleset, patch); err != nil {
		logError(log, req, "RuleSet", err, "Failed to patch status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// RuleSet Validation
// -----------------------------------------------------------------------------

// validateAggregatedRules validates the aggregated rule set via Coraza.
// Sets Degraded status and emits Warning events on failure.
func (r *RuleSetReconciler) validateAggregatedRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	conf coraza.WAFConfig,
	aggregatedErrors []error,
) error {
	if _, err := coraza.NewWAF(conf); err != nil {
		msg := fmt.Sprintf("Ruleset is invalid\n%v", sanitizeErrorMessage(err))
		r.Recorder.Eventf(ruleset, nil, "Warning", "InvalidRuleSet", "Reconcile", msg)

		for _, cmapErr := range aggregatedErrors {
			r.Recorder.Eventf(ruleset, nil, "Warning", "InvalidConfigMap", "Reconcile", cmapErr.Error())
			msg = fmt.Sprintf("%s\n%v", msg, cmapErr)
		}

		patch := client.MergeFrom(ruleset.DeepCopy())
		setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "InvalidRuleSet", msg)
		if updateErr := r.Status().Patch(ctx, ruleset, patch); updateErr != nil {
			logError(log, req, "RuleSet", updateErr, "Failed to patch status")
			return updateErr
		}

		return sanitizeErrorMessage(err)
	}
	return nil
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
	r.Recorder.Eventf(ruleset, nil, "Warning", "UnsupportedRules", "Reconcile", msg)

	if ruleset.Annotations[wafv1alpha1.AnnotationSkipUnsupportedRulesCheck] == "true" {
		logDebug(log, req, "RuleSet", "Unsupported rules check overridden by annotation; not degrading")
		return false, msg, nil
	}

	patch := client.MergeFrom(ruleset.DeepCopy())
	setStatusConditionDegraded(log, req, "RuleSet", &ruleset.Status.Conditions, ruleset.Generation, "UnsupportedRules", msg)
	if updateErr := r.Status().Patch(ctx, ruleset, patch); updateErr != nil {
		logError(log, req, "RuleSet", updateErr, "Failed to patch status")
		return true, "", updateErr
	}

	return true, "", nil
}

// -----------------------------------------------------------------------------
// Private Utilities
// -----------------------------------------------------------------------------

// buildCacheReadyMessage constructs the Ready condition message after successful
// caching. When unsupportedMsg is non-empty (annotation override active), the
// detected unsupported rules are appended so they remain visible in the status.
func buildCacheReadyMessage(namespace, name, unsupportedMsg string) string {
	msg := fmt.Sprintf("Successfully cached rules for %s/%s", namespace, name)
	if unsupportedMsg != "" {
		msg += "\n[annotation override] " + unsupportedMsg
	}
	return msg
}

// getDataSecret fetches the named Secret and returns its data.
func (r *RuleSetReconciler) getDataSecret(ctx context.Context, name, namespace string) (map[string][]byte, bool, error) {
	var ruleData corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, &ruleData); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("an error occurred while fetching the secret: %w", err)
	}

	if ruleData.Type != wafv1alpha1.RuleDataSecretType {
		return nil, true, fmt.Errorf("the secret type must be of type %s", wafv1alpha1.RuleDataSecretType)
	}
	return ruleData.Data, true, nil
}

// getDataFilesystem converts secret data into an in-memory filesystem for Coraza.
// Returns nil if secretdata is nil.
func getDataFilesystem(secretdata map[string][]byte) fs.FS {
	if secretdata == nil {
		return nil
	}
	memfs := utils.NewMemFS()
	for filename, data := range secretdata {
		memfs.WriteFile(filename, data)
	}
	return memfs
}

func sanitizeErrorMessage(err error) error {
	matches := sanitizeFilePath.FindStringSubmatch(err.Error())

	if len(matches) < 2 {
		return err
	}

	// matches[1] is the full path. filepath.Base pulls the last element.
	fileName := filepath.Base(matches[1])

	return fmt.Errorf("open %s: data does not exist", fileName)

}

// shouldSkipMissingFileError reports whether a missing-file validation error should
// be skipped because the file is present in secretData.
func shouldSkipMissingFileError(err error, secretData map[string][]byte) bool {
	if secretData == nil {
		return false
	}

	matches := sanitizeFilePath.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return false
	}

	// Extract the filename from the error. matches[1] contains the full path.
	fileName := filepath.Base(matches[1])

	// Only skip if this file is actually present in the RuleData Secret
	_, exists := secretData[fileName]
	return exists
}
