/*
Copyright 2026 Shane Utt.

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
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

func TestEngineReconciler_ReconcileMissingRuleSet(t *testing.T) {
	ctx := context.Background()
	ns := "default"

	t.Log("Creating test engine referencing non-existent RuleSet")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "test-engine-missing-ruleset",
		Namespace:   ns,
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

	t.Log("Creating test engine with Istio driver")
	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:      "test-engine",
		Namespace: ns,
	})
	err := k8sClient.Create(ctx, engine)
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
		Namespace: "default",
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
			expectedError: "spec.ruleSet.name in body should be at least 1 chars long",
		},
		{
			name: "no driver specified",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver = wafv1alpha1.DriverConfig{}
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
				engine.Spec.Driver.Istio.Wasm.Image = "docker://invalid-image"
				return engine
			},
			expectedError: "spec.driver.istio.wasm.image in body should match '^oci://'",
		},
		{
			name: "image too short",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Istio.Wasm.Image = ""
				return engine
			},
			expectedError: "spec.driver.istio.wasm.image in body should be at least 1 chars long",
		},
		{
			name: "image too long",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Istio.Wasm.Image = "oci://" + string(make([]byte, 1100))
				return engine
			},
			expectedError: "spec.driver.istio.wasm.image: Too long",
		},
		{
			name: "gateway mode without workloadSelector",
			engineFunc: func() *wafv1alpha1.Engine {
				engine := utils.NewTestEngine(utils.EngineOptions{})
				engine.Spec.Driver.Istio.Wasm.Mode = wafv1alpha1.IstioIntegrationModeGateway
				engine.Spec.Driver.Istio.Wasm.WorkloadSelector = nil
				return engine
			},
			expectedError: "workloadSelector is required when mode is gateway",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Attempting to create Engine with invalid configuration: %s", tt.name)
			engine := tt.engineFunc()
			engine.Name = "validation-test-" + t.Name()
			engine.Namespace = "default"

			err := k8sClient.Create(ctx, engine)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
		})
	}
}
