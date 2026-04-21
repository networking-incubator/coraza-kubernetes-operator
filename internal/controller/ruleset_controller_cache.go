package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// RuleSetReconciler - Cache Storage
// -----------------------------------------------------------------------------

// cacheEntryPayloadSize computes the byte size of a cache entry payload using
// the same accounting as RuleSetCache.TotalSize: len(rules) plus, for each
// data file, len(filename) + len(content).
func cacheEntryPayloadSize(rules string, dataFiles map[string][]byte) int {
	size := len(rules)
	for name, content := range dataFiles {
		size += len(name) + len(content)
	}
	return size
}

// cacheRules stores the aggregated rules in the cache and patches the RuleSet
// status to Ready. If the payload exceeds the per-instance budget
// (MaxPayloadSize), it rejects the entry and marks the RuleSet Degraded.
func (r *RuleSetReconciler) cacheRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	aggregatedRules string,
	secretData map[string][]byte,
	unsupportedMsg string,
) (ctrl.Result, error) {
	cacheKey := fmt.Sprintf("%s/%s", ruleset.Namespace, ruleset.Name)

	if r.MaxPayloadSize > 0 {
		payloadSize := cacheEntryPayloadSize(aggregatedRules, secretData)
		if payloadSize > r.MaxPayloadSize {
			msg := fmt.Sprintf(
				"Aggregated rules payload (%d bytes) exceeds maximum allowed size (%d bytes). Reduce the number or size of referenced ConfigMaps/Secrets.",
				payloadSize, r.MaxPayloadSize,
			)
			logError(log, req, "RuleSet", fmt.Errorf("payload %d bytes exceeds limit %d bytes", payloadSize, r.MaxPayloadSize),
				"Rejecting oversized cache entry", "cacheKey", cacheKey)
			if err := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "OversizedRules", msg); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// NOTE: The data stored in the cache (including any RuleData sourced from a Secret)
	// is served by the cache HTTP server for consumption by the WASM plugin and must
	// therefore not contain sensitive or credential material. Treat the cache server
	// endpoint as internal / trusted-only in deployments.
	r.Cache.Put(cacheKey, aggregatedRules, secretData)
	logInfo(log, req, "RuleSet", "Stored rules in cache", "cacheKey", cacheKey)

	statusMsg := buildCacheReadyMessage(ruleset.Namespace, ruleset.Name, unsupportedMsg)
	if err := patchReady(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, "RulesCached", statusMsg); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}
