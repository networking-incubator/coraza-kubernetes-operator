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

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// TestReservedDataFileNameBlocksAcceptance covers RuleData objects created
// before the CEL rule rejected ".", which CEL cannot guard on read.
func TestReservedDataFileNameBlocksAcceptance(t *testing.T) {
	ctx := context.Background()

	data := utils.NewTestRuleData("legacy-data", testNamespace, map[string]string{".": "evildata\n"})
	source := utils.NewTestRuleSource("legacy-source", testNamespace, `SecRule ARGS "@pmFromFile ." "id:1,phase:1,pass,nolog"`)
	apimeta.SetStatusCondition(&source.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: source.Generation,
		Reason:             ruleSourceReadyReasonValidated,
		LastTransitionTime: metav1.Now(),
	})
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "legacy-ruleset",
		Namespace: testNamespace,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
		Data:      []wafv1alpha1.DataReference{{Name: data.Name}},
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&wafv1alpha1.RuleSet{}, &wafv1alpha1.RuleData{}, &wafv1alpha1.Engine{}).
		WithObjects(ruleset, source, data).
		Build()

	metrics, err := NewCorazaMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	ruleSetReconciler := &RuleSetReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: utils.NewTestRecorder(),
		Metrics:  metrics,
	}

	rulesetReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ruleset)}
	_, err = ruleSetReconciler.Reconcile(ctx, rulesetReq)
	require.NoError(t, err)

	var updatedRuleSet wafv1alpha1.RuleSet
	require.NoError(t, c.Get(ctx, rulesetReq.NamespacedName, &updatedRuleSet))
	require.Nil(t, updatedRuleSet.Status.AcceptedContent, "invalid data file names must not be accepted")
	degraded := apimeta.FindStatusCondition(updatedRuleSet.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, metav1.ConditionTrue, degraded.Status)
	assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
	assert.Contains(t, degraded.Message, `"."`)
	assert.Contains(t, degraded.Message, "RuleData legacy-data")

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "legacy-engine",
		Namespace:   testNamespace,
		GatewayName: "legacy-gw",
		RuleSetName: ruleset.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	require.NoError(t, c.Create(ctx, engine))

	engineReconciler := &EngineReconciler{
		Client:       c,
		Scheme:       scheme,
		Recorder:     utils.NewTestRecorder(),
		assetBaseURL: testAssetBaseURL,
		xdsConfig:    testXDSConfig(),
	}
	engineReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
	_, err = engineReconciler.provisionWasmDriver(ctx, logr.Discard(), engineReq, engine)
	require.NoError(t, err)

	var updatedEngine wafv1alpha1.Engine
	require.NoError(t, c.Get(ctx, engineReq.NamespacedName, &updatedEngine))
	assert.Nil(t, updatedEngine.Status.AcceptedConfiguration, "the Engine must not accept a degraded RuleSet")
	progressing := apimeta.FindStatusCondition(updatedEngine.Status.Conditions, conditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, reasonAwaitingRuleSetAcceptance, progressing.Reason)
}

// TestHostWritingDirectiveInIncludedRuleDataBlocksAcceptance covers an Include
// of a RuleData file whose content is parsed as rule text: the aggregate
// validation must reject a host-writing directive hidden there. The RuleSource
// is validated for real: its Include of a RuleData name is deferred, not
// rejected.
func TestHostWritingDirectiveInIncludedRuleDataBlocksAcceptance(t *testing.T) {
	ctx := context.Background()

	const marker = "included-data-marker-83cf"
	data := utils.NewTestRuleData("debug-data", testNamespace, map[string]string{
		"bad.data": "SecDebugLog /dev/shm/" + marker + ".log\n",
	})
	source := utils.NewTestRuleSource("debug-source", testNamespace, "Include bad.data")
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "debug-ruleset",
		Namespace: testNamespace,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
		Data:      []wafv1alpha1.DataReference{{Name: data.Name}},
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&wafv1alpha1.RuleSet{}, &wafv1alpha1.RuleData{}, &wafv1alpha1.Engine{}, &wafv1alpha1.RuleSource{}).
		WithObjects(ruleset, source, data).
		Build()

	reconcileTestRuleSource(t, ctx, c, source)

	metrics, err := NewCorazaMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	ruleSetReconciler := &RuleSetReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: utils.NewTestRecorder(),
		Metrics:  metrics,
	}

	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ruleset)}
	_, err = ruleSetReconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	var updated wafv1alpha1.RuleSet
	require.NoError(t, c.Get(ctx, req.NamespacedName, &updated))
	require.Nil(t, updated.Status.AcceptedContent, "rules that write host files must not be accepted")
	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
	assert.Contains(t, degraded.Message, "SecDebugLog")
	assert.NotContains(t, degraded.Message, marker)
}

// TestLegacyAcceptedContentIsRevalidatedOnUpgrade covers a RuleSet whose
// content an older build accepted and an Engine that published it. The accepted
// fast path must still run the directive and Include guard: after the upgrade
// the RuleSet must become Degraded, drop its accepted record, and the Engine
// must clear the accepted configuration instead of keeping rejected content
// published.
func TestLegacyAcceptedContentIsRevalidatedOnUpgrade(t *testing.T) {
	tests := []struct {
		name  string
		rules string
		want  string
	}{
		{name: "host writing directive", rules: "SecRuleEngine On\nSecDebugLog /dev/stdout", want: "SecDebugLog"},
		{name: "parent-relative include", rules: "SecRuleEngine On\nInclude ../../etc/passwd", want: "parent-relative"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			source := utils.NewTestRuleSource("legacy-accepted-source", testNamespace, tc.rules)
			// The old build validated this fragment before the guard existed,
			// so its status is Ready for the current generation.
			apimeta.SetStatusCondition(&source.Status.Conditions, metav1.Condition{
				Type:               conditionReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: source.Generation,
				Reason:             ruleSourceReadyReasonValidated,
				LastTransitionTime: metav1.Now(),
			})
			ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
				Name:      "legacy-accepted-ruleset",
				Namespace: testNamespace,
				Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
			})
			// The fake client does not assign UIDs, and render requires them.
			ruleset.UID = "legacy-accepted-ruleset-uid"

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&wafv1alpha1.RuleSet{}, &wafv1alpha1.Engine{}).
				WithObjects(ruleset, source).
				Build()

			// Record accepted content as the old build would: matching hash,
			// Ready, and a current observed generation.
			var current wafv1alpha1.RuleSet
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ruleset), &current))
			inputs, err := loadRuleSetInputs(ctx, c, &current)
			require.NoError(t, err)
			content, err := assemble.Assemble(inputs)
			require.NoError(t, err)
			hash, err := render.RuleSetContentHash(content)
			require.NoError(t, err)

			patch := client.MergeFrom(current.DeepCopy())
			current.Status.AcceptedContent = &wafv1alpha1.RuleSetAcceptedContent{
				ObservedGeneration: current.Generation,
				ContentHash:        hash,
			}
			apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type:               conditionReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: current.Generation,
				Reason:             ruleSetReadyReasonRulesAccepted,
				LastTransitionTime: metav1.Now(),
			})
			require.NoError(t, c.Status().Patch(ctx, &current, patch))

			// The Engine already exists and published the accepted content
			// before the upgrade. Without this precondition the "must not
			// publish" assertion below would hold trivially.
			engine := utils.NewTestEngine(utils.EngineOptions{
				Name:        "legacy-accepted-engine",
				Namespace:   testNamespace,
				GatewayName: "legacy-accepted-gw",
				RuleSetName: ruleset.Name,
				WasmURL:     utils.DefaultTestWasmURL,
				WasmSHA256:  utils.DefaultTestWasmSHA256,
			})
			engine.UID = "legacy-accepted-engine-uid"
			// provisionWasmDriver is called directly, so initialize the status a
			// full reconcile would have created before this point.
			engine.Status = &wafv1alpha1.EngineStatus{}
			require.NoError(t, c.Create(ctx, engine))

			engineReconciler := &EngineReconciler{
				Client:       c,
				Scheme:       scheme,
				Recorder:     utils.NewTestRecorder(),
				assetBaseURL: testAssetBaseURL,
				xdsConfig:    testXDSConfig(),
			}
			engineReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
			_, err = engineReconciler.provisionWasmDriver(ctx, logr.Discard(), engineReq, engine)
			require.NoError(t, err)

			var published wafv1alpha1.Engine
			require.NoError(t, c.Get(ctx, engineReq.NamespacedName, &published))
			require.NotNil(t, published.Status.AcceptedConfiguration, "precondition: the old build published this content")

			// The new build must run the guard before the accepted fast path.
			metrics, err := NewCorazaMetrics(prometheus.NewRegistry())
			require.NoError(t, err)
			ruleSetReconciler := &RuleSetReconciler{
				Client:   c,
				Scheme:   scheme,
				Recorder: utils.NewTestRecorder(),
				Metrics:  metrics,
			}

			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ruleset)}
			_, err = ruleSetReconciler.Reconcile(ctx, req)
			require.NoError(t, err)

			var updated wafv1alpha1.RuleSet
			require.NoError(t, c.Get(ctx, req.NamespacedName, &updated))
			degraded := apimeta.FindStatusCondition(updated.Status.Conditions, conditionDegraded)
			require.NotNil(t, degraded)
			assert.Equal(t, metav1.ConditionTrue, degraded.Status)
			assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
			assert.Contains(t, degraded.Message, tc.want)

			ready := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, metav1.ConditionFalse, ready.Status, "a re-rejected RuleSet must not stay Ready")
			assert.Nil(t, updated.Status.AcceptedContent, "the rejected content must not stay recorded as accepted")

			// The Engine sees the rejection and drops the content it published.
			_, err = engineReconciler.provisionWasmDriver(ctx, logr.Discard(), engineReq, &published)
			require.NoError(t, err)

			var updatedEngine wafv1alpha1.Engine
			require.NoError(t, c.Get(ctx, engineReq.NamespacedName, &updatedEngine))
			assert.Nil(t, updatedEngine.Status.AcceptedConfiguration, "the Engine must clear the re-rejected configuration")
			progressing := apimeta.FindStatusCondition(updatedEngine.Status.Conditions, conditionProgressing)
			require.NotNil(t, progressing)
			assert.Equal(t, reasonAwaitingRuleSetAcceptance, progressing.Reason)
		})
	}
}

// TestMarkRulesInvalidClearsOnlyRejectedAcceptedContent covers the compile
// failure path: a last-known-good record with another hash survives, while the
// legacy record for the exact content this build rejects is cleared.
func TestMarkRulesInvalidClearsOnlyRejectedAcceptedContent(t *testing.T) {
	tests := []struct {
		name          string
		acceptedHash  func(t *testing.T, ruleset *wafv1alpha1.RuleSet, source *wafv1alpha1.RuleSource) string
		wantPreserved bool
	}{
		{
			name: "different last-known-good hash is preserved",
			acceptedHash: func(*testing.T, *wafv1alpha1.RuleSet, *wafv1alpha1.RuleSource) string {
				return "sha256:last-known-good"
			},
			wantPreserved: true,
		},
		{
			name: "legacy accepted hash for rejected content is cleared",
			acceptedHash: func(t *testing.T, ruleset *wafv1alpha1.RuleSet, source *wafv1alpha1.RuleSource) string {
				t.Helper()
				content, err := assemble.Assemble(assemble.Inputs{
					RuleSetUID:  ruleset.UID,
					RuleSetName: ruleset.Name,
					Namespace:   ruleset.Namespace,
					Sources: []assemble.SourceInput{{
						Name:       source.Name,
						UID:        source.UID,
						Generation: source.Generation,
						Rules:      source.Spec.Rules,
					}},
				})
				require.NoError(t, err)
				hash, err := render.RuleSetContentHash(content)
				require.NoError(t, err)
				return hash
			},
			wantPreserved: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			source := utils.NewTestRuleSource("compile-invalid-src", testNamespace, `SecDefaultActionXPTO "INVALID"`)
			source.UID = "compile-invalid-src-uid"
			apimeta.SetStatusCondition(&source.Status.Conditions, metav1.Condition{
				Type:               conditionReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: source.Generation,
				Reason:             ruleSourceReadyReasonValidated,
				LastTransitionTime: metav1.Now(),
			})
			ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
				Name:      "compile-invalid-ruleset",
				Namespace: testNamespace,
				Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
			})
			ruleset.UID = "compile-invalid-ruleset-uid"

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&wafv1alpha1.RuleSet{}).
				WithObjects(ruleset, source).
				Build()

			var current wafv1alpha1.RuleSet
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(ruleset), &current))
			patch := client.MergeFrom(current.DeepCopy())
			current.Status.AcceptedContent = &wafv1alpha1.RuleSetAcceptedContent{
				ObservedGeneration: current.Generation,
				ContentHash:        tc.acceptedHash(t, ruleset, source),
			}
			require.NoError(t, c.Status().Patch(ctx, &current, patch))

			metrics, err := NewCorazaMetrics(prometheus.NewRegistry())
			require.NoError(t, err)
			reconciler := &RuleSetReconciler{
				Client:   c,
				Scheme:   scheme,
				Recorder: utils.NewTestRecorder(),
				Metrics:  metrics,
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ruleset)}
			_, err = reconciler.Reconcile(ctx, req)
			require.NoError(t, err)

			var updated wafv1alpha1.RuleSet
			require.NoError(t, c.Get(ctx, req.NamespacedName, &updated))
			degraded := apimeta.FindStatusCondition(updated.Status.Conditions, conditionDegraded)
			require.NotNil(t, degraded)
			assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
			if tc.wantPreserved {
				require.NotNil(t, updated.Status.AcceptedContent, "a different accepted record must be preserved")
			} else {
				assert.Nil(t, updated.Status.AcceptedContent, "the rejected accepted record must be cleared")
			}
		})
	}
}
