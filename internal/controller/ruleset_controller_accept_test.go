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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/validation"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// expectedRuleSetContentHash recomputes the hash the reconciler must record
// from the current API state.
func expectedRuleSetContentHash(t *testing.T, ctx context.Context, nn types.NamespacedName) string {
	t.Helper()

	var ruleset wafv1alpha1.RuleSet
	require.NoError(t, k8sClient.Get(ctx, nn, &ruleset))

	inputs := assemble.Inputs{
		RuleSetUID:       ruleset.UID,
		RuleSetName:      ruleset.Name,
		Namespace:        ruleset.Namespace,
		EmptyRulesPolicy: assemble.EmptyRulesPolicy(ruleset.Spec.EmptyRulesPolicy),
	}
	for _, ref := range ruleset.Spec.Sources {
		var source wafv1alpha1.RuleSource
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ref.Name}, &source))
		inputs.Sources = append(inputs.Sources, assemble.SourceInput{
			Name:       source.Name,
			UID:        source.UID,
			Generation: source.Generation,
			Rules:      source.Spec.Rules,
		})
	}
	for _, ref := range ruleset.Spec.Data {
		var data wafv1alpha1.RuleData
		require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Namespace: ruleset.Namespace, Name: ref.Name}, &data))
		inputs.Data = append(inputs.Data, assemble.DataInput{
			Name:       data.Name,
			UID:        data.UID,
			Generation: data.Generation,
			Files:      data.Spec.Files,
		})
	}

	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	hash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)
	return hash
}

func TestRuleSetReconciler_AcceptsContentByHash(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	source := utils.NewTestRuleSource("accept-hash-src", ns,
		`SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny,status:403"`)
	require.NoError(t, k8sClient.Create(ctx, source))
	data := utils.NewTestRuleData("accept-hash-data", ns, map[string]string{"blocked.data": "attack\n"})
	require.NoError(t, k8sClient.Create(ctx, data))

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "accept-hash-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
		Data:      []wafv1alpha1.DataReference{{Name: data.Name}},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))

	recorder := utils.NewFakeRecorder()
	reconciler := &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: recorder,
	}
	nn := types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace}
	_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	require.NotNil(t, ruleSet.Status.AcceptedContent, "acceptedContent must be recorded")
	assert.Equal(t, ruleSet.Generation, ruleSet.Status.AcceptedContent.ObservedGeneration)
	assert.Equal(t, expectedRuleSetContentHash(t, ctx, nn), ruleSet.Status.AcceptedContent.ContentHash)

	ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, ruleSetReadyReasonRulesAccepted, ready.Reason)

	assert.True(t, recorder.HasEvent("Normal", "RulesAccepted"),
		"expected Normal/RulesAccepted event; got: %v", recorder.Events)
}

func TestRuleSetReconciler_PreservesAcceptedContentOnInvalidUpdate(t *testing.T) {
	tests := []struct {
		name         string
		invalidRules string
		policy       validation.RulePolicy
		wantReason   string
		wantMessage  string
	}{
		{
			name:         "empty rules under Reject",
			invalidRules: "   ",
			wantReason:   ruleSetDegradedReasonInvalidRuleSet,
		},
		{
			name:         "invalid pmFromFile reference",
			invalidRules: `SecRule ARGS "@pmFromFile missing.data" "id:55555,phase:1,deny,status:403"`,
			wantReason:   ruleSetDegradedReasonInvalidRuleSet,
		},
		{
			name:         "operator the gateway module does not support",
			invalidRules: `SecRule REMOTE_ADDR "@rbl sbl.spamhaus.org" "id:55556,phase:1,deny,status:403"`,
			wantReason:   "ReferencedRuleSourceInvalid",
		},
		{
			name:         "edit the cluster rule policy denies",
			invalidRules: `SecRule ARGS "@pm attack" "id:55557,phase:1,deny,status:403"`,
			policy:       validation.RulePolicy{DeniedOperators: []string{"pm"}},
			wantReason:   ruleSetDegradedReasonInvalidRuleSet,
			wantMessage:  "@pm is not allowed by the cluster rule policy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ns := setupTestNamespace(t, ctx)

			source := utils.NewTestRuleSource("preserve-src", ns,
				`SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny,status:403"`)
			require.NoError(t, k8sClient.Create(ctx, source))

			ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
				Name:      "preserve-ruleset",
				Namespace: ns,
				Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
			})
			require.NoError(t, k8sClient.Create(ctx, ruleSet))

			reconciler := &RuleSetReconciler{
				Client:     k8sClient,
				Scheme:     scheme,
				Recorder:   utils.NewFakeRecorder(),
				RulePolicy: tt.policy,
			}
			nn := types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace}
			_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
			require.NoError(t, err)

			require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
			require.NotNil(t, ruleSet.Status.AcceptedContent)
			accepted := *ruleSet.Status.AcceptedContent

			var updated wafv1alpha1.RuleSource
			require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: source.Name, Namespace: ns}, &updated))
			updated.Spec.Rules = tt.invalidRules
			require.NoError(t, k8sClient.Update(ctx, &updated))

			_, err = reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
			require.NoError(t, err)

			require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
			degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
			require.NotNil(t, degraded, "invalid input must degrade the RuleSet")
			assert.Equal(t, metav1.ConditionTrue, degraded.Status)
			assert.Equal(t, tt.wantReason, degraded.Reason)
			assert.Contains(t, degraded.Message, tt.wantMessage)

			require.NotNil(t, ruleSet.Status.AcceptedContent, "last-known-good acceptance must be preserved")
			assert.Equal(t, accepted, *ruleSet.Status.AcceptedContent)
		})
	}
}

// editRuleSetSource replaces the rules of source, reconciles ruleSet with
// reconciler, and reads ruleSet back.
func editRuleSetSource(t *testing.T, ctx context.Context, source *wafv1alpha1.RuleSource, rules string, ruleSet *wafv1alpha1.RuleSet, reconciler *RuleSetReconciler) {
	t.Helper()
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(source), source))
	source.Spec.Rules = rules
	require.NoError(t, k8sClient.Update(ctx, source))
	nn := client.ObjectKeyFromObject(ruleSet)
	_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
}

// TestRuleSetReconciler_RulePolicyKeepsAcceptedContent covers a cluster rule
// policy tightened across an operator restart: content accepted before stays
// Ready and accepted, because the policy judges only content not accepted yet.
func TestRuleSetReconciler_RulePolicyKeepsAcceptedContent(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	rules := `SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny,status:403"`
	source := utils.NewTestRuleSource("policy-restart-src", ns, rules)
	require.NoError(t, k8sClient.Create(ctx, source))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "policy-restart-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))

	nn := types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace}
	_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, &RuleSetReconciler{
		Client:   k8sClient,
		Scheme:   scheme,
		Recorder: utils.NewFakeRecorder(),
	})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	require.NotNil(t, ruleSet.Status.AcceptedContent)
	accepted := *ruleSet.Status.AcceptedContent

	policy := validation.RulePolicy{DeniedOperators: []string{"contains"}}
	require.Error(t, validation.CheckRulePolicy(rules, nil, policy), "the new policy denies the accepted rule")
	restarted := &RuleSetReconciler{
		Client:     k8sClient,
		Scheme:     scheme,
		Recorder:   utils.NewFakeRecorder(),
		RulePolicy: policy,
	}
	_, err = reconcileRuleSetWithRuleSources(ctx, t, nn, restarted)
	require.NoError(t, err)

	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	require.NotNil(t, ruleSet.Status.AcceptedContent)
	assert.Equal(t, accepted, *ruleSet.Status.AcceptedContent)
	ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, ruleSetReadyReasonRulesAccepted, ready.Reason)
	assert.False(t, apimeta.IsStatusConditionTrue(ruleSet.Status.Conditions, conditionDegraded))

	// A spec change that leaves the content unchanged is accepted again.
	ruleSet.Spec.EmptyRulesPolicy = wafv1alpha1.EmptyRulesPolicyAllow
	require.NoError(t, k8sClient.Update(ctx, ruleSet))
	_, err = reconcileRuleSetWithRuleSources(ctx, t, nn, restarted)
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	assert.True(t, isConditionCurrent(ruleSet.Status.Conditions, conditionReady, ruleSetReadyReasonRulesAccepted, ruleSet.Generation))
	require.NotNil(t, ruleSet.Status.AcceptedContent)
	assert.Equal(t, accepted.ContentHash, ruleSet.Status.AcceptedContent.ContentHash)

	// An edit the policy denies, then a return to the accepted content, which
	// the policy also denies: the returned content is accepted again.
	editSource := func(text string) { editRuleSetSource(t, ctx, source, text, ruleSet, restarted) }
	editSource(`SecRule REQUEST_URI "@contains /private" "id:1,phase:1,deny,status:403"`)
	degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Contains(t, degraded.Message, "@contains is not allowed by the cluster rule policy")
	assert.Equal(t, accepted.ContentHash, ruleSet.Status.AcceptedContent.ContentHash)
	editSource(rules)
	assert.True(t, isConditionCurrent(ruleSet.Status.Conditions, conditionReady, ruleSetReadyReasonRulesAccepted, ruleSet.Generation))
	assert.False(t, apimeta.IsStatusConditionTrue(ruleSet.Status.Conditions, conditionDegraded))
	assert.Equal(t, accepted.ContentHash, ruleSet.Status.AcceptedContent.ContentHash)
}

// TestRuleSetReconciler_RulePolicyRevertToAcceptedContent covers an edit the
// cluster rule policy denies: the RuleSet is Degraded with the last accepted
// content kept, reconciling it again writes nothing, and reverting the edit
// accepts the content again.
func TestRuleSetReconciler_RulePolicyRevertToAcceptedContent(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	accepted := `SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny,status:403"`
	source := utils.NewTestRuleSource("policy-revert-src", ns, accepted)
	require.NoError(t, k8sClient.Create(ctx, source))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "policy-revert-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	reconciler := &RuleSetReconciler{
		Client:     k8sClient,
		Scheme:     scheme,
		Recorder:   utils.NewFakeRecorder(),
		RulePolicy: validation.RulePolicy{DeniedOperators: []string{"pm"}},
	}
	nn := types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace}
	editSource := func(rules string) { editRuleSetSource(t, ctx, source, rules, ruleSet, reconciler) }

	_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	require.NotNil(t, ruleSet.Status.AcceptedContent)
	acceptedContent := *ruleSet.Status.AcceptedContent

	editSource(`SecRule ARGS "@pm attack" "id:1,phase:1,deny,status:403"`)
	degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
	assert.Contains(t, degraded.Message, "@pm is not allowed by the cluster rule policy")
	require.NotNil(t, ruleSet.Status.AcceptedContent)
	assert.Equal(t, acceptedContent, *ruleSet.Status.AcceptedContent)

	resourceVersion := ruleSet.ResourceVersion
	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: nn})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
	assert.Equal(t, resourceVersion, ruleSet.ResourceVersion, "reconciling a denied RuleSet again writes nothing")

	editSource(accepted)
	assert.True(t, isConditionCurrent(ruleSet.Status.Conditions, conditionReady, ruleSetReadyReasonRulesAccepted, ruleSet.Generation))
	assert.False(t, apimeta.IsStatusConditionTrue(ruleSet.Status.Conditions, conditionDegraded))
	assert.Equal(t, acceptedContent.ContentHash, ruleSet.Status.AcceptedContent.ContentHash)
}

func TestRuleSetReconciler_EmptyRulesPolicyAcceptance(t *testing.T) {
	t.Run("Allow accepts and records a hash for empty rules", func(t *testing.T) {
		ctx := context.Background()
		ns := setupTestNamespace(t, ctx)

		source := utils.NewTestRuleSource("empty-allow-src", ns, "   ")
		require.NoError(t, k8sClient.Create(ctx, source))

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "empty-allow-ruleset",
			Namespace: ns,
			Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
		})
		ruleSet.Spec.EmptyRulesPolicy = wafv1alpha1.EmptyRulesPolicyAllow
		require.NoError(t, k8sClient.Create(ctx, ruleSet))

		reconciler := &RuleSetReconciler{
			Client:   k8sClient,
			Scheme:   scheme,
			Recorder: utils.NewFakeRecorder(),
		}
		nn := types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace}
		_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
		require.NoError(t, err)

		require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
		require.NotNil(t, ruleSet.Status.AcceptedContent, "Allow must record accepted content")
		assert.Equal(t, expectedRuleSetContentHash(t, ctx, nn), ruleSet.Status.AcceptedContent.ContentHash)

		ready := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionReady)
		require.NotNil(t, ready)
		assert.Equal(t, metav1.ConditionTrue, ready.Status)
		assert.Equal(t, ruleSetReadyReasonRulesAccepted, ready.Reason)
	})

	t.Run("default Reject rejects empty rules", func(t *testing.T) {
		ctx := context.Background()
		ns := setupTestNamespace(t, ctx)

		source := utils.NewTestRuleSource("empty-reject-src", ns, "   ")
		require.NoError(t, k8sClient.Create(ctx, source))

		ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
			Name:      "empty-reject-ruleset",
			Namespace: ns,
			Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
		})
		require.NoError(t, k8sClient.Create(ctx, ruleSet))

		reconciler := &RuleSetReconciler{
			Client:   k8sClient,
			Scheme:   scheme,
			Recorder: utils.NewFakeRecorder(),
		}
		nn := types.NamespacedName{Name: ruleSet.Name, Namespace: ruleSet.Namespace}
		_, err := reconcileRuleSetWithRuleSources(ctx, t, nn, reconciler)
		require.NoError(t, err)

		require.NoError(t, k8sClient.Get(ctx, nn, ruleSet))
		assert.Nil(t, ruleSet.Status.AcceptedContent, "Reject must not record accepted content")

		degraded := apimeta.FindStatusCondition(ruleSet.Status.Conditions, conditionDegraded)
		require.NotNil(t, degraded)
		assert.Equal(t, metav1.ConditionTrue, degraded.Status)
		assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
		assert.Contains(t, degraded.Message, "assembled rules are empty")
	})
}
