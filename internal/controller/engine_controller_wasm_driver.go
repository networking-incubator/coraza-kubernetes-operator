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
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
)

// -----------------------------------------------------------------------------
// Engine Controller - Istio RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=networking.istio.io,resources=envoyfilters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions.istio.io,resources=wasmplugins,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch

// -----------------------------------------------------------------------------
// Engine Controller - Istio Consts
// -----------------------------------------------------------------------------

const (
	// legacyWasmPluginNamePrefix is the name prefix of the retired WasmPlugin
	// child resource. It is kept only so cleanup can find and delete it.
	legacyWasmPluginNamePrefix = "coraza-engine-"

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

// legacyWasmPluginName returns the deterministic name of the retired
// WasmPlugin child resource derived from the given Engine name.
func legacyWasmPluginName(engineName string) string {
	return legacyWasmPluginNamePrefix + engineName
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

	logDebug(log, req, "Engine", "Checking RuleSet acceptance")
	var ruleSet wafv1alpha1.RuleSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Spec.RuleSet.Name}, &ruleSet); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("RuleSet %s not found", engine.Spec.RuleSet.Name)
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RuleSetNotFound", msg); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{}, err
		}
		logAPIError(log, req, "Engine", err, "Failed to get RuleSet", nil)
		return ctrl.Result{}, fmt.Errorf("failed to get RuleSet %s: %w", engine.Spec.RuleSet.Name, err)
	}

	if msg, ok := ruleSetAcceptancePending(&ruleSet); !ok {
		return r.awaitRuleSetAcceptance(ctx, log, req, engine, msg)
	}
	acceptedContentHash := ruleSet.Status.AcceptedContent.ContentHash

	logDebug(log, req, "Engine", "Assembling accepted RuleSet content")
	inputs, err := loadRuleSetInputs(ctx, r.Client, &ruleSet)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RuleSetInputsUnavailable", fmt.Sprintf("Failed to load RuleSet inputs: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}
	content, err := assemble.Assemble(inputs)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "InvalidRuleSet", fmt.Sprintf("Failed to assemble RuleSet content: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}
	contentHash, err := render.RuleSetContentHash(content)
	if err != nil {
		return ctrl.Result{}, err
	}
	if contentHash != acceptedContentHash {
		msg := fmt.Sprintf("RuleSet %s accepted content hash %s, assembled content is %s", ruleSet.Name, acceptedContentHash, contentHash)
		return r.awaitRuleSetAcceptance(ctx, log, req, engine, msg)
	}

	failurePolicy := engineFailurePolicy(engine)
	logDebug(log, req, "Engine", "Rendering Engine configuration")
	artifact, err := render.Render(render.EngineInput{
		Namespace:             engine.Namespace,
		Name:                  engine.Name,
		UID:                   engine.UID,
		RuleSet:               render.RuleSetAccepted{UID: ruleSet.UID, ContentHash: contentHash},
		Content:               content,
		EmptyRulesPolicy:      string(ruleSet.Spec.EmptyRulesPolicy),
		FailurePolicy:         failurePolicy,
		EnableFilterStateLogs: observabilityEnabled(engine),
		DriverType:            string(wafv1alpha1.DriverTypeWasm),
		Binary:                binary,
	})
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "RenderFailed", fmt.Sprintf("Failed to render Engine configuration: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	changed, err := r.engineInputsChanged(ctx, engine, &ruleSet, inputs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if changed {
		logInfo(log, req, "Engine", "Inputs changed before acceptance; requeueing")
		return ctrl.Result{Requeue: true}, nil
	}

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
	telemetryErr := r.reconcileTelemetry(ctx, engine)
	if observabilityEnabled(engine) {
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

	if err := r.deleteLegacyWasmPlugin(ctx, log, req, engine); err != nil {
		return ctrl.Result{}, err
	}

	result := ctrl.Result{}
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

// awaitRuleSetAcceptance marks the Engine Progressing and requeues until the
// RuleSet accepts content for its current generation.
func (r *EngineReconciler) awaitRuleSetAcceptance(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine, message string) (ctrl.Result, error) {
	if engine.Status == nil {
		engine.Status = &wafv1alpha1.EngineStatus{}
	}
	if current := apimeta.FindStatusCondition(engine.Status.Conditions, conditionProgressing); current == nil ||
		current.Status != metav1.ConditionTrue ||
		current.Reason != reasonAwaitingRuleSetAcceptance ||
		current.ObservedGeneration != engine.Generation {
		patch := client.MergeFrom(engine.DeepCopy())
		before := snapshotConditions(engine.Status.Conditions)
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
	if len(r.xdsConfig.CAPEM) == 0 {
		err := fmt.Errorf("CA PEM is not configured: set --xds-ca-path so the Gateway can verify the operator certificate")
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, reasonTLSNotConfigured, err.Error()); patchErr != nil {
			return "", patchErr
		}
		return "", err
	}

	var existing unstructured.Unstructured
	existing.SetGroupVersionKind(envoyFilterGVK)
	previous := ""
	err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: envoyFilterName(engine.Name)}, &existing)
	switch {
	case err == nil:
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
		CAPEM:            r.xdsConfig.CAPEM,
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

	if err := serverSideApply(ctx, r.Client, envoyFilter); err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", engine, &engine.Status.Conditions, engine.Generation, "ProvisioningFailed", fmt.Sprintf("Failed to create or update EnvoyFilter: %v", err)); patchErr != nil {
			return "", patchErr
		}
		return "", err
	}

	logInfo(log, req, "Engine", "EnvoyFilter provisioned", "envoyFilter", envoyFilter.GetName(), "bootstrapPolicy", applied)
	return applied, nil
}

// patchAcceptedConfiguration records the accepted configuration and Ready
// condition in a single status patch.
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
	patch := client.MergeFrom(engine.DeepCopy())
	before := snapshotConditions(engine.Status.Conditions)
	engine.Status.AcceptedConfiguration = &wafv1alpha1.EngineAcceptedConfiguration{
		ObservedGeneration:     engine.Generation,
		RenderingSchemaVersion: render.RenderingSchemaVersion,
		Version:                artifact.Version,
		RuleSetUID:             string(ruleSetUID),
		RuleSetContentHash:     contentHash,
		BinaryURL:              binary.URL,
		WasmSHA256:             binary.SHA256,
		FailurePolicy:          wafv1alpha1.FailurePolicy(failurePolicy),
	}
	applyStatusReady(&engine.Status.Conditions, engine.Generation, reasonConfigurationAccepted, message)
	r.Recorder.Eventf(engine, nil, "Normal", reasonConfigurationAccepted, "Provision", truncateEventNote(message))
	if err := r.Status().Patch(ctx, engine, patch); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to patch status", engine)
		return err
	}
	logConditionTransitions(log, req, "Engine", before, engine.Status.Conditions)
	return nil
}

// deleteLegacyWasmPlugin removes the retired WasmPlugin child resource. It runs
// only after the EnvoyFilter apply succeeds, so there is no window without a
// WAF attachment.
func (r *EngineReconciler) deleteLegacyWasmPlugin(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) error {
	wasmPlugin := &unstructured.Unstructured{}
	wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "extensions.istio.io",
		Version: "v1alpha1",
		Kind:    "WasmPlugin",
	})
	name := legacyWasmPluginName(engine.Name)
	if err := r.Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: name}, wasmPlugin); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		logAPIError(log, req, "Engine", err, "Failed to get legacy WasmPlugin", nil)
		return err
	}

	if err := r.Delete(ctx, wasmPlugin); client.IgnoreNotFound(err) != nil {
		logAPIError(log, req, "Engine", err, "Failed to delete legacy WasmPlugin", wasmPlugin)
		return err
	}
	logInfo(log, req, "Engine", "Deleted legacy WasmPlugin", "wasmPlugin", name)
	return nil
}

func engineFailurePolicy(engine *wafv1alpha1.Engine) string {
	if engine.Spec.FailurePolicy == "" {
		return bootstrapPolicyFail
	}
	return string(engine.Spec.FailurePolicy)
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
