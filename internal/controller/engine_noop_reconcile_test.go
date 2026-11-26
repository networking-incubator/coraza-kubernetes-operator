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
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

func TestEngineReconciler_NoOpKeepsAcceptedAndPublishedVersionWithoutResourceWrites(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	counts := useEngineOperationClient(t, fixture)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))
	require.NotNil(t, before.Status.AcceptedConfiguration)
	acceptedBefore := *before.Status.AcceptedConfiguration

	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	snapshotBefore, found := publication.Store.Get(req.NamespacedName.String())
	require.True(t, found)
	assert.Equal(t, acceptedBefore.Version, snapshotBefore.Version)
	assert.Equal(t, acceptedBefore.RuleSetContentHash, snapshotBefore.RuleSetHash)

	counts.reset()
	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)

	assert.EqualValues(t, 1, counts.apiDeletes, "disabled Telemetry is cleaned up before provisioning")
	assert.EqualValues(t, 1, counts.apiGets, "the live EnvoyFilter read still detects external drift")
	assert.Zero(t, counts.apiCreates)
	assert.Zero(t, counts.apiUpdates, "an unchanged NetworkPolicy must not be updated")
	assert.Zero(t, counts.apiPatches, "an unchanged EnvoyFilter must not be applied")
	assert.Zero(t, counts.statusPatches, "the current accepted status must not be rewritten")

	var after wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &after))
	require.NotNil(t, after.Status.AcceptedConfiguration)
	assert.Equal(t, acceptedBefore, *after.Status.AcceptedConfiguration)
	assert.True(t, isConditionCurrent(after.Status.Conditions, conditionReady, reasonConfigurationAccepted, after.Generation))

	_, err = publication.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	snapshotAfter, found := publication.Store.Get(req.NamespacedName.String())
	require.True(t, found)
	assert.Equal(t, snapshotBefore.Version, snapshotAfter.Version)
	assert.Equal(t, snapshotBefore.ResourceName, snapshotAfter.ResourceName)
}

func TestEngineReconciler_NoOpUsesCachedRenderWithoutAssembly(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))
	require.NotNil(t, before.Status.AcceptedConfiguration)
	contentless := &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	fixture.reconciler.Client = contentless

	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, 2, contentless.sourceGets, "the no-op reads current inputs and performs the final race check")
	require.Equal(t, 2, contentless.dataGets, "the no-op reads current inputs and performs the final race check")

	var after wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &after))
	require.Equal(t, before.Status.AcceptedConfiguration, after.Status.AcceptedConfiguration)
}

func TestEngineReconciler_UsesVerifiedRenderAfterCacheEviction(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	var engine wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &engine))
	require.NotNil(t, engine.Status.AcceptedConfiguration)
	acceptedBefore := *engine.Status.AcceptedConfiguration
	filterBefore := requireEnvoyFilterExists(t, fixture.ctx, engine.Name, engine.Namespace)
	filterVersion := filterBefore.GetAnnotations()["waf.k8s.coraza.io/configuration-version"]
	binary, err := fixture.reconciler.resolveBinaryRef(&engine)
	require.NoError(t, err)

	fixture.reconciler.Client = &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	accepted, result, err := fixture.reconciler.acceptedRuleSetContent(
		fixture.ctx,
		logr.Discard(),
		req,
		&engine,
		binary,
	)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	require.True(t, accepted.hasCachedRender)
	require.Empty(t, accepted.content.Rules)
	require.Equal(t, acceptedBefore.RuleSetContentHash, accepted.contentHash)

	// Evict after identity verification but before rendering. The verified entry
	// carried by acceptedRuleSet must prevent an incomplete render input.
	engineKey := req.NamespacedName
	fixture.reconciler.renderCache.delete(engineKey)
	input := engineRenderInput(&engine, &accepted.ruleSet, accepted.content, accepted.contentHash, binary)
	artifact, failureMessage, err := fixture.reconciler.renderEngineArtifact(
		engineKey,
		input,
		accepted.contentLimits,
		&accepted.cachedRender,
	)
	require.NoError(t, err, failureMessage)
	require.Equal(t, acceptedBefore.Version, artifact.Version)
	require.Nil(t, artifact.Payload)
	require.Nil(t, artifact.Resource)
	_, found := fixture.reconciler.renderCache.get(engineKey, accepted.cachedRender.key)
	require.False(t, found, "the test must exercise an actual cache miss at render time")

	var afterRender wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &afterRender))
	require.Equal(t, acceptedBefore, *afterRender.Status.AcceptedConfiguration)
	filterAfter := requireEnvoyFilterExists(t, fixture.ctx, engine.Name, engine.Namespace)
	require.Equal(t, filterVersion, filterAfter.GetAnnotations()["waf.k8s.coraza.io/configuration-version"])

	// A later normal reconcile repopulates the cache from full, hash-verified
	// content and keeps the accepted configuration stable.
	fixture.reconciler.Client = k8sClient
	result, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	var recovered wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &recovered))
	require.Equal(t, acceptedBefore, *recovered.Status.AcceptedConfiguration)
	_, found = fixture.reconciler.renderCache.get(engineKey, accepted.cachedRender.key)
	require.True(t, found)
}

func TestEngineReconciler_NoOpRechecksCachedContentLimits(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	engineKey := req.NamespacedName
	entry, found := fixture.reconciler.renderCache.get(engineKey, func() engineRenderCacheKey {
		var current wafv1alpha1.Engine
		require.NoError(t, k8sClient.Get(fixture.ctx, engineKey, &current))
		var ruleSet wafv1alpha1.RuleSet
		require.NoError(t, k8sClient.Get(fixture.ctx, types.NamespacedName{Namespace: current.Namespace, Name: current.Spec.RuleSet.Name}, &ruleSet))
		binary, err := fixture.reconciler.resolveBinaryRef(&current)
		require.NoError(t, err)
		return engineRenderCacheKeyFor(engineRenderInput(&current, &ruleSet, assemble.Content{}, ruleSet.Status.AcceptedContent.ContentHash, binary))
	}())
	require.True(t, found)
	require.Positive(t, entry.contentLimits.decodedDataBytes)

	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, engineKey, &before))
	limit := entry.contentLimits.decodedDataBytes - 1
	fixture.reconciler.Limits = xds.Limits{MaxDecodedDataBytes: limit}
	fixture.reconciler.Client = &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	_, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.ErrorContains(t, err, "decoded RuleData size")

	var rejected wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, engineKey, &rejected))
	require.Equal(t, before.Status.AcceptedConfiguration, rejected.Status.AcceptedConfiguration)
	require.True(t, isConditionCurrent(rejected.Status.Conditions, conditionDegraded, "InvalidRuleSet", rejected.Generation))

	fixture.reconciler.Client = k8sClient
	fixture.reconciler.Limits = xds.Limits{}
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	var recovered wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, engineKey, &recovered))
	require.True(t, isConditionCurrent(recovered.Status.Conditions, conditionReady, reasonConfigurationAccepted, recovered.Generation))
}

func TestEngineReconciler_RecreatedEngineStartsWithItsOwnUID(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	oldUID := fixture.engine.UID
	oldKey := request.NamespacedName
	fixture.reconciler.renderCache.mu.Lock()
	entry, found := fixture.reconciler.renderCache.entries[oldKey]
	fixture.reconciler.renderCache.mu.Unlock()
	require.True(t, found)

	require.NoError(t, k8sClient.Delete(fixture.ctx, fixture.engine))
	_, err := fixture.reconciler.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var current wafv1alpha1.Engine
		return apierrors.IsNotFound(k8sClient.Get(fixture.ctx, oldKey, &current))
	}, 5*time.Second, 10*time.Millisecond, "the deleted Engine must release its name")
	_, found = fixture.reconciler.renderCache.get(oldKey, entry.key)
	require.False(t, found, "deletion must release the old Engine's render identity")

	recreated := utils.NewTestEngine(utils.EngineOptions{
		Name:        fixture.engine.Name,
		Namespace:   fixture.engine.Namespace,
		RuleSetName: fixture.ruleSet.Name,
		GatewayName: fixture.gateway.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	require.NoError(t, k8sClient.Create(fixture.ctx, recreated))
	require.NotEqual(t, oldUID, recreated.UID)
	request = ctrl.Request{NamespacedName: client.ObjectKeyFromObject(recreated)}

	result, err := fixture.reconciler.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.NotZero(t, result.RequeueAfter, "the recreated Engine must add its own finalizer")

	contentless := &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	fixture.reconciler.Client = contentless
	_, err = fixture.reconciler.Reconcile(fixture.ctx, request)
	require.Error(t, err, "a recreated Engine must not inherit the prior UID's contentless cache entry")
	require.Equal(t, 1, contentless.sourceGets, "a cache miss must fall straight through to one full-content read")
	require.Equal(t, 1, contentless.dataGets)

	fixture.reconciler.Client = k8sClient
	_, err = fixture.reconciler.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	var accepted wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, request.NamespacedName, &accepted))
	require.NotNil(t, accepted.Status.AcceptedConfiguration)
	require.Equal(t, recreated.UID, accepted.UID)
}

func TestEngineReconciler_InputMetadataUpdatesKeepAcceptedConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		update func(*testing.T, *engineNoopFixture)
	}{
		{
			name: "validation annotation and status",
			update: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				var source wafv1alpha1.RuleSource
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &source))
				source.Annotations = map[string]string{wafv1alpha1.AnnotationSkipValidation: "false"}
				require.NoError(t, k8sClient.Update(fixture.ctx, &source))
				_, err := (&RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder()}).
					Reconcile(fixture.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&source)})
				require.NoError(t, err)
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&source), &source))
				require.True(t, isConditionCurrent(source.Status.Conditions, conditionReady, ruleSourceReadyReasonValidationSkipped, source.Generation))
			},
		},
		{
			name: "status-only update",
			update: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				var source wafv1alpha1.RuleSource
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &source))
				source.Status.Conditions = []metav1.Condition{{
					Type:               conditionReady,
					Status:             metav1.ConditionTrue,
					ObservedGeneration: source.Generation,
					Reason:             ruleSourceReadyReasonValidated,
					LastTransitionTime: metav1.Now(),
				}}
				require.NoError(t, k8sClient.Status().Update(fixture.ctx, &source))
			},
		},
		{
			name: "RuleSet annotation-only update",
			update: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				var ruleSet wafv1alpha1.RuleSet
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.ruleSet), &ruleSet))
				generation, resourceVersion := ruleSet.Generation, ruleSet.ResourceVersion
				ruleSet.Annotations = map[string]string{wafv1alpha1.AnnotationSkipUnsupportedRulesCheck: "true"}
				require.NoError(t, k8sClient.Update(fixture.ctx, &ruleSet))
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&ruleSet), &ruleSet))
				require.Equal(t, generation, ruleSet.Generation)
				require.NotEqual(t, resourceVersion, ruleSet.ResourceVersion)
			},
		},
		{
			name: "RuleSet status-only update",
			update: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				var ruleSet wafv1alpha1.RuleSet
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.ruleSet), &ruleSet))
				generation, resourceVersion := ruleSet.Generation, ruleSet.ResourceVersion
				apimeta.SetStatusCondition(&ruleSet.Status.Conditions, metav1.Condition{
					Type:               "MetadataProbe",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: ruleSet.Generation,
					Reason:             "MetadataOnly",
					LastTransitionTime: metav1.Now(),
				})
				require.NoError(t, k8sClient.Status().Update(fixture.ctx, &ruleSet))
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&ruleSet), &ruleSet))
				require.Equal(t, generation, ruleSet.Generation)
				require.NotEqual(t, resourceVersion, ruleSet.ResourceVersion)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineNoopFixture(t, false)
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
			var before wafv1alpha1.Engine
			require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))
			require.NotNil(t, before.Status.AcceptedConfiguration)
			test.update(t, fixture)

			// Stripping specs makes a cache miss fail the accepted content check,
			// while a metadata-only update still takes the cached-render path.
			counted := &ruleSetInputTestClient{
				Client:              k8sClient,
				stripRuleSourceSpec: true,
				stripRuleDataSpec:   true,
			}
			fixture.reconciler.Client = counted
			result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)
			require.Equal(t, 2, counted.sourceGets, "metadata-only changes need one input read and the final race check")
			require.Equal(t, 2, counted.dataGets, "metadata-only changes need one input read and the final race check")

			var after wafv1alpha1.Engine
			require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &after))
			require.Equal(t, before.Status.AcceptedConfiguration, after.Status.AcceptedConfiguration)
			require.NotNil(t, after.Status.AcceptedConfiguration)
			require.True(t, isConditionCurrent(after.Status.Conditions, conditionReady, reasonConfigurationAccepted, after.Generation))
		})
	}
}

func TestEngineReconciler_MetadataChangeStillRunsFinalInputRaceCheck(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, request.NamespacedName, &before))
	require.NotNil(t, before.Status.AcceptedConfiguration)
	filterBefore := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	filterVersion := filterBefore.GetAnnotations()["waf.k8s.coraza.io/configuration-version"]

	sourceKey := client.ObjectKeyFromObject(fixture.source)
	var source wafv1alpha1.RuleSource
	require.NoError(t, k8sClient.Get(fixture.ctx, sourceKey, &source))
	generation := source.Generation
	resourceVersion := source.ResourceVersion
	source.Annotations = map[string]string{"example.com/metadata-probe": "changed"}
	require.NoError(t, k8sClient.Update(fixture.ctx, &source))
	require.NoError(t, k8sClient.Get(fixture.ctx, sourceKey, &source))
	require.Equal(t, generation, source.Generation)
	require.NotEqual(t, resourceVersion, source.ResourceVersion)

	mutating := &mutateRuleSourceOnGetClient{
		Client:    k8sClient,
		sourceKey: sourceKey,
		newRules:  source.Spec.Rules + "\nSecRule ARGS \"@contains race\" \"id:987654,phase:2,deny\"",
		mutateAt:  2,
	}
	fixture.reconciler.Client = mutating
	result, err := fixture.reconciler.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Equal(t, 100*time.Millisecond, result.RequeueAfter)
	require.Equal(t, 2, mutating.sourceGets, "the input read succeeds, then the final race check observes the spec edit")

	var after wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, request.NamespacedName, &after))
	require.Equal(t, before.Status.AcceptedConfiguration, after.Status.AcceptedConfiguration,
		"a failed final race check must not replace the last accepted configuration")
	filterAfter := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	require.Equal(t, filterVersion, filterAfter.GetAnnotations()["waf.k8s.coraza.io/configuration-version"])
}

func TestEngineReconciler_MissingInputOnWarmFastPathKeepsAcceptedConfiguration(t *testing.T) {
	for _, test := range []struct {
		name        string
		remove      func(*testing.T, *engineNoopFixture)
		sourceGets  int
		dataGets    int
		missingKind string
	}{
		{
			name: "RuleSource",
			remove: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				require.NoError(t, k8sClient.Delete(fixture.ctx, fixture.source))
			},
			sourceGets:  1,
			missingKind: "RuleSource",
		},
		{
			name: "RuleData",
			remove: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				require.NoError(t, k8sClient.Delete(fixture.ctx, fixture.data))
			},
			sourceGets:  1,
			dataGets:    1,
			missingKind: "RuleData",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineNoopFixture(t, false)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
			var before wafv1alpha1.Engine
			require.NoError(t, k8sClient.Get(fixture.ctx, request.NamespacedName, &before))
			require.NotNil(t, before.Status.AcceptedConfiguration)

			test.remove(t, fixture)
			counted := &ruleSetInputTestClient{Client: k8sClient}
			fixture.reconciler.Client = counted
			_, err := fixture.reconciler.Reconcile(fixture.ctx, request)
			require.Error(t, err)
			require.True(t, apierrors.IsNotFound(err), "a missing input remains a retriable NotFound: %v", err)
			require.ErrorContains(t, err, "get "+test.missingKind)
			require.Equal(t, test.sourceGets, counted.sourceGets)
			require.Equal(t, test.dataGets, counted.dataGets)

			var after wafv1alpha1.Engine
			require.NoError(t, k8sClient.Get(fixture.ctx, request.NamespacedName, &after))
			require.Equal(t, before.Status.AcceptedConfiguration, after.Status.AcceptedConfiguration)
			require.True(t, isConditionCurrent(after.Status.Conditions, conditionDegraded, "RuleSetInputsUnavailable", after.Generation))
		})
	}
}

func TestEngineReconciler_InputReadErrorKeepsAcceptedConfiguration(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))

	fixture.reconciler.Client = &ruleSetInputTestClient{
		Client:          k8sClient,
		failRuleDataGet: errors.New("injected RuleData cache failure"),
	}
	_, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.ErrorContains(t, err, "injected RuleData cache failure")

	var afterFailure wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &afterFailure))
	require.Equal(t, before.Status.AcceptedConfiguration, afterFailure.Status.AcceptedConfiguration)
	require.True(t, isConditionCurrent(afterFailure.Status.Conditions, conditionDegraded, "RuleSetInputsUnavailable", afterFailure.Generation))

	fixture.reconciler.Client = k8sClient
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	var recovered wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &recovered))
	require.True(t, isConditionCurrent(recovered.Status.Conditions, conditionReady, reasonConfigurationAccepted, recovered.Generation))
}

func TestEngineReconciler_CachedAndLiveInputSkewRequeuesBeforeAcceptance(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	var staleSource wafv1alpha1.RuleSource
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &staleSource))
	var currentSource wafv1alpha1.RuleSource
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &currentSource))
	originalRules := currentSource.Spec.Rules
	currentSource.Spec.Rules += "\nSecRule ARGS \"@contains transient\" \"id:4,phase:2,deny\""
	require.NoError(t, k8sClient.Update(fixture.ctx, &currentSource))
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&currentSource), &currentSource))
	currentSource.Spec.Rules = originalRules
	require.NoError(t, k8sClient.Update(fixture.ctx, &currentSource))
	require.Greater(t, currentSource.Generation, staleSource.Generation)

	var engine wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &engine))
	engine.Spec.FailurePolicy = wafv1alpha1.FailurePolicyAllow
	require.NoError(t, k8sClient.Update(fixture.ctx, &engine))

	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))
	skewed := &skewedRuleSourceClient{
		Client:      k8sClient,
		sourceKey:   client.ObjectKeyFromObject(&staleSource),
		staleSource: staleSource.DeepCopy(),
	}
	fixture.reconciler.Client = skewed
	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	require.Equal(t, 100*time.Millisecond, result.RequeueAfter)
	require.Equal(t, 2, skewed.sourceGets, "the final identity check must reject inputs read from a newer object")

	var after wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &after))
	require.Equal(t, before.Status.AcceptedConfiguration, after.Status.AcceptedConfiguration,
		"a cached/live reader skew must not replace the last accepted configuration")
	envoyFilter := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	require.Equal(t, before.Status.AcceptedConfiguration.Version, envoyFilter.GetAnnotations()["waf.k8s.coraza.io/configuration-version"])

	fixture.reconciler.Client = k8sClient
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	var accepted wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &accepted))
	require.NotNil(t, accepted.Status.AcceptedConfiguration)
	require.Equal(t, wafv1alpha1.FailurePolicyAllow, accepted.Status.AcceptedConfiguration.FailurePolicy)
}

func TestEngineReconciler_FailurePolicyChangeCannotReuseContentlessRenderEntry(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))
	require.NotNil(t, before.Status.AcceptedConfiguration)

	var edited wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &edited))
	edited.Spec.FailurePolicy = wafv1alpha1.FailurePolicyAllow
	require.NoError(t, k8sClient.Update(fixture.ctx, &edited))
	contentless := &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	fixture.reconciler.Client = contentless
	_, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.Error(t, err, "a new render key must load and verify content before acceptance")
	require.Equal(t, 1, contentless.sourceGets, "a render-cache key miss reads each input once")
	require.Equal(t, 1, contentless.dataGets)

	var afterFailure wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &afterFailure))
	require.Equal(t, before.Status.AcceptedConfiguration, afterFailure.Status.AcceptedConfiguration)

	fixture.reconciler.Client = k8sClient
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	var accepted wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &accepted))
	require.NotNil(t, accepted.Status.AcceptedConfiguration)
	require.Equal(t, wafv1alpha1.FailurePolicyAllow, accepted.Status.AcceptedConfiguration.FailurePolicy)
}

func TestEngineReconciler_SameContentInputUIDOrGenerationDoesNotUseContentlessFastPath(t *testing.T) {
	for _, test := range []struct {
		name                string
		emptyRulesPolicy    wafv1alpha1.EmptyRulesPolicy
		stripRuleSourceSpec bool
		stripRuleDataSpec   bool
		mutate              func(*testing.T, *engineNoopFixture)
	}{
		{
			name:                "RuleSource recreated",
			emptyRulesPolicy:    wafv1alpha1.EmptyRulesPolicyAllow,
			stripRuleSourceSpec: true,
			mutate: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				old := fixture.source.DeepCopy()
				require.NoError(t, k8sClient.Delete(fixture.ctx, old))
				recreated := utils.NewTestRuleSource(old.Name, old.Namespace, old.Spec.Rules)
				require.NoError(t, k8sClient.Create(fixture.ctx, recreated))
				require.NotEqual(t, old.UID, recreated.UID)
				require.Equal(t, old.Generation, recreated.Generation)
				fixture.source = recreated
			},
		},
		{
			name:                "RuleSource generation changed with same content",
			stripRuleSourceSpec: true,
			mutate: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				var source wafv1alpha1.RuleSource
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &source))
				original := source.Spec.Rules
				source.Spec.Rules += "\nSecRule ARGS \"@contains temp\" \"id:3,phase:2,deny\""
				require.NoError(t, k8sClient.Update(fixture.ctx, &source))
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&source), &source))
				source.Spec.Rules = original
				require.NoError(t, k8sClient.Update(fixture.ctx, &source))
				require.Greater(t, source.Generation, fixture.source.Generation)
			},
		},
		{
			name:              "RuleData recreated",
			stripRuleDataSpec: true,
			mutate: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				old := fixture.data.DeepCopy()
				require.NoError(t, k8sClient.Delete(fixture.ctx, old))
				recreated := utils.NewTestRuleData(old.Name, old.Namespace, old.Spec.Files)
				require.NoError(t, k8sClient.Create(fixture.ctx, recreated))
				require.NotEqual(t, old.UID, recreated.UID)
				require.Equal(t, old.Generation, recreated.Generation)
				fixture.data = recreated
			},
		},
		{
			name:              "RuleData generation changed with same content",
			stripRuleDataSpec: true,
			mutate: func(t *testing.T, fixture *engineNoopFixture) {
				t.Helper()
				var data wafv1alpha1.RuleData
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.data), &data))
				original := data.Spec.Files["some.data"]
				data.Spec.Files["some.data"] = "temporary\n"
				require.NoError(t, k8sClient.Update(fixture.ctx, &data))
				require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&data), &data))
				data.Spec.Files["some.data"] = original
				require.NoError(t, k8sClient.Update(fixture.ctx, &data))
				require.Greater(t, data.Generation, fixture.data.Generation)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineNoopFixtureWithEmptyRulesPolicy(t, false, test.emptyRulesPolicy)
			var before wafv1alpha1.Engine
			require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.engine), &before))
			test.mutate(t, fixture)

			contentless := &ruleSetInputTestClient{
				Client:              k8sClient,
				stripRuleSourceSpec: test.stripRuleSourceSpec,
				stripRuleDataSpec:   test.stripRuleDataSpec,
			}
			fixture.reconciler.Client = contentless
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
			result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
			require.Equal(t, 1, contentless.sourceGets, "an identity miss must use the already-read input")
			require.Equal(t, 1, contentless.dataGets, "an identity miss must use the already-read input")
			if test.emptyRulesPolicy == wafv1alpha1.EmptyRulesPolicyAllow {
				require.NoError(t, err)
				require.Equal(t, ruleSetAcceptanceRetryAfter, result.RequeueAfter)
			} else {
				require.Error(t, err, "changed input identity must not reuse the old contentless cache entry")
			}

			var afterFailure wafv1alpha1.Engine
			require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &afterFailure))
			require.Equal(t, before.Status.AcceptedConfiguration, afterFailure.Status.AcceptedConfiguration,
				"unverified new input must not replace the last accepted configuration")

			fixture.reconciler.Client = k8sClient
			result, err = fixture.reconciler.Reconcile(fixture.ctx, req)
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)
			var recovered wafv1alpha1.Engine
			require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &recovered))
			require.True(t, isConditionCurrent(recovered.Status.Conditions, conditionReady, reasonConfigurationAccepted, recovered.Generation))
		})
	}
}

func TestEngineReconciler_ForeignOwnedRevisionDoesNotApplyRepeatedly(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	counts := useEngineOperationClient(t, fixture)
	directClient := k8sClient.(*indexedTestClient).Client
	filterKey := types.NamespacedName{Namespace: fixture.engine.Namespace, Name: envoyFilterName(fixture.engine.Name)}
	const foreignManager = "foreign-revision-controller"

	applyEnvoyFilterRevision(t, fixture.ctx, directClient, filterKey, foreignManager, "foreign-revision")

	existing := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	assert.Equal(t, "foreign-revision", existing.GetLabels()["istio.io/rev"])
	require.True(t, managedFieldOwnsRevision(t, existing, foreignManager),
		"the API server must record the foreign SSA field owner")
	assert.False(t, managedFieldOwnsRevision(t, existing, fieldManager))

	counts.reset()
	_, err := fixture.reconciler.Reconcile(fixture.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)})
	require.NoError(t, err)
	firstApplyCount := counts.apiPatches
	existing = requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	assert.Equal(t, "foreign-revision", existing.GetLabels()["istio.io/rev"], "SSA without the revision must retain a foreign-owned label")
	assert.True(t, managedFieldOwnsRevision(t, existing, foreignManager))
	assert.False(t, managedFieldOwnsRevision(t, existing, fieldManager))

	counts.reset()
	_, err = fixture.reconciler.Reconcile(fixture.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)})
	require.NoError(t, err)
	secondApplyCount := counts.apiPatches
	assert.Zero(t, firstApplyCount, "an omitted foreign-owned revision cannot be removed by the operator's SSA")
	assert.Zero(t, secondApplyCount, "the unchanged foreign-owned revision must not cause another apply")
}

func TestEngineReconciler_OperatorOwnedRevisionIsRemoved(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	counts := useEngineOperationClient(t, fixture)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	fixture.reconciler.istioRevision = "operator-revision"
	counts.reset()
	_, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	assert.EqualValues(t, 1, counts.apiPatches)

	existing := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	require.Equal(t, "operator-revision", existing.GetLabels()[istioRevisionLabel])
	require.True(t, managedFieldOwnsRevision(t, existing, fieldManager))

	fixture.reconciler.istioRevision = ""
	counts.reset()
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	assert.EqualValues(t, 1, counts.apiPatches, "the operator must apply once to remove its revision")

	existing = requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	assert.NotContains(t, existing.GetLabels(), istioRevisionLabel)
	assert.False(t, managedFieldOwnsRevision(t, existing, fieldManager))

	counts.reset()
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	assert.Zero(t, counts.apiPatches)
}

func TestEngineReconciler_SharedRevisionOwnershipIsReleased(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	counts := useEngineOperationClient(t, fixture)
	directClient := k8sClient.(*indexedTestClient).Client
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	filterKey := types.NamespacedName{Namespace: fixture.engine.Namespace, Name: envoyFilterName(fixture.engine.Name)}
	const foreignManager = "foreign-revision-controller"

	fixture.reconciler.istioRevision = "shared-revision"
	counts.reset()
	_, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	assert.EqualValues(t, 1, counts.apiPatches)

	applyEnvoyFilterRevision(t, fixture.ctx, directClient, filterKey, foreignManager, "shared-revision")
	existing := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	require.True(t, managedFieldOwnsRevision(t, existing, fieldManager))
	require.True(t, managedFieldOwnsRevision(t, existing, foreignManager))

	fixture.reconciler.istioRevision = ""
	counts.reset()
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	assert.EqualValues(t, 1, counts.apiPatches, "the operator must apply once to relinquish its shared field")

	existing = requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	assert.Equal(t, "shared-revision", existing.GetLabels()[istioRevisionLabel], "the foreign owner must retain the shared label")
	assert.False(t, managedFieldOwnsRevision(t, existing, fieldManager))
	assert.True(t, managedFieldOwnsRevision(t, existing, foreignManager))

	counts.reset()
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	assert.Zero(t, counts.apiPatches)
}

func applyEnvoyFilterRevision(
	t testing.TB,
	ctx context.Context,
	c client.Client,
	key types.NamespacedName,
	manager, revision string,
) {
	t.Helper()
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(envoyFilterGVK)
	desired.SetName(key.Name)
	desired.SetNamespace(key.Namespace)
	desired.SetLabels(map[string]string{istioRevisionLabel: revision})
	require.NoError(t, c.Patch(ctx, desired, client.Apply, client.FieldOwner(manager)))
}

func managedFieldOwnsRevision(t testing.TB, object *unstructured.Unstructured, manager string) bool {
	t.Helper()
	for _, entry := range object.GetManagedFields() {
		if entry.Manager != manager || entry.Operation != metav1.ManagedFieldsOperationApply || entry.Subresource != "" {
			continue
		}
		require.Equal(t, "FieldsV1", entry.FieldsType)
		require.NotNil(t, entry.FieldsV1)

		var objectFields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(entry.FieldsV1.Raw, &objectFields))
		metadataRaw, found := objectFields["f:metadata"]
		if !found {
			continue
		}
		var metadataFields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(metadataRaw, &metadataFields))
		labelsRaw, found := metadataFields["f:labels"]
		if !found {
			continue
		}
		var labelFields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(labelsRaw, &labelFields))
		if _, found := labelFields["f:"+istioRevisionLabel]; found {
			return true
		}
	}
	return false
}

func TestWaitForEngineReadyWaitsForStatus(t *testing.T) {
	engine := &wafv1alpha1.Engine{ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: "test", Generation: 1}}
	ready := engine.DeepCopy()
	ready.Status = &wafv1alpha1.EngineStatus{
		AcceptedConfiguration: &wafv1alpha1.EngineAcceptedConfiguration{},
		Conditions: []metav1.Condition{{
			Type: conditionReady, Status: metav1.ConditionTrue,
			Reason: reasonConfigurationAccepted, ObservedGeneration: 1,
		}},
	}
	reader := &delayedReadyReader{ready: ready}

	waitForEngineReady(t, reader, engine)
	require.Equal(t, 2, reader.reads)
}

type delayedReadyReader struct {
	client.Reader
	ready *wafv1alpha1.Engine
	reads int
}

func (r *delayedReadyReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	r.reads++
	if r.reads > 1 {
		*obj.(*wafv1alpha1.Engine) = *r.ready.DeepCopy()
	}
	return nil
}

func TestEngineReconciler_CachedRenderRepairsNetworkPolicyEnvoyFilterTelemetryAndCA(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	counts := useEngineOperationClient(t, fixture)
	directClient := k8sClient.(*indexedTestClient).Client
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	policy := engineNetworkPolicyFor(t, fixture.ctx, fixture.reconciler.operatorNamespace, fixture.engine)
	port := intstr.FromInt(1234)
	policy.Spec.Ingress[0].Ports[0].Port = &port
	require.NoError(t, directClient.Update(fixture.ctx, policy))

	envoyFilter := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	annotations := envoyFilter.GetAnnotations()
	annotations["waf.k8s.coraza.io/ca-hash"] = "sha256:stale"
	envoyFilter.SetAnnotations(annotations)
	targetRefs, found, err := unstructured.NestedSlice(envoyFilter.Object, "spec", "targetRefs")
	require.NoError(t, err)
	require.True(t, found)
	targetRefs[0].(map[string]any)["name"] = "wrong-gateway"
	require.NoError(t, unstructured.SetNestedSlice(envoyFilter.Object, targetRefs, "spec", "targetRefs"))
	require.NoError(t, directClient.Update(fixture.ctx, envoyFilter))

	telemetry := buildTelemetry(fixture.engine, wafLogCollectorProvider)
	require.NoError(t, directClient.Create(fixture.ctx, telemetry))

	cacheReader := k8sClient.(*indexedTestClient).reader
	require.Eventually(t, func() bool {
		var policies networkingv1.NetworkPolicyList
		if err := cacheReader.List(fixture.ctx, &policies,
			client.InNamespace(fixture.reconciler.operatorNamespace),
			engineNetworkPolicyLabels(fixture.engine.Namespace, fixture.engine.Name),
		); err != nil || len(policies.Items) != 1 {
			return false
		}
		return policies.Items[0].Spec.Ingress[0].Ports[0].Port.IntVal == 1234
	}, 5*time.Second, 10*time.Millisecond)

	rotatedCA := []byte("rotated test CA")
	fixture.reconciler.xdsConfig.CAPEM = rotatedCA
	counts.reset()
	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)

	assert.EqualValues(t, 1, counts.apiDeletes)
	assert.EqualValues(t, 1, counts.apiGets)
	assert.EqualValues(t, 1, counts.apiUpdates, "NetworkPolicy spec drift must be repaired")
	assert.EqualValues(t, 1, counts.apiPatches, "EnvoyFilter target and CA drift must be repaired")

	policy = engineNetworkPolicyFor(t, fixture.ctx, fixture.reconciler.operatorNamespace, fixture.engine)
	assert.Equal(t, int32(DefaultXDSListenPort), policy.Spec.Ingress[0].Ports[0].Port.IntVal)

	envoyFilter = requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	assert.Equal(t, caHash(rotatedCA), envoyFilter.GetAnnotations()["waf.k8s.coraza.io/ca-hash"])
	targetRefs, found, err = unstructured.NestedSlice(envoyFilter.Object, "spec", "targetRefs")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, fixture.gateway.Name, targetRefs[0].(map[string]any)["name"])

	var deletedTelemetry unstructured.Unstructured
	deletedTelemetry.SetGroupVersionKind(telemetryGVK)
	err = directClient.Get(fixture.ctx, types.NamespacedName{Namespace: telemetry.GetNamespace(), Name: telemetry.GetName()}, &deletedTelemetry)
	assert.True(t, apierrors.IsNotFound(err), "disabled observability must continue deleting externally recreated Telemetry")
}

func TestEngineReconciler_CachedRenderRepairsEnabledTelemetryAndWritesStatus(t *testing.T) {
	fixture := newEngineNoopFixture(t, true)
	counts := useEngineOperationClient(t, fixture)
	directClient := k8sClient.(*indexedTestClient).Client
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	telemetry := &unstructured.Unstructured{}
	telemetry.SetGroupVersionKind(telemetryGVK)
	key := types.NamespacedName{Namespace: fixture.engine.Namespace, Name: telemetryName(fixture.engine.Name)}
	require.NoError(t, directClient.Get(fixture.ctx, key, telemetry))
	accessLogging, found, err := unstructured.NestedSlice(telemetry.Object, "spec", "accessLogging")
	require.NoError(t, err)
	require.True(t, found)
	providers := accessLogging[0].(map[string]any)["providers"].([]any)
	providers[0].(map[string]any)["name"] = "wrong-provider"
	require.NoError(t, unstructured.SetNestedSlice(telemetry.Object, accessLogging, "spec", "accessLogging"))
	require.NoError(t, directClient.Update(fixture.ctx, telemetry))

	counts.reset()
	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)

	assert.EqualValues(t, 1, counts.apiGets, "the EnvoyFilter drift check remains live")
	assert.EqualValues(t, 1, counts.apiPatches, "Telemetry drift must still be repaired")
	assert.Zero(t, counts.apiUpdates)
	assert.Zero(t, counts.apiDeletes)
	assert.EqualValues(t, 1, counts.statusPatches, "ObservabilityReady recovery/update must not be skipped")

	require.NoError(t, directClient.Get(fixture.ctx, key, telemetry))
	accessLogging, found, err = unstructured.NestedSlice(telemetry.Object, "spec", "accessLogging")
	require.NoError(t, err)
	require.True(t, found)
	providers = accessLogging[0].(map[string]any)["providers"].([]any)
	assert.Equal(t, wafLogCollectorProvider, providers[0].(map[string]any)["name"])

	var after wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &after))
	assert.True(t, isConditionCurrent(after.Status.Conditions, conditionObservabilityReady, "TelemetryReady", after.Generation))
}

func TestEngineReconciler_DisabledTelemetryCleanupPrecedesDegradedReturn(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	counts := useEngineOperationClient(t, fixture)
	directClient := k8sClient.(*indexedTestClient).Client
	cacheReader := k8sClient.(*indexedTestClient).reader
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	telemetry := buildTelemetry(fixture.engine, wafLogCollectorProvider)
	require.NoError(t, directClient.Create(fixture.ctx, telemetry))

	var ruleSet wafv1alpha1.RuleSet
	require.NoError(t, directClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.ruleSet), &ruleSet))
	patch := client.MergeFrom(ruleSet.DeepCopy())
	apimeta.SetStatusCondition(&ruleSet.Status.Conditions, metav1.Condition{
		Type:               conditionDegraded,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ruleSet.Generation,
		Reason:             "Injected",
		Message:            "test early return",
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(t, directClient.Status().Patch(fixture.ctx, &ruleSet, patch))
	waitForCachedObject(t, cacheReader, client.ObjectKeyFromObject(fixture.ruleSet), &wafv1alpha1.RuleSet{}, func(obj client.Object) bool {
		return isConditionCurrent(obj.(*wafv1alpha1.RuleSet).Status.Conditions, conditionDegraded, "Injected", ruleSet.Generation)
	})

	counts.reset()
	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
	assert.EqualValues(t, 1, counts.apiDeletes, "the early cleanup must still run before the degraded RuleSet return")
	assert.EqualValues(t, 1, counts.statusPatches, "the degraded Engine status must still be recorded")

	var deletedTelemetry unstructured.Unstructured
	deletedTelemetry.SetGroupVersionKind(telemetryGVK)
	err = directClient.Get(fixture.ctx, types.NamespacedName{Namespace: telemetry.GetNamespace(), Name: telemetry.GetName()}, &deletedTelemetry)
	assert.True(t, apierrors.IsNotFound(err))

	var after wafv1alpha1.Engine
	require.NoError(t, directClient.Get(fixture.ctx, req.NamespacedName, &after))
	assert.True(t, isConditionCurrent(after.Status.Conditions, conditionDegraded, "RuleSetDegraded", after.Generation))
}

func TestEngineReconciler_DisabledTelemetryCleanupErrorIsPreserved(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	directClient := k8sClient.(*indexedTestClient).Client
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	telemetry := buildTelemetry(fixture.engine, wafLogCollectorProvider)
	require.NoError(t, directClient.Create(fixture.ctx, telemetry))

	var before wafv1alpha1.Engine
	require.NoError(t, directClient.Get(fixture.ctx, req.NamespacedName, &before))
	failingClient := &telemetryDeleteErrorClient{
		Client: k8sClient,
		err:    errors.New("injected Telemetry delete failure"),
	}
	fixture.reconciler.Client = failingClient
	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.ErrorContains(t, err, "injected Telemetry delete failure")
	require.Zero(t, result.RequeueAfter)
	assert.Equal(t, 1, failingClient.deletes)

	var after wafv1alpha1.Engine
	require.NoError(t, directClient.Get(fixture.ctx, req.NamespacedName, &after))
	assert.Equal(t, before.Status, after.Status, "a cleanup error must not change Engine status")

	var retainedTelemetry unstructured.Unstructured
	retainedTelemetry.SetGroupVersionKind(telemetryGVK)
	require.NoError(t, directClient.Get(fixture.ctx, types.NamespacedName{Namespace: telemetry.GetNamespace(), Name: telemetry.GetName()}, &retainedTelemetry))
}

func TestEngineReconciler_StatusRecoversAfterDelayedEngineCacheRead(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	var healthy wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &healthy))
	require.NotNil(t, healthy.Status.AcceptedConfiguration)

	var current wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &current))
	patch := client.MergeFrom(current.DeepCopy())
	applyStatusConditionDegraded(&current.Status.Conditions, current.Generation, "Injected", "simulated status update")
	require.NoError(t, k8sClient.Status().Patch(fixture.ctx, &current, patch))

	delayed := &delayedEngineGetClient{
		Client:      k8sClient,
		engineKey:   req.NamespacedName,
		staleEngine: healthy.DeepCopy(),
		stale:       true,
	}
	fixture.reconciler.Client = delayed
	_, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)

	var afterStaleRead wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &afterStaleRead))
	require.NotNil(t, apimeta.FindStatusCondition(afterStaleRead.Status.Conditions, conditionDegraded))

	delayed.stale = false
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)

	var recovered wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &recovered))
	assert.Nil(t, apimeta.FindStatusCondition(recovered.Status.Conditions, conditionDegraded))
	assert.True(t, isConditionCurrent(recovered.Status.Conditions, conditionReady, reasonConfigurationAccepted, recovered.Generation))
	assert.Equal(t, healthy.Status.AcceptedConfiguration, recovered.Status.AcceptedConfiguration)
}

func TestEngineReconciler_CachedRenderRechecksCurrentDeliveryLimits(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	engineKey := types.NamespacedName{Namespace: fixture.engine.Namespace, Name: fixture.engine.Name}

	fixture.reconciler.renderCache.mu.Lock()
	entry, found := fixture.reconciler.renderCache.entries[engineKey]
	fixture.reconciler.renderCache.mu.Unlock()
	require.True(t, found)
	require.Greater(t, entry.resourceSize, 0)

	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))
	require.NotNil(t, before.Status.AcceptedConfiguration)
	acceptedBefore := *before.Status.AcceptedConfiguration

	limit := entry.resourceSize + messageEnvelopeBytes - 1
	require.Greater(t, limit, len(testAcceptRuleText+"\n"+testAcceptDataRuleText))
	fixture.reconciler.Limits = xds.Limits{MaxMessageBytes: limit}
	_, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ECDS response size")

	var rejected wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &rejected))
	require.NotNil(t, rejected.Status.AcceptedConfiguration)
	assert.Equal(t, acceptedBefore, *rejected.Status.AcceptedConfiguration)
	assert.True(t, isConditionCurrent(rejected.Status.Conditions, conditionDegraded, "RenderFailed", rejected.Generation))

	fixture.reconciler.Limits = xds.Limits{}
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)

	var recovered wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &recovered))
	assert.True(t, isConditionCurrent(recovered.Status.Conditions, conditionReady, reasonConfigurationAccepted, recovered.Generation))
	assert.Nil(t, apimeta.FindStatusCondition(recovered.Status.Conditions, conditionDegraded))
}

func TestEngineReconciler_RenderCacheRevalidatesChangesAndClearsOnDelete(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	var before wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &before))
	require.NotNil(t, before.Status.AcceptedConfiguration)
	oldVersion := before.Status.AcceptedConfiguration.Version
	oldHash := before.Status.AcceptedConfiguration.RuleSetContentHash

	newRules := testAcceptRuleText + "\nSecRule ARGS \"@contains changed\" \"id:3,phase:2,deny,status:403\""
	mutatingClient := &mutateRuleSourceOnGetClient{
		Client:    k8sClient,
		sourceKey: client.ObjectKeyFromObject(fixture.source),
		newRules:  newRules,
		mutateAt:  2,
	}
	fixture.reconciler.Client = mutatingClient
	result, err := fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	assert.Equal(t, 100*time.Millisecond, result.RequeueAfter, "a source edit during verification must still requeue before applying")
	assert.Equal(t, 2, mutatingClient.sourceGets)

	var afterChangedRead wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &afterChangedRead))
	require.NotNil(t, afterChangedRead.Status.AcceptedConfiguration)
	assert.Equal(t, oldVersion, afterChangedRead.Status.AcceptedConfiguration.Version)
	assert.Equal(t, oldHash, afterChangedRead.Status.AcceptedConfiguration.RuleSetContentHash)
	envoyFilter := requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	assert.Equal(t, oldVersion, envoyFilter.GetAnnotations()["waf.k8s.coraza.io/configuration-version"])

	acceptTestRuleSet(t, fixture.ctx, fixture.ruleSet, newRules)
	fixture.reconciler.Client = k8sClient
	_, err = fixture.reconciler.Reconcile(fixture.ctx, req)
	require.NoError(t, err)

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &updated))
	require.NotNil(t, updated.Status.AcceptedConfiguration)
	assert.NotEqual(t, oldVersion, updated.Status.AcceptedConfiguration.Version)
	assert.NotEqual(t, oldHash, updated.Status.AcceptedConfiguration.RuleSetContentHash)
	envoyFilter = requireEnvoyFilterExists(t, fixture.ctx, fixture.engine.Name, fixture.engine.Namespace)
	assert.Equal(t, updated.Status.AcceptedConfiguration.Version, envoyFilter.GetAnnotations()["waf.k8s.coraza.io/configuration-version"])

	publication := newPublicationReconciler(t)
	_, err = publication.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	snapshot, found := publication.Store.Get(req.NamespacedName.String())
	require.True(t, found)
	assert.Equal(t, updated.Status.AcceptedConfiguration.Version, snapshot.Version)
	assert.Equal(t, updated.Status.AcceptedConfiguration.RuleSetContentHash, snapshot.RuleSetHash)

	restarted := newAcceptedEngineTestReconciler()
	_, err = restarted.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	var afterRestart wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(fixture.ctx, req.NamespacedName, &afterRestart))
	require.NotNil(t, afterRestart.Status.AcceptedConfiguration)
	assert.Equal(t, updated.Status.AcceptedConfiguration, afterRestart.Status.AcceptedConfiguration)

	require.NoError(t, k8sClient.Delete(fixture.ctx, fixture.engine))
	_, err = restarted.Reconcile(fixture.ctx, req)
	require.NoError(t, err)
	restarted.renderCache.mu.Lock()
	_, cached := restarted.renderCache.entries[req.NamespacedName]
	restarted.renderCache.mu.Unlock()
	assert.False(t, cached, "deletion must release the Engine's render metadata")

	missingKey := types.NamespacedName{Namespace: fixture.engine.Namespace, Name: "missing-engine"}
	restarted.renderCache.set(missingKey, engineRenderCacheEntry{})
	restarted.Metrics, _ = newTestMetrics(t)
	_, err = restarted.Reconcile(fixture.ctx, ctrl.Request{NamespacedName: missingKey})
	require.NoError(t, err)
	_, cached = restarted.renderCache.get(missingKey, engineRenderCacheKey{})
	assert.False(t, cached, "a NotFound reconcile must release the Engine's render metadata")
}

func newEngineNoopFixture(t *testing.T, observability bool) *engineNoopFixture {
	return newEngineNoopFixtureWithEmptyRulesPolicy(t, observability, "")
}

func newEngineNoopFixtureWithEmptyRulesPolicy(
	t *testing.T,
	observability bool,
	emptyRulesPolicy wafv1alpha1.EmptyRulesPolicy,
) *engineNoopFixture {
	t.Helper()
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)
	gatewayName := "noop-gateway"
	gateway := createTestGateway(t, ctx, k8sClient, gatewayName, namespace)

	var gatewayClass *gatewayv1.GatewayClass
	if observability {
		gatewayClass = &gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: "noop-" + uuid.NewString()[:8],
				Annotations: map[string]string{
					wafCollectorAnnotation: "collector.example.svc:4317",
				},
			},
			Spec: gatewayv1.GatewayClassSpec{ControllerName: "example.com/gateway-controller"},
		}
		require.NoError(t, k8sClient.Create(ctx, gatewayClass))
		gateway.Spec.GatewayClassName = gatewayv1.ObjectName(gatewayClass.Name)
		require.NoError(t, k8sClient.Update(ctx, gateway))
		t.Cleanup(func() {
			if err := k8sClient.Delete(ctx, gatewayClass); err != nil && !apierrors.IsNotFound(err) {
				t.Logf("delete test GatewayClass: %v", err)
			}
		})
	}

	source := utils.NewTestRuleSource("noop-source", namespace, testAcceptRuleText+"\n"+testAcceptDataRuleText)
	data := utils.NewTestRuleData("noop-data", namespace, map[string]string{"some.data": "evildata\n"})
	require.NoError(t, k8sClient.Create(ctx, source))
	require.NoError(t, k8sClient.Create(ctx, data))

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "noop-ruleset",
		Namespace: namespace,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
		Data:      []wafv1alpha1.DataReference{{Name: data.Name}},
	})
	ruleSet.Spec.EmptyRulesPolicy = emptyRulesPolicy
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	acceptTestRuleSet(t, ctx, ruleSet, testAcceptRuleText+"\n"+testAcceptDataRuleText)

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "noop-engine",
		Namespace:   namespace,
		GatewayName: gatewayName,
		RuleSetName: ruleSet.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	if observability {
		engine.Spec.Observability.Mode = wafv1alpha1.ObservabilityModeEnabled
	}
	require.NoError(t, k8sClient.Create(ctx, engine))

	reconciler := newAcceptedEngineTestReconciler()
	reconcileEngine(t, ctx, reconciler, engine)
	fixture := &engineNoopFixture{
		ctx:          ctx,
		engine:       engine,
		ruleSet:      ruleSet,
		source:       source,
		data:         data,
		gateway:      gateway,
		gatewayClass: gatewayClass,
		reconciler:   reconciler,
	}
	t.Cleanup(func() {
		req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
		reconciler.Client = k8sClient
		var current wafv1alpha1.Engine
		if err := k8sClient.Get(ctx, req.NamespacedName, &current); err == nil {
			if err := k8sClient.Delete(ctx, &current); err == nil {
				if _, err := reconciler.Reconcile(ctx, req); err != nil {
					t.Logf("cleanup Engine finalizer: %v", err)
				}
			}
		}
		filter := &unstructured.Unstructured{}
		filter.SetGroupVersionKind(envoyFilterGVK)
		filter.SetNamespace(engine.Namespace)
		filter.SetName(envoyFilterName(engine.Name))
		if err := k8sClient.Delete(ctx, filter); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup EnvoyFilter: %v", err)
		}
		telemetry := &unstructured.Unstructured{}
		telemetry.SetGroupVersionKind(telemetryGVK)
		telemetry.SetNamespace(engine.Namespace)
		telemetry.SetName(telemetryName(engine.Name))
		if err := k8sClient.Delete(ctx, telemetry); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup Telemetry: %v", err)
		}
	})
	return fixture
}

type engineNoopFixture struct {
	ctx          context.Context
	engine       *wafv1alpha1.Engine
	ruleSet      *wafv1alpha1.RuleSet
	source       *wafv1alpha1.RuleSource
	data         *wafv1alpha1.RuleData
	gateway      *gatewayv1.Gateway
	gatewayClass *gatewayv1.GatewayClass
	reconciler   *EngineReconciler
}

func useEngineOperationClient(t *testing.T, fixture *engineNoopFixture) *engineClientOperationCounts {
	t.Helper()
	indexed := k8sClient.(*indexedTestClient)
	counts := &engineClientOperationCounts{}
	fixture.reconciler.Client = &engineBenchmarkClient{Client: k8sClient, cache: indexed.reader, counts: counts}
	waitForEngineInputsCache(t, indexed.reader, fixture.engine, fixture.ruleSet,
		[]wafv1alpha1.SourceReference{{Name: fixture.source.Name}},
		fixture.data, fixture.ruleSet.Status.AcceptedContent.ContentHash)
	waitForCachedObject(t, indexed.reader, client.ObjectKeyFromObject(fixture.gateway), &gatewayv1.Gateway{})
	if fixture.gatewayClass != nil {
		waitForCachedObject(t, indexed.reader, client.ObjectKeyFromObject(fixture.gatewayClass), &gatewayv1.GatewayClass{})
	}
	waitForEngineReady(t, indexed.reader, fixture.engine)
	return counts
}

func engineNetworkPolicyFor(
	t testing.TB,
	ctx context.Context,
	operatorNamespace string,
	engine *wafv1alpha1.Engine,
) *networkingv1.NetworkPolicy {
	t.Helper()
	directClient := k8sClient.(*indexedTestClient).Client
	var policies networkingv1.NetworkPolicyList
	require.NoError(t, directClient.List(ctx, &policies,
		client.InNamespace(operatorNamespace),
		engineNetworkPolicyLabels(engine.Namespace, engine.Name),
	))
	require.Len(t, policies.Items, 1)
	return policies.Items[0].DeepCopy()
}

type delayedEngineGetClient struct {
	client.Client
	engineKey   types.NamespacedName
	staleEngine *wafv1alpha1.Engine
	stale       bool
}

func (c *delayedEngineGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.stale && key == c.engineKey {
		*obj.(*wafv1alpha1.Engine) = *c.staleEngine.DeepCopy()
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type mutateRuleSourceOnGetClient struct {
	client.Client
	sourceKey  types.NamespacedName
	newRules   string
	mutateAt   int
	sourceGets int
}

type ruleSetInputTestClient struct {
	client.Client
	stripRuleSourceSpec bool
	stripRuleDataSpec   bool
	failRuleSourceGet   error
	failRuleDataGet     error
	sourceGets          int
	dataGets            int
}

// skewedRuleSourceClient serves a stale cache view around one live content read.
type skewedRuleSourceClient struct {
	client.Client
	sourceKey   types.NamespacedName
	staleSource *wafv1alpha1.RuleSource
	sourceGets  int
}

func (c *skewedRuleSourceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == c.sourceKey {
		c.sourceGets++
		if c.sourceGets == 1 || c.sourceGets == 3 {
			*obj.(*wafv1alpha1.RuleSource) = *c.staleSource.DeepCopy()
			return nil
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *ruleSetInputTestClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch obj.(type) {
	case *wafv1alpha1.RuleSource:
		c.sourceGets++
		if c.failRuleSourceGet != nil {
			return c.failRuleSourceGet
		}
	case *wafv1alpha1.RuleData:
		c.dataGets++
		if c.failRuleDataGet != nil {
			return c.failRuleDataGet
		}
	}
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if source, ok := obj.(*wafv1alpha1.RuleSource); ok && c.stripRuleSourceSpec {
		source.Spec = wafv1alpha1.RuleSourceSpec{}
	}
	if data, ok := obj.(*wafv1alpha1.RuleData); ok && c.stripRuleDataSpec {
		data.Spec = wafv1alpha1.RuleDataSpec{}
	}
	return nil
}

type telemetryDeleteErrorClient struct {
	client.Client
	err     error
	deletes int
}

func (c *telemetryDeleteErrorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if telemetry, ok := obj.(*unstructured.Unstructured); ok && telemetry.GetKind() == telemetryGVK.Kind {
		c.deletes++
		return c.err
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *mutateRuleSourceOnGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == c.sourceKey {
		c.sourceGets++
		if c.sourceGets == c.mutateAt {
			var source wafv1alpha1.RuleSource
			if err := c.Client.Get(ctx, key, &source); err != nil {
				return err
			}
			source.Spec.Rules = c.newRules
			if err := c.Client.Update(ctx, &source); err != nil {
				return err
			}
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
