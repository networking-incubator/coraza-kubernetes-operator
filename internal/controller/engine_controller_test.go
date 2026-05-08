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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/defaults"
	rcache "github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// createTestGateway creates a Gateway resource in the envtest cluster for target
// validation tests. The resource is cleaned up via t.Cleanup. The returned
// object can be used for manual deletion in tests that need to remove the
// Gateway mid-test (the cleanup will log but not fail on NotFound).
func createTestGateway(t *testing.T, ctx context.Context, c client.Client, name, namespace string) *unstructured.Unstructured {
	t.Helper()
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "Gateway",
	})
	gw.SetName(name)
	gw.SetNamespace(namespace)
	gw.Object["spec"] = map[string]any{
		"gatewayClassName": "istio",
		"listeners": []any{
			map[string]any{
				"name":     "http",
				"port":     int64(80),
				"protocol": "HTTP",
			},
		},
	}
	require.NoError(t, c.Create(ctx, gw))
	t.Cleanup(func() {
		if err := c.Delete(ctx, gw); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete gateway: %v", err)
		}
	})
	return gw
}

func TestEngineReconciler_ReconcileNotFound(t *testing.T) {
	ctx, cleanup := setupTest(t)
	defer cleanup()

	t.Log("Creating reconciler for non-existent engine test")
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}

	t.Log("Reconciling non-existent engine - should not error")
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "non-existent",
			Namespace: "default",
		},
	})

	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
}

func TestEngineReconciler_BuildWasmPlugin_IstioRevisionLabel(t *testing.T) {
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:      "rev-label-engine",
		Namespace: testNamespace,
	})

	const testWasmOCI = "oci://test.example/wasm:latest"
	withRev := &EngineReconciler{
		ruleSetCacheServerCluster: "test-cluster",
		istioRevision:             "canary",
	}
	w := withRev.buildWasmPlugin(engine, testWasmOCI, "test-token")
	assert.Equal(t, "canary", w.GetLabels()["istio.io/rev"])

	noRev := &EngineReconciler{
		ruleSetCacheServerCluster: "test-cluster",
		operatorNamespace:         testNamespace,
	}
	w2 := noRev.buildWasmPlugin(engine, testWasmOCI, "test-token")
	_, has := w2.GetLabels()["istio.io/rev"]
	assert.False(t, has, "istio.io/rev should not be set when revision is empty")
}

func TestEngineReconciler_BuildWasmPlugin_CacheToken(t *testing.T) {
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:      "token-test-engine",
		Namespace: testNamespace,
	})

	reconciler := &EngineReconciler{
		ruleSetCacheServerCluster: "test-cluster",
	}

	t.Run("cache_token is set in pluginConfig", func(t *testing.T) {
		w := reconciler.buildWasmPlugin(engine, "oci://test.example/wasm:latest", "my-jwt-token")

		spec, found, err := getNestedMap(w.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)

		pluginConfig, found, err := getNestedMap(spec, "pluginConfig")
		require.NoError(t, err)
		require.True(t, found)

		token, found, err := getNestedString(pluginConfig, "cache_token")
		require.NoError(t, err)
		require.True(t, found, "cache_token should be present in pluginConfig")
		assert.Equal(t, "my-jwt-token", token)
	})

	t.Run("empty token is still set", func(t *testing.T) {
		w := reconciler.buildWasmPlugin(engine, "oci://test.example/wasm:latest", "")

		spec, found, err := getNestedMap(w.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)

		pluginConfig, found, err := getNestedMap(spec, "pluginConfig")
		require.NoError(t, err)
		require.True(t, found)

		token, found, err := getNestedString(pluginConfig, "cache_token")
		require.NoError(t, err)
		require.True(t, found, "cache_token key should exist even when empty")
		assert.Empty(t, token)
	})
}

func TestEngineReconciler_ReconcileMissingRuleSet(t *testing.T) {
	ctx := context.Background()

	createTestGateway(t, ctx, k8sClient, "test-gw", testNamespace)

	t.Log("Creating test engine referencing non-existent RuleSet")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "test-engine-missing-ruleset",
		Namespace:   testNamespace,
		RuleSetName: "non-existent-ruleset",
	})
	err := k8sClient.Create(ctx, engine)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	}()

	t.Log("Reconciling Engine with missing RuleSet")
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer and requeues after a short delay
	// (metadata-only changes don't bump generation, so the predicate would
	// filter out the update event).
	result, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile detects the missing RuleSet and marks Engine degraded.
	result, err = reconciler.Reconcile(ctx, req)

	t.Log("Verifying reconciliation behavior")
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
}

func TestEngineReconciler_ReconcileIstioDriver(t *testing.T) {
	ctx := context.Background()
	ns := utils.NewTestEngine(utils.EngineOptions{}).Namespace

	createTestGateway(t, ctx, k8sClient, "test-gw", ns)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "test-ruleset",
		Namespace: ns,
	})
	err := k8sClient.Create(ctx, ruleset)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	}()

	t.Log("Creating test engine with Istio driver")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:      "test-engine",
		Namespace: ns,
	})
	err = k8sClient.Create(ctx, engine)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	}()

	t.Log("Reconciling Istio Engine")
	recorder := utils.NewFakeRecorder()
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  recorder,
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer and requeues after a short delay.
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile proceeds with provisioning and schedules token renewal.
	result, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "should schedule token renewal requeue")

	t.Log("Verifying engine status")
	var updated wafv1alpha1.Engine
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      engine.Name,
		Namespace: engine.Namespace,
	}, &updated)
	require.NoError(t, err)
	assert.Len(t, updated.Status.Conditions, 2, "should have Ready and Accepted conditions")
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "Configured", readyCond.Reason)
	acceptedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedCond)
	assert.Equal(t, metav1.ConditionTrue, acceptedCond.Status)

	assert.True(t, recorder.HasEvent("Normal", "WasmPluginCreated"),
		"expected Normal/WasmPluginCreated event; got: %v", recorder.Events)
}

func TestEngineReconciler_StatusUpdateHandling(t *testing.T) {
	ctx := context.Background()

	createTestGateway(t, ctx, k8sClient, "test-gw", testNamespace)

	t.Log("Creating test engine for status update testing")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:      "status-test",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	t.Log("Reconciling engine to verify status update")
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer and requeues after a short delay.
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile proceeds with status updates.
	_, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)

	t.Log("Verifying status conditions were set")
	var updated wafv1alpha1.Engine
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      engine.Name,
		Namespace: engine.Namespace,
	}, &updated)
	require.NoError(t, err)
	require.NotNil(t, updated.Status)
	if len(updated.Status.Conditions) > 0 {
		condition := updated.Status.Conditions[0]
		assert.NotEmpty(t, condition.Type)
		assert.NotEmpty(t, condition.Status)
		assert.NotEmpty(t, condition.Reason)
	}
}

func TestEngineReconciler_FailurePolicyInWasmPluginConfig(t *testing.T) {
	ctx := context.Background()

	createTestGateway(t, ctx, k8sClient, "test-gw", testNamespace)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "test-ruleset",
		Namespace: testNamespace,
	})
	err := k8sClient.Create(ctx, ruleset)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	tests := []struct {
		name                  string
		failurePolicy         wafv1alpha1.FailurePolicy
		expectedFailurePolicy string
	}{
		{
			name:                  "failure policy fail",
			failurePolicy:         wafv1alpha1.FailurePolicyFail,
			expectedFailurePolicy: "fail",
		},
		{
			name:                  "failure policy allow",
			failurePolicy:         wafv1alpha1.FailurePolicyAllow,
			expectedFailurePolicy: "allow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			t.Logf("Creating test engine with failure policy: %s", tt.failurePolicy)
			engine := utils.NewTestEngine(utils.EngineOptions{
				Name:          "test-engine-" + string(tt.failurePolicy),
				Namespace:     testNamespace,
				FailurePolicy: tt.failurePolicy,
			})
			err := k8sClient.Create(ctx, engine)
			require.NoError(t, err)
			defer func() {
				if err := k8sClient.Delete(ctx, engine); err != nil {
					t.Logf("Failed to delete engine: %v", err)
				}
			}()

			t.Log("Reconciling engine")
			reconciler := &EngineReconciler{
				Client:                    k8sClient,
				Scheme:                    scheme,
				Recorder:                  utils.NewTestRecorder(),
				kubeClient:                testKubeClient,
				ruleSetCacheServerCluster: "test-cluster",
				defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
				operatorNamespace:         testNamespace,
			}
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      engine.Name,
					Namespace: engine.Namespace,
				},
			}

			// First reconcile adds the finalizer and requeues after a short delay.
			result, err := reconciler.Reconcile(ctx, req)
			require.NoError(t, err)
			assert.NotZero(t, result.RequeueAfter)

			// Second reconcile provisions the WasmPlugin and schedules token renewal.
			result, err = reconciler.Reconcile(ctx, req)
			require.NoError(t, err)
			assert.NotZero(t, result.RequeueAfter, "should schedule token renewal requeue")

			t.Log("Fetching created WasmPlugin")
			wasmURL, _ := reconciler.wasmPluginOCIURLSource(engine)
			wasmPlugin := reconciler.buildWasmPlugin(engine, wasmURL, "test-token")
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      wasmPlugin.GetName(),
				Namespace: wasmPlugin.GetNamespace(),
			}, wasmPlugin)
			require.NoError(t, err)

			t.Log("Verifying pluginConfig contains failure_policy")
			spec, found, err := getNestedMap(wasmPlugin.Object, "spec")
			require.NoError(t, err)
			require.True(t, found, "spec not found in WasmPlugin")

			pluginConfig, found, err := getNestedMap(spec, "pluginConfig")
			require.NoError(t, err)
			require.True(t, found, "pluginConfig not found in WasmPlugin spec")

			failurePolicy, found, err := getNestedString(pluginConfig, "failure_policy")
			require.NoError(t, err)
			require.True(t, found, "failure_policy not found in pluginConfig")
			assert.Equal(t, tt.expectedFailurePolicy, failurePolicy)

			cacheToken, found, err := getNestedString(pluginConfig, "cache_token")
			require.NoError(t, err)
			require.True(t, found, "cache_token not found in pluginConfig")
			assert.NotEmpty(t, cacheToken, "cache_token should be a non-empty JWT token")
		})
	}
}

// getNestedMap retrieves a nested map from an unstructured object
func getNestedMap(obj map[string]any, key string) (map[string]any, bool, error) {
	val, found := obj[key]
	if !found {
		return nil, false, nil
	}
	mapVal, ok := val.(map[string]any)
	if !ok {
		return nil, false, assert.AnError
	}
	return mapVal, true, nil
}

// getNestedString retrieves a nested string from an unstructured object
func getNestedString(obj map[string]any, key string) (string, bool, error) {
	val, found := obj[key]
	if !found {
		return "", false, nil
	}
	strVal, ok := val.(string)
	if !ok {
		return "", false, assert.AnError
	}
	return strVal, true, nil
}

func TestEngineReconciler_ImagePullSecretInWasmPlugin(t *testing.T) {
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		operatorNamespace:         testNamespace,
	}

	t.Run("imagePullSecret is set when specified", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{
			Name:            "engine-with-pull-secret",
			Namespace:       testNamespace,
			ImagePullSecret: "my-registry-secret",
		})

		wasmPlugin := reconciler.buildWasmPlugin(engine, "", "")

		spec, found, err := getNestedMap(wasmPlugin.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)

		secret, found, err := getNestedString(spec, "imagePullSecret")
		require.NoError(t, err)
		require.True(t, found, "imagePullSecret should be present in WasmPlugin spec")
		assert.Equal(t, "my-registry-secret", secret)
	})

	t.Run("imagePullSecret is omitted when empty", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{
			Name:      "engine-without-pull-secret",
			Namespace: testNamespace,
		})

		wasmPlugin := reconciler.buildWasmPlugin(engine, "", "")

		spec, found, err := getNestedMap(wasmPlugin.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)

		_, found = spec["imagePullSecret"]
		assert.False(t, found, "imagePullSecret should not be present in WasmPlugin spec when empty")
	})
}

func TestEngineReconciler_ImagePullSecretEnvtest(t *testing.T) {
	ctx := context.Background()

	createTestGateway(t, ctx, k8sClient, "test-gw", testNamespace)

	t.Log("Creating RuleSet for imagePullSecret envtest")
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "pull-secret-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		operatorNamespace:         testNamespace,
		kubeClient:                testKubeClient,
	}

	t.Run("imagePullSecret persisted in WasmPlugin via server-side apply", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{
			Name:            "engine-pullsecret-envtest",
			Namespace:       testNamespace,
			RuleSetName:     ruleset.Name,
			ImagePullSecret: "my-registry-secret",
		})
		require.NoError(t, k8sClient.Create(ctx, engine))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, engine); err != nil {
				t.Logf("Failed to delete engine: %v", err)
			}
		})

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      engine.Name,
				Namespace: engine.Namespace,
			},
		}

		// First reconcile adds the finalizer and requeues after a short delay.
		result, err := reconciler.Reconcile(ctx, req)
		require.NoError(t, err)
		assert.NotZero(t, result.RequeueAfter)

		// Second reconcile provisions the WasmPlugin and schedules token renewal.
		result, err = reconciler.Reconcile(ctx, req)
		require.NoError(t, err)
		assert.NotZero(t, result.RequeueAfter, "should schedule token renewal requeue")

		t.Log("Fetching WasmPlugin from API server")
		wasmPlugin := &unstructured.Unstructured{}
		wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "extensions.istio.io",
			Version: "v1alpha1",
			Kind:    "WasmPlugin",
		})
		err = k8sClient.Get(ctx, types.NamespacedName{
			Name:      wasmPluginName(engine.Name),
			Namespace: engine.Namespace,
		}, wasmPlugin)
		require.NoError(t, err)

		spec, found, err := getNestedMap(wasmPlugin.Object, "spec")
		require.NoError(t, err)
		require.True(t, found, "spec not found in WasmPlugin")

		secret, found, err := getNestedString(spec, "imagePullSecret")
		require.NoError(t, err)
		require.True(t, found, "imagePullSecret should be persisted in WasmPlugin spec after server-side apply")
		assert.Equal(t, "my-registry-secret", secret)
	})

	t.Run("imagePullSecret omitted in WasmPlugin when not set", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{
			Name:        "engine-no-pullsecret-envtest",
			Namespace:   testNamespace,
			RuleSetName: ruleset.Name,
		})
		require.NoError(t, k8sClient.Create(ctx, engine))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, engine); err != nil {
				t.Logf("Failed to delete engine: %v", err)
			}
		})

		req := ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      engine.Name,
				Namespace: engine.Namespace,
			},
		}

		// First reconcile adds the finalizer and requeues after a short delay.
		result, err := reconciler.Reconcile(ctx, req)
		require.NoError(t, err)
		assert.NotZero(t, result.RequeueAfter)

		// Second reconcile provisions the WasmPlugin and schedules token renewal.
		result, err = reconciler.Reconcile(ctx, req)
		require.NoError(t, err)
		assert.NotZero(t, result.RequeueAfter, "should schedule token renewal requeue")

		t.Log("Fetching WasmPlugin from API server")
		wasmPlugin := &unstructured.Unstructured{}
		wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "extensions.istio.io",
			Version: "v1alpha1",
			Kind:    "WasmPlugin",
		})
		err = k8sClient.Get(ctx, types.NamespacedName{
			Name:      wasmPluginName(engine.Name),
			Namespace: engine.Namespace,
		}, wasmPlugin)
		require.NoError(t, err)

		spec, found, err := getNestedMap(wasmPlugin.Object, "spec")
		require.NoError(t, err)
		require.True(t, found, "spec not found in WasmPlugin")

		_, found = spec["imagePullSecret"]
		assert.False(t, found, "imagePullSecret should not be present in WasmPlugin spec when not set")
	})
}

func TestEngineReconciler_HandleInvalidDriverConfiguration_NilStatus(t *testing.T) {
	ctx := context.Background()

	engine := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nil-status-engine",
			Namespace: testNamespace,
		},
		Spec: wafv1alpha1.EngineSpec{
			RuleSet: wafv1alpha1.RuleSetReference{Name: "test-ruleset"},
		},
	}
	// Status is nil (zero value for *EngineStatus).
	require.Nil(t, engine.Status, "precondition: Status must be nil")

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}

	// handleInvalidDriverConfiguration must not panic when engine.Status is nil.
	err := reconciler.handleInvalidDriverConfiguration(ctx, ctrl.Log, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}, engine)
	require.Error(t, err, "should return an error for unsupported driver type")
	assert.Contains(t, err.Error(), "unsupported driver type")
	assert.NotNil(t, engine.Status, "Status should be initialized after the call")
}

func TestEngineReconciler_SelectDriver_NilDriverDefaultsToWasm(t *testing.T) {
	ctx := context.Background()

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "selectdriver-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	validEngine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "selectdriver-nil-driver",
		Namespace:   testNamespace,
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, validEngine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, validEngine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	// Fetch back and strip the driver — should default to wasm.
	var fetched wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      validEngine.Name,
		Namespace: validEngine.Namespace,
	}, &fetched))
	fetched.Spec.Driver = wafv1alpha1.DriverConfig{}
	if fetched.Status == nil {
		fetched.Status = &wafv1alpha1.EngineStatus{}
	}

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}

	// selectDriver with nil driver should default to wasm and not panic.
	_, err := reconciler.selectDriver(ctx, ctrl.Log, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      fetched.Name,
			Namespace: fetched.Namespace,
		},
	}, fetched)
	require.NoError(t, err)
}

func TestEngineReconciler_NilTargetRef_MarksDegraded(t *testing.T) {
	ctx := context.Background()

	// Create a valid engine so the status patch inside
	// provisionWasmDriver can talk to the API server.
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "nil-ws-engine",
		Namespace:   testNamespace,
		RuleSetName: "test-ruleset",
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// Fetch the persisted engine and clear TargetRef in-memory to
	// simulate bypassed CRD validation (e.g. direct API write). We cannot
	// use k8sClient.Update because the CRD webhook rejects the change.
	var fetched wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, engineReq.NamespacedName, &fetched))
	fetched.Spec.Target = wafv1alpha1.EngineTarget{}
	if fetched.Status == nil {
		fetched.Status = &wafv1alpha1.EngineStatus{}
	}

	// Call provisionWasmDriver directly — it should detect the empty
	// TargetRef and mark the Engine Degraded instead of creating a
	// WasmPlugin that matches all workloads.
	_, err := reconciler.provisionWasmDriver(ctx, ctrl.Log, engineReq, fetched)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target is required")

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, engineReq.NamespacedName, &updated))
	require.NotNil(t, updated.Status)

	degradedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Degraded")
	require.NotNil(t, degradedCond, "Engine should have Degraded condition")
	assert.Equal(t, metav1.ConditionTrue, degradedCond.Status)
	assert.Equal(t, "InvalidConfiguration", degradedCond.Reason)
	assert.Contains(t, degradedCond.Message, "target is required")
}

func TestEngineReconciler_ValidationRejection(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		engineFunc    func() *wafv1alpha1.Engine
		expectedError string
	}{
		{
			name: "ruleset with empty name",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.RuleSet = wafv1alpha1.RuleSetReference{
					Name: "",
				}
				return engine
			},
			expectedError: "spec.ruleSet: Required value",
		},
		{
			name: "image doesn't start with oci://",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Wasm.Image = "docker://invalid-image"
				return engine
			},
			expectedError: "image must start with oci:// when set",
		},
		{
			name: "image too long",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Wasm.Image = "oci://" + string(make([]byte, 1100))
				return engine
			},
			expectedError: "Too long: may not be more than 1024 bytes",
		},
		{
			name: "targetRef missing",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target = wafv1alpha1.EngineTarget{}
				return engine
			},
			expectedError: "spec.target: Required value",
		},
		{
			name: "targetRef name exceeds label value limit (64 chars)",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target.Name = "a234567890123456789012345678901234567890123456789012345678901234"
				return engine
			},
			expectedError: "spec.target.name",
		},
		{
			name: "target name with spaces rejected",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target.Name = "my gateway"
				return engine
			},
			expectedError: "spec.target.name",
		},
		{
			name: "target name starting with hyphen rejected",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target.Name = "-my-gw"
				return engine
			},
			expectedError: "spec.target.name",
		},
		{
			name: "target name with dots rejected",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target.Name = "my.gateway"
				return engine
			},
			expectedError: "name must be a valid DNS-1035 label",
		},
		{
			name: "target name starting with digit rejected",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target.Name = "123-gw"
				return engine
			},
			expectedError: "name must be a valid DNS-1035 label",
		},
		{
			name: "invalid provider rejected",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target.Provider = "InvalidProvider"
				return engine
			},
			expectedError: "spec.target.provider",
		},
		{
			name: "provider Istio accepted with Gateway target type",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Target.Provider = wafv1alpha1.EngineTargetProviderIstio
				engine.Spec.Target.Type = wafv1alpha1.EngineTargetTypeGateway
				return engine
			},
			expectedError: "",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := tt.engineFunc()
			engine.Name = fmt.Sprintf("validation-test-%d", i)
			engine.Namespace = testNamespace

			err := k8sClient.Create(ctx, engine)
			if tt.expectedError == "" {
				require.NoError(t, err, "expected creation to succeed")
				t.Cleanup(func() {
					_ = k8sClient.Delete(ctx, engine)
				})
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedError)
			}
		})
	}
}

func TestEngineReconciler_ProviderImmutable(t *testing.T) {
	ctx := context.Background()

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:      "immutable-provider-test",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, engine)
	})

	var fetched wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(engine), &fetched))

	fetched.Spec.Target.Provider = "Istio"
	err := k8sClient.Update(ctx, &fetched)
	require.NoError(t, err, "updating with the same provider value should succeed")

	// NOTE: We cannot test rejection of a different provider value via the
	// API server because the enum validation (Enum=Istio) rejects unknown
	// values before the transition rule evaluates. The oldSelf CEL rule
	// will be exercised once additional providers are added to the enum.
}

func TestEngineReconciler_DegradedWhenRuleSetDegraded(t *testing.T) {
	ctx := context.Background()

	createTestGateway(t, ctx, k8sClient, "test-gw", testNamespace)

	t.Log("Creating RuleSet with a Degraded status condition")
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "degraded-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

	t.Log("Setting RuleSet status to Degraded")
	patch := client.MergeFrom(ruleSet.DeepCopy())
	apimeta.SetStatusCondition(&ruleSet.Status.Conditions, metav1.Condition{
		Type:               "Degraded",
		Status:             metav1.ConditionTrue,
		Reason:             "UnsupportedRules",
		Message:            "rule 950150: response body inspection is unsupported",
		LastTransitionTime: metav1.Now(),
	})
	apimeta.SetStatusCondition(&ruleSet.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "UnsupportedRules",
		Message:            "rule 950150: response body inspection is unsupported",
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, k8sClient.Status().Patch(ctx, ruleSet, patch))

	t.Log("Creating Engine referencing the degraded RuleSet")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "engine-with-degraded-ruleset",
		Namespace:   testNamespace,
		RuleSetName: ruleSet.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete Engine: %v", err)
		}
	})

	t.Log("Reconciling Engine")
	recorder := utils.NewFakeRecorder()
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  recorder,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          "oci://test.example/wasm:latest",
		operatorNamespace:         testNamespace,
	}
	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer and requeues after a short delay.
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile detects the degraded RuleSet.
	result, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	t.Log("Verifying Engine is marked Degraded with reason RuleSetDegraded")
	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
		Name:      engine.Name,
		Namespace: engine.Namespace,
	}, &updated))

	require.NotNil(t, updated.Status)
	degradedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Degraded")
	require.NotNil(t, degradedCond, "Engine should have Degraded condition")
	assert.Equal(t, metav1.ConditionTrue, degradedCond.Status)
	assert.Equal(t, "RuleSetDegraded", degradedCond.Reason)
	assert.Contains(t, degradedCond.Message, ruleSet.Name)

	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)

	assert.True(t, recorder.HasEvent("Warning", "RuleSetDegraded"),
		"expected Warning/RuleSetDegraded event; got: %v", recorder.Events)
}

func TestEngineReconciler_ValidationAllowsOmittedWasmImage(t *testing.T) {
	ctx := context.Background()

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:      "omit-wasm-image",
		Namespace: testNamespace,
	})
	engine.Spec.Driver.Wasm.Image = ""

	err := k8sClient.Create(ctx, engine)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})
}

func TestEngineReconciler_BuildWasmPlugin_WasmImageResolution(t *testing.T) {
	const operatorDefault = "oci://operator-default.example/wasm@digest"

	t.Run("nil image uses operator default", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{})
		engine.Spec.Driver.Wasm.Image = ""
		r := &EngineReconciler{defaultWasmImage: operatorDefault}
		wasmURL, _ := r.wasmPluginOCIURLSource(engine)
		wp := r.buildWasmPlugin(engine, wasmURL, "")
		spec, found, err := getNestedMap(wp.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)
		url, found, err := getNestedString(spec, "url")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, operatorDefault, url)
	})

	t.Run("nil wasm uses operator default", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{})
		engine.Spec.Driver.Wasm = nil
		r := &EngineReconciler{defaultWasmImage: operatorDefault}
		wasmURL, _ := r.wasmPluginOCIURLSource(engine)
		assert.Equal(t, operatorDefault, wasmURL)
	})

	t.Run("explicit image wins over operator default", func(t *testing.T) {
		custom := "oci://custom.example/wasm:v2"
		engine := utils.NewTestEngine(utils.EngineOptions{})
		engine.Spec.Driver.Wasm.Image = custom
		r := &EngineReconciler{defaultWasmImage: operatorDefault}
		wasmURL, _ := r.wasmPluginOCIURLSource(engine)
		wp := r.buildWasmPlugin(engine, wasmURL, "")
		spec, found, err := getNestedMap(wp.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)
		url, found, err := getNestedString(spec, "url")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, custom, url)
	})
}

func TestEngineReconciler_TokenStoreIntegration(t *testing.T) {
	ctx := context.Background()

	createTestGateway(t, ctx, k8sClient, "test-gw", testNamespace)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "tokenstore-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "tokenstore-engine",
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
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile: adds finalizer, no token yet.
	result, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	tokenKey := fmt.Sprintf("%s/%s/%s", engine.Namespace, engine.Name, ruleset.Name)
	_, found := reconciler.tokenStore.Load(tokenKey)
	assert.False(t, found, "tokenStore should be empty after finalizer-only reconcile")

	// Second reconcile: provisions SA, token, and WasmPlugin.
	result, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "should schedule token renewal requeue")

	t.Run("token is stored with correct key", func(t *testing.T) {
		val, found := reconciler.tokenStore.Load(tokenKey)
		require.True(t, found, "tokenStore should have entry for %s", tokenKey)

		entry := val.(*TokenEntry)
		assert.NotEmpty(t, entry.Token, "stored token should not be empty")
		assert.False(t, entry.IssuedAt.IsZero(), "IssuedAt should be set")
		assert.True(t, entry.ExpiresAt.After(time.Now()), "token should not be expired")
		assert.False(t, entry.NeedsRenewal(), "freshly issued token should not need renewal")
	})

	t.Run("ServiceAccount created with correct labels", func(t *testing.T) {
		var saList corev1.ServiceAccountList
		err := k8sClient.List(ctx, &saList,
			client.InNamespace(testNamespace),
			client.MatchingLabels(cacheClientSALabels(engine.Name)),
		)
		require.NoError(t, err)
		require.Len(t, saList.Items, 1, "exactly one SA should exist for the engine")

		sa := saList.Items[0]
		assert.True(t, len(sa.Name) > len(rcache.CacheEngineSAPrefix),
			"SA name should be server-generated via GenerateName")
		assert.Equal(t, "cache-client", sa.Labels["app.kubernetes.io/component"])
		assert.Equal(t, engine.Name, sa.Labels["app.kubernetes.io/instance"])

		require.Len(t, sa.OwnerReferences, 1, "SA should have an owner reference")
		assert.Equal(t, engine.Name, sa.OwnerReferences[0].Name)
		assert.Equal(t, "Engine", sa.OwnerReferences[0].Kind)
	})

	t.Run("token in WasmPlugin matches tokenStore", func(t *testing.T) {
		val, _ := reconciler.tokenStore.Load(tokenKey)
		storedToken := val.(*TokenEntry).Token

		wasmPlugin := &unstructured.Unstructured{}
		wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "extensions.istio.io",
			Version: "v1alpha1",
			Kind:    "WasmPlugin",
		})
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      wasmPluginName(engine.Name),
			Namespace: engine.Namespace,
		}, wasmPlugin)
		require.NoError(t, err)

		spec, found, err := getNestedMap(wasmPlugin.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)
		pluginConfig, found, err := getNestedMap(spec, "pluginConfig")
		require.NoError(t, err)
		require.True(t, found)
		cacheToken, found, err := getNestedString(pluginConfig, "cache_token")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, storedToken, cacheToken, "WasmPlugin cache_token should match tokenStore entry")
	})

	t.Run("second reconcile reuses cached token", func(t *testing.T) {
		val, _ := reconciler.tokenStore.Load(tokenKey)
		firstToken := val.(*TokenEntry).Token

		result, err := reconciler.Reconcile(ctx, req)
		require.NoError(t, err)
		assert.NotZero(t, result.RequeueAfter)

		val, _ = reconciler.tokenStore.Load(tokenKey)
		secondToken := val.(*TokenEntry).Token
		assert.Equal(t, firstToken, secondToken, "token should be reused from cache on subsequent reconcile")
	})

	t.Run("expired token is renewed on reconcile", func(t *testing.T) {
		val, _ := reconciler.tokenStore.Load(tokenKey)
		oldToken := val.(*TokenEntry).Token

		// Simulate an expired token by overwriting the entry.
		reconciler.tokenStore.Store(tokenKey, &TokenEntry{
			Token:     "expired-token",
			IssuedAt:  time.Now().Add(-2 * time.Hour),
			ExpiresAt: time.Now().Add(-1 * time.Hour),
		})

		result, err := reconciler.Reconcile(ctx, req)
		require.NoError(t, err)
		assert.NotZero(t, result.RequeueAfter)

		val, found := reconciler.tokenStore.Load(tokenKey)
		require.True(t, found)
		newEntry := val.(*TokenEntry)
		assert.NotEqual(t, "expired-token", newEntry.Token, "expired token should be replaced")
		assert.NotEqual(t, oldToken, newEntry.Token, "a fresh token should be generated")
		assert.True(t, newEntry.ExpiresAt.After(time.Now()), "new token should not be expired")
	})
}

func TestEngineReconciler_NetworkPolicyCreated(t *testing.T) {
	ctx := context.Background()

	createTestGateway(t, ctx, k8sClient, "test-gw", testNamespace)

	t.Log("Creating RuleSet for NetworkPolicy test")
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "netpol-test-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	t.Log("Creating test engine with gateway workload selector")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "netpol-test-engine",
		Namespace:   testNamespace,
		RuleSetName: ruleset.Name,
		GatewayName: "test-gw",
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	t.Log("Reconciling engine with operator namespace set")
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	engineReq := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer and requeues after a short delay
	// (metadata-only changes don't bump generation, so the predicate would
	// filter out the update event).
	result, err := reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "first reconcile should requeue after adding finalizer")

	t.Log("Verifying finalizer was added to Engine")
	var updatedEngine wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: engine.Name, Namespace: engine.Namespace}, &updatedEngine))
	assert.Contains(t, updatedEngine.Finalizers, "waf.k8s.coraza.io/network-policy-cleanup",
		"Engine should have the NetworkPolicy cleanup finalizer")

	// Second reconcile proceeds with provisioning and schedules token renewal.
	result, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "should schedule token renewal requeue")

	t.Log("Verifying NetworkPolicy was created")
	var npList networkingv1.NetworkPolicyList
	err = k8sClient.List(ctx, &npList,
		client.InNamespace(testNamespace),
		engineNetworkPolicyLabels(engine.Namespace, engine.Name),
	)
	require.NoError(t, err)
	require.Len(t, npList.Items, 1, "exactly one NetworkPolicy should exist for the Engine")
	np := npList.Items[0]

	t.Log("Verifying NetworkPolicy uses GenerateName")
	assert.True(t, len(np.Name) > len(NetworkPolicyGenerateName), "name should be server-generated")

	t.Log("Verifying NetworkPolicy labels")
	assert.Equal(t, engine.Name, np.Labels["waf.k8s.coraza.io/engine-name"])
	assert.Equal(t, engine.Namespace, np.Labels["waf.k8s.coraza.io/engine-namespace"])
	assert.Equal(t, "coraza-kubernetes-operator", np.Labels["app.kubernetes.io/managed-by"])

	t.Log("Verifying NetworkPolicy targets operator pods")
	assert.Equal(t, operatorPodLabelValue, np.Spec.PodSelector.MatchLabels[operatorPodLabelKey])

	t.Log("Verifying NetworkPolicy allows ingress only from gateway pods")
	require.Len(t, np.Spec.PolicyTypes, 1)
	assert.Equal(t, networkingv1.PolicyTypeIngress, np.Spec.PolicyTypes[0])
	require.Len(t, np.Spec.Ingress, 1)
	require.Len(t, np.Spec.Ingress[0].From, 1)

	peer := np.Spec.Ingress[0].From[0]
	require.NotNil(t, peer.NamespaceSelector)
	assert.Equal(t, engine.Namespace, peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	require.NotNil(t, peer.PodSelector)
	assert.Equal(t, "test-gw", peer.PodSelector.MatchLabels["gateway.networking.k8s.io/gateway-name"])

	t.Log("Verifying NetworkPolicy allows only cache server port")
	require.Len(t, np.Spec.Ingress[0].Ports, 1)
	assert.Equal(t, int32(DefaultRuleSetCacheServerPort), np.Spec.Ingress[0].Ports[0].Port.IntVal)

	t.Log("Verifying finalizer blocks deletion and cleans up NetworkPolicy")
	require.NoError(t, k8sClient.Delete(ctx, engine))

	// The Engine should still exist (blocked by finalizer) but have a DeletionTimestamp.
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: engine.Name, Namespace: engine.Namespace}, &updatedEngine))
	assert.False(t, updatedEngine.DeletionTimestamp.IsZero(), "Engine should have a deletion timestamp")

	// Reconcile processes the finalizer: cleans up NetworkPolicy and removes the finalizer.
	_, err = reconciler.Reconcile(ctx, engineReq)
	require.NoError(t, err)

	err = k8sClient.List(ctx, &npList,
		client.InNamespace(testNamespace),
		engineNetworkPolicyLabels(engine.Namespace, engine.Name),
	)
	require.NoError(t, err)
	assert.Empty(t, npList.Items, "NetworkPolicy should be deleted after finalizer runs")
}

// -----------------------------------------------------------------------------
// Target Status Tests
// -----------------------------------------------------------------------------

func TestEngineReconciler_TargetNotFound(t *testing.T) {
	ctx := context.Background()

	t.Log("Creating Engine targeting a non-existent Gateway")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "target-notfound-engine",
		Namespace:   testNamespace,
		GatewayName: "gateway-does-not-exist",
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	recorder := utils.NewFakeRecorder()
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  recorder,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer and requeues.
	result, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile detects the missing Gateway.
	result, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "should not requeue when target is not found")

	t.Log("Verifying Engine has Accepted=False with reason TargetNotFound")
	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, req.NamespacedName, &updated))
	require.NotNil(t, updated.Status)

	acceptedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedCond, "Engine should have Accepted condition")
	assert.Equal(t, metav1.ConditionFalse, acceptedCond.Status)
	assert.Equal(t, "TargetNotFound", acceptedCond.Reason)
	assert.Contains(t, acceptedCond.Message, "gateway-does-not-exist")

	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	require.NotNil(t, readyCond, "Engine should have Ready condition")
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)

	t.Log("Verifying no WasmPlugin was created")
	wasmPlugin := &unstructured.Unstructured{}
	wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "extensions.istio.io",
		Version: "v1alpha1",
		Kind:    "WasmPlugin",
	})
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      wasmPluginName(engine.Name),
		Namespace: engine.Namespace,
	}, wasmPlugin)
	assert.True(t, apierrors.IsNotFound(err), "WasmPlugin should not exist when target is not found")

	assert.True(t, recorder.HasEvent("Warning", "TargetNotFound"),
		"expected Warning/TargetNotFound event; got: %v", recorder.Events)
}

func TestEngineReconciler_TargetNotFound_Resolves(t *testing.T) {
	ctx := context.Background()

	t.Log("Creating Engine targeting a Gateway that does not yet exist")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "target-resolves-engine",
		Namespace:   testNamespace,
		GatewayName: "gw-resolves-later",
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil {
			t.Logf("Failed to delete engine: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile adds the finalizer.
	result, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile hits TargetNotFound.
	result, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, req.NamespacedName, &updated))
	require.NotNil(t, updated.Status)
	acceptedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedCond, "Engine should have Accepted=False before Gateway exists")
	assert.Equal(t, metav1.ConditionFalse, acceptedCond.Status)
	assert.Equal(t, "TargetNotFound", acceptedCond.Reason)

	t.Log("Creating the missing Gateway")
	createTestGateway(t, ctx, k8sClient, "gw-resolves-later", testNamespace)

	// Third reconcile: Gateway now exists, Engine should progress past target check.
	// It will hit the RuleSet check next (test-ruleset does not exist), resulting
	// in a Degraded condition with reason RuleSetNotFound. This proves the Engine
	// is no longer blocked by TargetNotFound.
	result, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, req.NamespacedName, &updated))
	require.NotNil(t, updated.Status)

	acceptedCond = apimeta.FindStatusCondition(updated.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedCond, "Engine should have Accepted condition after target resolves")
	assert.Equal(t, metav1.ConditionTrue, acceptedCond.Status,
		"Accepted should be True after the target is found")

	degradedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Degraded")
	require.NotNil(t, degradedCond, "Engine should have Degraded condition after passing target check")
	assert.Equal(t, metav1.ConditionTrue, degradedCond.Status)
	assert.Equal(t, "RuleSetNotFound", degradedCond.Reason,
		"Engine should be degraded due to missing RuleSet, not blocked by TargetNotFound")
}

func TestEngineReconciler_TargetConflict(t *testing.T) {
	ctx := context.Background()

	t.Log("Creating Gateway for conflict test")
	createTestGateway(t, ctx, k8sClient, "conflict-gw", testNamespace)

	t.Log("Creating RuleSet for conflict test")
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "conflict-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	t.Log("Creating Engine A (the older engine that wins)")
	engineA := utils.NewTestEngine(utils.EngineOptions{
		Name:        "conflict-engine-a",
		Namespace:   testNamespace,
		GatewayName: "conflict-gw",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineA))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engineA); err != nil {
			t.Logf("Failed to delete engine A: %v", err)
		}
	})

	recorderA := utils.NewFakeRecorder()
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  recorderA,
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	reqA := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engineA.Name,
			Namespace: engineA.Namespace,
		},
	}

	// Reconcile Engine A: finalizer + provisioning.
	result, err := reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	result, err = reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)

	t.Log("Verifying Engine A is Accepted and Ready")
	var updatedA wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, reqA.NamespacedName, &updatedA))
	require.NotNil(t, updatedA.Status)
	acceptedA := apimeta.FindStatusCondition(updatedA.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedA, "Engine A should have Accepted condition")
	assert.Equal(t, metav1.ConditionTrue, acceptedA.Status)

	t.Log("Creating Engine B (the newer engine that loses)")
	engineB := utils.NewTestEngine(utils.EngineOptions{
		Name:        "conflict-engine-b",
		Namespace:   testNamespace,
		GatewayName: "conflict-gw",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineB))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engineB); err != nil {
			t.Logf("Failed to delete engine B: %v", err)
		}
	})

	recorderB := utils.NewFakeRecorder()
	reconcilerB := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  recorderB,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	reqB := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engineB.Name,
			Namespace: engineB.Namespace,
		},
	}

	// Reconcile Engine B: finalizer + conflict detection.
	result, err = reconcilerB.Reconcile(ctx, reqB)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	result, err = reconcilerB.Reconcile(ctx, reqB)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "conflicted Engine should not requeue")

	t.Log("Verifying Engine B has Accepted=False with reason TargetConflict")
	var updatedB wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, reqB.NamespacedName, &updatedB))
	require.NotNil(t, updatedB.Status)

	acceptedB := apimeta.FindStatusCondition(updatedB.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedB, "Engine B should have Accepted condition")
	assert.Equal(t, metav1.ConditionFalse, acceptedB.Status)
	assert.Equal(t, "TargetConflict", acceptedB.Reason)
	assert.Contains(t, acceptedB.Message, engineA.Name,
		"conflict message should mention the winning Engine's name")

	readyB := apimeta.FindStatusCondition(updatedB.Status.Conditions, "Ready")
	require.NotNil(t, readyB)
	assert.Equal(t, metav1.ConditionFalse, readyB.Status)

	assert.True(t, recorderB.HasEvent("Warning", "TargetConflict"),
		"expected Warning/TargetConflict event; got: %v", recorderB.Events)
}

func TestEngineReconciler_TargetConflict_NameTiebreak(t *testing.T) {
	ctx := context.Background()

	t.Log("Creating Gateway and RuleSet for name-tiebreak conflict test")
	createTestGateway(t, ctx, k8sClient, "tiebreak-gw", testNamespace)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "tiebreak-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	// Create "zzz" first, then "aaa" immediately after. When timestamps are
	// equal (second-granularity in envtest), lexicographic name breaks the tie:
	// "aaa" < "zzz", so "aaa" wins even though it was created second.
	engineZ := utils.NewTestEngine(utils.EngineOptions{
		Name:        "tiebreak-zzz",
		Namespace:   testNamespace,
		GatewayName: "tiebreak-gw",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineZ))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engineZ); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete engine Z: %v", err)
		}
	})

	engineA := utils.NewTestEngine(utils.EngineOptions{
		Name:        "tiebreak-aaa",
		Namespace:   testNamespace,
		GatewayName: "tiebreak-gw",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineA))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engineA); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete engine A: %v", err)
		}
	})

	// Envtest timestamps have second granularity, so both engines created in
	// rapid succession will naturally share the same creationTimestamp. Verify
	// this assumption so the name-based tiebreak path is actually exercised.
	// (creationTimestamp is read-only in the Kubernetes API; mutating it via
	// Update is silently ignored by the server.)
	var fetchedZ, fetchedA wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: engineZ.Name, Namespace: testNamespace}, &fetchedZ))
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: engineA.Name, Namespace: testNamespace}, &fetchedA))
	assert.Equal(t, fetchedZ.CreationTimestamp, fetchedA.CreationTimestamp,
		"Both engines should have the same creationTimestamp to exercise the name tiebreak")

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}

	// Reconcile "zzz" — should lose because "aaa" wins the tiebreak.
	reqZ := ctrl.Request{NamespacedName: types.NamespacedName{Name: engineZ.Name, Namespace: testNamespace}}
	result, err := reconciler.Reconcile(ctx, reqZ)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
	_, err = reconciler.Reconcile(ctx, reqZ)
	require.NoError(t, err)

	var updatedZ wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, reqZ.NamespacedName, &updatedZ))
	require.NotNil(t, updatedZ.Status)
	acceptedZ := apimeta.FindStatusCondition(updatedZ.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedZ, "Engine zzz should have Accepted condition")
	assert.Equal(t, metav1.ConditionFalse, acceptedZ.Status,
		"Engine zzz should lose the tiebreak to aaa")
	assert.Equal(t, "TargetConflict", acceptedZ.Reason)
	assert.Contains(t, acceptedZ.Message, "tiebreak-aaa",
		"conflict message should mention the winner (aaa)")

	// Reconcile "aaa" — should win.
	reqA := ctrl.Request{NamespacedName: types.NamespacedName{Name: engineA.Name, Namespace: testNamespace}}
	result, err = reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
	_, err = reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)

	var updatedA wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, reqA.NamespacedName, &updatedA))
	require.NotNil(t, updatedA.Status)
	acceptedA := apimeta.FindStatusCondition(updatedA.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedA, "Engine aaa should have Accepted condition")
	assert.Equal(t, metav1.ConditionTrue, acceptedA.Status,
		"Engine aaa should win the tiebreak by lexicographic name")
}

func TestEngineReconciler_TargetConflict_Resolves(t *testing.T) {
	ctx := context.Background()

	t.Log("Creating Gateway for conflict resolution test")
	createTestGateway(t, ctx, k8sClient, "conflict-resolve-gw", testNamespace)

	t.Log("Creating RuleSet for conflict resolution test")
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "conflict-resolve-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	t.Log("Creating Engine A (the older winner)")
	engineA := utils.NewTestEngine(utils.EngineOptions{
		Name:        "resolve-engine-a",
		Namespace:   testNamespace,
		GatewayName: "conflict-resolve-gw",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineA))

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	reqA := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engineA.Name,
			Namespace: engineA.Namespace,
		},
	}

	// Reconcile Engine A: finalizer + provision.
	result, err := reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
	_, err = reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)

	t.Log("Creating Engine B (the newer loser)")
	engineB := utils.NewTestEngine(utils.EngineOptions{
		Name:        "resolve-engine-b",
		Namespace:   testNamespace,
		GatewayName: "conflict-resolve-gw",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineB))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engineB); err != nil {
			t.Logf("Failed to delete engine B: %v", err)
		}
	})

	reconcilerB := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	reqB := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engineB.Name,
			Namespace: engineB.Namespace,
		},
	}

	// Reconcile Engine B: finalizer + conflict.
	result, err = reconcilerB.Reconcile(ctx, reqB)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
	result, err = reconcilerB.Reconcile(ctx, reqB)
	require.NoError(t, err)

	var updatedB wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, reqB.NamespacedName, &updatedB))
	require.NotNil(t, updatedB.Status)
	acceptedB := apimeta.FindStatusCondition(updatedB.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedB)
	assert.Equal(t, metav1.ConditionFalse, acceptedB.Status)
	assert.Equal(t, "TargetConflict", acceptedB.Reason)

	t.Log("Deleting Engine A to resolve the conflict")
	require.NoError(t, k8sClient.Delete(ctx, engineA))

	// The finalizer on Engine A will block deletion until reconciled.
	// Reconcile Engine A to run its finalizer cleanup.
	_, err = reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)

	t.Log("Reconciling Engine B after Engine A is deleted")
	result, err = reconcilerB.Reconcile(ctx, reqB)
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, reqB.NamespacedName, &updatedB))
	require.NotNil(t, updatedB.Status)

	acceptedB = apimeta.FindStatusCondition(updatedB.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedB, "Engine B should have Accepted condition after conflict resolution")
	assert.Equal(t, metav1.ConditionTrue, acceptedB.Status,
		"Engine B should be Accepted after competing Engine A is deleted")
	assert.Equal(t, "Accepted", acceptedB.Reason)
}

func TestEngineReconciler_TargetConflict_ResolvesOnRetarget(t *testing.T) {
	ctx := context.Background()

	t.Log("Creating Gateways and RuleSet for retarget conflict test")
	createTestGateway(t, ctx, k8sClient, "retarget-gw-x", testNamespace)
	createTestGateway(t, ctx, k8sClient, "retarget-gw-y", testNamespace)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "retarget-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleset); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete ruleset: %v", err)
		}
	})

	t.Log("Creating Engine A targeting Gateway X (older, wins)")
	engineA := utils.NewTestEngine(utils.EngineOptions{
		Name:        "retarget-engine-a",
		Namespace:   testNamespace,
		GatewayName: "retarget-gw-x",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineA))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engineA); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete engine A: %v", err)
		}
	})

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	reqA := ctrl.Request{NamespacedName: types.NamespacedName{Name: engineA.Name, Namespace: testNamespace}}

	// Reconcile Engine A: finalizer + provisioning.
	result, err := reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
	_, err = reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)

	t.Log("Creating Engine B targeting Gateway X (newer, loses)")
	engineB := utils.NewTestEngine(utils.EngineOptions{
		Name:        "retarget-engine-b",
		Namespace:   testNamespace,
		GatewayName: "retarget-gw-x",
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engineB))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engineB); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("Failed to delete engine B: %v", err)
		}
	})

	reqB := ctrl.Request{NamespacedName: types.NamespacedName{Name: engineB.Name, Namespace: testNamespace}}

	// Reconcile Engine B: finalizer + conflict.
	result, err = reconciler.Reconcile(ctx, reqB)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)
	_, err = reconciler.Reconcile(ctx, reqB)
	require.NoError(t, err)

	var updatedB wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, reqB.NamespacedName, &updatedB))
	require.NotNil(t, updatedB.Status)
	acceptedB := apimeta.FindStatusCondition(updatedB.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedB)
	assert.Equal(t, metav1.ConditionFalse, acceptedB.Status, "precondition: Engine B should be rejected")
	assert.Equal(t, "TargetConflict", acceptedB.Reason)

	t.Log("Retargeting Engine A from Gateway X to Gateway Y")
	var latestA wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, reqA.NamespacedName, &latestA))
	latestA.Spec.Target.Name = "retarget-gw-y"
	require.NoError(t, k8sClient.Update(ctx, &latestA))

	// Reconcile Engine A on the new target.
	_, err = reconciler.Reconcile(ctx, reqA)
	require.NoError(t, err)

	// In a real controller, the competingEngineHandler would enqueue Engine B
	// after Engine A's spec update (generation changed). Simulate that requeue.
	_, err = reconciler.Reconcile(ctx, reqB)
	require.NoError(t, err)

	t.Log("Verifying Engine B is now Accepted on Gateway X")
	require.NoError(t, k8sClient.Get(ctx, reqB.NamespacedName, &updatedB))
	require.NotNil(t, updatedB.Status)
	acceptedB = apimeta.FindStatusCondition(updatedB.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedB, "Engine B should have Accepted condition after retarget")
	assert.Equal(t, metav1.ConditionTrue, acceptedB.Status,
		"Engine B should be Accepted after Engine A retargets away from Gateway X")
	assert.Equal(t, "Accepted", acceptedB.Reason)
}

func TestEngineReconciler_ReadyToTargetNotFound(t *testing.T) {
	ctx := context.Background()

	t.Log("Creating Gateway and RuleSet for ready-to-notfound test")
	gwName := "ready-notfound-gw"
	gw := createTestGateway(t, ctx, k8sClient, gwName, testNamespace)

	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "ready-notfound-ruleset",
		Namespace: testNamespace,
	})
	require.NoError(t, k8sClient.Create(ctx, ruleset))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ruleset)
	})

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "ready-notfound-engine",
		Namespace:   testNamespace,
		GatewayName: gwName,
		RuleSetName: ruleset.Name,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, engine)
	})

	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		kubeClient:                testKubeClient,
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
		operatorNamespace:         testNamespace,
	}
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	}

	// First reconcile: finalizer.
	result, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter)

	// Second reconcile: full provisioning → Ready.
	result, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, req.NamespacedName, &updated))
	require.NotNil(t, updated.Status)
	readyCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	require.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status, "precondition: Engine should be Ready")
	acceptedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedCond)
	assert.Equal(t, metav1.ConditionTrue, acceptedCond.Status, "precondition: Engine should be Accepted")

	t.Log("Deleting the Gateway to trigger TargetNotFound")
	require.NoError(t, k8sClient.Delete(ctx, gw))

	// Reconcile after Gateway deletion.
	recorder := utils.NewFakeRecorder()
	reconciler.Recorder = recorder
	result, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter, "should not requeue when target is not found")

	t.Log("Verifying Engine transitions to Accepted=False, Ready=False")
	require.NoError(t, k8sClient.Get(ctx, req.NamespacedName, &updated))
	require.NotNil(t, updated.Status)

	acceptedCond = apimeta.FindStatusCondition(updated.Status.Conditions, "Accepted")
	require.NotNil(t, acceptedCond, "Engine should have Accepted condition")
	assert.Equal(t, metav1.ConditionFalse, acceptedCond.Status)
	assert.Equal(t, "TargetNotFound", acceptedCond.Reason)

	readyCond = apimeta.FindStatusCondition(updated.Status.Conditions, "Ready")
	require.NotNil(t, readyCond, "Engine should have Ready condition")
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)

	degradedCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Degraded")
	assert.Nil(t, degradedCond, "Degraded should be cleared by applyStatusNotAccepted")

	progressingCond := apimeta.FindStatusCondition(updated.Status.Conditions, "Progressing")
	assert.Nil(t, progressingCond, "Progressing should be cleared by applyStatusNotAccepted")

	assert.True(t, recorder.HasEvent("Warning", "TargetNotFound"),
		"expected Warning/TargetNotFound event; got: %v", recorder.Events)

	t.Log("Verifying WasmPlugin was deleted")
	wasmPlugin := &unstructured.Unstructured{}
	wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "extensions.istio.io",
		Version: "v1alpha1",
		Kind:    "WasmPlugin",
	})
	wasmPluginKey := types.NamespacedName{
		Name:      wasmPluginName(engine.Name),
		Namespace: engine.Namespace,
	}
	err = k8sClient.Get(ctx, wasmPluginKey, wasmPlugin)
	assert.True(t, apierrors.IsNotFound(err), "WasmPlugin should be deleted when Engine is not accepted, got: %v", err)

	t.Log("Verifying token store entry was removed")
	tokenKey := fmt.Sprintf("%s/%s/%s", engine.Namespace, engine.Name, engine.Spec.RuleSet.Name)
	_, found := reconciler.tokenStore.Load(tokenKey)
	assert.False(t, found, "token store entry should be removed when Engine is not accepted")
}
