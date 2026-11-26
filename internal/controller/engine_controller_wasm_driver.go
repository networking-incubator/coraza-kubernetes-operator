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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

// -----------------------------------------------------------------------------
// Engine Controller - Istio RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=networking.istio.io,resources=envoyfilters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch

// -----------------------------------------------------------------------------
// Engine Controller - Istio Consts
// -----------------------------------------------------------------------------

const (
	telemetryRetryAfter         = 30 * time.Second
	ruleSetAcceptanceRetryAfter = 3 * time.Second
	bootstrapConvergeRetryAfter = 1 * time.Second

	// reasonConfigurationAccepted is the Ready reason for a provisioned Engine.
	reasonConfigurationAccepted = "ConfigurationAccepted"
	// reasonAwaitingRuleSetAcceptance means the Engine is waiting for the
	// referenced RuleSet to accept content for its current generation.
	reasonAwaitingRuleSetAcceptance = "AwaitingRuleSetAcceptance"
	// reasonTLSNotConfigured means the operator has no verified CA chain to
	// inject into the EnvoyFilter.
	reasonTLSNotConfigured = "TLSNotConfigured"
)

type engineRenderCacheKey struct {
	namespace             string
	name                  string
	uid                   types.UID
	ruleSetUID            types.UID
	contentHash           string
	emptyRulesPolicy      string
	failurePolicy         string
	enableFilterStateLogs bool
	driverType            string
	binary                render.BinaryRef
	vm                    render.VMSettings
}

type engineRenderCacheEntry struct {
	key              engineRenderCacheKey
	inputFingerprint string
	// contentLimits comes from content matching key.contentHash.
	contentLimits contentLimitSummary
	artifact      render.Artifact
	payloadSize   int
	resourceSize  int
}

// engineRenderCache reuses only successful renders with identical inputs and retains no ECDS payload.
// It is process-local, so a new binary starts with a miss and runs render.Render again.
type engineRenderCache struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]engineRenderCacheEntry
}

func (c *engineRenderCache) get(engineKey types.NamespacedName, key engineRenderCacheKey) (engineRenderCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[engineKey]
	return entry, ok && entry.key == key
}

func (c *engineRenderCache) set(engineKey types.NamespacedName, entry engineRenderCacheEntry) {
	entry.artifact.Payload = nil
	entry.artifact.Resource = nil

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[types.NamespacedName]engineRenderCacheEntry)
	}
	c.entries[engineKey] = entry
}

func (c *engineRenderCache) rememberInputFingerprint(
	engineKey types.NamespacedName,
	key engineRenderCacheKey,
	inputFingerprint string,
	limits contentLimitSummary,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[engineKey]
	if !ok || entry.key != key {
		return
	}
	entry.inputFingerprint = inputFingerprint
	entry.contentLimits = limits
	c.entries[engineKey] = entry
}

// renderEngineArtifact returns metadata-only artifacts on misses and hits. It
// measures payload/resource on misses, caches only their sizes, and rechecks
// limits on every call.
func (r *EngineReconciler) renderEngineArtifact(
	engineKey types.NamespacedName,
	input render.EngineInput,
	contentLimits contentLimitSummary,
	verifiedEntry *engineRenderCacheEntry,
) (render.Artifact, string, error) {
	key := engineRenderCacheKeyFor(input)
	var entry engineRenderCacheEntry
	var cacheHit bool
	if verifiedEntry != nil {
		if verifiedEntry.key != key {
			err := errors.New("verified render cache entry no longer matches Engine inputs")
			return render.Artifact{}, fmt.Sprintf("Failed to render Engine configuration: %v", err), err
		}
		entry, cacheHit = *verifiedEntry, true
	} else {
		entry, cacheHit = r.renderCache.get(engineKey, key)
	}
	if !cacheHit {
		artifact, err := render.Render(input)
		if err != nil {
			return render.Artifact{}, fmt.Sprintf("Failed to render Engine configuration: %v", err), err
		}
		entry = engineRenderCacheEntry{
			key:           key,
			contentLimits: contentLimits,
			artifact:      artifact,
			payloadSize:   len(artifact.Payload),
			resourceSize:  proto.Size(artifact.Resource),
		}
	}

	if err := validateRenderedPayloadSizes(entry.resourceSize, entry.payloadSize, r.Limits.WithDefaults()); err != nil {
		return render.Artifact{}, fmt.Sprintf("Rendered Engine configuration exceeds delivery limits: %v", err), err
	}
	if !cacheHit {
		r.renderCache.set(engineKey, entry)
	}
	artifact := entry.artifact
	artifact.Payload = nil
	artifact.Resource = nil
	return artifact, "", nil
}

func (c *engineRenderCache) delete(engineKey types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, engineKey)
}

// The key represents RuleSet content by its accepted hash, not rule text;
// renderEngineArtifact rejects a verified entry whose key no longer matches.
func engineRenderCacheKeyFor(in render.EngineInput) engineRenderCacheKey {
	return engineRenderCacheKey{
		namespace:             in.Namespace,
		name:                  in.Name,
		uid:                   in.UID,
		ruleSetUID:            in.RuleSet.UID,
		contentHash:           in.RuleSet.ContentHash,
		emptyRulesPolicy:      in.EmptyRulesPolicy,
		failurePolicy:         in.FailurePolicy,
		enableFilterStateLogs: in.EnableFilterStateLogs,
		driverType:            in.DriverType,
		binary:                in.Binary,
		vm:                    in.VMSettings,
	}
}

// -----------------------------------------------------------------------------
// Engine Controller - WASM Driver - Provisioning
// -----------------------------------------------------------------------------

// provisionWasmDriver renders the accepted Engine configuration, attaches the
// EnvoyFilter that consumes it, and records the accepted configuration.
// engine is passed by pointer so that status patches are visible to the caller.
func (r *EngineReconciler) provisionWasmDriver(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) (ctrl.Result, error) {
	ws := targetLabelSelector(engine)
	if ws == nil {
		err := fmt.Errorf("target is required: cannot derive workload selector")
		logError(log, req, "Engine", err, "Invalid target configuration")
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "InvalidConfiguration", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	binary, err := r.resolveBinaryRef(engine)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "InvalidConfiguration", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	accepted, result, err := r.acceptedRuleSetContent(ctx, log, req, engine, binary)
	if accepted == nil {
		return result, err
	}
	ruleSet := &accepted.ruleSet
	inputs := accepted.inputs
	content := accepted.content
	contentHash := accepted.contentHash

	failurePolicy := engineFailurePolicy(engine)
	logDebug(log, req, "Engine", "Rendering Engine configuration")
	input := engineRenderInput(engine, ruleSet, content, contentHash, binary)
	engineKey := types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}
	var verifiedEntry *engineRenderCacheEntry
	if accepted.hasCachedRender {
		verifiedEntry = &accepted.cachedRender
	}
	artifact, failureMessage, err := r.renderEngineArtifact(engineKey, input, accepted.contentLimits, verifiedEntry)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RenderFailed", failureMessage); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	changed, err := r.engineInputsChanged(ctx, engine, ruleSet, inputs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		logInfo(log, req, "Engine", "Inputs changed before acceptance; requeueing")
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}
	// The limit summary and input fingerprint are verified against this accepted
	// content; remember them only after the final read-consistency check above.
	r.renderCache.rememberInputFingerprint(
		engineKey,
		engineRenderCacheKeyFor(input),
		accepted.inputFingerprint,
		accepted.contentLimits,
	)

	// Apply NetworkPolicy first: the WAF must never be attached without its
	// intended network restrictions.
	if err := r.applyNetworkPolicy(ctx, log, req, engine); err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "NetworkPolicyFailed", fmt.Sprintf("Failed to apply NetworkPolicy: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	// Observability is optional: a missing or temporarily invalid collector
	// declaration must not block WAF provisioning or traffic handling.
	var telemetryErr error
	if observabilityEnabled(engine) {
		telemetryErr = r.reconcileTelemetry(ctx, engine)
		patch := client.MergeFrom(engine.DeepCopy())
		before := snapshotConditions(engine.Status.Conditions)
		setObservabilityReady(&engine.Status.Conditions, engine.Generation, telemetryErr)
		if err := r.Status().Patch(ctx, engine, patch); err != nil {
			return ctrl.Result{}, err
		}
		logConditionTransitions(log, req, "Engine", before, engine.Status.Conditions)
	}
	if telemetryErr != nil {
		logError(log, req, "Engine", telemetryErr, "Failed to reconcile optional Telemetry")
		r.Recorder.Eventf(engine, nil, "Warning", "TelemetryProvisioningFailed", "Provision", "Failed to reconcile Telemetry: %v", telemetryErr)
	}

	logDebug(log, req, "Engine", "Applying EnvoyFilter")
	applied, err := r.applyEnvoyFilter(ctx, log, req, engine, artifact, binary, failurePolicy)
	if err != nil {
		return ctrl.Result{}, err
	}

	logDebug(log, req, "Engine", "Recording accepted configuration")
	if err := r.patchAcceptedConfiguration(ctx, log, req, engine, artifact, binary, ruleSet.UID, contentHash, failurePolicy); err != nil {
		return ctrl.Result{}, err
	}

	result = ctrl.Result{}
	if telemetryErr != nil {
		result.RequeueAfter = telemetryRetryAfter
	}
	if applied != failurePolicy {
		// Status-only updates do not bump the Engine generation, so the
		// generation-gated watch will not fire. Requeue to converge a deferred
		// bootstrap weakening after the accepted configuration exists.
		result.RequeueAfter = bootstrapConvergeRetryAfter
	}
	return result, nil
}

// acceptedRuleSet has verified the RuleSet's accepted content for this Engine.
// On the fast path, content is zero; inputs remain local for the final race check.
type acceptedRuleSet struct {
	ruleSet          wafv1alpha1.RuleSet
	inputs           assemble.Inputs
	content          assemble.Content
	contentHash      string
	inputFingerprint string
	contentLimits    contentLimitSummary
	// A fast-path result must carry its verified cache entry through rendering.
	cachedRender    engineRenderCacheEntry
	hasCachedRender bool
}

// acceptedRuleSetContent waits for the referenced RuleSet to accept the current
// content and verifies that this replica reconstructs the same, deliverable
// configuration. A nil result means reconcile should return the given result.
func (r *EngineReconciler) acceptedRuleSetContent(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	engine *wafv1alpha1.Engine,
	binary render.BinaryRef,
) (*acceptedRuleSet, ctrl.Result, error) {
	logDebug(log, req, "Engine", "Checking RuleSet acceptance")
	var ruleSet wafv1alpha1.RuleSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Spec.RuleSet.Name}, &ruleSet); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("RuleSet %s not found", engine.Spec.RuleSet.Name)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RuleSetNotFound", msg, func() { clearAcceptedConfiguration(engine) }); patchErr != nil {
				return nil, ctrl.Result{}, patchErr
			}
			return nil, ctrl.Result{}, err
		}
		logAPIError(log, req, "Engine", err, "Failed to get RuleSet", nil)
		return nil, ctrl.Result{}, fmt.Errorf("failed to get RuleSet %s: %w", engine.Spec.RuleSet.Name, err)
	}

	if msg, ok := ruleSetAcceptancePending(&ruleSet); !ok {
		result, err := r.awaitRuleSetAcceptance(ctx, log, req, engine, msg, acceptedRecordUnjustified(engine, &ruleSet))
		return nil, result, err
	}
	acceptedContentHash := ruleSet.Status.AcceptedContent.ContentHash

	cacheKey := engineRenderCacheKeyFor(engineRenderInput(engine, &ruleSet, assemble.Content{}, acceptedContentHash, binary))
	engineKey := types.NamespacedName{Namespace: engine.Namespace, Name: engine.Name}
	entry, hasRenderEntry := r.renderCache.get(engineKey, cacheKey)
	inputs, err := loadRuleSetInputs(ctx, r.Client, &ruleSet)
	if err != nil {
		return r.ruleSetInputsUnavailable(ctx, log, req, engine, &ruleSet, err)
	}
	inputFingerprint := assemble.Fingerprint(inputs)
	if hasRenderEntry && entry.inputFingerprint != "" && entry.inputFingerprint == inputFingerprint {
		// ResourceVersion can change without changing the input spec. This shortcut
		// relies on assemble.Fingerprint's precondition that each input's
		// UID+generation matches the spec represented by cached content.
		if err := validateContentLimitSummary(entry.contentLimits, r.Limits.WithDefaults()); err != nil {
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "InvalidRuleSet", fmt.Sprintf("RuleSet %s content exceeds delivery limits: %v", ruleSet.Name, err)); patchErr != nil {
				return nil, ctrl.Result{}, patchErr
			}
			return nil, ctrl.Result{}, err
		}
		return &acceptedRuleSet{
			ruleSet:          ruleSet,
			inputs:           inputs,
			contentHash:      acceptedContentHash,
			inputFingerprint: inputFingerprint,
			contentLimits:    entry.contentLimits,
			cachedRender:     entry,
			hasCachedRender:  true,
		}, ctrl.Result{}, nil
	}

	logDebug(log, req, "Engine", "Assembling accepted RuleSet content")
	if err := checkInputsSize(inputs); err != nil {
		// Content past the limit was never accepted: an input grew after the
		// RuleSet accepted its content. Wait as for any other unaccepted edit.
		result, err := r.awaitRuleSetAcceptance(ctx, log, req, engine, err.Error(), acceptedRecordUnjustified(engine, &ruleSet))
		return nil, result, err
	}
	content, err := assemble.Assemble(inputs)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "InvalidRuleSet", fmt.Sprintf("Failed to assemble RuleSet content: %v", err)); patchErr != nil {
			return nil, ctrl.Result{}, patchErr
		}
		return nil, ctrl.Result{}, err
	}
	contentHash, err := render.RuleSetContentHash(content)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if contentHash != acceptedContentHash {
		msg := fmt.Sprintf("RuleSet %s accepted content hash %s, assembled content is %s", ruleSet.Name, acceptedContentHash, contentHash)
		result, err := r.awaitRuleSetAcceptance(ctx, log, req, engine, msg, acceptedRecordUnjustified(engine, &ruleSet))
		return nil, result, err
	}

	// Accept only content the ECDS serving plane can deliver. Checking here
	// keeps readiness honest: the publication reconciler refuses oversized
	// payloads silently. The RuleSet refuses such content too, so this is
	// reached when the limits changed or the content changed after the RuleSet
	// checked it; the accepted configuration keeps serving.
	contentLimits, err := summarizeAndValidateContentLimits(content, r.Limits.WithDefaults())
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "InvalidRuleSet", fmt.Sprintf("RuleSet %s content exceeds delivery limits: %v", ruleSet.Name, err)); patchErr != nil {
			return nil, ctrl.Result{}, patchErr
		}
		return nil, ctrl.Result{}, err
	}

	return &acceptedRuleSet{
		ruleSet:          ruleSet,
		inputs:           inputs,
		content:          content,
		contentHash:      contentHash,
		inputFingerprint: inputFingerprint,
		contentLimits:    contentLimits,
	}, ctrl.Result{}, nil
}

func (r *EngineReconciler) ruleSetInputsUnavailable(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	engine *wafv1alpha1.Engine,
	ruleSet *wafv1alpha1.RuleSet,
	err error,
) (*acceptedRuleSet, ctrl.Result, error) {
	var clear []func()
	// A missing input is a wait state, like the RuleSet controller's: an
	// Engine reconcile can run before the RuleSet reports the missing
	// RuleData or RuleSource. Keep the last-known-good configuration
	// unless the RuleSet no longer justifies it; the readiness deadline
	// bounds a record that can never be reconstructed.
	if acceptedRecordUnjustified(engine, ruleSet) {
		clear = append(clear, func() { clearAcceptedConfiguration(engine) })
	}
	if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RuleSetInputsUnavailable", fmt.Sprintf("Failed to load RuleSet inputs: %v", err), clear...); patchErr != nil {
		return nil, ctrl.Result{}, patchErr
	}
	return nil, ctrl.Result{}, err
}

func engineRenderInput(
	engine *wafv1alpha1.Engine,
	ruleSet *wafv1alpha1.RuleSet,
	content assemble.Content,
	contentHash string,
	binary render.BinaryRef,
) render.EngineInput {
	return render.EngineInput{
		Namespace:             engine.Namespace,
		Name:                  engine.Name,
		UID:                   engine.UID,
		RuleSet:               render.RuleSetAccepted{UID: ruleSet.UID, ContentHash: contentHash},
		Content:               content,
		EmptyRulesPolicy:      string(ruleSet.Spec.EmptyRulesPolicy),
		FailurePolicy:         engineFailurePolicy(engine),
		EnableFilterStateLogs: observabilityEnabled(engine),
		DriverType:            string(wafv1alpha1.DriverTypeWasm),
		Binary:                binary,
	}
}

// ruleSetAcceptancePending returns a message and false when the RuleSet has no
// accepted content for its current generation. The second return value is true
// when the RuleSet may be used for rendering.
func ruleSetAcceptancePending(ruleSet *wafv1alpha1.RuleSet) (string, bool) {
	accepted := ruleSet.Status.AcceptedContent
	switch {
	case accepted == nil:
		return fmt.Sprintf("RuleSet %s has not accepted any content yet", ruleSet.Name), false
	case accepted.ObservedGeneration != ruleSet.Generation:
		return fmt.Sprintf("RuleSet %s accepted content for generation %d, current generation is %d", ruleSet.Name, accepted.ObservedGeneration, ruleSet.Generation), false
	}

	degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
	if degraded != nil && degraded.Status == metav1.ConditionTrue && degraded.ObservedGeneration == ruleSet.Generation {
		return fmt.Sprintf("RuleSet %s is degraded: %s", ruleSet.Name, degraded.Message), false
	}

	return "", true
}

// acceptedRecordUnjustified reports whether the accepted configuration can no
// longer be justified by the RuleSet. A different RuleSet object and content the
// delivery contract rejects are un-reconstructable by definition. An ordinary
// invalid edit keeps the last-known-good record: the RuleSet may become valid
// again without changing the previously accepted content, and the readiness
// deadline remains the safety net.
func acceptedRecordUnjustified(engine *wafv1alpha1.Engine, ruleSet *wafv1alpha1.RuleSet) bool {
	if engine.Status == nil || engine.Status.AcceptedConfiguration == nil {
		return false
	}
	if engine.Status.AcceptedConfiguration.RuleSetUID != string(ruleSet.UID) {
		return true
	}
	return isConditionCurrent(ruleSet.Status.Conditions, conditionDegraded, ruleSetDegradedReasonRejectedContent, ruleSet.Generation)
}

// awaitRuleSetAcceptance marks the Engine Progressing and requeues until the
// RuleSet accepts content for its current generation. When clearAccepted is
// set, the stale accepted record is dropped in the same patch.
func (r *EngineReconciler) awaitRuleSetAcceptance(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine, message string, clearAccepted bool) (ctrl.Result, error) {
	if engine.Status == nil {
		engine.Status = &wafv1alpha1.EngineStatus{}
	}
	conditionStale := false
	if current := apimeta.FindStatusCondition(engine.Status.Conditions, conditionProgressing); current == nil ||
		current.Status != metav1.ConditionTrue ||
		current.Reason != reasonAwaitingRuleSetAcceptance ||
		current.ObservedGeneration != engine.Generation {
		conditionStale = true
	}
	if conditionStale || (clearAccepted && engine.Status.AcceptedConfiguration != nil) {
		patch := client.MergeFrom(engine.DeepCopy())
		before := snapshotConditions(engine.Status.Conditions)
		if clearAccepted {
			clearAcceptedConfiguration(engine)
		}
		applyStatusProgressing(&engine.Status.Conditions, engine.Generation, reasonAwaitingRuleSetAcceptance, message)
		if err := r.Status().Patch(ctx, engine, patch); err != nil {
			logAPIError(log, req, "Engine", err, "Failed to patch status", engine)
			return ctrl.Result{}, err
		}
		logConditionTransitions(log, req, "Engine", before, engine.Status.Conditions)
	}

	logInfo(log, req, "Engine", "Awaiting RuleSet acceptance", "message", message)
	return ctrl.Result{RequeueAfter: ruleSetAcceptanceRetryAfter}, nil
}

// applyEnvoyFilter applies the EnvoyFilter attachment and returns the bootstrap
// policy that was actually applied (which may be stricter than desired).
func (r *EngineReconciler) applyEnvoyFilter(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	engine *wafv1alpha1.Engine,
	artifact render.Artifact,
	binary render.BinaryRef,
	desiredPolicy string,
) (string, error) {
	caPEM := r.xdsConfig.caPEM()
	if len(caPEM) == 0 {
		err := fmt.Errorf("CA PEM is not configured: set --xds-ca-path so the Gateway can verify the operator certificate")
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, reasonTLSNotConfigured, err.Error()); patchErr != nil {
			return "", patchErr
		}
		return "", err
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(envoyFilterGVK)
	previous := ""
	existingFound := false
	// This unstructured GET is live with controller-runtime v0.25.3's default
	// Cache.Unstructured=false. Ownership detection needs its managedFields; the
	// manager cache transform strips them, so revisit this if either behavior changes.
	err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: envoyFilterName(engine.Name)}, &existing)
	switch {
	case err == nil:
		existingFound = true
		previous = existing.GetAnnotations()["waf.k8s.coraza.io/bootstrap-policy"]
	case !apierrors.IsNotFound(err):
		logAPIError(log, req, "Engine", err, "Failed to get EnvoyFilter", nil)
		return "", err
	}

	accepted := engine.Status.AcceptedConfiguration
	acceptedActive := accepted != nil &&
		accepted.Version == artifact.Version &&
		string(accepted.FailurePolicy) == desiredPolicy
	applied := bootstrapPolicy(previous, desiredPolicy, acceptedActive)

	envoyFilter, err := buildEnvoyFilter(engine, artifact, binary, EnvoyFilterConfig{
		AdvertiseAddress: r.xdsConfig.AdvertiseAddress,
		CAPEM:            caPEM,
		IstioRevision:    r.istioRevision,
	}, applied)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "ProvisioningFailed", fmt.Sprintf("Failed to build EnvoyFilter: %v", err)); patchErr != nil {
			return "", patchErr
		}
		return "", err
	}

	if err := controllerutil.SetControllerReference(engine, envoyFilter, r.Scheme); err != nil {
		logError(log, req, "Engine", err, "Failed to set owner reference on EnvoyFilter")
		return "", err
	}

	if existingFound && envoyFilterMatches(&existing, envoyFilter) {
		return applied, nil
	}

	if err := serverSideApply(ctx, r.Client, envoyFilter); err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "ProvisioningFailed", fmt.Sprintf("Failed to create or update EnvoyFilter: %v", err)); patchErr != nil {
			return "", patchErr
		}
		return "", err
	}

	logInfo(log, req, "Engine", "EnvoyFilter provisioned", "envoyFilter", envoyFilter.GetName(), "bootstrapPolicy", applied)
	return applied, nil
}

func envoyFilterMatches(existing, desired *unstructured.Unstructured) bool {
	if deletionTimestamp := existing.GetDeletionTimestamp(); deletionTimestamp != nil && !deletionTimestamp.IsZero() {
		return false
	}
	if existing.GetAPIVersion() != desired.GetAPIVersion() ||
		existing.GetKind() != desired.GetKind() ||
		existing.GetName() != desired.GetName() ||
		existing.GetNamespace() != desired.GetNamespace() ||
		!unstructuredJSONEqual(existing.Object["spec"], desired.Object["spec"]) ||
		!apiequality.Semantic.DeepEqual(existing.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return false
	}

	existingLabels := existing.GetLabels()
	desiredLabels := desired.GetLabels()
	for key, value := range desiredLabels {
		if existingLabels[key] != value {
			return false
		}
	}
	// istio.io/rev is currently the only optional operator-owned label or
	// annotation. Add an ownership check for any other omitted or conditional
	// operator metadata, or this path may skip the SSA apply needed to prune it.
	if _, desiredRevision := desiredLabels[istioRevisionLabel]; !desiredRevision {
		if _, existingRevision := existingLabels[istioRevisionLabel]; existingRevision {
			ownsRevision, known := operatorOwnsIstioRevision(existing)
			if !known || ownsRevision {
				return false
			}
		}
	}

	existingAnnotations := existing.GetAnnotations()
	for key, value := range desired.GetAnnotations() {
		if existingAnnotations[key] != value {
			return false
		}
	}
	return true
}

// operatorOwnsIstioRevision reports whether the operator's main-resource Apply
// manager owns the revision label. Unknown managedFields must not suppress an
// apply that may be needed to remove operator-owned drift.
func operatorOwnsIstioRevision(existing *unstructured.Unstructured) (bool, bool) {
	managedFields := existing.GetManagedFields()
	if len(managedFields) == 0 || managedfields.ValidateManagedFields(managedFields) != nil {
		return false, false
	}

	ownsRevision := false
	for _, managed := range managedFields {
		if managed.Manager == "" || managed.FieldsV1 == nil || len(managed.FieldsV1.Raw) == 0 {
			return false, false
		}

		if managed.Manager != fieldManager ||
			managed.Operation != metav1.ManagedFieldsOperationApply ||
			managed.Subresource != "" {
			continue
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(managed.FieldsV1.Raw, &fields); err != nil || fields == nil {
			return false, false
		}

		metadataRaw, found := fields["f:metadata"]
		if !found {
			continue
		}
		var metadataFields map[string]json.RawMessage
		if err := json.Unmarshal(metadataRaw, &metadataFields); err != nil || metadataFields == nil {
			return false, false
		}

		labelsRaw, found := metadataFields["f:labels"]
		if !found {
			continue
		}
		var labelFields map[string]json.RawMessage
		if err := json.Unmarshal(labelsRaw, &labelFields); err != nil || labelFields == nil {
			return false, false
		}

		revisionRaw, found := labelFields["f:"+istioRevisionLabel]
		if !found {
			continue
		}
		var revisionField map[string]json.RawMessage
		if err := json.Unmarshal(revisionRaw, &revisionField); err != nil || revisionField == nil || len(revisionField) != 0 {
			return false, false
		}
		ownsRevision = true
	}

	return ownsRevision, true
}

func unstructuredJSONEqual(a, b any) bool {
	// Generated and API-decoded unstructured numbers can have different Go types.
	aJSON, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bJSON, err := json.Marshal(b)
	return err == nil && bytes.Equal(aJSON, bJSON)
}

// clearAcceptedConfiguration drops an accepted record the RuleSet can no longer
// justify. The publisher then deletes the Engine's snapshot and readiness stops
// depending on a record this build refuses to publish.
func clearAcceptedConfiguration(engine *wafv1alpha1.Engine) {
	if engine.Status != nil {
		engine.Status.AcceptedConfiguration = nil
	}
}

// patchAcceptedConfiguration records the accepted configuration and Ready
// condition in a single status patch.
//
// Ready records operator-side acceptance: the configuration was rendered and
// its EnvoyFilter applied. It does not wait for a proxy ACK, so a plugin-side
// rejection is visible only in the Gateway logs and the operator retry logs.
func (r *EngineReconciler) patchAcceptedConfiguration(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	engine *wafv1alpha1.Engine,
	artifact render.Artifact,
	binary render.BinaryRef,
	ruleSetUID types.UID,
	contentHash, failurePolicy string,
) error {
	if engine.Status == nil {
		engine.Status = &wafv1alpha1.EngineStatus{}
	}

	message := fmt.Sprintf("Accepted configuration %s provisioned", artifact.Version)
	desired := &wafv1alpha1.EngineAcceptedConfiguration{
		ObservedGeneration:     engine.Generation,
		RenderingSchemaVersion: render.RenderingSchemaVersion,
		Version:                artifact.Version,
		RuleSetUID:             string(ruleSetUID),
		RuleSetContentHash:     contentHash,
		BinaryURL:              binary.URL,
		WasmSHA256:             binary.SHA256,
		FailurePolicy:          wafv1alpha1.FailurePolicy(failurePolicy),
		EmptyRulesPolicy:       wafv1alpha1.EmptyRulesPolicy(artifact.EmptyRulesPolicy),
		FilterStateLogs:        filterStateLogsMode(artifact.EnableFilterStateLogs),
		DriverType:             wafv1alpha1.DriverType(artifact.DriverType),
		VM: wafv1alpha1.EngineAcceptedVMSettings{
			Runtime:     artifact.VM.Runtime,
			VMID:        artifact.VM.VMID,
			CodeTimeout: artifact.VM.CodeTimeout.String(),
		},
	}
	// Most reconciles are triggered by Pod, RuleSet status, or telemetry
	// updates that leave the accepted record unchanged; skip the write and the
	// duplicate event then. applyStatusReady also drops Degraded and
	// Progressing, so either one still requires the patch.
	if apiequality.Semantic.DeepEqual(engine.Status.AcceptedConfiguration, desired) &&
		isConditionCurrent(engine.Status.Conditions, conditionReady, reasonConfigurationAccepted, engine.Generation) &&
		apimeta.FindStatusCondition(engine.Status.Conditions, conditionDegraded) == nil &&
		apimeta.FindStatusCondition(engine.Status.Conditions, conditionProgressing) == nil {
		return nil
	}
	patch := client.MergeFrom(engine.DeepCopy())
	before := snapshotConditions(engine.Status.Conditions)
	engine.Status.AcceptedConfiguration = desired
	applyStatusReady(&engine.Status.Conditions, engine.Generation, reasonConfigurationAccepted, message)
	r.Recorder.Eventf(engine, nil, "Normal", reasonConfigurationAccepted, "Provision", truncateEventNote(message))
	if err := r.Status().Patch(ctx, engine, patch); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to patch status", engine)
		return err
	}
	logConditionTransitions(log, req, "Engine", before, engine.Status.Conditions)
	return nil
}

func engineFailurePolicy(engine *wafv1alpha1.Engine) string {
	if engine.Spec.FailurePolicy == "" {
		return bootstrapPolicyFail
	}
	return string(engine.Spec.FailurePolicy)
}

// filterStateLogsMode records the resolved filter-state-log rendering input.
func filterStateLogsMode(enabled bool) wafv1alpha1.FilterStateLogsMode {
	if enabled {
		return wafv1alpha1.FilterStateLogsEnabled
	}
	return wafv1alpha1.FilterStateLogsDisabled
}

func setObservabilityReady(conditions *[]metav1.Condition, generation int64, telemetryErr error) {
	if telemetryErr != nil {
		setConditionFalse(conditions, generation, conditionObservabilityReady, "TelemetryProvisioningFailed", telemetryErr.Error())
		return
	}
	setConditionTrue(conditions, generation, conditionObservabilityReady, "TelemetryReady", "Telemetry successfully created or updated")
}

func observabilityEnabled(engine *wafv1alpha1.Engine) bool {
	return engine != nil &&
		engine.Spec.Observability.Mode == wafv1alpha1.ObservabilityModeEnabled
}

// -----------------------------------------------------------------------------
// Engine Controller - WASM Driver - Gateway Matching
// -----------------------------------------------------------------------------

// gatewayNameLabel is the well-known label that Istio applies to Gateway pods
// to identify which Gateway resource they belong to.
const gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"

var binarySHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// resolveBinaryRef returns the raw WASM binary location and digest for an
// Engine. An explicit spec.driver.wasm.url/sha256 pair wins; omitting both
// selects the module bundled with the operator.
//
// shortcut: full SSRF/host-class validation and custom WAsm CA are Phase 5; add them here.
func (r *EngineReconciler) resolveBinaryRef(engine *wafv1alpha1.Engine) (render.BinaryRef, error) {
	var explicitURL, explicitSHA string
	if engine.Spec.Driver.Wasm != nil {
		explicitURL = engine.Spec.Driver.Wasm.URL
		explicitSHA = engine.Spec.Driver.Wasm.SHA256
	}

	switch {
	case explicitURL != "" && explicitSHA != "":
		ref := render.BinaryRef{URL: explicitURL, SHA256: explicitSHA}
		if err := validateBinaryRefBasic(ref); err != nil {
			return render.BinaryRef{}, err
		}
		return ref, nil

	case explicitURL == "" && explicitSHA == "":
		manifest, _, err := assets.ResolveDefault()
		if err != nil {
			return render.BinaryRef{}, fmt.Errorf("resolve bundled WASM module: %w", err)
		}
		ref := render.BinaryRef{
			URL:        strings.TrimRight(r.assetBaseURL, "/") + "/wasm/" + manifest.SHA256 + ".wasm",
			SHA256:     manifest.SHA256,
			CRSVersion: manifest.CRSVersion,
		}
		if err := validateBinaryRefBasic(ref); err != nil {
			return render.BinaryRef{}, fmt.Errorf("resolve bundled WASM module: %w", err)
		}
		return ref, nil

	default:
		return render.BinaryRef{}, errors.New("spec.driver.wasm.url and spec.driver.wasm.sha256 must be set together; omit both to use the bundled module")
	}
}

// validateBinaryRefBasic validates the URL shape and digest of a binary
// reference. The CEL rules already reject these shapes for new writes; this is
// the runtime guard for already-stored objects.
func validateBinaryRefBasic(ref render.BinaryRef) error {
	parsed, err := url.Parse(ref.URL)
	if err != nil {
		return errors.New("parse WASM binary URL: invalid URL")
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("WASM binary URL must be an absolute https URL with a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("WASM binary URL must not contain userinfo, query, or fragment")
	}
	if !binarySHA256Pattern.MatchString(ref.SHA256) {
		return fmt.Errorf("WASM sha256 %q must be 64 lowercase hex characters", ref.SHA256)
	}
	return nil
}
