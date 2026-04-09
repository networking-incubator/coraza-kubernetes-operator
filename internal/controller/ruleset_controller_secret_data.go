package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/memfs"
)

// -----------------------------------------------------------------------------
// RuleSetReconciler - Secret Data
// -----------------------------------------------------------------------------

// getDataSecret fetches the named Secret and returns its data.
// Returns a *secretNotFoundError when the Secret does not exist, or a
// *secretTypeMismatchError when the Secret exists but is not of the expected type.
func (r *RuleSetReconciler) getDataSecret(ctx context.Context, name, namespace string) (map[string][]byte, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &secretNotFoundError{name: name}
		}
		return nil, fmt.Errorf("failed to fetch secret %s: %w", name, err)
	}

	if secret.Type != wafv1alpha1.RuleDataSecretType {
		return nil, &secretTypeMismatchError{name: name}
	}

	return secret.Data, nil
}

// loadRuleDataSecret fetches the RuleData Secret referenced by the RuleSet.
// Returns the secret data, whether reconciliation should stop (done), and any error.
func (r *RuleSetReconciler) loadRuleDataSecret(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
) (map[string][]byte, bool, error) {
	var ruleDataName string
	if ruleset.Spec.RuleData != "" {
		ruleDataName = ruleset.Spec.RuleData
	}
	if ruleDataName == "" {
		return nil, false, nil
	}

	logDebug(log, req, "RuleSet", "Fetching RuleData secret", "secretName", ruleDataName, "secretNamespace", ruleset.Namespace)
	secretData, err := r.getDataSecret(ctx, ruleDataName, ruleset.Namespace)
	if err == nil {
		logDebug(log, req, "RuleSet", "RuleData secret loaded", "secretName", ruleDataName)
		return secretData, false, nil
	}

	var notFound *secretNotFoundError
	var typeMismatch *secretTypeMismatchError
	switch {
	case errors.As(err, &notFound):
		logInfo(log, req, "RuleSet", "Referenced Secret not found; waiting for it to appear", "secretName", ruleDataName)
		msg := fmt.Sprintf("Referenced Secret %s does not exist", ruleDataName)
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "SecretNotFound", msg); patchErr != nil {
			return nil, true, patchErr
		}
		return nil, true, nil
	case errors.As(err, &typeMismatch):
		logError(log, req, "RuleSet", err, "RuleData secret has wrong type", "secretName", ruleDataName)
		msg := fmt.Sprintf("Failed to use RuleData secret %s: %v", ruleDataName, err)
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RuleDataSecretTypeMismatch", msg); patchErr != nil {
			return nil, true, patchErr
		}
		return nil, true, nil
	default:
		logAPIError(log, req, "RuleSet", err, "Failed to access RuleData secret", ruleset, "secretName", ruleDataName)
		msg := fmt.Sprintf("Failed to access RuleData secret %s: %v", ruleDataName, err)
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "SecretAccessError", msg); patchErr != nil {
			return nil, true, patchErr
		}
		return nil, true, err
	}
}

// getDataFilesystem converts secret data into an in-memory filesystem for Coraza.
// Returns nil if secretdata is nil.
func getDataFilesystem(secretdata map[string][]byte) fs.FS {
	if secretdata == nil {
		return nil
	}

	mfs := memfs.NewMemFS()
	for filename, data := range secretdata {
		mfs.WriteFile(filename, data)
	}

	return mfs
}

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

// secretNotFoundError indicates the referenced Secret does not exist.
type secretNotFoundError struct {
	name string
}

func (e *secretNotFoundError) Error() string {
	return fmt.Sprintf("referenced Secret %s does not exist", e.name)
}

// secretTypeMismatchError indicates the Secret exists but has the wrong type.
type secretTypeMismatchError struct {
	name string
}

func (e *secretTypeMismatchError) Error() string {
	return fmt.Sprintf("the secret type must be of type %s", wafv1alpha1.RuleDataSecretType)
}
