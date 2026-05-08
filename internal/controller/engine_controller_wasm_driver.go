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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Controller - Istio RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=extensions.istio.io,resources=wasmplugins,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=serviceentries;destinationrules,verbs=get;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch

// -----------------------------------------------------------------------------
// Engine Controller - Istio Consts
// -----------------------------------------------------------------------------

// WasmPluginNamePrefix is the prefix used for all created WasmPlugin resources
const WasmPluginNamePrefix = "coraza-engine-"

// wasmPluginName returns the deterministic name for the WasmPlugin child
// resource derived from the given Engine name. All call sites MUST use this
// helper to keep the naming scheme consistent.
func wasmPluginName(engineName string) string {
	return WasmPluginNamePrefix + engineName
}

// -----------------------------------------------------------------------------
// Engine Controller - WASM Driver - Provisioning
// -----------------------------------------------------------------------------

// provisionWasmDriver provisions the Istio WasmPlugin resource for the Engine.
func (r *EngineReconciler) provisionWasmDriver(ctx context.Context, log logr.Logger, req ctrl.Request, engine wafv1alpha1.Engine) (ctrl.Result, error) {
	ws := targetLabelSelector(&engine)
	if ws == nil {
		err := fmt.Errorf("target is required: cannot derive workload selector")
		logError(log, req, "Engine", err, "Invalid target configuration")
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "InvalidConfiguration", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	// Apply NetworkPolicy first to ensure network restrictions are in place
	// before the WasmPlugin starts running. This prevents a partially-provisioned
	// state where the plugin is active without the intended cache-server network
	// restrictions.
	if err := r.applyNetworkPolicy(ctx, log, req, &engine); err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "NetworkPolicyFailed", fmt.Sprintf("Failed to apply NetworkPolicy: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	logDebug(log, req, "Engine", "Ensuring cache client ServiceAccount")
	saName, err := r.ensureCacheClientServiceAccount(ctx, log, req, &engine)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "ServiceAccountFailed", fmt.Sprintf("Failed to ensure cache client ServiceAccount: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	r.cleanupStaleTokens(req.Namespace, req.Name, engine.Spec.RuleSet.Name)

	logDebug(log, req, "Engine", "Ensuring cache client token")
	cacheToken, renewAt, err := r.ensureCacheToken(ctx, log, req, saName, engine.Spec.RuleSet.Name)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "TokenFailed", fmt.Sprintf("Failed to ensure cache client token: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	wasmPlugin, err := r.applyWasmPlugin(ctx, log, req, &engine, cacheToken)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "ProvisioningFailed", fmt.Sprintf("Failed to create or update WasmPlugin: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	logDebug(log, req, "Engine", "Updating status after successful provisioning")
	if patchErr := patchReady(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "Configured", "WasmPlugin successfully created/updated"); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	r.Recorder.Eventf(&engine, nil, "Normal", "WasmPluginCreated", "Provision", "Created WasmPlugin %s/%s", wasmPlugin.GetNamespace(), wasmPlugin.GetName())

	// Schedule re-reconciliation at the token's renewal deadline. This is a
	// single requeue that fires exactly when the token needs refreshing,
	// avoiding repeated intermediate reconciliations.
	requeueAfter := max(time.Until(renewAt), time.Second)
	logDebug(log, req, "Engine", "Scheduling token renewal", "requeueAfter", requeueAfter)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// applyWasmPlugin builds the WasmPlugin resource, sets the controller reference,
// and applies it via server-side apply.
func (r *EngineReconciler) applyWasmPlugin(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine, cacheToken string) (*unstructured.Unstructured, error) {
	logDebug(log, req, "Engine", "Building WasmPlugin resource")
	wasmURL, fromSpec := r.wasmPluginOCIURLSource(engine)
	if fromSpec {
		logDebug(log, req, "Engine", "WasmPlugin OCI URL from Engine spec", "url", wasmURL)
	} else {
		logDebug(log, req, "Engine", "WasmPlugin OCI URL from operator default", "url", wasmURL)
	}
	wasmPlugin := r.buildWasmPlugin(engine, wasmURL, cacheToken)

	logDebug(log, req, "Engine", "Setting controller reference on WasmPlugin")
	if err := controllerutil.SetControllerReference(engine, wasmPlugin, r.Scheme); err != nil {
		logError(log, req, "Engine", err, "Failed to set owner reference on WasmPlugin")
		return nil, err
	}

	logDebug(log, req, "Engine", "Applying WasmPlugin", "wasmPluginName", wasmPlugin.GetName())
	if err := serverSideApply(ctx, r.Client, wasmPlugin); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to create or update WasmPlugin", wasmPlugin)
		return nil, err
	}

	logInfo(log, req, "Engine", "WasmPlugin provisioned", "wasmNamespace", wasmPlugin.GetNamespace(), "wasmName", wasmPlugin.GetName())
	return wasmPlugin, nil
}

// -----------------------------------------------------------------------------
// Engine Controller - WASM Driver - WasmPlugin Builder
// -----------------------------------------------------------------------------

func (r *EngineReconciler) wasmPluginOCIURLSource(engine *wafv1alpha1.Engine) (url string, fromSpec bool) {
	if engine.Spec.Driver.Wasm != nil && engine.Spec.Driver.Wasm.Image != "" {
		return engine.Spec.Driver.Wasm.Image, true
	}
	return r.defaultWasmImage, false
}

func (r *EngineReconciler) buildWasmPlugin(engine *wafv1alpha1.Engine, wasmURL string, cacheToken string) *unstructured.Unstructured {
	rulesetKey := fmt.Sprintf("%s/%s", engine.Namespace, engine.Spec.RuleSet.Name)

	failurePolicy := wafv1alpha1.FailurePolicyFail
	if engine.Spec.FailurePolicy != "" {
		failurePolicy = engine.Spec.FailurePolicy
	}

	pluginConfig := map[string]any{
		"cache_server_instance": rulesetKey,
		"cache_server_cluster":  r.ruleSetCacheServerCluster,
		"failure_policy":        string(failurePolicy),
		"cache_token":           cacheToken,
	}

	if engine.Spec.RuleSetCacheServer != nil {
		pluginConfig["rule_reload_interval_seconds"] = engine.Spec.RuleSetCacheServer.PollIntervalSeconds
	}

	ws := targetLabelSelector(engine)
	matchLabels := map[string]string{}
	if ws != nil && ws.MatchLabels != nil {
		matchLabels = ws.MatchLabels
	}

	wasmPlugin := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.istio.io/v1alpha1",
			"kind":       "WasmPlugin",
			"metadata": map[string]any{
				"name":      wasmPluginName(engine.Name),
				"namespace": engine.Namespace,
			},
			"spec": map[string]any{
				"url":          wasmURL,
				"pluginConfig": pluginConfig,
				"selector": map[string]any{
					"matchLabels": matchLabels,
				},
			},
		},
	}

	if engine.Spec.Driver.Wasm != nil && engine.Spec.Driver.Wasm.ImagePullSecret != "" {
		spec := wasmPlugin.Object["spec"].(map[string]any)
		spec["imagePullSecret"] = engine.Spec.Driver.Wasm.ImagePullSecret
	}

	wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "extensions.istio.io",
		Version: "v1alpha1",
		Kind:    "WasmPlugin",
	})

	if r.istioRevision != "" {
		labels := wasmPlugin.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["istio.io/rev"] = r.istioRevision
		wasmPlugin.SetLabels(labels)
	}

	return wasmPlugin
}

// -----------------------------------------------------------------------------
// Engine Controller - WASM Driver - Gateway Matching
// -----------------------------------------------------------------------------

// gatewayNameLabel is the well-known label that Istio applies to Gateway pods
// to identify which Gateway resource they belong to.
const gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"
