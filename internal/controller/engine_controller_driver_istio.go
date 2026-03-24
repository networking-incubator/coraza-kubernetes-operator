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
	"slices"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Controller - Istio RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=extensions.istio.io,resources=wasmplugins,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch

// -----------------------------------------------------------------------------
// Engine Controller - Istio Consts
// -----------------------------------------------------------------------------

// WasmPluginNamePrefix is the prefix used for all created WasmPlugin resources
const WasmPluginNamePrefix = "coraza-engine-"

// -----------------------------------------------------------------------------
// Engine Controller - Istio Driver - Provisioning
// -----------------------------------------------------------------------------

// provisionIstioEngineWithWasm provisions the Istio WasmPlugin resource for
// the Engine.
func (r *EngineReconciler) provisionIstioEngineWithWasm(ctx context.Context, log logr.Logger, req ctrl.Request, engine wafv1alpha1.Engine) (ctrl.Result, error) {
	logDebug(log, req, "Engine", "Building WasmPlugin resource")
	wasmPlugin := r.buildWasmPlugin(&engine)

	logDebug(log, req, "Engine", "Setting controller reference on WasmPlugin")
	if err := controllerutil.SetControllerReference(&engine, wasmPlugin, r.Scheme); err != nil {
		logError(log, req, "Engine", err, "Failed to set owner reference on WasmPlugin")
		return ctrl.Result{}, err
	}

	logDebug(log, req, "Engine", "Applying WasmPlugin", "wasmPluginName", wasmPlugin.GetName())
	if err := serverSideApply(ctx, r.Client, wasmPlugin); err != nil {
		logError(log, req, "Engine", err, "Failed to create or update WasmPlugin")
		r.Recorder.Eventf(&engine, nil, "Warning", "ProvisioningFailed", "Provision", "Failed to create WasmPlugin: %v", err)

		patch := client.MergeFrom(engine.DeepCopy())
		setStatusConditionDegraded(log, req, "Engine", &engine.Status.Conditions, engine.Generation, "ProvisioningFailed", fmt.Sprintf("Failed to create or update WasmPlugin: %v", err))
		if updateErr := r.Status().Patch(ctx, &engine, patch); updateErr != nil {
			logError(log, req, "Engine", updateErr, "Failed to patch status after provisioning failure")
		}

		return ctrl.Result{}, err
	}
	logInfo(log, req, "Engine", "WasmPlugin provisioned", "wasmNamespace", wasmPlugin.GetNamespace(), "wasmName", wasmPlugin.GetName())

	logDebug(log, req, "Engine", "Finding matched Gateways")
	gateways, err := r.matchedGateways(ctx, log, req, &engine)
	if err != nil {
		logError(log, req, "Engine", err, "Failed to find matched Gateways, not updating Gateway status")
	}

	logDebug(log, req, "Engine", "Updating status after successful provisioning")
	patch := client.MergeFrom(engine.DeepCopy())
	if err == nil {
		engine.Status.Gateways = gateways
	}
	setStatusReady(log, req, "Engine", &engine.Status.Conditions, engine.Generation, "Configured", "WasmPlugin successfully created/updated")
	if err := r.Status().Patch(ctx, &engine, patch); err != nil {
		logError(log, req, "Engine", err, "Failed to patch status")
		return ctrl.Result{}, err
	}
	r.Recorder.Eventf(&engine, nil, "Normal", "WasmPluginCreated", "Provision", "Created WasmPlugin %s/%s", wasmPlugin.GetNamespace(), wasmPlugin.GetName())

	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// Engine Controller - Istio Driver - WasmPlugin Builder
// -----------------------------------------------------------------------------

func (r *EngineReconciler) buildWasmPlugin(engine *wafv1alpha1.Engine) *unstructured.Unstructured {
	rulesetKey := fmt.Sprintf("%s/%s", engine.Namespace, engine.Spec.RuleSet.Name)

	failurePolicy := wafv1alpha1.FailurePolicyFail
	if engine.Spec.FailurePolicy != nil {
		failurePolicy = *engine.Spec.FailurePolicy
	}

	pluginConfig := map[string]any{
		"cache_server_instance": rulesetKey,
		"cache_server_cluster":  r.ruleSetCacheServerCluster,
		"failure_policy":        string(failurePolicy),
	}

	if engine.Spec.Driver.Istio.Wasm.RuleSetCacheServer != nil && engine.Spec.Driver.Istio.Wasm.RuleSetCacheServer.PollIntervalSeconds != nil {
		pluginConfig["rule_reload_interval_seconds"] = *engine.Spec.Driver.Istio.Wasm.RuleSetCacheServer.PollIntervalSeconds
	}

	matchLabels := engine.Spec.Driver.Istio.Wasm.WorkloadSelector.MatchLabels
	if matchLabels == nil {
		matchLabels = map[string]string{}
	}

	wasmPlugin := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "extensions.istio.io/v1alpha1",
			"kind":       "WasmPlugin",
			"metadata": map[string]any{
				"name":      fmt.Sprintf("%s%s", WasmPluginNamePrefix, engine.Name),
				"namespace": engine.Namespace,
			},
			"spec": map[string]any{
				"url":          engine.Spec.Driver.Istio.Wasm.Image,
				"pluginConfig": pluginConfig,
				"selector": map[string]any{
					"matchLabels": matchLabels,
				},
			},
		},
	}

	wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "extensions.istio.io",
		Version: "v1alpha1",
		Kind:    "WasmPlugin",
	})

	return wasmPlugin
}

// -----------------------------------------------------------------------------
// Engine Controller - Istio Driver - Gateway Matching
// -----------------------------------------------------------------------------

// gatewayNameLabel is the well-known label that Istio applies to Gateway pods
// to identify which Gateway resource they belong to.
const gatewayNameLabel = "gateway.networking.k8s.io/gateway-name"

// matchedGateways finds Gateways whose pods match the Engine's workload selector.
// It lists pods matching the selector, then extracts unique Gateway names from
// the well-known gateway-name label on each pod.
func (r *EngineReconciler) matchedGateways(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) ([]wafv1alpha1.GatewayReference, error) {
	if engine.Spec.Driver.Istio.Wasm.WorkloadSelector == nil {
		return nil, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(engine.Spec.Driver.Istio.Wasm.WorkloadSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid workload selector: %w", err)
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(engine.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	var names []string
	for _, pod := range podList.Items {
		if gwName := pod.Labels[gatewayNameLabel]; gwName != "" {
			names = append(names, gwName)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	gateways := make([]wafv1alpha1.GatewayReference, len(names))
	for i, name := range names {
		gateways[i] = wafv1alpha1.GatewayReference{Name: name}
	}

	logDebug(log, req, "Engine", "Matched Gateways", "count", len(gateways))
	return gateways, nil
}
