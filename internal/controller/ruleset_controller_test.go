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

func TestRuleSetReconciler_ReconcileRuleSources(t *testing.T) {
	tests := []struct {
		name          string
		ruleSetName   string
		ruleSources   map[string]string
		expectedRules string
	}{
		{
			name:        "single RuleSource",
			ruleSetName: "single-src-ruleset",
			ruleSources: map[string]string{
				"test-rules": "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"",
			},
			expectedRules: "SecRule REQUEST_URI \"@contains /admin\" \"id:1,deny\"",
		},
		{
			name:        "multiple RuleSources",
			ruleSetName: "multi-src-ruleset",
			ruleSources: map[string]string{
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

			t.Logf("Creating %d RuleSource(s)", len(tt.ruleSources))
			var refs []wafv1alpha1.SourceReference
			var names []string
			for name := range tt.ruleSources {
				names = append(names, name)
			}
			sort.Strings(names)

			t.Logf("Creating RuleSources: %v", names)
			for _, name := range names {
				data := tt.ruleSources[name]
				rs := utils.NewTestRuleSource(name, testNamespace, data)
				require.NoError(t, k8sClient.Create(ctx, rs))

				t.Cleanup(func() {
					if err := k8sClient.Delete(ctx, rs); err != nil {
						t.Logf("Failed to delete RuleSource %s: %v", name, err)
					}
				})

				refs = append(refs, wafv1alpha1.SourceReference{Name: name})
			}

			t.Log("Creating RuleSet referencing RuleSources")
			ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
				Name:      tt.ruleSetName,
				Namespace: testNamespace,
				Sources:   refs,
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

func TestRuleSetReconciler_MissingRuleSource(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := cache.NewRuleSetCache()

	t.Log("Creating RuleSet referencing non-existent RuleSource")
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "missing-src-ruleset",
		Namespace: testNamespace,
		Sources: []wafv1alpha1.SourceReference{
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

	t.Log("Reconciling RuleSet - should requeue due to missing RuleSource")
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

	t.Log("Verifying cache was not populated due to missing RuleSource")
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result, "Should requeue when RuleSource is not found")
	cacheKey := testNamespace + "/missing-src-ruleset"
	_, ok := ruleSetCache.Get(cacheKey)
	assert.False(t, ok)

	assert.True(t, recorder.HasEvent("Warning", "RuleSourceNotFound"),
		"expected Warning/RuleSourceNotFound event; got: %v", recorder.Events)
}

func TestRuleSetReconciler_ValidationRejection(t *testing.T) {
	tests := []struct {
		name          string
		ruleSetName   string
		sources       []wafv1alpha1.SourceReference
		expectedError string
	}{
		{
			name:          "no sources specified",
			ruleSetName:   "no-sources-ruleset",
			sources:       []wafv1alpha1.SourceReference{},
			expectedError: "spec.sources: Required value",
		},
		{
			name:        "too many sources",
			ruleSetName: "too-many-sources-ruleset",
			sources: func() []wafv1alpha1.SourceReference {
				sources := make([]wafv1alpha1.SourceReference, 2049)
				for i := range sources {
					sources[i] = wafv1alpha1.SourceReference{Name: "test"}
				}
				return sources
			}(),
			expectedError: "spec.sources: Too many",
		},
		{
			name:        "empty source name",
			ruleSetName: "empty-name-ruleset",
			sources: []wafv1alpha1.SourceReference{
				{Name: ""},
			},
			expectedError: "spec.sources[0].name: Required value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			t.Logf("Attempting to create RuleSet with invalid configuration: %s", tt.name)
			ruleSet := &wafv1alpha1.RuleSet{}
			ruleSet.Name = tt.ruleSetName
			ruleSet.Namespace = testNamespace
			ruleSet.Spec.Sources = tt.sources
			err := k8sClient.Create(ctx, ruleSet)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
		})
	}
}

func TestRuleSetReconciler_UpdateCache(t *testing.T) {
	ctx := context.Background()

	ruleSetCache := cache.NewRuleSetCache()

	t.Log("Creating RuleSource with initial rules")
	rs := utils.NewTestRuleSource("update-rules", "default", "SecDefaultAction \"phase:1,log,auditlog,pass\"")
	err := k8sClient.Create(ctx, rs)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, rs); err != nil {
			t.Logf("Failed to delete RuleSource: %v", err)
		}
	})

	t.Log("Creating RuleSet referencing RuleSource")
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "update-ruleset",
		Namespace: testNamespace,
		Sources: []wafv1alpha1.SourceReference{
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

	t.Log("Updating RuleSource with new rules")
	cacheKey := testNamespace + "/update-ruleset"
	entry1, _ := ruleSetCache.Get(cacheKey)
	uuid1 := entry1.UUID
	var updatedRS wafv1alpha1.RuleSource
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "update-rules", Namespace: testNamespace}, &updatedRS)
	require.NoError(t, err)
	updatedRS.Spec.Rules = "SecDefaultAction \"phase:2,log,auditlog,pass\""
	err = k8sClient.Update(ctx, &updatedRS)
	require.NoError(t, err)

	t.Log("Reconciling after RuleSource update to refresh cache")
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

func TestRuleSetReconciler_MissingRuleData(t *testing.T) {
	ctx := context.Background()
	ruleSetCache := cache.NewRuleSetCache()

	ruleSrc := utils.NewTestRuleSource("missing-data-rule", testNamespace, "SecCollectionTimeout 1")
	require.NoError(t, k8sClient.Create(ctx, ruleSrc))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSrc); err != nil {
			t.Logf("failed to delete %s: %v", ruleSrc.Name, err)
		}
	})

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "missing-data-ruleset",
		Namespace: testNamespace,
		Sources: []wafv1alpha1.SourceReference{
			{Name: "missing-data-rule"},
		},
		Data: []wafv1alpha1.DataReference{
			{Name: "non-existent-data"},
		},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("failed to delete RuleSet: %v", err)
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

	cacheKey := testNamespace + "/missing-data-ruleset"
	_, ok := ruleSetCache.Get(cacheKey)
	assert.False(t, ok, "cache should be empty when RuleData is missing")

	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace}, ruleSet))
	ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Equal(t, "RuleDataNotFound", ready.Reason)
	assert.Contains(t, ready.Message, "non-existent-data")

	assert.True(t, recorder.HasEvent("Warning", "RuleDataNotFound"),
		"expected Warning/RuleDataNotFound event; got: %v", recorder.Events)
}

func TestRuleSetReconciler_DataSourcesDuplicateFileKeysLastListedWins(t *testing.T) {
	ctx := context.Background()
	ruleSetCache := cache.NewRuleSetCache()

	dataFirst := utils.NewTestRuleData("dup-data-first", testNamespace, map[string]string{
		"overlap.data": "alpha",
	})
	dataSecond := utils.NewTestRuleData("dup-data-second", testNamespace, map[string]string{
		"overlap.data": "bravo",
	})
	ruleSrc := utils.NewTestRuleSource("dup-rule", testNamespace,
		`SecRule ARGS "@pmFromFile overlap.data" "id:77777,phase:1,pass,nolog"`,
	)

	require.NoError(t, k8sClient.Create(ctx, dataFirst))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, dataFirst); err != nil {
			t.Logf("failed to delete %s: %v", dataFirst.Name, err)
		}
	})
	require.NoError(t, k8sClient.Create(ctx, dataSecond))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, dataSecond); err != nil {
			t.Logf("failed to delete %s: %v", dataSecond.Name, err)
		}
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSrc))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSrc); err != nil {
			t.Logf("failed to delete %s: %v", ruleSrc.Name, err)
		}
	})

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "dup-key-ruleset",
		Namespace: testNamespace,
		Sources: []wafv1alpha1.SourceReference{
			{Name: "dup-rule"},
		},
		Data: []wafv1alpha1.DataReference{
			{Name: "dup-data-first"},
			{Name: "dup-data-second"},
		},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil {
			t.Logf("failed to delete RuleSet: %v", err)
		}
	})

	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewTestRecorder(),
		Cache:    ruleSetCache,
	}
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace},
	})
	require.NoError(t, err)

	cacheKey := testNamespace + "/dup-key-ruleset"
	entry, ok := ruleSetCache.Get(cacheKey)
	require.True(t, ok)
	require.Contains(t, entry.DataFiles, "overlap.data")
	assert.Equal(t, []byte("bravo"), entry.DataFiles["overlap.data"],
		"later-listed RuleData should overwrite the same files map key")
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

	ruleSources := []struct {
		name    string
		content string
	}{
		{
			name:    "update-rules-src",
			content: "SecDefaultAction \"phase:1,log,auditlog,pass\"",
		},
		{
			name:    "dumb-rule-src",
			content: "SecRule REMOTE_ADDR \".*\" \"id:12345,phase:1,pass,nolog,msg:'Test rule'\"",
		},
		{
			name:    "invalid-rule-src",
			content: "SecDefaultActionXPTO \"THIS IS VERY MUCH INVALID\"",
		},
		{
			name:    "referother-src",
			content: "SecRuleUpdateTargetById 12345 \"REMOTE_ADDR\"",
		},
		{
			name:    "withdata-src",
			content: "SecRule REQUEST_URI \"@pmFromFile rule1.data\" \"id:55555,phase:1,deny,status:403,msg:'File Match'\"",
		},
	}
	for _, rule := range ruleSources {
		rs := utils.NewTestRuleSource(rule.name, "default", rule.content)
		err := k8sClient.Create(ctx, rs)
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, rs); err != nil {
				t.Logf("Failed to delete RuleSource: %v", err)
			}
		})
	}

	ruleData := utils.NewTestRuleData("ruledata-src", "default", map[string]string{
		"rule1.data": "something\nanotherthing",
	})
	require.NoError(t, k8sClient.Create(ctx, ruleData))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleData); err != nil {
			t.Logf("Failed to delete RuleData: %v", err)
		}
	})

	t.Run("single rule should reconcile", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-simple",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "update-rules-src"},
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
			Sources: []wafv1alpha1.SourceReference{
				{Name: "update-rules-src"},
				{Name: "invalid-rule-src"},
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
		assert.Contains(t, ready.Message, "RuleSource invalid-rule-src doesn't contain valid rules: invalid WAF config from string: unknown directive \"secdefaultactionxpto\"")
		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Degraded")
		assert.Equal(t, metav1.ConditionTrue, degraded.Status)
		assert.Equal(t, "InvalidRuleSet", degraded.Reason)
		assert.Contains(t, degraded.Message, "RuleSource invalid-rule-src doesn't contain valid rules: invalid WAF config from string: unknown directive \"secdefaultactionxpto\"")
	})

	t.Run("ruleset referring other rules should pass", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-references",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "update-rules-src"},
				{Name: "dumb-rule-src"},
				{Name: "referother-src"},
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

	t.Run("ruleset using a valid data source should pass", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-validdata",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "withdata-src"},
			},
			Data: []wafv1alpha1.DataReference{
				{Name: "ruledata-src"},
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

	t.Run("ruleset referring missing RuleSource should fail", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-missingsrc",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "dumb-rule-src"},
				{Name: "notvalid"},
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
		assert.Equal(t, metav1.ConditionFalse, ready.Status)
		assert.Equal(t, "RuleSourceNotFound", ready.Reason)
		assert.Equal(t, "Referenced RuleSource notvalid does not exist", ready.Message)
	})

	t.Run("ruleset referring @pmFromFile without a Data source should fail", func(t *testing.T) {
		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "ruleset-invaliddata",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "withdata-src"},
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

		t.Log("Creating RuleSource with an unsupported multipart charset detection rule")
		rs := utils.NewTestRuleSource("unsupported-rules-src", testNamespace,
			`SecRule ARGS "@rx test" "id:922110,phase:2,deny,status:403,msg:'Multipart charset'"`)
		require.NoError(t, k8sClient.Create(ctx, rs))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, rs); err != nil {
				t.Logf("Failed to delete RuleSource: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "unsupported-ruleset",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "unsupported-rules-src"},
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
		assert.Contains(t, ready.Message, "922110")
		assert.Contains(t, ready.Message, "multipart charset detection")

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

		t.Log("Creating RuleSource with only supported rules")
		rs := utils.NewTestRuleSource("supported-rules-src", testNamespace,
			`SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny,status:403"`)
		require.NoError(t, k8sClient.Create(ctx, rs))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, rs); err != nil {
				t.Logf("Failed to delete RuleSource: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "supported-ruleset",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "supported-rules-src"},
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

		rsSupported := utils.NewTestRuleSource("mix-supported-src", testNamespace,
			`SecRule REQUEST_URI "@contains /test" "id:1,phase:1,pass,nolog"`)
		require.NoError(t, k8sClient.Create(ctx, rsSupported))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, rsSupported); err != nil {
				t.Logf("Failed to delete RuleSource: %v", err)
			}
		})

		rsUnsupported := utils.NewTestRuleSource("mix-unsupported-src", testNamespace,
			`SecRule ARGS "@rx leak" "id:922110,phase:2,deny,status:403"`)
		require.NoError(t, k8sClient.Create(ctx, rsUnsupported))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, rsUnsupported); err != nil {
				t.Logf("Failed to delete RuleSource: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "mixed-ruleset",
			Namespace: testNamespace,
			Sources: []wafv1alpha1.SourceReference{
				{Name: "mix-supported-src"},
				{Name: "mix-unsupported-src"},
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

		t.Log("Creating RuleSource with unsupported rules (simulating a bad update)")
		rs := utils.NewTestRuleSource("update-to-unsupported-rules-src", testNamespace,
			`SecRule ARGS "@rx error" "id:922110,phase:2,deny,status:403,msg:'Bad update'"`)
		require.NoError(t, k8sClient.Create(ctx, rs))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, rs); err != nil {
				t.Logf("Failed to delete RuleSource: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "update-to-unsupported",
			Namespace: testNamespace,
			Sources:   []wafv1alpha1.SourceReference{{Name: "update-to-unsupported-rules-src"}},
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

		t.Log("Creating RuleSource with an unsupported response body inspection rule")
		const unsupportedRule = `SecRule ARGS "@rx error" "id:922110,phase:2,deny,status:403,msg:'Multipart charset'"`
		rs := utils.NewTestRuleSource("skip-annotation-rules-src", testNamespace, unsupportedRule)
		require.NoError(t, k8sClient.Create(ctx, rs))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, rs); err != nil {
				t.Logf("Failed to delete RuleSource: %v", err)
			}
		})

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "skip-annotation-ruleset",
			Namespace: testNamespace,
			Sources:   []wafv1alpha1.SourceReference{{Name: "skip-annotation-rules-src"}},
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
		assert.Contains(t, entry.Rules, "id:922110")

		t.Log("Verifying Ready=True with unsupported rules listed in the message")
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{
			Name:      ruleSet.Name,
			Namespace: ruleSet.Namespace,
		}, ruleSet))
		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Ready")
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, "RulesCached", ready.Reason)
		assert.Contains(t, ready.Message, "922110", "ready message should list the detected unsupported rule ID")

		t.Log("Verifying Degraded condition is absent")
		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, "Degraded")
		assert.Nil(t, degraded, "Degraded condition must not be set when annotation overrides")

		t.Log("Verifying Warning/UnsupportedRules event was still emitted")
		assert.True(t, recorder.HasEvent("Warning", "UnsupportedRules"),
			"expected Warning/UnsupportedRules event even with annotation override; got: %v", recorder.Events)
	})
}
