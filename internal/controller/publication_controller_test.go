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
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

const publicationRules = "SecRuleEngine On\nSecRule REQUEST_URI \"@contains attack\" \"id:1001,phase:1,deny,status:403\""

func newPublicationReconciler(t *testing.T) *PublicationReconciler {
	t.Helper()
	reconciler, _ := newPublicationReconcilerWithMetrics(t, nil)
	return reconciler
}

func newPublicationReconcilerWithMetrics(t *testing.T, revoke xds.RevokeFunc) (*PublicationReconciler, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics, err := xds.NewMetrics(registry)
	require.NoError(t, err)
	// Reconcile awaits startup initialization; a unit-test reconciler starts
	// with the startup list already applied (empty).
	readiness := xds.NewPublicationReadiness()
	readiness.Initialize(nil)
	return &PublicationReconciler{
		Client:    k8sClient,
		APIReader: k8sClient,
		Scheme:    scheme,
		Store:     xds.NewSnapshotStore(),
		Authz:     xds.NewAuthzIndex(k8sClient, k8sClient, 5*time.Minute, revoke),
		Readiness: readiness,
		Metrics:   metrics,
		Limits:    xds.DefaultLimits(),
	}, registry
}

// gaugeValue reads a gauge registered on a private test registry.
func gaugeValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == name {
			require.Len(t, family.GetMetric(), 1, "gauge %s", name)
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %s not registered", name)
	return 0
}

// TestPublicationLagGaugeAgesWithoutReconcile pins scrape-time aging: a stuck
// Engine returns no requeue, so the gauge must keep growing on its own.
func TestPublicationLagGaugeAgesWithoutReconcile(t *testing.T) {
	reconciler, registry := newPublicationReconcilerWithMetrics(t, nil)
	reconciler.Metrics.SetPublicationLagSource(reconciler.oldestMismatchSeconds)

	reconciler.mu.Lock()
	reconciler.states = map[types.NamespacedName]*publicationState{
		{Namespace: "shop", Name: "checkout"}: {mismatchSince: time.Now().Add(-2 * time.Minute)},
	}
	reconciler.mu.Unlock()

	first := gaugeValue(t, registry, "coraza_xds_publication_lag_seconds")
	require.GreaterOrEqual(t, first, 120.0)

	time.Sleep(20 * time.Millisecond)
	second := gaugeValue(t, registry, "coraza_xds_publication_lag_seconds")
	require.Greater(t, second, first, "the gauge must age between scrapes")
}

// counterValue reads a counter registered on a private test registry.
func counterValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == name {
			require.Len(t, family.GetMetric(), 1, "counter %s", name)
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("counter %q is not registered", name)
	return 0
}

func createPublicationRuleSet(t *testing.T, ctx context.Context, namespace, name string) *wafv1alpha1.RuleSet {
	t.Helper()
	ruleSet := &wafv1alpha1.RuleSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: wafv1alpha1.RuleSetSpec{
			Sources: []wafv1alpha1.SourceReference{{Name: name + "-source"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, ruleSet); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("failed to delete RuleSet: %v", err)
		}
	})
	return ruleSet
}

func createPublicationRuleSource(t *testing.T, ctx context.Context, namespace, name, rules string) *wafv1alpha1.RuleSource {
	t.Helper()
	source := &wafv1alpha1.RuleSource{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       wafv1alpha1.RuleSourceSpec{Rules: rules},
	}
	require.NoError(t, k8sClient.Create(ctx, source))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, source); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("failed to delete RuleSource: %v", err)
		}
	})
	return source
}

func createPublicationEngine(t *testing.T, ctx context.Context, namespace, name, ruleSetName, gatewayName string) *wafv1alpha1.Engine {
	t.Helper()
	engine := &wafv1alpha1.Engine{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: wafv1alpha1.EngineSpec{
			RuleSet: wafv1alpha1.RuleSetReference{Name: ruleSetName},
			Target: wafv1alpha1.EngineTarget{
				Type: wafv1alpha1.EngineTargetTypeGateway,
				Name: gatewayName,
			},
			FailurePolicy: wafv1alpha1.FailurePolicyFail,
		},
	}
	require.NoError(t, k8sClient.Create(ctx, engine))
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, engine); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("failed to delete Engine: %v", err)
		}
	})
	return engine
}

// acceptedConfigurationFor renders the currently resolvable inputs and returns
// the accepted record a leader would have written. mutate may adjust the
// rendering inputs before rendering.
func acceptedConfigurationFor(t *testing.T, engine *wafv1alpha1.Engine, ruleSet *wafv1alpha1.RuleSet, content assemble.Content, mutate func(*render.EngineInput)) *wafv1alpha1.EngineAcceptedConfiguration {
	t.Helper()

	contentHash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)

	input := render.EngineInput{
		Namespace:        engine.Namespace,
		Name:             engine.Name,
		UID:              engine.UID,
		RuleSet:          render.RuleSetAccepted{UID: ruleSet.UID, ContentHash: contentHash},
		Content:          content,
		FailurePolicy:    string(wafv1alpha1.FailurePolicyFail),
		Binary:           render.BinaryRef{URL: testAssetBaseURL + "/wasm/" + testSHA256 + ".wasm", SHA256: testSHA256},
		EmptyRulesPolicy: string(assemble.EmptyRulesPolicyReject),
	}
	if mutate != nil {
		mutate(&input)
	}
	artifact, err := render.Render(input)
	require.NoError(t, err)

	return &wafv1alpha1.EngineAcceptedConfiguration{
		ObservedGeneration:     engine.Generation,
		RenderingSchemaVersion: render.RenderingSchemaVersion,
		Version:                artifact.Version,
		RuleSetUID:             string(ruleSet.UID),
		RuleSetContentHash:     contentHash,
		BinaryURL:              testAssetBaseURL + "/wasm/" + testSHA256 + ".wasm",
		WasmSHA256:             testSHA256,
		FailurePolicy:          wafv1alpha1.FailurePolicyFail,
		EmptyRulesPolicy:       wafv1alpha1.EmptyRulesPolicy(artifact.EmptyRulesPolicy),
		FilterStateLogs:        filterStateLogsMode(artifact.EnableFilterStateLogs),
		DriverType:             wafv1alpha1.DriverType(artifact.DriverType),
		VM: wafv1alpha1.EngineAcceptedVMSettings{
			Runtime:     artifact.VM.Runtime,
			VMID:        artifact.VM.VMID,
			CodeTimeout: artifact.VM.CodeTimeout.String(),
		},
	}
}

func writeAcceptedConfiguration(t *testing.T, ctx context.Context, engine *wafv1alpha1.Engine, accepted *wafv1alpha1.EngineAcceptedConfiguration) {
	t.Helper()
	engine.Status = &wafv1alpha1.EngineStatus{
		Conditions: []metav1.Condition{{
			Type:               conditionAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             "Accepted",
			ObservedGeneration: engine.Generation,
			LastTransitionTime: metav1.Now(),
		}},
		AcceptedConfiguration: accepted,
	}
	require.NoError(t, k8sClient.Status().Update(ctx, engine))
}

func TestPublicationReconcilerStatusBeforeInput(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "pub-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "rules")
	engine := createPublicationEngine(t, ctx, namespace, "engine", ruleSet.Name, "pub-gw")

	expected := assemble.Content{Rules: publicationRules}
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, expected, nil))

	reconciler := newPublicationReconciler(t)
	engineKey := namespace + "/" + engine.Name

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	_, ok := reconciler.Store.Get(engineKey)
	require.False(t, ok, "a missing RuleSource must not be published")

	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}})
	require.NoError(t, err)

	snapshot, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok, "the accepted configuration must be published once inputs exist")
	require.Equal(t, engine.Status.AcceptedConfiguration.Version, snapshot.Version)
	require.Equal(t, engine.Status.AcceptedConfiguration.RuleSetContentHash, snapshot.RuleSetHash)
}

// TestPublicationReconcilerWaitsForStartupInitialization pins the Reconcile
// startup gate: with an uninitialized readiness tracker the item cannot be
// processed (a bounded context returns an error and nothing is published), and
// the very same item publishes once Initialize has applied the startup list.
func TestPublicationReconcilerWaitsForStartupInitialization(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "pub-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "engine", ruleSet.Name, "pub-gw")
	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))

	reconciler, _ := newPublicationReconcilerWithMetrics(t, nil)
	reconciler.Readiness = xds.NewPublicationReadiness()
	engineKey := namespace + "/" + engine.Name
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}

	// One reconcile call on an active bounded context: it must stay blocked
	// while readiness is uninitialized (so a removed gate cannot hide behind
	// a context error) and complete once the startup list is applied.
	type reconcileOutcome struct {
		result ctrl.Result
		err    error
	}
	done := make(chan reconcileOutcome, 1)
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	go func() {
		result, err := reconciler.Reconcile(bounded, request)
		done <- reconcileOutcome{result, err}
	}()

	select {
	case <-done:
		t.Fatal("Reconcile completed before startup initialization; the readiness gate is missing")
	case <-time.After(250 * time.Millisecond):
	}
	_, ok := reconciler.Store.Get(engineKey)
	require.False(t, ok, "no snapshot may be published before the startup list is applied")

	reconciler.Readiness.Initialize(nil)
	select {
	case outcome := <-done:
		require.NoError(t, outcome.err)
		require.Equal(t, ctrl.Result{}, outcome.result)
	case <-bounded.Done():
		t.Fatal("the waiting reconcile did not complete once the startup list was applied")
	}
	snapshot, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok, "the waiting item must be processed once the startup list is applied")
	require.Equal(t, engine.Status.AcceptedConfiguration.Version, snapshot.Version)
}

func TestPublicationReconcilerInputBeforeStatus(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "pub-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "engine", ruleSet.Name, "pub-gw")

	reconciler := newPublicationReconciler(t)
	engineKey := namespace + "/" + engine.Name
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}

	_, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	_, ok := reconciler.Store.Get(engineKey)
	require.False(t, ok, "desired state must not be published before acceptance")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	snapshot, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok)
	require.Equal(t, engine.Status.AcceptedConfiguration.Version, snapshot.Version)
}

func TestPublicationReconcilerMismatchKeepsPreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "pub-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "rules")
	source := createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "engine", ruleSet.Name, "pub-gw")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))

	reconciler := newPublicationReconciler(t)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}
	engineKey := namespace + "/" + engine.Name

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	first, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok)

	// Change the RuleSet inputs without a new acceptance record.
	source.Spec.Rules = publicationRules + "\nSecRule ARGS \"@contains other\" \"id:1002,phase:1,deny\""
	require.NoError(t, k8sClient.Update(ctx, source))

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)

	after, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok)
	require.Equal(t, first.Version, after.Version, "a mismatch must keep the previous snapshot")
}

func TestPublicationReconciler_NoOpCoalescesWithoutAssembly(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	before, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	contentless := &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	publication.Client = contentless
	_, err = publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, contentless.sourceGets)
	require.Equal(t, 1, contentless.dataGets)

	state := publication.states[request.NamespacedName]
	require.NotNil(t, state)
	require.True(t, state.mismatchSince.IsZero(), "the metadata-only no-op must remain a successful publication")
	after, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, before.Version, after.Version)
	require.Equal(t, before.EngineUID, after.EngineUID)
}

func TestPublicationReconciler_RuleSourceMetadataChangeKeepsSnapshot(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	before, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	var source wafv1alpha1.RuleSource
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &source))
	generation, resourceVersion := source.Generation, source.ResourceVersion
	source.Annotations = map[string]string{wafv1alpha1.AnnotationSkipValidation: "false"}
	require.NoError(t, k8sClient.Update(fixture.ctx, &source))
	_, err = (&RuleSourceReconciler{Client: k8sClient, Recorder: utils.NewTestRecorder()}).
		Reconcile(fixture.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&source)})
	require.NoError(t, err)
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&source), &source))
	require.Equal(t, generation, source.Generation)
	require.NotEqual(t, resourceVersion, source.ResourceVersion)
	require.True(t, isConditionCurrent(source.Status.Conditions, conditionReady, ruleSourceReadyReasonValidationSkipped, source.Generation))

	counted := &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	publication.Client = counted
	_, err = publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, counted.sourceGets, "metadata-only changes must coalesce without assembling content")
	require.Equal(t, 1, counted.dataGets)

	state := publication.states[request.NamespacedName]
	require.NotNil(t, state)
	require.True(t, state.mismatchSince.IsZero())

	after, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, before.Version, after.Version)
	require.Equal(t, before.EngineUID, after.EngineUID)

	// Repeated status or annotation updates must not require content assembly.
	counted = &ruleSetInputTestClient{
		Client:              k8sClient,
		stripRuleSourceSpec: true,
		stripRuleDataSpec:   true,
	}
	publication.Client = counted
	_, err = publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, counted.sourceGets)
	require.Equal(t, 1, counted.dataGets)
	after, found = publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, before.Version, after.Version)
}

func TestPublicationReconcilerContentGenerationChangeReadsFullContent(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	before, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	var source wafv1alpha1.RuleSource
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &source))
	oldGeneration := source.Generation
	source.Spec.Rules += "\nSecRule ARGS \"@contains changed\" \"id:1002,phase:1,deny,status:403\""
	require.NoError(t, k8sClient.Update(fixture.ctx, &source))
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(&source), &source))
	require.Greater(t, source.Generation, oldGeneration)

	counted := &ruleSetInputTestClient{Client: k8sClient}
	publication.Client = counted
	_, err = publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, counted.sourceGets, "an input generation change must use the already-read input")
	require.Equal(t, 1, counted.dataGets)

	after, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, before.Version, after.Version, "unaccepted content must keep the last published snapshot")
	state := publication.states[request.NamespacedName]
	require.NotNil(t, state)
	require.False(t, state.mismatchSince.IsZero(), "changed content must be reported as publication lag")
}

func TestPublicationReconciler_RebuildsWhenLocalSnapshotIsMissing(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	before, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	publication.Store.DeleteEngine(request.Namespace, request.Name)
	counted := &ruleSetInputTestClient{Client: k8sClient}
	publication.Client = counted
	_, err = publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, counted.sourceGets, "a missing local snapshot must use one full-input read")
	require.Equal(t, 1, counted.dataGets)

	after, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found, "a replica with a missing local snapshot must reconstruct it")
	require.Equal(t, before.Version, after.Version)
	require.Equal(t, before.EngineUID, after.EngineUID)
}

func TestPublicationReconcilerEachReplicaReconstructsItsOwnSnapshot(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}

	first := newPublicationReconciler(t)
	_, err := first.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	firstSnapshot, found := first.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	second := newPublicationReconciler(t)
	counted := &ruleSetInputTestClient{Client: k8sClient}
	second.Client = counted
	_, err = second.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	require.Equal(t, 1, counted.sourceGets, "a new replica has no local coalescing state")
	require.Equal(t, 1, counted.dataGets)

	secondSnapshot, found := second.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, firstSnapshot.Version, secondSnapshot.Version)
	require.Equal(t, firstSnapshot.EngineUID, secondSnapshot.EngineUID)
}

func TestPublicationReconcilerDoesNotCoalesceAcrossInputUIDOrGenerationChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *engineNoopFixture)
		strip  func(*ruleSetInputTestClient)
	}{
		{
			name: "RuleSource recreated",
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
			strip: func(c *ruleSetInputTestClient) { c.stripRuleSourceSpec = true },
		},
		{
			name: "RuleData generation changed with same content",
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
			strip: func(c *ruleSetInputTestClient) { c.stripRuleDataSpec = true },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEngineNoopFixture(t, false)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
			publication := newPublicationReconciler(t)
			_, err := publication.Reconcile(fixture.ctx, request)
			require.NoError(t, err)
			before, found := publication.Store.Get(request.NamespacedName.String())
			require.True(t, found)
			test.mutate(t, fixture)

			contentless := &ruleSetInputTestClient{Client: k8sClient}
			test.strip(contentless)
			publication.Client = contentless
			_, err = publication.Reconcile(fixture.ctx, request)
			require.NoError(t, err, "unavailable input content is a retained-snapshot wait state")
			require.Equal(t, 1, contentless.sourceGets)
			require.Equal(t, 1, contentless.dataGets)
			state := publication.states[request.NamespacedName]
			require.NotNil(t, state)
			require.False(t, state.mismatchSince.IsZero(), "changed input identity must not take the no-op path")
			after, found := publication.Store.Get(request.NamespacedName.String())
			require.True(t, found)
			require.Equal(t, before.Version, after.Version, "an unverified input must not replace the accepted snapshot")

			publication.Client = k8sClient
			_, err = publication.Reconcile(fixture.ctx, request)
			require.NoError(t, err)
			state = publication.states[request.NamespacedName]
			require.True(t, state.mismatchSince.IsZero(), "publication must recover once full content is available")
			after, found = publication.Store.Get(request.NamespacedName.String())
			require.True(t, found)
			require.Equal(t, before.Version, after.Version, "same effective content preserves the accepted version")
		})
	}
}

func TestPublicationReconcilerRetriesInputReadErrorsBeforeCoalescing(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	before, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	publication.Client = &ruleSetInputTestClient{
		Client:            k8sClient,
		failRuleSourceGet: errors.New("injected cached read failure"),
	}
	_, err = publication.Reconcile(fixture.ctx, request)
	require.ErrorContains(t, err, "injected cached read failure")
	after, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, before.Version, after.Version)
}

func TestPublicationReconcilerColdInputReadErrorsRemainTransient(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	counted := &ruleSetInputTestClient{
		Client:          k8sClient,
		failRuleDataGet: errors.New("injected cold RuleData read failure"),
	}
	publication.Client = counted

	_, err := publication.Reconcile(fixture.ctx, request)
	require.ErrorContains(t, err, "injected cold RuleData read failure")
	require.Equal(t, 1, counted.sourceGets)
	require.Equal(t, 1, counted.dataGets)
	_, found := publication.Store.Get(request.NamespacedName.String())
	require.False(t, found, "a transient cold read failure must not publish or create coalescing state")
	require.Empty(t, publication.states)
}

func TestPublicationReconcilerMissingInputRetainsPublishedSnapshot(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	before, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	require.NoError(t, k8sClient.Delete(fixture.ctx, fixture.source))
	counted := &ruleSetInputTestClient{Client: k8sClient}
	publication.Client = counted
	_, err = publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err, "a missing input is a retained-snapshot wait state, not a transient API error")
	require.Equal(t, 1, counted.sourceGets)
	require.Zero(t, counted.dataGets, "RuleData reads must stop after the missing RuleSource")

	after, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, before.Version, after.Version)
	state := publication.states[request.NamespacedName]
	require.NotNil(t, state)
	require.False(t, state.mismatchSince.IsZero())
}

func TestPublicationCoalescingRequiresCurrentEngineUIDAndLocalSnapshot(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	state := publication.states[request.NamespacedName]
	require.NotNil(t, state)

	_, ok := publication.coalescedState(request.NamespacedName, state.acceptedVersion, state.engineUID)
	require.True(t, ok)
	_, ok = publication.coalescedState(
		request.NamespacedName,
		state.acceptedVersion,
		types.UID("recreated-engine"),
	)
	require.False(t, ok, "a snapshot for the deleted Engine UID cannot satisfy a recreated Engine")
	_, ok = publication.coalescedState(
		request.NamespacedName,
		state.acceptedVersion+"-stale",
		state.engineUID,
	)
	require.False(t, ok, "a snapshot for a different accepted version cannot coalesce")

	publication.Store.DeleteEngine(request.Namespace, request.Name)
	_, ok = publication.coalescedState(request.NamespacedName, state.acceptedVersion, state.engineUID)
	require.False(t, ok, "the snapshot must exist locally before a reconcile can coalesce")
}

func TestPublicationReconcilerRecreatedEnginePublishesNewResourceIdentity(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)
	createTestGateway(t, ctx, k8sClient, "recreated-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "recreated-rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)

	engineName := "recreated-engine"
	engine := createPublicationEngine(t, ctx, namespace, engineName, ruleSet.Name, "recreated-gw")
	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))

	reconciler := newPublicationReconciler(t)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	oldSnapshot, found := reconciler.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, engine.UID, oldSnapshot.EngineUID)
	require.Equal(t, xds.ResourceName(namespace, engine.Name, engine.UID), oldSnapshot.ResourceName)

	require.NoError(t, k8sClient.Delete(ctx, engine))
	require.Eventually(t, func() bool {
		var current wafv1alpha1.Engine
		return apierrors.IsNotFound(k8sClient.Get(ctx, request.NamespacedName, &current))
	}, 5*time.Second, 10*time.Millisecond, "the old Engine must be deleted before name reuse")
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	_, found = reconciler.Store.Get(request.NamespacedName.String())
	require.False(t, found, "deleting the Engine must withdraw its UID-scoped snapshot")

	recreated := createPublicationEngine(t, ctx, namespace, engineName, ruleSet.Name, "recreated-gw")
	require.NotEqual(t, engine.UID, recreated.UID)
	writeAcceptedConfiguration(t, ctx, recreated, acceptedConfigurationFor(t, recreated, ruleSet, content, nil))
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)

	newSnapshot, found := reconciler.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, recreated.UID, newSnapshot.EngineUID)
	require.Equal(t, xds.ResourceName(namespace, recreated.Name, recreated.UID), newSnapshot.ResourceName)
	require.NotEqual(t, oldSnapshot.ResourceName, newSnapshot.ResourceName)
}

func TestPublicationReconcilerConcurrentNoOpReconciles(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)

	const concurrent = 8
	var wg sync.WaitGroup
	errs := make(chan error, concurrent)
	for range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := publication.Reconcile(fixture.ctx, request)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	state := publication.states[request.NamespacedName]
	require.NotNil(t, state)
	require.True(t, state.mismatchSince.IsZero())
}

func TestPublicationReconcilerConcurrentInputEditKeepsPreviousSnapshot(t *testing.T) {
	fixture := newEngineNoopFixture(t, false)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(fixture.engine)}
	publication := newPublicationReconciler(t)
	_, err := publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err)
	before, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)

	var edited wafv1alpha1.RuleSource
	require.NoError(t, k8sClient.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.source), &edited))
	edited.Spec.Rules += "\nSecRule ARGS \"@contains first-edit\" \"id:5,phase:2,deny\""
	require.NoError(t, k8sClient.Update(fixture.ctx, &edited))

	mutating := &mutateRuleSourceOnGetClient{
		Client:    k8sClient,
		sourceKey: client.ObjectKeyFromObject(fixture.source),
		newRules:  testAcceptRuleText + "\nSecRule ARGS \"@contains concurrent\" \"id:4,phase:2,deny\"",
		mutateAt:  1,
	}
	publication.Client = mutating
	_, err = publication.Reconcile(fixture.ctx, request)
	require.NoError(t, err, "a content mismatch is retained and reported through readiness, not returned as a transient API error")
	require.Equal(t, 1, mutating.sourceGets, "fingerprint and content must come from the same full input read")

	after, found := publication.Store.Get(request.NamespacedName.String())
	require.True(t, found)
	require.Equal(t, before.Version, after.Version)
	state := publication.states[request.NamespacedName]
	require.NotZero(t, state.mismatchSince, "the concurrent content divergence must be recorded")
}

// recordingRevoker records immediate stream revocations. It proves a reconcile
// reached the StreamRegistry, not only the snapshot store.
type recordingRevoker struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingRevoker) RevokeResource(resourceName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, resourceName)
}

func (r *recordingRevoker) revoked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recordingRevoker) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// TestPublicationReconcilerWithdrawalRevokesStreams proves that withdrawing an
// accepted configuration revokes the Engine's authorization and live streams.
// ClearSnapshot alone orphans the proxy's open watch: the proxy would keep
// enforcing the withdrawn configuration and miss the next publish.
func TestPublicationReconcilerWithdrawalRevokesStreams(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "withdraw-gw", namespace)
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "withdraw-sa"}}
	require.NoError(t, k8sClient.Create(ctx, account))
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "withdraw-pod",
			Labels:    map[string]string{gatewayNameLabel: "withdraw-gw"},
			// A managed Gateway data-plane pod: authorization ignores pods
			// without a ReplicaSet/Deployment owner chain.
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "withdraw-rs",
				UID:        types.UID("withdraw-rs-uid"),
				Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: account.Name,
			Containers:         []corev1.Container{{Name: "gateway", Image: "gateway:test"}},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))
	pod.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Status().Update(ctx, pod))

	ruleSet := createPublicationRuleSet(t, ctx, namespace, "rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "engine", ruleSet.Name, "withdraw-gw")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))

	revoker := &recordingRevoker{}
	reconciler, _ := newPublicationReconcilerWithMetrics(t, revoker.RevokeResource)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}
	engineKey := namespace + "/" + engine.Name

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	_, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok, "the fixture must publish the accepted configuration first")

	resourceName := xds.ResourceName(namespace, engine.Name, engine.UID)
	identity := xds.Identity{Namespace: namespace, Name: account.Name, UID: string(account.UID)}
	require.True(t, reconciler.Authz.Authorize(resourceName, identity),
		"the fixture must authorize the Gateway workload first")

	// Withdraw the accepted record while the Accepted condition stays True:
	// only the withdrawal path may revoke the stream.
	engine.Status.AcceptedConfiguration = nil
	require.NoError(t, k8sClient.Status().Update(ctx, engine))

	revoker.reset()
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)

	_, ok = reconciler.Store.Get(engineKey)
	require.False(t, ok, "withdrawal must delete the published snapshot")
	require.False(t, reconciler.Authz.Authorize(resourceName, identity),
		"withdrawal must revoke the authorization, not only the snapshot")
	require.Equal(t, []string{resourceName}, revoker.revoked(),
		"withdrawal must close the live streams of the withdrawn resource")

	// Withdrawal is idempotent: a repeated reconcile keeps the same state.
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	_, ok = reconciler.Store.Get(engineKey)
	require.False(t, ok)
	require.False(t, reconciler.Authz.Authorize(resourceName, identity))
}

// TestPublicationReconcilerReconstructsNonDefaultInputs proves that an
// accepted record with non-default rendering inputs (empty-rules policy,
// filter-state logs, driver type, VM settings) reconstructs and publishes even
// though the live spec still carries the defaults.
func TestPublicationReconcilerReconstructsNonDefaultInputs(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "inputs-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "inputs-rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source",
		`SecRuleEngine On`+"\n"+`SecRule ARGS "@contains probe" "id:1002,phase:2,deny,status:403"`)
	engine := createPublicationEngine(t, ctx, namespace, "inputs-engine", ruleSet.Name, "inputs-gw")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)

	// The leader accepted non-default inputs the live spec does not carry.
	accepted := acceptedConfigurationFor(t, engine, ruleSet, content, func(in *render.EngineInput) {
		in.EmptyRulesPolicy = string(assemble.EmptyRulesPolicyAllow)
		in.EnableFilterStateLogs = true
		in.DriverType = string(wafv1alpha1.DriverTypeWasm)
		in.VMSettings = render.VMSettings{
			Runtime:     "envoy.wasm.runtime.wasmtime",
			VMID:        "shared-vm",
			CodeTimeout: 5 * time.Second,
		}
	})
	writeAcceptedConfiguration(t, ctx, engine, accepted)

	reconciler := newPublicationReconciler(t)
	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}})
	require.NoError(t, err)

	snapshot, ok := reconciler.Store.Get(namespace + "/" + engine.Name)
	require.True(t, ok, "a record of non-default inputs must reconstruct from the record alone")
	require.Equal(t, accepted.Version, snapshot.Version)
}

// TestPublicationReconcilerLiveSpecChangeKeepsAcceptedVersion proves the
// complementary property: editing live spec fields that are rendering inputs
// no longer changes the reconstructed version. Publication stays a pure
// function of the accepted record until the Engine is re-accepted.
func TestPublicationReconcilerLiveSpecChangeKeepsAcceptedVersion(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "spec-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "spec-rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "spec-engine", ruleSet.Name, "spec-gw")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))

	reconciler := newPublicationReconciler(t)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}
	engineKey := namespace + "/" + engine.Name

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	published, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok)

	// Flip live rendering inputs without a new acceptance record.
	engine.Spec.Observability.Mode = wafv1alpha1.ObservabilityModeEnabled
	require.NoError(t, k8sClient.Update(ctx, engine))
	ruleSet.Spec.EmptyRulesPolicy = wafv1alpha1.EmptyRulesPolicyAllow
	require.NoError(t, k8sClient.Update(ctx, ruleSet))

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err, "a live spec change must not stall publication on a mismatch")

	after, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok)
	require.Equal(t, published.Version, after.Version, "the reconstructed version must come from the accepted record, not the live spec")
	// A mismatch would keep the previous snapshot too; prove the reconcile
	// took the clean path, not the mismatch path.
	for _, state := range reconciler.states {
		require.True(t, state.mismatchSince.IsZero(), "a live spec change must not be recorded as a reconstruction mismatch")
	}
}

// TestPublicationReconcilerLegacyRecordKeepsPreviousSnapshot covers accepted
// records written before the rendering inputs were persisted: they must
// mismatch and keep the previous snapshot rather than publish a configuration
// recomputed from defaults or the live spec.
func TestPublicationReconcilerLegacyRecordKeepsPreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "legacy-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "legacy-rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "legacy-engine", ruleSet.Name, "legacy-gw")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)

	// A record as an older operator binary would have written it: the
	// accepted version covers non-default inputs, but they are not persisted.
	accepted := acceptedConfigurationFor(t, engine, ruleSet, content, func(in *render.EngineInput) {
		in.EmptyRulesPolicy = string(assemble.EmptyRulesPolicyAllow)
		in.EnableFilterStateLogs = true
		in.VMSettings = render.VMSettings{
			Runtime:     "envoy.wasm.runtime.wasmtime",
			VMID:        "shared-vm",
			CodeTimeout: 5 * time.Second,
		}
	})
	legacy := accepted.DeepCopy()
	legacy.EmptyRulesPolicy = ""
	legacy.FilterStateLogs = ""
	legacy.DriverType = ""
	legacy.VM = wafv1alpha1.EngineAcceptedVMSettings{}
	writeAcceptedConfiguration(t, ctx, engine, legacy)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}
	engineKey := namespace + "/" + engine.Name

	// A replica without a snapshot must not publish a legacy record.
	fresh := newPublicationReconciler(t)
	_, err = fresh.Reconcile(ctx, request)
	require.NoError(t, err)
	_, ok := fresh.Store.Get(engineKey)
	require.False(t, ok, "a legacy record without rendering inputs must not publish")

	// A replica holding the accepted snapshot must keep it.
	writeAcceptedConfiguration(t, ctx, engine, accepted)
	published := newPublicationReconciler(t)
	_, err = published.Reconcile(ctx, request)
	require.NoError(t, err)
	snapshot, ok := published.Store.Get(engineKey)
	require.True(t, ok)
	require.Equal(t, accepted.Version, snapshot.Version)

	writeAcceptedConfiguration(t, ctx, engine, legacy)
	_, err = published.Reconcile(ctx, request)
	require.NoError(t, err)
	after, ok := published.Store.Get(engineKey)
	require.True(t, ok)
	require.Equal(t, snapshot.Version, after.Version, "a legacy record must not publish over the accepted snapshot")
}

// TestPublicationReconcilerHashMismatchMetric gates
// coraza_xds_hash_mismatch_total on actual divergences from the accepted
// record: a content edit without re-acceptance counts, while a RuleSet UID
// divergence and unavailable inputs do not. The cold-reconstruction counter
// stays zero: only the readiness deadline may increment it.
func TestPublicationReconcilerHashMismatchMetric(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "metric-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "rules")
	source := createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "engine", ruleSet.Name, "metric-gw")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)

	// A RuleSet UID divergence is not a hash mismatch.
	uidMismatch := acceptedConfigurationFor(t, engine, ruleSet, content, nil)
	uidMismatch.RuleSetUID = "33333333-3333-4333-8333-333333333333"
	writeAcceptedConfiguration(t, ctx, engine, uidMismatch)

	reconciler, registry := newPublicationReconcilerWithMetrics(t, nil)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}
	engineKey := namespace + "/" + engine.Name

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	_, ok := reconciler.Store.Get(engineKey)
	require.False(t, ok, "a UID divergence must not publish")
	require.Zero(t, counterValue(t, registry, "coraza_xds_hash_mismatch_total"),
		"a RuleSet UID divergence is not a hash mismatch")

	// Publish the matching record.
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))
	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	published, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok)

	// A rule-text edit without re-acceptance is the canonical hash mismatch.
	source.Spec.Rules += "\nSecRule ARGS \"@contains other\" \"id:1002,phase:1,deny\""
	require.NoError(t, k8sClient.Update(ctx, source))

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	after, ok := reconciler.Store.Get(engineKey)
	require.True(t, ok)
	require.Equal(t, published.Version, after.Version, "a mismatch must keep the previous snapshot")
	require.Equal(t, 1.0, counterValue(t, registry, "coraza_xds_hash_mismatch_total"),
		"a content divergence from the accepted record is a hash mismatch")
	require.Zero(t, counterValue(t, registry, "coraza_xds_cold_reconstruction_failures_total"),
		"only the readiness deadline may count a cold reconstruction failure")
}

func TestPublicationReconcilerSetupWithoutLeaderElection(t *testing.T) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	require.NoError(t, err)

	reconciler := newPublicationReconciler(t)
	require.NoError(t, reconciler.SetupWithManager(mgr),
		"publication must run on every replica (NeedLeaderElection=false)")
}

type publicationMappingClient struct {
	client.Client

	engineListFilters        []client.MatchingFields
	unindexedEngineListCalls int
}

func (c *publicationMappingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*wafv1alpha1.EngineList); ok {
		indexed := false
		for _, opt := range opts {
			if fields, ok := opt.(client.MatchingFields); ok && len(fields) > 0 {
				c.engineListFilters = append(c.engineListFilters, fields)
				indexed = true
				break
			}
		}
		if !indexed {
			c.unindexedEngineListCalls++
			return errors.New("unindexed Engine list in mapping")
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func TestPublicationMappersUseFieldIndexes(t *testing.T) {
	ctx := context.Background()
	namespace := "publication-mapping"
	sharedRuleSet := &wafv1alpha1.RuleSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "shared-rules"},
		Spec: wafv1alpha1.RuleSetSpec{
			Sources: []wafv1alpha1.SourceReference{{Name: "shared-source"}},
			Data:    []wafv1alpha1.DataReference{{Name: "shared-data"}},
		},
	}
	otherRuleSet := &wafv1alpha1.RuleSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "other-rules"},
		Spec: wafv1alpha1.RuleSetSpec{
			Sources: []wafv1alpha1.SourceReference{{Name: "other-source"}},
			Data:    []wafv1alpha1.DataReference{{Name: "other-data"}},
		},
	}
	newEngine := func(namespace, name, ruleSet, gateway string) *wafv1alpha1.Engine {
		return &wafv1alpha1.Engine{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec: wafv1alpha1.EngineSpec{
				RuleSet: wafv1alpha1.RuleSetReference{Name: ruleSet},
				Target: wafv1alpha1.EngineTarget{
					Type: wafv1alpha1.EngineTargetTypeGateway,
					Name: gateway,
				},
			},
		}
	}
	engineA := newEngine(namespace, "engine-a", sharedRuleSet.Name, "gateway")
	engineB := newEngine(namespace, "engine-b", sharedRuleSet.Name, "gateway")
	engineC := newEngine(namespace, "engine-c", otherRuleSet.Name, "gateway")
	otherGatewayEngine := newEngine(namespace, "other-gateway-engine", sharedRuleSet.Name, "other-gateway")
	foreignEngine := newEngine("other-namespace", "foreign-engine", sharedRuleSet.Name, "gateway")
	objects := []client.Object{
		sharedRuleSet,
		otherRuleSet,
		engineA,
		engineB,
		engineC,
		otherGatewayEngine,
		foreignEngine,
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(&wafv1alpha1.Engine{}, engineRuleSetIndex, engineRuleSetIndexValues).
		WithIndex(&wafv1alpha1.Engine{}, engineTargetNameIndex, engineTargetNameIndexValues).
		WithIndex(&wafv1alpha1.RuleSet{}, "spec.sources.name", func(obj client.Object) []string {
			ruleSet := obj.(*wafv1alpha1.RuleSet)
			names := make([]string, len(ruleSet.Spec.Sources))
			for i, source := range ruleSet.Spec.Sources {
				names[i] = source.Name
			}
			return names
		}).
		WithIndex(&wafv1alpha1.RuleSet{}, "spec.data.name", func(obj client.Object) []string {
			ruleSet := obj.(*wafv1alpha1.RuleSet)
			names := make([]string, len(ruleSet.Spec.Data))
			for i, data := range ruleSet.Spec.Data {
				names[i] = data.Name
			}
			return names
		}).
		Build()
	mappingClient := &publicationMappingClient{Client: baseClient}
	publication := &PublicationReconciler{Client: mappingClient}
	engineReconciler := &EngineReconciler{Client: mappingClient}
	requestNames := func(requests []reconcile.Request) []string {
		names := make([]string, len(requests))
		for i, request := range requests {
			names[i] = request.Name
		}
		return names
	}

	sharedEngineNames := []string{"engine-a", "engine-b", "other-gateway-engine"}
	sharedSource := &wafv1alpha1.RuleSource{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "shared-source"}}
	sharedData := &wafv1alpha1.RuleData{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "shared-data"}}
	otherSource := &wafv1alpha1.RuleSource{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "other-source"}}
	otherData := &wafv1alpha1.RuleData{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "other-data"}}
	// These input event objects are absent from the fake client, as on deletion.
	require.ElementsMatch(t, sharedEngineNames, requestNames(publication.mapRuleSet(ctx, sharedRuleSet)))
	require.ElementsMatch(t, sharedEngineNames, requestNames(publication.mapRuleInput(ctx, sharedSource)))
	require.ElementsMatch(t, sharedEngineNames, requestNames(publication.mapRuleInput(ctx, sharedData)))
	require.ElementsMatch(t, sharedEngineNames, requestNames(engineReconciler.findEnginesForRuleSet(ctx, sharedRuleSet)))

	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "gateway"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Labels:    map[string]string{gatewayNameLabel: "gateway"},
	}}
	gatewayEngineNames := []string{"engine-a", "engine-b", "engine-c"}
	require.ElementsMatch(t, gatewayEngineNames, requestNames(publication.mapGateway(ctx, gateway)))
	require.ElementsMatch(t, gatewayEngineNames, requestNames(publication.mapPod(ctx, pod)))
	require.ElementsMatch(t, gatewayEngineNames, requestNames(engineReconciler.findEnginesForPod(ctx, pod)))

	engineB.Spec.RuleSet.Name = otherRuleSet.Name
	require.NoError(t, baseClient.Update(ctx, engineB))
	renamedSharedEngineNames := []string{"engine-a", "other-gateway-engine"}
	renamedOtherEngineNames := []string{"engine-b", "engine-c"}
	require.ElementsMatch(t, renamedSharedEngineNames, requestNames(publication.mapRuleSet(ctx, sharedRuleSet)))
	require.ElementsMatch(t, renamedSharedEngineNames, requestNames(publication.mapRuleInput(ctx, sharedSource)))
	require.ElementsMatch(t, renamedOtherEngineNames, requestNames(publication.mapRuleSet(ctx, otherRuleSet)))
	require.ElementsMatch(t, renamedOtherEngineNames, requestNames(publication.mapRuleInput(ctx, otherSource)))
	require.ElementsMatch(t, renamedOtherEngineNames, requestNames(publication.mapRuleInput(ctx, otherData)))
	require.ElementsMatch(t, renamedOtherEngineNames, requestNames(engineReconciler.findEnginesForRuleSet(ctx, otherRuleSet)))

	require.NoError(t, baseClient.Delete(ctx, sharedRuleSet))
	require.ElementsMatch(t, renamedSharedEngineNames, requestNames(publication.mapRuleSet(ctx, sharedRuleSet)),
		"a deleted RuleSet event must still map Engines that reference its name")

	require.Contains(t, mappingClient.engineListFilters, client.MatchingFields{engineRuleSetIndex: sharedRuleSet.Name})
	require.Contains(t, mappingClient.engineListFilters, client.MatchingFields{engineTargetNameIndex: gateway.Name})
	require.Zero(t, mappingClient.unindexedEngineListCalls, "mapping must not list every Engine")
}

// TestPublicationRateLimiterKeepsOverallBucket proves the publication rate
// limiter bounds overall throughput, not only per-item retries: distinct items
// drain one shared bucket, so once the burst is spent the delay grows beyond
// the per-item exponential backoff.
func TestPublicationRateLimiterKeepsOverallBucket(t *testing.T) {
	limiter := publicationRateLimiter()

	item := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "engine"}}
	require.Equal(t, time.Second, limiter.When(item), "a first failure backs off one second")

	for i := 0; i < 200; i++ {
		other := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: fmt.Sprintf("engine-%d", i)}}
		limiter.When(other)
	}

	// 201 reservations against a 100-token, 10 qps bucket owe roughly ten
	// seconds; the per-item backoff alone would stay at two.
	require.Greater(t, limiter.When(item), 5*time.Second,
		"the overall bucket must slow a burst of distinct items")
}

// sentMessageBytes returns the size of the Response the ECDS server puts on the
// wire for artifact: go-control-plane wraps the resource in an Any inside a
// DiscoveryResponse and adds a version, type URL and nonce.
func sentMessageBytes(t *testing.T, artifact render.Artifact) int {
	t.Helper()

	resource, err := anypb.New(artifact.Resource)
	require.NoError(t, err)
	raw, err := proto.Marshal(&discoveryv3.DiscoveryResponse{
		VersionInfo: artifact.Version,
		Resources:   []*anypb.Any{resource},
		TypeUrl:     "type.googleapis.com/envoy.config.core.v3.TypedExtensionConfig",
		Nonce:       "9223372036854775807",
	})
	require.NoError(t, err)
	return len(raw)
}

// TestValidateContentLimitsRejectsPerFileOverPluginCap pins envoy-coraza's
// per-file rule_data ceiling: the aggregate byte check alone would accept a
// single file the plugin refuses at configure time.
func TestValidateContentLimitsRejectsPerFileOverPluginCap(t *testing.T) {
	content := assemble.Content{DataFiles: map[string][]byte{
		"big.data": make([]byte, pluginMaxRuleDataFileBytes+1),
	}}
	err := validateContentLimits(content, xds.DefaultLimits())
	require.ErrorContains(t, err, "per-file limit")
}

func TestContentLimitSummaryMatchesColdValidation(t *testing.T) {
	largeFile := make([]byte, pluginMaxRuleDataFileBytes+1)
	tests := []struct {
		name    string
		content assemble.Content
		limits  xds.Limits
		want    string
	}{
		{
			name:    "rule text size",
			content: assemble.Content{Rules: "four"},
			limits:  xds.Limits{MaxMessageBytes: 3},
			want:    "rule text size 4 exceeds the message limit of 3",
		},
		{
			name: "RuleData file count",
			content: assemble.Content{DataFiles: map[string][]byte{
				"one.data": []byte("1"),
				"two.data": []byte("2"),
			}},
			limits: xds.Limits{MaxMessageBytes: 10, MaxDataFiles: 1, MaxDecodedDataBytes: 10},
			want:   "RuleData file count 2 exceeds the limit of 1",
		},
		{
			name: "per-file RuleData size",
			content: assemble.Content{DataFiles: map[string][]byte{
				"big.data":   largeFile,
				"small.data": []byte("aa"),
			}},
			limits: func() xds.Limits {
				limits := xds.DefaultLimits()
				limits.MaxDecodedDataBytes = 1
				return limits
			}(),
			want: fmt.Sprintf("a RuleData file of %d bytes exceeds the plugin per-file limit of %d", len(largeFile), pluginMaxRuleDataFileBytes),
		},
		{
			name: "total decoded RuleData size",
			content: assemble.Content{DataFiles: map[string][]byte{
				"one.data": []byte("aa"),
				"two.data": []byte("bb"),
			}},
			limits: xds.Limits{MaxMessageBytes: 10, MaxDataFiles: 2, MaxDecodedDataBytes: 3},
			want:   "decoded RuleData size 4 exceeds the limit of 3",
		},
		{
			name: "rules precede file count",
			content: assemble.Content{
				Rules: "four",
				DataFiles: map[string][]byte{
					"one.data": []byte("1"),
					"two.data": []byte("2"),
				},
			},
			limits: xds.Limits{MaxMessageBytes: 3, MaxDataFiles: 1, MaxDecodedDataBytes: 10},
			want:   "rule text size 4 exceeds the message limit of 3",
		},
		{
			name: "file count precedes per-file size",
			content: assemble.Content{DataFiles: map[string][]byte{
				"big.data":   largeFile,
				"small.data": []byte("1"),
			}},
			limits: xds.Limits{MaxMessageBytes: 10, MaxDataFiles: 1, MaxDecodedDataBytes: 10},
			want:   "RuleData file count 2 exceeds the limit of 1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary, coldErr := summarizeAndValidateContentLimits(test.content, test.limits)
			require.EqualError(t, coldErr, test.want)
			require.EqualError(t, validateContentLimitSummary(summary, test.limits), test.want)
		})
	}
}

// TestDefaultLimitsDeliverMaxDecodedData pins the arithmetic behind
// DefaultMaxDecodedDataBytes: the decoded-data limit, split across the maximum
// number of maximum-length file names, still produces an ECDS response both the
// gRPC send limit and the envoy-coraza configuration cap accept.
func TestDefaultLimitsDeliverMaxDecodedData(t *testing.T) {
	limits := xds.DefaultLimits()
	files := make(map[string][]byte, limits.MaxDataFiles)
	perFile := limits.MaxDecodedDataBytes / limits.MaxDataFiles
	remaining := limits.MaxDecodedDataBytes
	for i := range limits.MaxDataFiles {
		size := min(perFile, remaining)
		files[fmt.Sprintf("%0253d", i)] = []byte(strings.Repeat("a", size))
		remaining -= size
	}

	content := assemble.Content{Rules: publicationRules, DataFiles: files}
	require.NoError(t, validateContentLimits(content, limits), "the fixture must sit exactly at the content limits")

	hash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)
	artifact, err := render.Render(render.EngineInput{
		Namespace: "default",
		Name:      "delivery-limits",
		UID:       "11111111-1111-4111-8111-111111111111",
		RuleSet: render.RuleSetAccepted{
			UID:         "22222222-2222-4222-8222-222222222222",
			ContentHash: hash,
		},
		Content: content,
		Binary:  render.BinaryRef{URL: "https://example.com/plugin.wasm", SHA256: testSHA256},
	})
	require.NoError(t, err)
	require.LessOrEqual(t, sentMessageBytes(t, artifact), limits.MaxMessageBytes,
		"the response actually sent must fit the gRPC limit, not only the payload")
	require.NoError(t, validateRenderedPayload(artifact, limits))
}

// TestValidateRenderedPayloadReservesTheEnvelope pins the boundary: the largest
// resource that still leaves room for the ECDS envelope is accepted, one byte
// more is rejected.
func TestValidateRenderedPayloadReservesTheEnvelope(t *testing.T) {
	limits := xds.DefaultLimits()
	limits.MaxMessageBytes = 8192

	artifact := func(rules string) render.Artifact {
		content := assemble.Content{Rules: rules}
		hash, err := render.RuleSetContentHash(content)
		require.NoError(t, err)
		out, err := render.Render(render.EngineInput{
			Namespace: "default",
			Name:      "delivery-boundary",
			UID:       "11111111-1111-4111-8111-111111111111",
			RuleSet: render.RuleSetAccepted{
				UID:         "22222222-2222-4222-8222-222222222222",
				ContentHash: hash,
			},
			Content: content,
			Binary:  render.BinaryRef{URL: "https://example.com/plugin.wasm", SHA256: testSHA256},
		})
		require.NoError(t, err)
		return out
	}

	// Compression makes the rules-to-wire-size mapping non-linear, so the
	// boundary is exercised with incompressible rule text from a fixed seed.
	room := limits.MaxMessageBytes - messageEnvelopeBytes - proto.Size(artifact(publicationRules).Resource)
	require.Positive(t, room)

	// The envelope base64-expands its gzip member by 4/3, so even
	// incompressible rule text lands well under one byte of wire size per
	// byte of text; the two cases bracket the limit instead of pinning it to
	// the byte.
	atLimit := artifact(publicationRules + incompressibleRules(t, room/2))
	require.NoError(t, validateRenderedPayload(atLimit, limits))
	require.LessOrEqual(t, sentMessageBytes(t, atLimit), limits.MaxMessageBytes)

	over := artifact(publicationRules + incompressibleRules(t, room*2))
	require.ErrorContains(t, validateRenderedPayload(over, limits), "message limit of 8192")
}

// incompressibleRules returns n bytes of rule text that gzip cannot shrink,
// from a fixed seed so the boundary test is deterministic.
func incompressibleRules(t *testing.T, n int) string {
	t.Helper()
	rng := rand.New(rand.NewSource(580))
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('!' + rng.Intn(94)) // printable, deterministic, high-entropy
	}
	return string(out)
}

// Pod authorization changes arrive without any spec/label change, so the
// predicate must fire on phase and controller-owner transitions. Missing them
// leaves assigning Gateway pods unauthorized (503s) or keeps terminated pods
// authorized until the refresh tick.
func TestPodAuthorizationPredicate(t *testing.T) {
	gatewayPod := func(mutate func(*corev1.Pod)) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "shop",
				Name:      "gw-abc",
				Labels:    map[string]string{gatewayNameLabel: "gw"},
			},
			Spec:   corev1.PodSpec{ServiceAccountName: "gateway-sa"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       "gw-rs",
			UID:        types.UID("rs-1"),
			Controller: ptrTo(true),
		}}
		if mutate != nil {
			mutate(pod)
		}
		return pod
	}

	tests := []struct {
		name string
		old  *corev1.Pod
		new  *corev1.Pod
		want bool
	}{
		{name: "unchanged", old: gatewayPod(nil), new: gatewayPod(nil), want: false},
		{
			name: "pending to running",
			old:  gatewayPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }),
			new:  gatewayPod(nil),
			want: true,
		},
		{
			name: "running to succeeded",
			old:  gatewayPod(nil),
			new:  gatewayPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
			want: true,
		},
		{
			name: "running to failed",
			old:  gatewayPod(nil),
			new:  gatewayPod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }),
			want: true,
		},
		{
			name: "service account change",
			old:  gatewayPod(nil),
			new:  gatewayPod(func(p *corev1.Pod) { p.Spec.ServiceAccountName = "other" }),
			want: true,
		},
		{
			name: "controller owner change",
			old:  gatewayPod(nil),
			new:  gatewayPod(func(p *corev1.Pod) { p.OwnerReferences[0].UID = types.UID("rs-2") }),
			want: true,
		},
		{
			name: "owner removed",
			old:  gatewayPod(nil),
			new:  gatewayPod(func(p *corev1.Pod) { p.OwnerReferences = nil }),
			want: true,
		},
		{
			name: "deletion timestamp set",
			old:  gatewayPod(nil),
			new: gatewayPod(func(p *corev1.Pod) {
				now := metav1.Now()
				p.DeletionTimestamp = &now
			}),
			want: true,
		},
	}

	p := podAuthorizationPredicate()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})
			require.Equal(t, tc.want, got)
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

// failingGatewayReader fails reads of Gateway objects, leaving the rest of the
// cache usable. It simulates a transient API failure during authorization
// refresh.
type failingGatewayReader struct{ client.Reader }

func (r failingGatewayReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*gatewayv1.Gateway); ok {
		return errors.New("apiserver unavailable")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

// A failed authorization refresh must be returned so the workqueue retries it;
// publication itself still succeeds and keeps the snapshot.
func TestPublicationReconcilerRetriesFailedAuthorizationRefresh(t *testing.T) {
	ctx := context.Background()
	namespace := setupTestNamespace(t, ctx)

	createTestGateway(t, ctx, k8sClient, "pub-gw", namespace)
	ruleSet := createPublicationRuleSet(t, ctx, namespace, "rules")
	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)
	engine := createPublicationEngine(t, ctx, namespace, "engine", ruleSet.Name, "pub-gw")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, engine, ruleSet, content, nil))

	reconciler := newPublicationReconciler(t)
	reconciler.Authz = xds.NewAuthzIndex(failingGatewayReader{k8sClient}, k8sClient, 5*time.Minute, nil)

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}})
	require.ErrorContains(t, err, "apiserver unavailable", "a failed authorization refresh must be retried")

	_, ok := reconciler.Store.Get(namespace + "/" + engine.Name)
	require.True(t, ok, "publication must still succeed when only authorization refresh fails")
}

// TestValidateRenderedPayloadRejectsPluginConfigurationCap pins the second
// delivery bound: a payload can fit the gRPC message and still be NACKed by
// envoy-coraza's 8 MiB configuration cap.
func TestValidateRenderedPayloadRejectsPluginConfigurationCap(t *testing.T) {
	artifact := render.Artifact{Payload: make([]byte, render.MaxConfigurationBytes+1)}
	err := validateRenderedPayload(artifact, xds.DefaultLimits())
	require.ErrorContains(t, err, "plugin configuration limit")
}
