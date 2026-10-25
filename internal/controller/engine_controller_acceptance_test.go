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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/assets"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// -----------------------------------------------------------------------------
// Shared ECDS test fixtures
// -----------------------------------------------------------------------------

const (
	testAssetBaseURL   = "https://coraza-operator.coraza-system.svc.cluster.local:18080"
	testAcceptRuleText = `SecRule REQUEST_URI "@contains /admin" "id:1,phase:1,deny,status:403"`
)

const testCAPEM = "-----BEGIN CERTIFICATE-----\nMIIBtestCa\n-----END CERTIFICATE-----\n"

func testXDSConfig() ECDSConfig {
	return ECDSConfig{
		AdvertiseAddress: "coraza-operator.coraza-system.svc.cluster.local:18080",
		CAPEM:            []byte(testCAPEM),
	}
}

// acceptTestRuleSet creates the RuleSources referenced by ruleSet (when they do
// not exist yet) and records the assembled content hash in its status, as the
// RuleSet leader would.
func acceptTestRuleSet(t *testing.T, ctx context.Context, ruleSet *wafv1alpha1.RuleSet, rules string) {
	t.Helper()

	for _, ref := range ruleSet.Spec.Sources {
		source := utils.NewTestRuleSource(ref.Name, ruleSet.Namespace, rules)
		if err := k8sClient.Create(ctx, source); err != nil && !apierrors.IsAlreadyExists(err) {
			require.NoError(t, err)
		}
	}
	for _, ref := range ruleSet.Spec.Data {
		data := utils.NewTestRuleData(ref.Name, ruleSet.Namespace, map[string]string{})
		if err := k8sClient.Create(ctx, data); err != nil && !apierrors.IsAlreadyExists(err) {
			require.NoError(t, err)
		}
	}

	var current wafv1alpha1.RuleSet
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(ruleSet), &current))

	inputs, err := loadRuleSetInputs(ctx, k8sClient, &current)
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
	require.NoError(t, k8sClient.Status().Patch(ctx, &current, patch))
	*ruleSet = current
}

func newAcceptedEngineTestReconciler() *EngineReconciler {
	return &EngineReconciler{
		Client:            k8sClient,
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		xdsConfig:         testXDSConfig(),
		assetBaseURL:      testAssetBaseURL,
		operatorNamespace: testNamespace,
	}
}

func getEnvoyFilter(t *testing.T, ctx context.Context, engineName, namespace string) (*unstructured.Unstructured, error) {
	t.Helper()
	envoyFilter := &unstructured.Unstructured{}
	envoyFilter.SetGroupVersionKind(envoyFilterGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: envoyFilterName(engineName)}, envoyFilter)
	return envoyFilter, err
}

func requireEnvoyFilterExists(t *testing.T, ctx context.Context, engineName, namespace string) *unstructured.Unstructured {
	t.Helper()
	envoyFilter, err := getEnvoyFilter(t, ctx, engineName, namespace)
	require.NoError(t, err, "EnvoyFilter %s should exist", envoyFilterName(engineName))
	return envoyFilter
}

func requireNoEnvoyFilter(t *testing.T, ctx context.Context, engineName, namespace string) {
	t.Helper()
	_, err := getEnvoyFilter(t, ctx, engineName, namespace)
	assert.True(t, apierrors.IsNotFound(err), "EnvoyFilter should not exist, got: %v", err)
}

// reconcileEngine runs the finalizer reconcile and one provisioning reconcile.
func reconcileEngine(t *testing.T, ctx context.Context, reconciler *EngineReconciler, engine *wafv1alpha1.Engine) ctrl.Result {
	t.Helper()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
	_, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	result, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	return result
}

// -----------------------------------------------------------------------------
// Binary reference resolution
// -----------------------------------------------------------------------------

func TestResolveBinaryRef(t *testing.T) {
	reconciler := &EngineReconciler{assetBaseURL: testAssetBaseURL}

	t.Run("explicit url and digest are used", func(t *testing.T) {
		ref, err := reconciler.resolveBinaryRef(utils.NewTestEngine(utils.EngineOptions{
			WasmURL:    utils.DefaultTestWasmURL,
			WasmSHA256: utils.DefaultTestWasmSHA256,
		}))
		require.NoError(t, err)
		assert.Equal(t, utils.DefaultTestWasmURL, ref.URL)
		assert.Equal(t, utils.DefaultTestWasmSHA256, ref.SHA256)
	})

	t.Run("omitted pair resolves the bundled module", func(t *testing.T) {
		manifest, _, err := assets.ResolveDefault()
		require.NoError(t, err)

		engine := utils.NewTestEngine(utils.EngineOptions{})
		engine.Spec.Driver.Wasm = nil
		ref, err := reconciler.resolveBinaryRef(engine)
		require.NoError(t, err)
		assert.Equal(t, manifest.SHA256, ref.SHA256)
		assert.Equal(t, strings.TrimRight(testAssetBaseURL, "/")+"/wasm/"+manifest.SHA256+".wasm", ref.URL)
	})

	t.Run("only one of url and sha256 is rejected", func(t *testing.T) {
		_, err := reconciler.resolveBinaryRef(utils.NewTestEngine(utils.EngineOptions{WasmURL: utils.DefaultTestWasmURL}))
		require.Error(t, err)
		_, err = reconciler.resolveBinaryRef(utils.NewTestEngine(utils.EngineOptions{WasmSHA256: utils.DefaultTestWasmSHA256}))
		require.Error(t, err)
	})

	t.Run("invalid url or digest is rejected", func(t *testing.T) {
		engine := utils.NewTestEngine(utils.EngineOptions{WasmURL: "http://insecure.example/plugin.wasm", WasmSHA256: utils.DefaultTestWasmSHA256})
		_, err := reconciler.resolveBinaryRef(engine)
		require.Error(t, err)

		engine = utils.NewTestEngine(utils.EngineOptions{WasmURL: utils.DefaultTestWasmURL, WasmSHA256: "not-a-digest"})
		_, err = reconciler.resolveBinaryRef(engine)
		require.Error(t, err)
	})
}

// -----------------------------------------------------------------------------
// Engine acceptance and attachment
// -----------------------------------------------------------------------------

func TestEngineReconciler_EngineInputsChangedDetectsSourceUpdate(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)

	source := utils.NewTestRuleSource("changed-src", ns, testAcceptRuleText)
	require.NoError(t, k8sClient.Create(ctx, source))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "changed-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))

	engine := utils.NewTestEngine(utils.EngineOptions{Name: "changed-engine", Namespace: ns})
	require.NoError(t, k8sClient.Create(ctx, engine))

	inputs, err := loadRuleSetInputs(ctx, k8sClient, ruleSet)
	require.NoError(t, err)

	reconciler := newAcceptedEngineTestReconciler()
	changed, err := reconciler.engineInputsChanged(ctx, engine, ruleSet, inputs)
	require.NoError(t, err)
	assert.False(t, changed, "an unchanged read must be accepted")

	source.Spec.Rules = testAcceptRuleText + "\nSecRule REQUEST_URI \"@contains /root\" \"id:2,phase:1,deny,status:403\""
	require.NoError(t, k8sClient.Update(ctx, source))

	changed, err = reconciler.engineInputsChanged(ctx, engine, ruleSet, inputs)
	require.NoError(t, err)
	assert.True(t, changed, "a RuleSource generation change must invalidate the read")
}

func TestEngineReconciler_AwaitingRuleSetAcceptance(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	createTestGateway(t, ctx, k8sClient, "awaiting-gw", ns)

	source := utils.NewTestRuleSource("awaiting-src", ns, testAcceptRuleText)
	require.NoError(t, k8sClient.Create(ctx, source))
	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      "awaiting-ruleset",
		Namespace: ns,
		Sources:   []wafv1alpha1.SourceReference{{Name: source.Name}},
	})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "awaiting-engine",
		Namespace:   ns,
		GatewayName: "awaiting-gw",
		RuleSetName: ruleSet.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))

	result := reconcileEngine(t, ctx, newAcceptedEngineTestReconciler(), engine)
	assert.Equal(t, ruleSetAcceptanceRetryAfter, result.RequeueAfter)

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(engine), &updated))
	require.NotNil(t, updated.Status)
	assert.Nil(t, updated.Status.AcceptedConfiguration, "nothing may be accepted before RuleSet acceptance")

	progressing := apimeta.FindStatusCondition(updated.Status.Conditions, conditionProgressing)
	require.NotNil(t, progressing)
	assert.Equal(t, metav1.ConditionTrue, progressing.Status)
	assert.Equal(t, reasonAwaitingRuleSetAcceptance, progressing.Reason)

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)

	requireNoEnvoyFilter(t, ctx, engine.Name, engine.Namespace)
}

func TestEngineReconciler_TLSNotConfiguredFailsClosed(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	createTestGateway(t, ctx, k8sClient, "tls-gw", ns)

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{Name: "tls-ruleset", Namespace: ns})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	acceptTestRuleSet(t, ctx, ruleSet, testAcceptRuleText)

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "tls-engine",
		Namespace:   ns,
		GatewayName: "tls-gw",
		RuleSetName: ruleSet.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))

	reconciler := newAcceptedEngineTestReconciler()
	reconciler.xdsConfig.CAPEM = nil
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)}
	_, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)
	_, err = reconciler.Reconcile(ctx, req)
	require.Error(t, err, "an Engine without a verified CA must not be provisioned")

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(engine), &updated))
	require.NotNil(t, updated.Status)
	assert.Nil(t, updated.Status.AcceptedConfiguration)
	degraded := apimeta.FindStatusCondition(updated.Status.Conditions, conditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, metav1.ConditionTrue, degraded.Status)
	assert.Equal(t, reasonTLSNotConfigured, degraded.Reason)
	requireNoEnvoyFilter(t, ctx, engine.Name, engine.Namespace)
}

func TestEngineReconciler_AcceptsConfigurationAndAttachesEnvoyFilter(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	createTestGateway(t, ctx, k8sClient, "accept-gw", ns)

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{Name: "accept-ruleset", Namespace: ns})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	acceptTestRuleSet(t, ctx, ruleSet, testAcceptRuleText)

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "accept-engine",
		Namespace:   ns,
		GatewayName: "accept-gw",
		RuleSetName: ruleSet.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))

	reconcileEngine(t, ctx, newAcceptedEngineTestReconciler(), engine)

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(engine), &updated))
	require.NotNil(t, updated.Status)
	accepted := updated.Status.AcceptedConfiguration
	require.NotNil(t, accepted, "acceptedConfiguration must be recorded")

	assert.Equal(t, engine.Generation, accepted.ObservedGeneration)
	assert.Equal(t, int32(render.RenderingSchemaVersion), accepted.RenderingSchemaVersion)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, accepted.Version)
	assert.Equal(t, string(ruleSet.UID), accepted.RuleSetUID)
	assert.Equal(t, ruleSet.Status.AcceptedContent.ContentHash, accepted.RuleSetContentHash)
	assert.Equal(t, utils.DefaultTestWasmURL, accepted.BinaryURL)
	assert.Equal(t, utils.DefaultTestWasmSHA256, accepted.WasmSHA256)
	assert.Equal(t, wafv1alpha1.FailurePolicyFail, accepted.FailurePolicy)

	ready := apimeta.FindStatusCondition(updated.Status.Conditions, conditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, reasonConfigurationAccepted, ready.Reason)
	assert.Contains(t, ready.Message, accepted.Version)

	envoyFilter := requireEnvoyFilterExists(t, ctx, engine.Name, engine.Namespace)
	assert.Equal(t, accepted.Version, envoyFilter.GetAnnotations()["waf.k8s.coraza.io/configuration-version"])
	assert.Equal(t, bootstrapPolicyFail, envoyFilter.GetAnnotations()["waf.k8s.coraza.io/bootstrap-policy"])

	patches, found, err := unstructured.NestedSlice(envoyFilter.Object, "spec", "configPatches")
	require.NoError(t, err)
	require.True(t, found)
	assert.Len(t, patches, 2, "asset cluster and HTTP filter patches")

}

func TestEngineReconciler_CleanupNotAcceptedDeletesEnvoyFilter(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	gateway := createTestGateway(t, ctx, k8sClient, "cleanup-gw", ns)

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{Name: "cleanup-ruleset", Namespace: ns})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	acceptTestRuleSet(t, ctx, ruleSet, testAcceptRuleText)

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        "cleanup-engine",
		Namespace:   ns,
		GatewayName: "cleanup-gw",
		RuleSetName: ruleSet.Name,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))

	reconciler := newAcceptedEngineTestReconciler()
	reconcileEngine(t, ctx, reconciler, engine)
	requireEnvoyFilterExists(t, ctx, engine.Name, engine.Namespace)

	require.NoError(t, k8sClient.Delete(ctx, gateway))
	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)})
	require.NoError(t, err)

	requireNoEnvoyFilter(t, ctx, engine.Name, engine.Namespace)

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(engine), &updated))
	accepted := apimeta.FindStatusCondition(updated.Status.Conditions, conditionAccepted)
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, "TargetNotFound", accepted.Reason)
}

func TestEngineReconciler_DeferredBootstrapWeakening(t *testing.T) {
	ctx := context.Background()
	ns := setupTestNamespace(t, ctx)
	createTestGateway(t, ctx, k8sClient, "weaken-gw", ns)

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{Name: "weaken-ruleset", Namespace: ns})
	require.NoError(t, k8sClient.Create(ctx, ruleSet))
	acceptTestRuleSet(t, ctx, ruleSet, testAcceptRuleText)

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:          "weaken-engine",
		Namespace:     ns,
		GatewayName:   "weaken-gw",
		RuleSetName:   ruleSet.Name,
		WasmURL:       utils.DefaultTestWasmURL,
		WasmSHA256:    utils.DefaultTestWasmSHA256,
		FailurePolicy: wafv1alpha1.FailurePolicyAllow,
	})
	require.NoError(t, k8sClient.Create(ctx, engine))

	reconciler := newAcceptedEngineTestReconciler()
	result := reconcileEngine(t, ctx, reconciler, engine)
	assert.Equal(t, bootstrapConvergeRetryAfter, result.RequeueAfter, "weakening must converge on a follow-up reconcile")

	envoyFilter := requireEnvoyFilterExists(t, ctx, engine.Name, engine.Namespace)
	assert.Equal(t, bootstrapPolicyFail, envoyFilter.GetAnnotations()["waf.k8s.coraza.io/bootstrap-policy"],
		"fail-open must not be applied before the accepted configuration is active")

	var updated wafv1alpha1.Engine
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(engine), &updated))
	require.NotNil(t, updated.Status.AcceptedConfiguration)
	assert.Equal(t, wafv1alpha1.FailurePolicyAllow, updated.Status.AcceptedConfiguration.FailurePolicy)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(engine)})
	require.NoError(t, err)

	envoyFilter = requireEnvoyFilterExists(t, ctx, engine.Name, engine.Namespace)
	assert.Equal(t, bootstrapPolicyAllow, envoyFilter.GetAnnotations()["waf.k8s.coraza.io/bootstrap-policy"])

	patches, found, err := unstructured.NestedSlice(envoyFilter.Object, "spec", "configPatches")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, patches, 2)
	httpFilter := asMap(t, patches[1])
	patch := asMap(t, httpFilter["patch"])
	value := asMap(t, patch["value"])
	configDiscovery := asMap(t, value["config_discovery"])
	defaultConfig := asMap(t, configDiscovery["default_config"])
	rules := asMap(t, defaultConfig["rules"])
	assert.Equal(t, rbacActionDeny, rules["action"], "fail-open bootstrap allows traffic")
}
