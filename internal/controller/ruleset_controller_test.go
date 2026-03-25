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
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/cache"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

const (
	testNamespace = "default"
)

func TestRuleSetReconciler_ReconcileNotFound(t *testing.T) {
	ctx, cleanup := setupTest(t)
	t.Cleanup(cleanup)

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
	assert.Equal(t, reconcile.Result{}, result)
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
				"rules-1": "SecCollectionTimeout 1",
				"rules-2": "SecCollectionTimeout 2",
				"rules-3": "SecCollectionTimeout 3",
			},
			expectedRules: "SecCollectionTimeout 1\nSecCollectionTimeout 2\nSecCollectionTimeout 3",
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
			assert.Equal(t, reconcile.Result{}, result)
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
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

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
	assert.Equal(t, reconcile.Result{}, result, "Should requeue when ConfigMap is not found")
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
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, cm); err != nil {
			t.Logf("Failed to delete configmap: %v", err)
		}
	})

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
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

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
	assert.Equal(t, reconcile.Result{}, result)

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
			expectedError: "spec.rules: Required value",
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
			expectedError: "spec.rules[0].name: Required value",
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
	cm := utils.NewTestConfigMap("update-rules", "default", "SecDefaultAction \"phase:1,log,auditlog,pass\"")
	err := k8sClient.Create(ctx, cm)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, cm); err != nil {
			t.Logf("Failed to delete configmap: %v", err)
		}
	})

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
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("Failed to delete RuleSet: %v", err)
		}
	})

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
	updatedCM.Data["rules"] = "SecDefaultAction \"phase:2,log,auditlog,pass\""
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
	assert.Equal(t, "SecDefaultAction \"phase:2,log,auditlog,pass\"", entry2.Rules)
	assert.NotEqual(t, uuid1, entry2.UUID, "UUID should change when rules are updated")
}

func TestRuleSetReconciler_ValidateRules(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := cache.NewRuleSetCache()
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewTestRecorder(),
		Cache:    ruleSetCache,
	}

	rules := []struct {
		name    string
		content string
	}{
		{
			name:    "update-rules",
			content: "SecDefaultAction \"phase:1,log,auditlog,pass\"",
		},
		{
			name:    "dumb-rule",
			content: "SecRule REMOTE_ADDR \".*\" \"id:12345,phase:1,pass,nolog,msg:'Test rule'\"",
		},
		{
			name:    "invalid-rule",
			content: "SecDefaultActionXPTO \"THIS IS VERY MUCH INVALID\"",
		},
		{
			name:    "referother",
			content: "SecRuleUpdateTargetById 12345 \"REMOTE_ADDR\"",
		},
		{
			name:    "withdata",
			content: "SecRule REQUEST_URI \"@pmFromFile rule1.data\" \"id:55555,phase:1,deny,status:403,msg:'File Match'\"",
		},
	}
	for _, rule := range rules {
		cm := utils.NewTestConfigMap(rule.name, "default", rule.content)
		err := k8sClient.Create(ctx, cm)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, cm); err != nil {
				t.Logf("Failed to delete configmap: %v", err)
			}
		})
	}

	ruleData := map[string][]byte{
		"rule1.data": []byte("something\nanotherthing"),
	}
	secretdata := utils.NewTestRuleData("ruledata", "default", ruleData)
	require.NoError(t, k8sClient.Create(ctx, secretdata))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, secretdata); err != nil {
			t.Logf("Failed to delete secret: %v", err)
		}
	})
	secretdataWrongType := utils.NewTestRuleData("ruledatawrongtype", "default", ruleData)
	secretdataWrongType.Type = "generic"
	require.NoError(t, k8sClient.Create(ctx, secretdataWrongType))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, secretdataWrongType); err != nil {
			t.Logf("Failed to delete secret: %v", err)
		}
	})

	t.Run("single rule should reconcile", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-simple",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "update-rules"},
			},
		})
		err := k8sClient.Create(ctx, ruleSet)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})
		t.Log("Performing initial reconciliation to populate cache")

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Name:      ruleSet.Name,
				Namespace: ruleSet.Namespace,
			},
		})
		require.NoError(t, err)
	})

	t.Run("ruleset containing invalid rule should fail", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-invalid",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "update-rules"},
				{Name: "invalid-rule"},
			},
		})
		err := k8sClient.Create(ctx, ruleSet)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})
		t.Log("Performing initial reconciliation to populate cache")
		resource := types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: resource,
		})
		assert.ErrorContains(t, err, "invalid WAF config from string: unknown directive \"secdefaultactionxpto\"")
		err = k8sClient.Get(ctx, resource, ruleSet)
		require.NoError(t, err)
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, "InvalidRuleSet", ready.Reason)
		assert.Contains(t, ready.Message, "ConfigMap invalid-rule doesn't contain valid rules: invalid WAF config from string: unknown directive \"secdefaultactionxpto\"")
		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Degraded")
		assert.Equal(t, metav1.ConditionTrue, degraded.Status)
		assert.Equal(t, "InvalidRuleSet", degraded.Reason)
		assert.Contains(t, degraded.Message, "ConfigMap invalid-rule doesn't contain valid rules: invalid WAF config from string: unknown directive \"secdefaultactionxpto\"")
	})

	t.Run("ruleset referring other rules should pass", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-references",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "update-rules"},
				{Name: "dumb-rule"},
				{Name: "referother"},
			},
		})
		err := k8sClient.Create(ctx, ruleSet)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})
		t.Log("Performing initial reconciliation")
		resource := types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: resource,
		})
		require.NoError(t, err)
		err = k8sClient.Get(ctx, resource, ruleSet)
		require.NoError(t, err)
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, "RulesCached", ready.Reason)
	})

	t.Run("ruleset using a valid ruledata should pass", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-validdata",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "withdata"},
			},
			RuleData: "ruledata",
		})
		err := k8sClient.Create(ctx, ruleSet)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})
		t.Log("Performing initial reconciliation")
		resource := types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: resource,
		})
		require.NoError(t, err)
		err = k8sClient.Get(ctx, resource, ruleSet)
		require.NoError(t, err)
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, "RulesCached", ready.Reason)
	})

	t.Run("ruleset using a ruledata with wrong type should fail", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-invaliddatatype",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "withdata"},
			},
			RuleData: "ruledatawrongtype",
		})
		err := k8sClient.Create(ctx, ruleSet)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})
		t.Log("Performing initial reconciliation")
		resource := types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: resource,
		})
		require.NoError(t, err)
		err = k8sClient.Get(ctx, resource, ruleSet)
		require.NoError(t, err)
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, "RuleDataSecretTypeMismatch", ready.Reason)
		assert.Equal(t, "Failed to use RuleData secret ruledatawrongtype: the secret type must be of type coraza/data", ready.Message)

	})

	t.Run("ruleset referring invalid ruledata secret should fail", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-invalidsecret",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "dumb-rule"},
			},
			RuleData: "notvalid",
		})
		err := k8sClient.Create(ctx, ruleSet)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})
		t.Log("Performing initial reconciliation")
		resource := types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: resource,
		})
		require.NoError(t, err)
		err = k8sClient.Get(ctx, resource, ruleSet)
		require.NoError(t, err)
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, "SecretNotFound", ready.Reason)
		assert.Equal(t, "Referenced Secret notvalid does not exist", ready.Message)
	})

	t.Run("ruleset referring @pmFromFile without a RuleData should fail", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-invaliddata",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "withdata"},
			},
		})
		err := k8sClient.Create(ctx, ruleSet)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})
		t.Log("Performing initial reconciliation")
		resource := types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}

		_, err = reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: resource,
		})
		assert.ErrorContains(t, err, "open rule1.data: data does not exist")
		err = k8sClient.Get(ctx, resource, ruleSet)
		require.NoError(t, err)
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, "InvalidRuleSet", ready.Reason)
		assert.Contains(t, ready.Message, "open rule1.data: data does not exist")
	})
}

func TestRuleSetReconciler_UnsupportedRules(t *testing.T) {
	ctx := context.Background()

	t.Run("ruleset with unsupported rule should be rejected", func(t *testing.T) {
		ruleSetCache := cache.NewRuleSetCache()

		t.Log("Creating ConfigMap with an unsupported response body inspection rule")
		cm := utils.NewTestConfigMap("unsupported-rules", testNamespace,
			`SecRule RESPONSE_BODY "@rx error" "id:950150,phase:4,deny,status:403,msg:'Data leakage'"`)
		require.NoError(t, k8sClient.Create(ctx, cm))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, cm); err != nil {
				t.Logf("Failed to delete ConfigMap: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "unsupported-ruleset",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "unsupported-rules"},
			},
		})
		require.NoError(t, k8sClient.Create(ctx, ruleSet))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})

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

		require.NoError(t, err, "should not return error (non-retriable)")
		assert.Equal(t, reconcile.Result{}, result)

		t.Log("Verifying cache was NOT populated")
		cacheKey := testNamespace + "/unsupported-ruleset"
		_, ok := ruleSetCache.Get(cacheKey)
		assert.False(t, ok, "cache should be empty for rejected ruleset")

		t.Log("Verifying status conditions")
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}, ruleSet))
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, "UnsupportedRules", ready.Reason)
		assert.Contains(t, ready.Message, "950150")
		assert.Contains(t, ready.Message, "response body inspection")

		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Degraded")
		require.NotNil(t, degraded)
		assert.Equal(t, metav1.ConditionTrue, degraded.Status)
		assert.Equal(t, "UnsupportedRules", degraded.Reason)

		t.Log("Verifying event was recorded")
		assert.True(t, recorder.HasEvent("Warning", "UnsupportedRules"),
			"expected Warning/UnsupportedRules event; got: %v", recorder.Events)
	})

	t.Run("ruleset with only supported rules should succeed", func(t *testing.T) {
		ruleSetCache := cache.NewRuleSetCache()

		t.Log("Creating ConfigMap with only supported rules")
		cm := utils.NewTestConfigMap("supported-rules", testNamespace,
			`SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny,status:403"`)
		require.NoError(t, k8sClient.Create(ctx, cm))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, cm); err != nil {
				t.Logf("Failed to delete ConfigMap: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "supported-ruleset",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "supported-rules"},
			},
		})
		require.NoError(t, k8sClient.Create(ctx, ruleSet))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})

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

		require.NoError(t, err)
		assert.Equal(t, reconcile.Result{}, result)

		t.Log("Verifying cache WAS populated")
		cacheKey := testNamespace + "/supported-ruleset"
		entry, ok := ruleSetCache.Get(cacheKey)
		assert.True(t, ok, "cache should contain entry for valid ruleset")
		assert.Contains(t, entry.Rules, "id:1")

		t.Log("Verifying Ready status")
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}, ruleSet))
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, "RulesCached", ready.Reason)

		assert.True(t, recorder.HasEvent("Normal", "RulesCached"),
			"expected Normal/RulesCached event; got: %v", recorder.Events)
	})

	t.Run("ruleset mixing supported and unsupported rules should be rejected", func(t *testing.T) {
		ruleSetCache := cache.NewRuleSetCache()

		cmSupported := utils.NewTestConfigMap("mix-supported", testNamespace,
			`SecRule REQUEST_URI "@contains /test" "id:1,phase:1,pass,nolog"`)
		require.NoError(t, k8sClient.Create(ctx, cmSupported))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, cmSupported); err != nil {
				t.Logf("Failed to delete ConfigMap: %v", err)
			}
		})

		cmUnsupported := utils.NewTestConfigMap("mix-unsupported", testNamespace,
			`SecRule RESPONSE_BODY "@rx leak" "id:956100,phase:4,deny,status:403"`)
		require.NoError(t, k8sClient.Create(ctx, cmUnsupported))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, cmUnsupported); err != nil {
				t.Logf("Failed to delete ConfigMap: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "mixed-ruleset",
			Namespace: testNamespace,
			Rules: []wafv1alpha1.RuleSourceReference{
				{Name: "mix-supported"},
				{Name: "mix-unsupported"},
			},
		})
		require.NoError(t, k8sClient.Create(ctx, ruleSet))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})

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

		require.NoError(t, err)
		assert.Equal(t, reconcile.Result{}, result)

		cacheKey := testNamespace + "/mixed-ruleset"
		_, ok := ruleSetCache.Get(cacheKey)
		assert.False(t, ok, "cache should be empty for rejected ruleset")

		assert.True(t, recorder.HasEvent("Warning", "UnsupportedRules"),
			"expected Warning/UnsupportedRules event; got: %v", recorder.Events)
	})

	t.Run("previously valid cache entry is preserved when ruleset update introduces unsupported rules", func(t *testing.T) {
		ruleSetCache := cache.NewRuleSetCache()
		cacheKey := testNamespace + "/update-to-unsupported"

		t.Log("Pre-populating cache to simulate a previously valid reconciliation")
		const previousRules = `SecCollectionTimeout 1`
		ruleSetCache.Put(cacheKey, previousRules, nil)
		prior, ok := ruleSetCache.Get(cacheKey)
		require.True(t, ok, "pre-condition: cache entry should exist")
		priorUUID := prior.UUID

		t.Log("Creating ConfigMap with unsupported rules (simulating a bad update)")
		cm := utils.NewTestConfigMap("update-to-unsupported-rules", testNamespace,
			`SecRule RESPONSE_BODY "@rx error" "id:950150,phase:4,deny,status:403,msg:'Bad update'"`)
		require.NoError(t, k8sClient.Create(ctx, cm))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, cm); err != nil {
				t.Logf("Failed to delete ConfigMap: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "update-to-unsupported",
			Namespace: testNamespace,
			Rules:     []wafv1alpha1.RuleSourceReference{{Name: "update-to-unsupported-rules"}},
		})
		require.NoError(t, k8sClient.Create(ctx, ruleSet))

		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})

		recorder := utils.NewFakeRecorder()
		reconciler := &RuleSetReconciler{
			Client:   k8sClient,
			Scheme:   scheme,
			Recorder: recorder,
			Cache:    ruleSetCache,
		}
		result, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace},
		})
		require.NoError(t, err)
		assert.Equal(t, reconcile.Result{}, result)

		t.Log("Verifying the previously valid entry is still served (last-known-good)")
		entry, ok := ruleSetCache.Get(cacheKey)
		require.True(t, ok, "prior cache entry must be preserved when update is rejected")
		assert.Equal(t, priorUUID, entry.UUID, "cache entry must not have changed")
		assert.Equal(t, previousRules, entry.Rules, "previously cached rules must still be served")
	})

	t.Run("ruleset with skip annotation should be cached with unsupported rules listed in status", func(t *testing.T) {
		ruleSetCache := cache.NewRuleSetCache()

		t.Log("Creating ConfigMap with an unsupported response body inspection rule")
		const unsupportedRule = `SecRule RESPONSE_BODY "@rx error" "id:950150,phase:4,deny,status:403,msg:'Data leakage'"`
		cm := utils.NewTestConfigMap("skip-annotation-rules", testNamespace, unsupportedRule)
		require.NoError(t, k8sClient.Create(ctx, cm))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, cm); err != nil {
				t.Logf("Failed to delete ConfigMap: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "skip-annotation-ruleset",
			Namespace: testNamespace,
			Rules:     []wafv1alpha1.RuleSourceReference{{Name: "skip-annotation-rules"}},
		})
		ruleSet.Annotations = map[string]string{
			wafv1alpha1.AnnotationSkipUnsupportedRulesCheck: "true",
		}
		require.NoError(t, k8sClient.Create(ctx, ruleSet))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, ruleSet); err != nil {
				t.Logf("Failed to delete RuleSet: %v", err)
			}
		})

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

		require.NoError(t, err)
		assert.Equal(t, reconcile.Result{}, result)

		t.Log("Verifying cache WAS populated despite unsupported rules")
		cacheKey := testNamespace + "/skip-annotation-ruleset"
		entry, ok := ruleSetCache.Get(cacheKey)
		assert.True(t, ok, "cache should contain entry when annotation overrides unsupported rules check")
		assert.Contains(t, entry.Rules, "id:950150")

		t.Log("Verifying Ready=True with unsupported rules listed in the message")
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}, ruleSet))
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, "RulesCached", ready.Reason)
		assert.Contains(t, ready.Message, "950150", "ready message should list the detected unsupported rule ID")

		t.Log("Verifying Degraded condition is absent")
		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Degraded")
		assert.Nil(t, degraded, "Degraded condition must not be set when annotation overrides")

		t.Log("Verifying Warning/UnsupportedRules event was still emitted")
		assert.True(t, recorder.HasEvent("Warning", "UnsupportedRules"),
			"expected Warning/UnsupportedRules event even with annotation override; got: %v", recorder.Events)
	})
}
