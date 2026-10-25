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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/xds"
)

const publicationRules = "SecRuleEngine On\nSecRule REQUEST_URI \"@contains attack\" \"id:1001,phase:1,deny,status:403\""

func newPublicationReconciler(t *testing.T) *PublicationReconciler {
	t.Helper()
	metrics, err := xds.NewMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	return &PublicationReconciler{
		Client:    k8sClient,
		APIReader: k8sClient,
		Scheme:    scheme,
		Store:     xds.NewSnapshotStore(),
		Authz:     xds.NewAuthzIndex(k8sClient, 5*time.Minute),
		Readiness: xds.NewPublicationReadiness(),
		Metrics:   metrics,
		Limits:    xds.DefaultLimits(),
	}
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
// the accepted record a leader would have written.
func acceptedConfigurationFor(t *testing.T, ctx context.Context, engine *wafv1alpha1.Engine, ruleSet *wafv1alpha1.RuleSet, content assemble.Content) *wafv1alpha1.EngineAcceptedConfiguration {
	t.Helper()

	contentHash, err := render.RuleSetContentHash(content)
	require.NoError(t, err)

	artifact, err := render.Render(render.EngineInput{
		Namespace:        engine.Namespace,
		Name:             engine.Name,
		UID:              engine.UID,
		RuleSet:          render.RuleSetAccepted{UID: ruleSet.UID, ContentHash: contentHash},
		Content:          content,
		FailurePolicy:    string(wafv1alpha1.FailurePolicyFail),
		Binary:           render.BinaryRef{URL: testAssetBaseURL + "/wasm/" + testSHA256 + ".wasm", SHA256: testSHA256},
		EmptyRulesPolicy: string(assemble.EmptyRulesPolicyReject),
	})
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
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, ctx, engine, ruleSet, expected))

	reconciler := newPublicationReconciler(t)
	resourceName := xds.ResourceName(namespace, engine.Name, engine.UID)

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}})
	require.NoError(t, err)
	require.False(t, result.Requeue)
	_, ok := reconciler.Store.Get(resourceName)
	require.False(t, ok, "a missing RuleSource must not be published")

	createPublicationRuleSource(t, ctx, namespace, ruleSet.Name+"-source", publicationRules)

	_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}})
	require.NoError(t, err)

	snapshot, ok := reconciler.Store.Get(resourceName)
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
	resourceName := xds.ResourceName(namespace, engine.Name, engine.UID)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}

	_, err := reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	_, ok := reconciler.Store.Get(resourceName)
	require.False(t, ok, "desired state must not be published before acceptance")

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(t, err)
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, ctx, engine, ruleSet, content))

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	snapshot, ok := reconciler.Store.Get(resourceName)
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
	writeAcceptedConfiguration(t, ctx, engine, acceptedConfigurationFor(t, ctx, engine, ruleSet, content))

	reconciler := newPublicationReconciler(t)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: engine.Name}}
	resourceName := xds.ResourceName(namespace, engine.Name, engine.UID)

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)
	first, ok := reconciler.Store.Get(resourceName)
	require.True(t, ok)

	// Change the RuleSet inputs without a new acceptance record.
	source.Spec.Rules = publicationRules + "\nSecRule ARGS \"@contains other\" \"id:1002,phase:1,deny\""
	require.NoError(t, k8sClient.Update(ctx, source))

	_, err = reconciler.Reconcile(ctx, request)
	require.NoError(t, err)

	after, ok := reconciler.Store.Get(resourceName)
	require.True(t, ok)
	require.Equal(t, first.Version, after.Version, "a mismatch must keep the previous snapshot")
	require.Same(t, first, after)
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
