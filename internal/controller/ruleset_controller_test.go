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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

const (
	testNamespace = "default"
	testInstance  = "test_instance"
)

func TestRuleSetReconciler_ReconcileNotFound(t *testing.T) {
	ctx, cleanup := setupTest(t)
	defer cleanup()

	t.Log("Reconciling non-existent RuleSet")
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewTestRecorder(),
		Cache:    cache.NewRuleSetCache(),
	}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "non-existent",
			Namespace: testNamespace,
		},
	})

	require.NoError(t, err)
	assert.False(t, result.Requeue)
}

func TestRuleSetReconciler_ReconcileConfigMaps(t *testing.T) {
	tests := []struct {
		name          string
		ruleSetName   string
		configMaps    map[string]string
		expectedRules string
	}{
		{
			name:        "single ConfigMap",
			ruleSetName: "single-cm-ruleset",
			configMaps: map[string]string{
				"test-rules": "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"",
			},
			expectedRules: "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"",
		},
		{
			name:        "multiple ConfigMaps",
			ruleSetName: "multi-cm-ruleset",
			configMaps: map[string]string{
				"rules-1": "rule 1",
				"rules-2": "rule 2",
				"rules-3": "rule 3",
			},
			expectedRules: "rule 1\nrule 2\nrule 3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ruleSetCache := cache.NewRuleSetCache()

			t.Logf("Creating %d ConfigMap(s)", len(tt.configMaps))
			var refs []wafv1alpha1.RuleSourceReference
			var names []string
			for name := range tt.configMaps {
				names = append(names, name)
			}
			sort.Strings(names) // need a consistent order for testing

			t.Logf("Creating ConfigMaps: %v", names)
			for _, name := range names {
				data := tt.configMaps[name]
				cm := utils.NewTestConfigMap(name, testNamespace, data)
				require.NoError(t, k8sClient.Create(ctx, cm))

				t.Cleanup(func() {
					if err := k8sClient.Delete(ctx, cm); err != nil {
						t.Logf("Failed to delete ConfigMap %s: %v", name, err)
					}
				})

				refs = append(refs, wafv1alpha1.RuleSourceReference{Name: name})
			}

			t.Log("Creating RuleSet referencing ConfigMaps")
			ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
				Name:      tt.ruleSetName,
				Namespace: testNamespace,
				Rules:     refs,
			})

			t.Log("Creating RuleSet in Kubernetes")
			require.NoError(t, k8sClient.Create(ctx, ruleSet))
			t.Cleanup(func() {
				if err := k8sClient.Delete(ctx, ruleSet); err != nil {
					t.Logf("Failed to delete RuleSet: %v", err)
				}
			})

			t.Logf("Reconciling RuleSet %s", tt.ruleSetName)
			recorder := utils.NewFakeRecorder()
			reconciler := &RuleSetReconciler{
				Client:   k8sClient,
				Scheme:   scheme,
				Recorder: recorder,
				Cache:    ruleSetCache,
			}
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      ruleSet.Name,
					Namespace: ruleSet.Namespace,
				},
			})

			t.Log("Verifying cache was populated with combined rules")
			require.NoError(t, err)
			assert.False(t, result.Requeue)
			cacheKey := testNamespace + "/" + tt.ruleSetName
			entry, ok := ruleSetCache.Get(cacheKey)
			require.True(t, ok, "Cache entry should exist")
			assert.Equal(t, tt.expectedRules, entry.Rules)
			assert.NotEmpty(t, entry.UUID)

			assert.True(t, recorder.HasEvent("Normal", "RulesCached"),
				"expected Normal/RulesCached event; got: %v", recorder.Events)
		})
	}
}

func TestRuleSetReconciler_MissingConfigMap(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := cache.NewRuleSetCache()

	t.Log("Creating RuleSet referencing non-existent ConfigMap")
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "missing-cm-ruleset",
		Namespace: testNamespace,
		Rules: []wafv1alpha1.RuleSourceReference{
			{Name: "non-existent"},
		},
	})
	err := k8sClient.Create(ctx, ruleSet)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	}()

	t.Log("Reconciling RuleSet - should requeue due to missing ConfigMap")
	recorder := utils.NewFakeRecorder()
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: recorder,
		Cache:    ruleSetCache,
	}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		},
	})

	t.Log("Verifying cache was not populated due to missing ConfigMap")
	require.NoError(t, err)
	assert.True(t, result.Requeue, "Should requeue when ConfigMap is not found")
	cacheKey := testNamespace + "/missing-cm-ruleset"
	_, ok := ruleSetCache.Get(cacheKey)
	assert.False(t, ok)

	assert.True(t, recorder.HasEvent("Warning", "ConfigMapNotFound"),
		"expected Warning/ConfigMapNotFound event; got: %v", recorder.Events)
}

func TestRuleSetReconciler_ConfigMapMissingRulesKey(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := cache.NewRuleSetCache()

	t.Log("Creating ConfigMap without 'rules' key")
	cm := &corev1.ConfigMap{}
	cm.Name = "invalid-cm"
	cm.Namespace = testNamespace
	cm.Data = map[string]string{"wrong-key": "some data"}
	err := k8sClient.Create(ctx, cm)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, cm); err != nil {
			t.Logf("Failed to delete configmap: %v", err)
		}
	}()

	t.Log("Creating RuleSet referencing invalid ConfigMap")
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "invalid-ruleset",
		Namespace: testNamespace,
		Rules: []wafv1alpha1.RuleSourceReference{
			{Name: "invalid-cm"},
		},
	})
	err = k8sClient.Create(ctx, ruleSet)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	}()

	t.Log("Reconciling RuleSet")
	recorder := utils.NewFakeRecorder()
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: recorder,
		Cache:    ruleSetCache,
	}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		},
	})

	t.Log("Verifying error due to missing 'rules' key in ConfigMap")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing 'rules' key")
	assert.False(t, result.Requeue)

	assert.True(t, recorder.HasEvent("Warning", "InvalidConfigMap"),
		"expected Warning/InvalidConfigMap event; got: %v", recorder.Events)
}

func TestRuleSetReconciler_ValidationRejection(t *testing.T) {
	tests := []struct {
		name          string
		ruleSetName   string
		rules         []wafv1alpha1.RuleSourceReference
		expectedError string
	}{
		{
			name:          "no rules specified",
			ruleSetName:   "no-rules-ruleset",
			rules:         []wafv1alpha1.RuleSourceReference{},
			expectedError: "spec.rules in body should have at least 1 items",
		},
		{
			name:        "too many rules",
			ruleSetName: "too-many-rules-ruleset",
			rules: func() []wafv1alpha1.RuleSourceReference {
				rules := make([]wafv1alpha1.RuleSourceReference, 2049)
				for i := range rules {
					rules[i] = wafv1alpha1.RuleSourceReference{Name: "test"}
				}
				return rules
			}(),
			expectedError: "spec.rules: Too many",
		},
		{
			name:        "empty rule name",
			ruleSetName: "empty-name-ruleset",
			rules: []wafv1alpha1.RuleSourceReference{
				{Name: ""},
			},
			expectedError: "spec.rules[0].name in body should be at least 1 chars long",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			t.Logf("Attempting to create RuleSet with invalid configuration: %s", tt.name)
			ruleSet := &wafv1alpha1.RuleSet{}
			ruleSet.Name = tt.ruleSetName
			ruleSet.Namespace = testNamespace
			ruleSet.Spec.Rules = tt.rules
			err := k8sClient.Create(ctx, ruleSet)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
		})
	}
}

func TestRuleSetReconciler_UpdateCache(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := cache.NewRuleSetCache()

	t.Log("Creating ConfigMap with initial rules")
	cm := utils.NewTestConfigMap("update-rules", "default", "initial rules")
	err := k8sClient.Create(ctx, cm)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, cm); err != nil {
			t.Logf("Failed to delete configmap: %v", err)
		}
	}()

	t.Log("Creating RuleSet referencing ConfigMap")
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "update-ruleset",
		Namespace: testNamespace,
		Rules: []wafv1alpha1.RuleSourceReference{
			{Name: "update-rules"},
		},
	})
	err = k8sClient.Create(ctx, ruleSet)
	require.NoError(t, err)
	defer func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	}()

	t.Log("Performing initial reconciliation to populate cache")
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewTestRecorder(),
		Cache:    ruleSetCache,
	}
	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		},
	})
	require.NoError(t, err)

	t.Log("Updating ConfigMap with new rules")
	cacheKey := testNamespace + "/update-ruleset"
	entry1, _ := ruleSetCache.Get(cacheKey)
	uuid1 := entry1.UUID
	var updatedCM corev1.ConfigMap
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "update-rules", Namespace: testNamespace}, &updatedCM)
	require.NoError(t, err)
	updatedCM.Data["rules"] = "updated rules"
	err = k8sClient.Update(ctx, &updatedCM)
	require.NoError(t, err)

	t.Log("Reconciling after ConfigMap update to refresh cache")
	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		},
	})
	require.NoError(t, err)

	t.Log("Verifying cache was updated with new rules and UUID changed")
	entry2, _ := ruleSetCache.Get(cacheKey)
	assert.Equal(t, "updated rules", entry2.Rules)
	assert.NotEqual(t, uuid1, entry2.UUID, "UUID should change when rules are updated")
}
