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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// reconcileTestRuleSource runs the real RuleSource validation, so the tests
// below exercise the per-source Include deferral instead of hand-setting Ready.
func reconcileTestRuleSource(t *testing.T, ctx context.Context, c client.Client, source *wafv1alpha1.RuleSource) {
	t.Helper()
	reconciler := &RuleSourceReconciler{Client: c, Recorder: utils.NewTestRecorder()}
	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(source)})
	require.NoError(t, err)

	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(source), source))
	ready := apimeta.FindStatusCondition(source.Status.Conditions, conditionReady)
	require.NotNil(t, ready, "the RuleSource must be validated before the RuleSet")
	assert.Equal(t, ruleSourceReadyReasonValidated, ready.Reason)
}

// TestIncludedRuleDataIsAcceptedAndDelivered covers `Include <RuleData name>`
// end to end: the RuleSource that Includes a RuleData file is validated (the
// include is deferred), the RuleSet aggregates and compiles it with the data
// files attached, and the Engine accepts the configuration.
func TestIncludedRuleDataIsAcceptedAndDelivered(t *testing.T) {
	ctx := context.Background()

	source := utils.NewTestRuleSource("include-source", testNamespace, "Include blocklist.conf")
	source.UID = "include-source-uid"
	data := utils.NewTestRuleData("blocklist-data", testNamespace, map[string]string{
		"blocklist.conf": `SecRule ARGS "@streq evilmonkey" "id:1,phase:1,deny,status:403"` + "\n",
	})
	data.UID = "blocklist-data-uid"
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "include-ruleset",
		Namespace: testNamespace,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
		Data:      []wafv1alpha1.DataReference{{Name: data.Name}},
	})
	ruleset.UID = "include-ruleset-uid"

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&wafv1alpha1.RuleSet{}, &wafv1alpha1.RuleData{}, &wafv1alpha1.Engine{}, &wafv1alpha1.RuleSource{}).
		WithObjects(ruleset, source, data).
		Build()

	reconcileTestRuleSource(t, ctx, c, source)
	ready := apimeta.FindStatusCondition(source.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, ruleSourceReadyReasonValidated, ready.Reason,
		"an Include of a RuleData name must be deferred, not rejected")
	assert.Nil(t, apimeta.FindStatusCondition(source.Status.Conditions, conditionDegraded))

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
	require.NotNil(t, updatedRuleSet.Status.AcceptedContent, "the included RuleData must be accepted")
	assert.Equal(t, ruleSetReadyReasonRulesAccepted, apimeta.FindStatusCondition(updatedRuleSet.Status.Conditions, conditionReady).Reason)
	assert.Nil(t, apimeta.FindStatusCondition(updatedRuleSet.Status.Conditions, conditionDegraded))

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "include-engine",
		Namespace:   testNamespace,
		GatewayName: "include-gw",
		RuleSetName: ruleset.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	engine.UID = "include-engine-uid"
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

	var updatedEngine wafv1alpha1.Engine
	require.NoError(t, c.Get(ctx, engineReq.NamespacedName, &updatedEngine))
	require.NotNil(t, updatedEngine.Status.AcceptedConfiguration, "the Engine must accept the included RuleData")
	assert.Equal(t, updatedRuleSet.Status.AcceptedContent.ContentHash, updatedEngine.Status.AcceptedConfiguration.RuleSetContentHash)
}

// TestMissingIncludedRuleDataIsDegraded covers the aggregate path: the per-source
// validation defers an unknown RuleData name, so the RuleSet compilation is the
// one that must report it. The status message keeps only the basename.
func TestMissingIncludedRuleDataIsDegraded(t *testing.T) {
	ctx := context.Background()

	source := utils.NewTestRuleSource("missing-include-source", testNamespace, "Include nope.conf")
	data := utils.NewTestRuleData("unrelated-data", testNamespace, map[string]string{"other.data": "x\n"})
	ruleset := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "missing-include-ruleset",
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
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: ruleset.Name, Namespace: ruleset.Namespace}}
	_, err = ruleSetReconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	var updated wafv1alpha1.RuleSet
	require.NoError(t, c.Get(ctx, req.NamespacedName, &updated))
	assert.Nil(t, updated.Status.AcceptedContent, "a missing included RuleData must not be accepted")
	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, ruleSetDegradedReasonInvalidRuleSet, degraded.Reason)
	assert.Contains(t, degraded.Message, "open nope.conf: data does not exist")
	assert.NotContains(t, degraded.Message, "/", "the status message must not leak a filesystem path")
}
