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

	"github.com/go-logr/logr"
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

	logDebug(log, req, "Engine", "Updating status after successful provisioning")
	patch := client.MergeFrom(engine.DeepCopy())
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

	pluginConfig := map[string]any{
		"cache_server_instance": rulesetKey,
		"cache_server_cluster":  r.ruleSetCacheServerCluster,
	}

	if engine.Spec.Driver.Istio.Wasm.RuleSetCacheServer != nil {
		pluginConfig["rule_reload_interval_seconds"] = engine.Spec.Driver.Istio.Wasm.RuleSetCacheServer.PollIntervalSeconds
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
					"matchLabels": engine.Spec.Driver.Istio.Wasm.WorkloadSelector.MatchLabels,
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
