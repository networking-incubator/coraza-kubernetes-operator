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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
)

// -----------------------------------------------------------------------------
// Engine Controller - Dynamic Module RBAC
// -----------------------------------------------------------------------------

// +kubebuilder:rbac:groups=networking.istio.io,resources=envoyfilters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// -----------------------------------------------------------------------------
// Engine Controller - Dynamic Module Constants
// -----------------------------------------------------------------------------

const (
	// EnvoyFilterNamePrefix is the prefix used for all created EnvoyFilter resources.
	EnvoyFilterNamePrefix = "coraza-engine-"

	// dmConfigMapPrefix is the prefix for ConfigMaps containing the Deployment
	// overlay that injects the dynamic module init container.
	dmConfigMapPrefix = "coraza-dm-"

	// dmVolumeName is the name of the shared emptyDir volume used to transfer
	// the .so from the init container to the istio-proxy container.
	dmVolumeName = "dynamic-modules"

	// dmToolsVolumeName is the name of the emptyDir volume used to share a
	// statically-linked cp binary from busybox with the module image init
	// container. This is needed because the module image is typically built
	// FROM scratch and contains no utilities.
	dmToolsVolumeName = "dm-tools"

	// dmMountPath is where the .so is placed inside the gateway pod.
	dmMountPath = "/etc/envoy/dynamic-modules"

	// dmToolsImage is a minimal image that provides a statically-linked cp
	// binary. The musl variant is required because it is statically linked —
	// the default glibc variant is dynamically linked and fails in scratch
	// containers with "no such file or directory" (missing dynamic linker).
	dmToolsImage = "busybox:stable-musl"

	// dmTmpVolumeName is the name of the tmpfs volume mounted at /tmp in the
	// istio-proxy container. Coraza performs a filesystem access check at
	// init time; without a writable /tmp the check fails on the read-only
	// root filesystem of Istio gateway pods.
	dmTmpVolumeName = "dm-tmp"
)

// -----------------------------------------------------------------------------
// Engine Controller - Dynamic Module Provisioning
// -----------------------------------------------------------------------------

// provisionDynamicModuleDriver provisions the Istio EnvoyFilter resource for
// the Engine using the dynamic module driver.
func (r *EngineReconciler) provisionDynamicModuleDriver(ctx context.Context, log logr.Logger, req ctrl.Request, engine wafv1alpha1.Engine) (ctrl.Result, error) {
	ws := targetLabelSelector(&engine)
	if ws == nil {
		err := fmt.Errorf("target is required: cannot derive workload selector")
		logError(log, req, "Engine", err, "Invalid dynamic module configuration")
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "InvalidConfiguration", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	ruleSetKey := fmt.Sprintf("%s/%s", engine.Namespace, engine.Spec.RuleSet.Name)
	entry, ok := r.ruleSetCache.Get(ruleSetKey)
	if !ok {
		err := fmt.Errorf("rules for RuleSet %s not yet available in cache", engine.Spec.RuleSet.Name)
		logInfo(log, req, "Engine", "RuleSet not cached yet; will retry", "ruleSet", engine.Spec.RuleSet.Name)
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "RulesNotCached", err.Error()); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	dmConfig := engine.Spec.Driver.DynamicModule
	if dmConfig != nil && dmConfig.ModuleImage != "" {
		if err := r.applyDynamicModuleConfigMap(ctx, log, req, &engine); err != nil {
			if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "ProvisioningFailed", fmt.Sprintf("Failed to apply module ConfigMap: %v", err)); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{}, err
		}
	}

	envoyFilter, err := r.applyEnvoyFilter(ctx, log, req, &engine, entry.Rules)
	if err != nil {
		if patchErr := patchDegraded(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "ProvisioningFailed", fmt.Sprintf("Failed to create or update EnvoyFilter: %v", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, err
	}

	logDebug(log, req, "Engine", "Updating status after successful provisioning")
	if patchErr := patchReady(ctx, r.Status(), r.Recorder, log, req, "Engine", &engine, &engine.Status.Conditions, engine.Generation, "Configured", "EnvoyFilter successfully created/updated"); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	r.Recorder.Eventf(&engine, nil, "Normal", "EnvoyFilterCreated", "Provision", "Created EnvoyFilter %s/%s", envoyFilter.GetNamespace(), envoyFilter.GetName())

	return ctrl.Result{}, nil
}

// -----------------------------------------------------------------------------
// Engine Controller - Dynamic Module EnvoyFilter
// -----------------------------------------------------------------------------

func (r *EngineReconciler) applyEnvoyFilter(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine, rules string) (*unstructured.Unstructured, error) {
	logDebug(log, req, "Engine", "Building EnvoyFilter resource")
	envoyFilter := r.buildEnvoyFilter(engine, rules)

	logDebug(log, req, "Engine", "Setting controller reference on EnvoyFilter")
	if err := controllerutil.SetControllerReference(engine, envoyFilter, r.Scheme); err != nil {
		logError(log, req, "Engine", err, "Failed to set owner reference on EnvoyFilter")
		return nil, err
	}

	logDebug(log, req, "Engine", "Applying EnvoyFilter", "envoyFilterName", envoyFilter.GetName())
	if err := serverSideApply(ctx, r.Client, envoyFilter); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to create or update EnvoyFilter", envoyFilter)
		return nil, err
	}

	logInfo(log, req, "Engine", "EnvoyFilter provisioned", "namespace", envoyFilter.GetNamespace(), "name", envoyFilter.GetName())
	return envoyFilter, nil
}

func (r *EngineReconciler) buildEnvoyFilter(engine *wafv1alpha1.Engine, rules string) *unstructured.Unstructured {
	dmConfig := engine.Spec.Driver.DynamicModule

	moduleName := "composer"
	filterName := "coraza-waf"
	filterMode := string(wafv1alpha1.DynamicModuleFilterModeFull)

	if dmConfig != nil {
		if dmConfig.ModuleName != "" {
			moduleName = dmConfig.ModuleName
		}
		if dmConfig.FilterName != "" {
			filterName = dmConfig.FilterName
		}
		if dmConfig.FilterMode != "" {
			filterMode = string(dmConfig.FilterMode)
		}
	}

	filterConfigJSON := buildDynamicModuleFilterConfig(rules, filterMode)

	ws := targetLabelSelector(engine)
	matchLabels := map[string]string{}
	if ws != nil && ws.MatchLabels != nil {
		matchLabels = ws.MatchLabels
	}

	envoyFilter := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "networking.istio.io/v1alpha3",
			"kind":       "EnvoyFilter",
			"metadata": map[string]any{
				"name":      fmt.Sprintf("%s%s", EnvoyFilterNamePrefix, engine.Name),
				"namespace": engine.Namespace,
			},
			"spec": map[string]any{
				"workloadSelector": map[string]any{
					"labels": matchLabels,
				},
				"configPatches": []any{
					map[string]any{
						"applyTo": "HTTP_FILTER",
						"match": map[string]any{
							"context": "GATEWAY",
							"listener": map[string]any{
								"filterChain": map[string]any{
									"filter": map[string]any{
										"name": "envoy.filters.network.http_connection_manager",
										"subFilter": map[string]any{
											"name": "envoy.filters.http.router",
										},
									},
								},
							},
						},
						"patch": map[string]any{
							"operation": "INSERT_BEFORE",
							"value": map[string]any{
								"name": filterName,
								"typed_config": map[string]any{
									"@type": "type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter",
									"dynamic_module_config": map[string]any{
										"name":         moduleName,
										"do_not_close": true,
									},
									"filter_name": filterName,
									"filter_config": map[string]any{
										"@type": "type.googleapis.com/google.protobuf.StringValue",
										"value": filterConfigJSON,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	envoyFilter.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1alpha3",
		Kind:    "EnvoyFilter",
	})

	if r.istioRevision != "" {
		labels := envoyFilter.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["istio.io/rev"] = r.istioRevision
		envoyFilter.SetLabels(labels)
	}

	return envoyFilter
}

// buildDynamicModuleFilterConfig creates the JSON config string for the BOE
// coraza-waf dynamic module. The rules string (aggregated SecLang directives)
// is split into individual lines for the "directives" array.
func buildDynamicModuleFilterConfig(rules string, mode string) string {
	var directives []string
	for _, line := range strings.Split(rules, "\n") {
		if line != "" {
			directives = append(directives, line)
		}
	}

	config := map[string]any{
		"directives": directives,
		"mode":       mode,
	}

	data, err := json.Marshal(config)
	if err != nil {
		return `{}`
	}
	return string(data)
}

// -----------------------------------------------------------------------------
// Engine Controller - Dynamic Module ConfigMap (Deployment overlay)
// -----------------------------------------------------------------------------

// dmConfigMapName returns the deterministic ConfigMap name for a given Engine.
func dmConfigMapName(engineName string) string {
	return dmConfigMapPrefix + engineName
}

// applyDynamicModuleConfigMap creates or updates the ConfigMap that contains a
// Deployment overlay for injecting the dynamic module init container into
// gateway pods.
func (r *EngineReconciler) applyDynamicModuleConfigMap(ctx context.Context, log logr.Logger, req ctrl.Request, engine *wafv1alpha1.Engine) error {
	cmName := dmConfigMapName(engine.Name)

	desired := buildDynamicModuleConfigMap(engine)

	if err := controllerutil.SetControllerReference(engine, desired, r.Scheme); err != nil {
		logError(log, req, "Engine", err, "Failed to set owner reference on ConfigMap")
		return err
	}

	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: engine.Namespace}, &existing)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			logAPIError(log, req, "Engine", err, "Failed to get ConfigMap", &existing)
			return err
		}
		logDebug(log, req, "Engine", "Creating dynamic module ConfigMap", "configMapName", cmName)
		if err := r.Create(ctx, desired); err != nil {
			logAPIError(log, req, "Engine", err, "Failed to create ConfigMap", desired)
			return err
		}
		logInfo(log, req, "Engine", "Dynamic module ConfigMap created", "configMapName", cmName)
		return nil
	}

	desired.ResourceVersion = existing.ResourceVersion
	logDebug(log, req, "Engine", "Updating dynamic module ConfigMap", "configMapName", cmName)
	if err := r.Update(ctx, desired); err != nil {
		logAPIError(log, req, "Engine", err, "Failed to update ConfigMap", desired)
		return err
	}
	logInfo(log, req, "Engine", "Dynamic module ConfigMap updated", "configMapName", cmName)
	return nil
}

// buildDynamicModuleConfigMap constructs a ConfigMap with a Deployment overlay
// YAML. Istio's gateway controller applies this as a Strategic Merge Patch
// when the Gateway references it via spec.infrastructure.parametersRef.
func buildDynamicModuleConfigMap(engine *wafv1alpha1.Engine) *corev1.ConfigMap {
	dmConfig := engine.Spec.Driver.DynamicModule

	moduleName := "composer"
	if dmConfig != nil && dmConfig.ModuleName != "" {
		moduleName = dmConfig.ModuleName
	}
	soFilename := "lib" + moduleName + ".so"

	proxyImageLine := ""
	if dmConfig != nil && dmConfig.ProxyImage != "" {
		proxyImageLine = fmt.Sprintf("\n        image: %s", dmConfig.ProxyImage)
	}

	moduleImage := ""
	if dmConfig != nil {
		moduleImage = dmConfig.ModuleImage
	}

	overlay := fmt.Sprintf(`spec:
  template:
    spec:
      initContainers:
      - name: dm-tools-init
        image: %s
        command: ["cp", "/bin/cp", "/tools/cp"]
        volumeMounts:
        - name: %s
          mountPath: /tools
      - name: dynamic-module-init
        image: %s
        command: ["/tools/cp", "/%s", "%s/%s"]
        volumeMounts:
        - name: %s
          mountPath: /tools
          readOnly: true
        - name: %s
          mountPath: %s
      containers:
      - name: istio-proxy%s
        env:
        - name: ENVOY_DYNAMIC_MODULES_SEARCH_PATH
          value: %s
        - name: GODEBUG
          value: cgocheck=0
        volumeMounts:
        - name: %s
          mountPath: %s
          readOnly: true
        - name: %s
          mountPath: /tmp
      volumes:
      - name: %s
        emptyDir: {}
      - name: %s
        emptyDir: {}
      - name: %s
        emptyDir:
          medium: Memory
`, dmToolsImage,
		dmToolsVolumeName,
		moduleImage, soFilename, dmMountPath, soFilename,
		dmToolsVolumeName,
		dmVolumeName, dmMountPath,
		proxyImageLine,
		dmMountPath,
		dmVolumeName, dmMountPath,
		dmTmpVolumeName,
		dmVolumeName,
		dmToolsVolumeName,
		dmTmpVolumeName)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dmConfigMapName(engine.Name),
			Namespace: engine.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "coraza-kubernetes-operator",
				"app.kubernetes.io/component":  "dynamic-module-overlay",
				"app.kubernetes.io/instance":   engine.Name,
			},
		},
		Data: map[string]string{
			"deployment": overlay,
		},
	}
}
