package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// RuleSetReconciler - Cache Storage
// -----------------------------------------------------------------------------

// cacheRules stores the aggregated rules in the cache and patches the RuleSet
// status to Ready. Cache Put duration is observed only when the cache content
// was updated (not when status is patched for content already in the cache).
func (r *RuleSetReconciler) cacheRules(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	ruleset *wafv1alpha1.RuleSet,
	aggregatedRules string,
	dataFiles map[string][]byte,
	unsupportedMsg string,
) error {
	cacheKey := fmt.Sprintf("%s/%s", ruleset.Namespace, ruleset.Name)
	alreadyReady := isConditionCurrent(ruleset.Status.Conditions, conditionReady, ruleSetReadyReasonRulesCached, ruleset.Generation)

	cacheUpdated := false
	var cacheDur time.Duration
	if !r.Cache.EntryMatches(cacheKey, aggregatedRules, dataFiles) {
		cacheStart := time.Now()
		r.Cache.Put(cacheKey, aggregatedRules, dataFiles)
		cacheDur = time.Since(cacheStart)
		cacheUpdated = true
		logInfo(log, req, "RuleSet", "Stored rules in cache", "cacheKey", cacheKey)
	}

	if alreadyReady {
		return nil
	}

	statusMsg := buildCacheReadyMessage(ruleset.Namespace, ruleset.Name, unsupportedMsg)
	if err := patchReady(ctx, r.Status(), r.Recorder, log, req, "RuleSet", ruleset, &ruleset.Status.Conditions, ruleset.Generation, ruleSetReadyReasonRulesCached, statusMsg); err != nil {
		return err
	}
	if cacheUpdated {
		r.Metrics.ObserveCacheSet(ruleset.Namespace, cacheDur)
	}
	return nil
}
