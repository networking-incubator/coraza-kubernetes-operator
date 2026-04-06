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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/defaults"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

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
	}

	t.Log("Reconciling non-existent engine - should not error")
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "non-existent",
			Namespace: "default",
		},
	})

	require.NoError(t, err)
	assert.False(t, result.Requeue)
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
	w := withRev.buildWasmPlugin(engine, testWasmOCI)
	assert.Equal(t, "canary", w.GetLabels()["istio.io/rev"])

	noRev := &EngineReconciler{
		ruleSetCacheServerCluster: "test-cluster",
	}
	w2 := noRev.buildWasmPlugin(engine, testWasmOCI)
	_, has := w2.GetLabels()["istio.io/rev"]
	assert.False(t, has, "istio.io/rev should not be set when revision is empty")
}

func TestEngineReconciler_ReconcileMissingRuleSet(t *testing.T) {
	ctx := context.Background()

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

	t.Log("Reconciling Engine with missing RuleSet - should requeue")
	reconciler := &EngineReconciler{
		Client:                    k8sClient,
		Scheme:                    scheme,
		Recorder:                  utils.NewTestRecorder(),
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
	}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	})

	t.Log("Verifying reconciliation behavior")
	if err != nil {
		assert.True(t, result.Requeue, "Should requeue when RuleSet is not found")
	}
}

func TestEngineReconciler_ReconcileIstioDriver(t *testing.T) {
	ctx := context.Background()
	ns := utils.NewTestEngine(utils.EngineOptions{}).Namespace

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
		ruleSetCacheServerCluster: "test-cluster",
		defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
	}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	})
	require.NoError(t, err)
	assert.False(t, result.Requeue)

	t.Log("Verifying engine status")
	var updated wafv1alpha1.Engine
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      engine.Name,
		Namespace: engine.Namespace,
	}, &updated)
	require.NoError(t, err)
	assert.Len(t, updated.Status.Conditions, 1)
	condition := updated.Status.Conditions[0]
	assert.Equal(t, "Ready", condition.Type)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "Configured", condition.Reason)

	assert.True(t, recorder.HasEvent("Normal", "WasmPluginCreated"),
		"expected Normal/WasmPluginCreated event; got: %v", recorder.Events)
}

func TestEngineReconciler_StatusUpdateHandling(t *testing.T) {
	ctx := context.Background()

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
	}
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	})
	require.NoError(t, err)

	t.Log("Verifying status conditions were set")
	var updated wafv1alpha1.Engine
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      engine.Name,
		Namespace: engine.Namespace,
	}, &updated)
	require.NoError(t, err)
	if len(updated.Status.Conditions) > 0 {
		condition := updated.Status.Conditions[0]
		assert.NotEmpty(t, condition.Type)
		assert.NotEmpty(t, condition.Status)
		assert.NotEmpty(t, condition.Reason)
	}
}

func TestEngineReconciler_FailurePolicyInWasmPluginConfig(t *testing.T) {
	ctx := context.Background()

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
				ruleSetCacheServerCluster: "test-cluster",
				defaultWasmImage:          defaults.DefaultCorazaWasmOCIReference,
			}
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      engine.Name,
					Namespace: engine.Namespace,
				},
			})
			require.NoError(t, err)
			assert.False(t, result.Requeue)

			t.Log("Fetching created WasmPlugin")
			wasmURL, _ := reconciler.wasmPluginOCIURLSource(engine)
			wasmPlugin := reconciler.buildWasmPlugin(engine, wasmURL)
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
	}

	t.Run("imagePullSecret is set when specified", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{
			Name:            "engine-with-pull-secret",
			Namespace:       testNamespace,
			ImagePullSecret: "my-registry-secret",
		})

		wasmPlugin := reconciler.buildWasmPlugin(engine)

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

		wasmPlugin := reconciler.buildWasmPlugin(engine)

		spec, found, err := getNestedMap(wasmPlugin.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)

		_, found = spec["imagePullSecret"]
		assert.False(t, found, "imagePullSecret should not be present in WasmPlugin spec when empty")
	})
}

func TestEngineReconciler_ImagePullSecretEnvtest(t *testing.T) {
	ctx := context.Background()

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

		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      engine.Name,
				Namespace: engine.Namespace,
			},
		})
		require.NoError(t, err)
		assert.False(t, result.Requeue)

		t.Log("Fetching WasmPlugin from API server")
		wasmPlugin := &unstructured.Unstructured{}
		wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "extensions.istio.io",
			Version: "v1alpha1",
			Kind:    "WasmPlugin",
		})
		err = k8sClient.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s%s", WasmPluginNamePrefix, engine.Name),
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

		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      engine.Name,
				Namespace: engine.Namespace,
			},
		})
		require.NoError(t, err)
		assert.False(t, result.Requeue)

		t.Log("Fetching WasmPlugin from API server")
		wasmPlugin := &unstructured.Unstructured{}
		wasmPlugin.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "extensions.istio.io",
			Version: "v1alpha1",
			Kind:    "WasmPlugin",
		})
		err = k8sClient.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s%s", WasmPluginNamePrefix, engine.Name),
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
			name: "no driver specified",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver = &wafv1alpha1.DriverConfig{}
				return engine
			},
			expectedError: "exactly one driver must be specified",
		},
		{
			name: "no istio integration mode specified",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Istio = &wafv1alpha1.IstioDriverConfig{}
				return engine
			},
			expectedError: "exactly one integration mechanism (Wasm, etc) must be specified",
		},
		{
			name: "image doesn't start with oci://",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Istio.Wasm.Image = ptr.To("docker://invalid-image")
				return engine
			},
			expectedError: "image must start with oci:// when set",
		},
		{
			name: "image too long",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Istio.Wasm.Image = ptr.To("oci://" + string(make([]byte, 1100)))
				return engine
			},
			expectedError: fmt.Sprintf("image must be at most %d characters when set", wafv1alpha1.MaxImageLen),
		},
		{
			name: "gateway mode without workloadSelector",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Istio.Wasm.Mode = ptr.To(wafv1alpha1.IstioIntegrationModeGateway)
				engine.Spec.Driver.Istio.Wasm.WorkloadSelector = nil
				return engine
			},
			expectedError: "workloadSelector is required when mode is gateway",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Attempting to create Engine with invalid configuration: %s", tt.name)
			engine := tt.engineFunc()
			engine.Name = fmt.Sprintf("validation-test-%d", i)
			engine.Namespace = testNamespace

			err := k8sClient.Create(ctx, engine)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
		})
	}
}

func TestEngineReconciler_DegradedWhenRuleSetDegraded(t *testing.T) {
	ctx := context.Background()

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
	if ruleSet.Status == nil {
		ruleSet.Status = &wafv1alpha1.RuleSetStatus{}
	}
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
	}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      engine.Name,
			Namespace: engine.Namespace,
		},
	})
	require.NoError(t, err)
	assert.False(t, result.Requeue)

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
	engine.Spec.Driver.Istio.Wasm.Image = nil

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
		engine.Spec.Driver.Istio.Wasm.Image = nil
		r := &EngineReconciler{defaultWasmImage: operatorDefault}
		wasmURL, _ := r.wasmPluginOCIURLSource(engine)
		wp := r.buildWasmPlugin(engine, wasmURL)
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
		engine.Spec.Driver.Istio.Wasm = nil
		r := &EngineReconciler{defaultWasmImage: operatorDefault}
		wasmURL, _ := r.wasmPluginOCIURLSource(engine)
		assert.Equal(t, operatorDefault, wasmURL)
	})

	t.Run("explicit image wins over operator default", func(t *testing.T) {
		custom := "oci://custom.example/wasm:v2"
		engine := utils.NewTestEngine(utils.EngineOptions{})
		engine.Spec.Driver.Istio.Wasm.Image = ptr.To(custom)
		r := &EngineReconciler{defaultWasmImage: operatorDefault}
		wasmURL, _ := r.wasmPluginOCIURLSource(engine)
		wp := r.buildWasmPlugin(engine, wasmURL)
		spec, found, err := getNestedMap(wp.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)
		url, found, err := getNestedString(spec, "url")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, custom, url)
	})
}
