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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	rcache "github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// -----------------------------------------------------------------------------
// Dynamic Module - EnvoyFilter Builder Tests
// -----------------------------------------------------------------------------

func TestBuildEnvoyFilter_BasicStructure(t *testing.T) {
	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:      "test-dm",
		Namespace: testNamespace,
	})

	reconciler := &EngineReconciler{}
	ef := reconciler.buildEnvoyFilter(engine, "SecRuleEngine On\nSecRequestBodyAccess On")

	assert.Equal(t, "networking.istio.io/v1alpha3", ef.GetAPIVersion())
	assert.Equal(t, "EnvoyFilter", ef.GetKind())
	assert.Equal(t, "coraza-engine-test-dm", ef.GetName())
	assert.Equal(t, testNamespace, ef.GetNamespace())

	spec, ok := ef.Object["spec"].(map[string]any)
	require.True(t, ok)

	ws, ok := spec["workloadSelector"].(map[string]any)
	require.True(t, ok)
	labels, ok := ws["labels"].(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "test-gw", labels[gatewayNameLabel])

	patches, ok := spec["configPatches"].([]any)
	require.True(t, ok)
	require.Len(t, patches, 1)

	patch := patches[0].(map[string]any)
	assert.Equal(t, "HTTP_FILTER", patch["applyTo"])

	patchValue := patch["patch"].(map[string]any)
	assert.Equal(t, "INSERT_BEFORE", patchValue["operation"])

	filterValue := patchValue["value"].(map[string]any)
	assert.Equal(t, "coraza-waf", filterValue["name"])

	typedConfig := filterValue["typed_config"].(map[string]any)
	assert.Equal(t, "type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter", typedConfig["@type"])
	assert.Equal(t, "coraza-waf", typedConfig["filter_name"])

	dmConfig := typedConfig["dynamic_module_config"].(map[string]any)
	assert.Equal(t, "composer", dmConfig["name"])
	assert.Equal(t, true, dmConfig["do_not_close"])
}

func TestBuildEnvoyFilter_IstioRevisionLabel(t *testing.T) {
	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:      "rev-test",
		Namespace: testNamespace,
	})

	withRev := &EngineReconciler{istioRevision: "canary"}
	ef := withRev.buildEnvoyFilter(engine, "SecRuleEngine On")
	assert.Equal(t, "canary", ef.GetLabels()["istio.io/rev"])

	noRev := &EngineReconciler{}
	ef2 := noRev.buildEnvoyFilter(engine, "SecRuleEngine On")
	_, has := ef2.GetLabels()["istio.io/rev"]
	assert.False(t, has, "istio.io/rev should not be set when revision is empty")
}

func TestBuildEnvoyFilter_RulesEmbedded(t *testing.T) {
	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:      "rules-test",
		Namespace: testNamespace,
	})

	rules := "SecRuleEngine On\nSecRequestBodyAccess On\nSecRule REQUEST_URI \"@contains /admin\" \"id:101,phase:1,deny,status:403\""

	reconciler := &EngineReconciler{}
	ef := reconciler.buildEnvoyFilter(engine, rules)

	spec := ef.Object["spec"].(map[string]any)
	patches := spec["configPatches"].([]any)
	patch := patches[0].(map[string]any)
	patchSpec := patch["patch"].(map[string]any)
	filterValue := patchSpec["value"].(map[string]any)
	typedConfig := filterValue["typed_config"].(map[string]any)
	filterConfig := typedConfig["filter_config"].(map[string]any)

	configJSON := filterConfig["value"].(string)

	var parsed struct {
		Directives []string `json:"directives"`
		Mode       string   `json:"mode"`
	}
	err := json.Unmarshal([]byte(configJSON), &parsed)
	require.NoError(t, err)

	assert.Equal(t, "FULL", parsed.Mode)
	require.Len(t, parsed.Directives, 3)
	assert.Equal(t, "SecRuleEngine On", parsed.Directives[0])
	assert.Equal(t, "SecRequestBodyAccess On", parsed.Directives[1])
	assert.Contains(t, parsed.Directives[2], "@contains /admin")
}

func TestBuildEnvoyFilter_FilterModes(t *testing.T) {
	tests := []struct {
		mode     wafv1alpha1.DynamicModuleFilterMode
		expected string
	}{
		{wafv1alpha1.DynamicModuleFilterModeRequestOnly, "REQUEST_ONLY"},
		{wafv1alpha1.DynamicModuleFilterModeResponseOnly, "RESPONSE_ONLY"},
		{wafv1alpha1.DynamicModuleFilterModeFull, "FULL"},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
				Name:       "mode-test",
				Namespace:  testNamespace,
				FilterMode: tt.mode,
			})

			reconciler := &EngineReconciler{}
			ef := reconciler.buildEnvoyFilter(engine, "SecRuleEngine On")

			spec := ef.Object["spec"].(map[string]any)
			patches := spec["configPatches"].([]any)
			patch := patches[0].(map[string]any)
			patchSpec := patch["patch"].(map[string]any)
			filterValue := patchSpec["value"].(map[string]any)
			typedConfig := filterValue["typed_config"].(map[string]any)
			filterConfig := typedConfig["filter_config"].(map[string]any)
			configJSON := filterConfig["value"].(string)

			var parsed struct {
				Mode string `json:"mode"`
			}
			err := json.Unmarshal([]byte(configJSON), &parsed)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, parsed.Mode)
		})
	}
}

func TestBuildEnvoyFilter_CustomModuleAndFilter(t *testing.T) {
	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:       "custom-names",
		Namespace:  testNamespace,
		ModuleName: "my-module",
		FilterName: "my-waf",
	})

	reconciler := &EngineReconciler{}
	ef := reconciler.buildEnvoyFilter(engine, "SecRuleEngine On")

	spec := ef.Object["spec"].(map[string]any)
	patches := spec["configPatches"].([]any)
	patch := patches[0].(map[string]any)
	patchSpec := patch["patch"].(map[string]any)
	filterValue := patchSpec["value"].(map[string]any)
	typedConfig := filterValue["typed_config"].(map[string]any)
	dmConfig := typedConfig["dynamic_module_config"].(map[string]any)

	assert.Equal(t, "my-module", dmConfig["name"])
	assert.Equal(t, "my-waf", typedConfig["filter_name"])
	assert.Equal(t, "my-waf", filterValue["name"])
}

// -----------------------------------------------------------------------------
// Dynamic Module - Filter Config Builder Tests
// -----------------------------------------------------------------------------

func TestBuildDynamicModuleFilterConfig(t *testing.T) {
	rules := "SecRuleEngine On\nSecRequestBodyAccess On\n\nSecRule ARGS \"test\" \"id:1\""
	result := buildDynamicModuleFilterConfig(rules, "FULL")

	var parsed struct {
		Directives []string `json:"directives"`
		Mode       string   `json:"mode"`
	}
	err := json.Unmarshal([]byte(result), &parsed)
	require.NoError(t, err)

	assert.Equal(t, "FULL", parsed.Mode)
	assert.Len(t, parsed.Directives, 3, "empty lines should be filtered out")
	assert.Equal(t, "SecRuleEngine On", parsed.Directives[0])
	assert.Equal(t, "SecRequestBodyAccess On", parsed.Directives[1])
	assert.Equal(t, "SecRule ARGS \"test\" \"id:1\"", parsed.Directives[2])
}

// -----------------------------------------------------------------------------
// Dynamic Module - ConfigMap Builder Tests
// -----------------------------------------------------------------------------

func TestBuildDynamicModuleConfigMap(t *testing.T) {
	t.Run("default module name", func(t *testing.T) {
		engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
			Name:        "cm-test",
			Namespace:   testNamespace,
			ModuleImage: "ghcr.io/tetratelabs/boe-composer:v0.6.0",
		})

		cm := buildDynamicModuleConfigMap(engine)

		assert.Equal(t, "coraza-dm-cm-test", cm.Name)
		assert.Equal(t, testNamespace, cm.Namespace)
		assert.Equal(t, "coraza-kubernetes-operator", cm.Labels["app.kubernetes.io/managed-by"])
		assert.Equal(t, "cm-test", cm.Labels["app.kubernetes.io/instance"])

		overlay := cm.Data["deployment"]
		assert.Contains(t, overlay, "ghcr.io/tetratelabs/boe-composer:v0.6.0")
		assert.Contains(t, overlay, "libcomposer.so")
		assert.Contains(t, overlay, "ENVOY_DYNAMIC_MODULES_SEARCH_PATH")
		assert.Contains(t, overlay, "/etc/envoy/dynamic-modules")
		assert.Contains(t, overlay, "GODEBUG")
		assert.Contains(t, overlay, "cgocheck=0")
		assert.Contains(t, overlay, "dynamic-module-init")
		assert.Contains(t, overlay, "istio-proxy")
		assert.Contains(t, overlay, "dm-tools-init")
		assert.Contains(t, overlay, "busybox:stable-musl")
		assert.Contains(t, overlay, "/tools/cp")
		assert.Contains(t, overlay, "dm-tools")
		assert.Contains(t, overlay, "dm-tmp")
		assert.Contains(t, overlay, "mountPath: /tmp")
		assert.Contains(t, overlay, "medium: Memory")
	})

	t.Run("proxy image override", func(t *testing.T) {
		engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
			Name:        "proxy-img",
			Namespace:   testNamespace,
			ModuleImage: "ghcr.io/tetratelabs/boe-composer:v0.6.0",
			ProxyImage:  "gcr.io/istio-testing/proxyv2:1.31-dev",
		})

		cm := buildDynamicModuleConfigMap(engine)
		overlay := cm.Data["deployment"]
		assert.Contains(t, overlay, "image: gcr.io/istio-testing/proxyv2:1.31-dev")
	})

	t.Run("no proxy image by default", func(t *testing.T) {
		engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
			Name:        "no-proxy-img",
			Namespace:   testNamespace,
			ModuleImage: "ghcr.io/tetratelabs/boe-composer:v0.6.0",
		})

		cm := buildDynamicModuleConfigMap(engine)
		overlay := cm.Data["deployment"]
		assert.NotContains(t, overlay, "image: gcr.io")
	})

	t.Run("custom module name changes so filename", func(t *testing.T) {
		engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
			Name:        "custom-mod",
			Namespace:   testNamespace,
			ModuleName:  "my-module",
			ModuleImage: "example.com/my-module:latest",
		})

		cm := buildDynamicModuleConfigMap(engine)
		overlay := cm.Data["deployment"]
		assert.Contains(t, overlay, "libmy-module.so")
		assert.NotContains(t, overlay, "libcomposer.so")
	})
}

// -----------------------------------------------------------------------------
// Dynamic Module - Integration Tests (envtest)
// -----------------------------------------------------------------------------

func TestDynamicModule_RulesNotCached(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := rcache.NewRuleSetCache()

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "dm-nocache-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:        "dm-no-cache",
		Namespace:   testNamespace,
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:            k8sClient,
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		ruleSetCache:      ruleSetCache,
		operatorNamespace: testNamespace,
	}

	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer and requeues.
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile detects rules are not cached.
	_, err = reconciler.Reconcile(ctx, engineReq)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet available in cache")

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: engine.Name, Namespace: engine.Namespace}, &updated))
	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, metav1.ConditionTrue, degraded.Status)
	assert.Equal(t, "RulesNotCached", degraded.Reason)
}

func TestDynamicModule_Provisioning(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := rcache.NewRuleSetCache()
	cacheKey := testNamespace + "/dm-provision-ruleset"
	ruleSetCache.Put(cacheKey, "SecRuleEngine On\nSecRequestBodyAccess On", nil)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "dm-provision-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:        "dm-provision",
		Namespace:   testNamespace,
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:            k8sClient,
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		ruleSetCache:      ruleSetCache,
		operatorNamespace: testNamespace,
	}

	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile: adds finalizer, requeues
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "expected requeue after finalizer add")

	// Second reconcile: provisions the EnvoyFilter
	result, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "dynamic module driver should not schedule token renewal")

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: engine.Name, Namespace: engine.Namespace}, &updated))
	ready := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, "Configured", ready.Reason)

	// Verify EnvoyFilter was created in the API server
	envoyFilter := &unstructured.Unstructured{}
	envoyFilter.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1alpha3",
		Kind:    "EnvoyFilter",
	})
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      fmt.Sprintf("%s%s", EnvoyFilterNamePrefix, engine.Name),
		Namespace: engine.Namespace,
	}, envoyFilter)
	require.NoError(t, err)
	assert.Equal(t, "EnvoyFilter", envoyFilter.GetKind())
}

func TestDynamicModule_ConfigMapCreated(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := rcache.NewRuleSetCache()
	cacheKey := testNamespace + "/dm-cm-ruleset"
	ruleSetCache.Put(cacheKey, "SecRuleEngine On", nil)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "dm-cm-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:        "dm-with-image",
		Namespace:   testNamespace,
		RuleSetName: ruleset.Name,
		ModuleImage: "ghcr.io/tetratelabs/boe-composer:v0.6.0",
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:            k8sClient,
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		ruleSetCache:      ruleSetCache,
		operatorNamespace: testNamespace,
	}

	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile: adds finalizer
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile: creates ConfigMap and EnvoyFilter
	_, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)

	// Verify ConfigMap was created
	var cm corev1.ConfigMap
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      dmConfigMapName(engine.Name),
		Namespace: engine.Namespace,
	}, &cm)
	require.NoError(t, err)

	assert.Equal(t, "coraza-kubernetes-operator", cm.Labels["app.kubernetes.io/managed-by"])
	overlay := cm.Data["deployment"]
	assert.Contains(t, overlay, "ghcr.io/tetratelabs/boe-composer:v0.6.0")
	assert.Contains(t, overlay, "libcomposer.so")
	assert.Contains(t, overlay, "ENVOY_DYNAMIC_MODULES_SEARCH_PATH")
	assert.Contains(t, overlay, "GODEBUG")

	// Verify owner reference is set
	require.Len(t, cm.OwnerReferences, 1)
	assert.Equal(t, engine.Name, cm.OwnerReferences[0].Name)
	assert.Equal(t, "Engine", cm.OwnerReferences[0].Kind)
}

func TestDynamicModule_NoConfigMapWithoutImage(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := rcache.NewRuleSetCache()
	cacheKey := testNamespace + "/dm-noimg-ruleset"
	ruleSetCache.Put(cacheKey, "SecRuleEngine On", nil)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "dm-noimg-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	engine := utils.NewTestDynamicModuleEngine(utils.DynamicModuleEngineOptions{
		Name:        "dm-no-image",
		Namespace:   testNamespace,
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:            k8sClient,
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		ruleSetCache:      ruleSetCache,
		operatorNamespace: testNamespace,
	}

	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile: adds finalizer
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile: provisions EnvoyFilter but no ConfigMap
	_, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)

	// Verify no ConfigMap was created
	var cm corev1.ConfigMap
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      dmConfigMapName(engine.Name),
		Namespace: engine.Namespace,
	}, &cm)
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil,
		"ConfigMap should not exist when moduleImage is not set")
}
