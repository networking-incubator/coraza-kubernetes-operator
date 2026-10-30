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
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
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
	return &PublicationReconciler{
		Client:    k8sClient,
		APIReader: k8sClient,
		Scheme:    scheme,
		Store:     xds.NewSnapshotStore(),
		Authz:     xds.NewAuthzIndex(k8sClient, k8sClient, 5*time.Minute, revoke),
		Readiness: xds.NewPublicationReadiness(),
		Metrics:   metrics,
		Limits:    xds.DefaultLimits(),
	}, registry
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

	room := limits.MaxMessageBytes - messageEnvelopeBytes - proto.Size(artifact(publicationRules).Resource)
	require.Positive(t, room)

	atLimit := artifact(publicationRules + strings.Repeat("x", room))
	require.Equal(t, limits.MaxMessageBytes-messageEnvelopeBytes, proto.Size(atLimit.Resource))
	require.NoError(t, validateRenderedPayload(atLimit, limits))
	require.LessOrEqual(t, sentMessageBytes(t, atLimit), limits.MaxMessageBytes)

	over := artifact(publicationRules + strings.Repeat("x", room+1))
	require.ErrorContains(t, validateRenderedPayload(over, limits), "message limit of 8192")
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
