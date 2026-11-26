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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	wafv1alpha1 "github.com/networking-incubator/coraza-kubernetes-operator/api/v1alpha1"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/ecds/render"
	"github.com/networking-incubator/coraza-kubernetes-operator/internal/rulesets/assemble"
	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

// BenchmarkEngineReconcileNoopEnvtest uses envtest with a synthetic 30-source,
// CRS-sized RuleSet and RuleData. Typed Gets and Lists use the test cache;
// unstructured Gets and writes use envtest's live API server, matching the
// controller-runtime v0.25.3 default that bypasses cache for unstructured Gets.
// Metrics is unset, so the benchmark omits RecordEngine and the deferred
// per-namespace Engine list. Treat it as a repeatable synthetic microbenchmark,
// not total production reconcile cost.
func BenchmarkEngineReconcileNoopEnvtest(b *testing.B) {
	benchmarkEngineReconcileEnvtest(b, false, false, "")
}

// BenchmarkEngineReconcileColdEnvtest measures the content assembly and render
// path after a process-local cache miss. The Kubernetes client cache stays warm.
func BenchmarkEngineReconcileColdEnvtest(b *testing.B) {
	benchmarkEngineReconcileEnvtest(b, true, false, "")
}

// BenchmarkEngineReconcileMetadataOnlyEnvtest measures metadata-only reconciles
// with full RuleSource and RuleData objects. An untimed spec-stripped preflight
// verifies the Engine coalescing path; timed GET counts alone cannot distinguish
// it from full processing. The timed loop checks for two Gets per input (current
// input read and final race check). Metadata writes and cache waits are untimed,
// but wall-time may still include residual GC, apiserver, or informer activity;
// this is not a perfectly isolated comparison.
func BenchmarkEngineReconcileMetadataOnlyEnvtest(b *testing.B) {
	benchmarkEngineReconcileEnvtest(b, false, true, "")
}

// BenchmarkEngineReconcileInputGenerationChangeEnvtest measures the acceptance
// wait after a RuleSource or RuleData spec update while the RuleSet still accepts
// the prior content. The changed input fingerprint misses the cached-input
// shortcut, so reconcile reads and assembles the inputs, detects the accepted
// hash mismatch, and requeues before rendering. It times that full-input read,
// assembly, and hash check; the update, cache propagation, and first status
// patch are excluded. Counters separate per-input cache Gets from live API Gets.
func BenchmarkEngineReconcileInputGenerationChangeEnvtest(b *testing.B) {
	for _, kind := range []string{"RuleSource", "RuleData"} {
		b.Run(kind, func(b *testing.B) {
			benchmarkEngineReconcileEnvtest(b, false, false, kind)
		})
	}
}

func benchmarkEngineReconcileEnvtest(b *testing.B, evictRenderCache, updateMetadata bool, inputGenerationChange string) {
	ctx := context.Background()
	namespace := "bench-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	gatewayName := "bench-gateway"
	engineName := "bench-engine"
	ruleSetName := "bench-rules"
	cacheReader := k8sClient.(*indexedTestClient).reader
	counts := &engineClientOperationCounts{}
	measuredClient := &engineBenchmarkClient{
		Client: k8sClient,
		cache:  cacheReader,
		counts: counts,
	}

	createBenchmarkNamespace(b, ctx, namespace)
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName, Namespace: namespace},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "istio",
			Listeners: []gatewayv1.Listener{{
				Name:     "http",
				Port:     gatewayv1.PortNumber(80),
				Protocol: gatewayv1.HTTPProtocolType,
			}},
		},
	}
	require.NoError(b, k8sClient.Create(ctx, gateway))

	sources := make([]wafv1alpha1.SourceReference, 30)
	rules := make([]string, len(sources))
	for i := range sources {
		name := fmt.Sprintf("source-%02d", i)
		sources[i] = wafv1alpha1.SourceReference{Name: name}
		rules[i] = benchmarkRuleSource(i)
		require.NoError(b, k8sClient.Create(ctx, utils.NewTestRuleSource(name, namespace, rules[i])))
	}
	var dataRules strings.Builder
	for i := range 7800 {
		fmt.Fprintf(&dataRules, "blocked-%05d.example\n", i)
	}
	dataValue := dataRules.String()
	data := utils.NewTestRuleData("bench-data", namespace, map[string]string{"blocked.data": dataValue})
	require.NoError(b, k8sClient.Create(ctx, data))

	ruleSet := utils.NewTestRuleSet(utils.RuleSetOptions{
		Name:      ruleSetName,
		Namespace: namespace,
		Sources:   sources,
		Data:      []wafv1alpha1.DataReference{{Name: data.Name}},
	})
	require.NoError(b, k8sClient.Create(ctx, ruleSet))
	var storedRuleSet wafv1alpha1.RuleSet
	require.NoError(b, k8sClient.Get(ctx, client.ObjectKeyFromObject(ruleSet), &storedRuleSet))
	inputs, err := loadRuleSetInputs(ctx, k8sClient, &storedRuleSet)
	require.NoError(b, err)
	content, err := assemble.Assemble(inputs)
	require.NoError(b, err)
	contentHash, err := render.RuleSetContentHash(content)
	require.NoError(b, err)
	patch := client.MergeFrom(storedRuleSet.DeepCopy())
	storedRuleSet.Status.AcceptedContent = &wafv1alpha1.RuleSetAcceptedContent{
		ObservedGeneration: storedRuleSet.Generation,
		ContentHash:        contentHash,
	}
	apimeta.SetStatusCondition(&storedRuleSet.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: storedRuleSet.Generation,
		Reason:             ruleSetReadyReasonRulesAccepted,
		LastTransitionTime: metav1.Now(),
	})
	require.NoError(b, k8sClient.Status().Patch(ctx, &storedRuleSet, patch))

	engine := utils.NewTestEngine(utils.EngineOptions{
		Name:        engineName,
		Namespace:   namespace,
		RuleSetName: ruleSetName,
		GatewayName: gatewayName,
		WasmURL:     utils.DefaultTestWasmURL,
		WasmSHA256:  utils.DefaultTestWasmSHA256,
	})
	require.NoError(b, k8sClient.Create(ctx, engine))

	reconciler := &EngineReconciler{
		Client:            measuredClient,
		Scheme:            scheme,
		Recorder:          utils.NewTestRecorder(),
		xdsConfig:         testXDSConfig(),
		assetBaseURL:      testAssetBaseURL,
		operatorNamespace: testNamespace,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: engine.Name, Namespace: engine.Namespace}}

	waitForEngineInputsCache(b, cacheReader, engine, &storedRuleSet, sources, data, contentHash)
	result, err := reconciler.Reconcile(ctx, req)
	require.NoError(b, err)
	require.NotZero(b, result.RequeueAfter, "the first reconcile adds the finalizer")
	waitForEngineFinalizer(b, cacheReader, engine)
	result, err = reconciler.Reconcile(ctx, req)
	require.NoError(b, err)
	require.Zero(b, result.RequeueAfter)
	waitForEngineReady(b, cacheReader, engine)
	if inputGenerationChange != "" {
		updateBenchmarkInputSpec(b, ctx, cacheReader, namespace, inputGenerationChange)
		result, err = reconciler.Reconcile(ctx, req)
		require.NoError(b, err)
		require.Equal(b, ruleSetAcceptanceRetryAfter, result.RequeueAfter)
		waitForCachedObject(b, cacheReader, req.NamespacedName, &wafv1alpha1.Engine{}, func(obj client.Object) bool {
			current := obj.(*wafv1alpha1.Engine)
			return current.Status != nil &&
				isConditionCurrent(current.Status.Conditions, conditionProgressing, reasonAwaitingRuleSetAcceptance, current.Generation)
		})
	}
	if updateMetadata {
		updateBenchmarkSourceMetadata(b, ctx, cacheReader, namespace, "preflight")

		var engineBefore wafv1alpha1.Engine
		require.NoError(b, k8sClient.Get(ctx, req.NamespacedName, &engineBefore))
		require.NotNil(b, engineBefore.Status)
		require.NotNil(b, engineBefore.Status.AcceptedConfiguration)
		require.Equal(b, engineBefore.Generation, engineBefore.Status.AcceptedConfiguration.ObservedGeneration)
		require.True(b, isConditionCurrent(engineBefore.Status.Conditions, conditionReady, reasonConfigurationAccepted, engineBefore.Generation))
		engineContentless := &ruleSetInputTestClient{
			Client:              measuredClient,
			stripRuleSourceSpec: true,
			stripRuleDataSpec:   true,
		}
		reconciler.Client = engineContentless
		result, err = reconciler.Reconcile(ctx, req)
		require.NoError(b, err)
		require.Zero(b, result.RequeueAfter)
		var engineAfter wafv1alpha1.Engine
		require.NoError(b, k8sClient.Get(ctx, req.NamespacedName, &engineAfter))
		require.NotNil(b, engineAfter.Status)
		require.NotNil(b, engineAfter.Status.AcceptedConfiguration)
		require.Equal(b, engineBefore.Status.AcceptedConfiguration, engineAfter.Status.AcceptedConfiguration)
		require.True(b, isConditionCurrent(engineAfter.Status.Conditions, conditionReady, reasonConfigurationAccepted, engineAfter.Generation))
		reconciler.Client = measuredClient
	}
	counts.reset()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		sourceGetsBefore := counts.ruleSourceGets
		dataGetsBefore := counts.ruleDataGets
		if updateMetadata {
			b.StopTimer()
			updateBenchmarkSourceMetadata(b, ctx, cacheReader, namespace, fmt.Sprint(i))
			b.StartTimer()
		}
		if evictRenderCache {
			reconciler.renderCache.delete(req.NamespacedName)
		}
		result, err = reconciler.Reconcile(ctx, req)
		if updateMetadata {
			b.StopTimer()
		}
		if err != nil {
			b.Fatalf("engine reconcile: %v", err)
		}
		wantRequeueAfter := time.Duration(0)
		if inputGenerationChange != "" {
			wantRequeueAfter = ruleSetAcceptanceRetryAfter
		}
		if result.RequeueAfter != wantRequeueAfter {
			b.Fatalf("engine reconcile requeued after %s; want %s", result.RequeueAfter, wantRequeueAfter)
		}
		if updateMetadata {
			wantSourceGets := int64(2 * len(sources))
			wantDataGets := int64(2 * len(ruleSet.Spec.Data))
			if got := counts.ruleSourceGets - sourceGetsBefore; got != wantSourceGets {
				b.Fatalf("metadata-only reconcile performed %d RuleSource Gets; want %d (identity and race check)", got, wantSourceGets)
			}
			if got := counts.ruleDataGets - dataGetsBefore; got != wantDataGets {
				b.Fatalf("metadata-only reconcile performed %d RuleData Gets; want %d (identity and race check)", got, wantDataGets)
			}
		}
		if inputGenerationChange != "" {
			wantSourceGets := int64(len(sources))
			wantDataGets := int64(len(ruleSet.Spec.Data))
			if got := counts.ruleSourceGets - sourceGetsBefore; got != wantSourceGets {
				b.Fatalf("input-generation reconcile performed %d RuleSource Gets; want %d (one full-input read)", got, wantSourceGets)
			}
			if got := counts.ruleDataGets - dataGetsBefore; got != wantDataGets {
				b.Fatalf("input-generation reconcile performed %d RuleData Gets; want %d (one full-input read)", got, wantDataGets)
			}
		}
	}
	b.StopTimer()

	counts.report(b, b.N)
	b.ReportMetric(float64(len(content.Rules)), "rule_bytes")
	b.ReportMetric(float64(len(dataValue)), "data_bytes")
}

func updateBenchmarkInputSpec(b *testing.B, ctx context.Context, cacheReader client.Reader, namespace, kind string) {
	b.Helper()
	switch kind {
	case "RuleSource":
		key := client.ObjectKey{Namespace: namespace, Name: "source-00"}
		var source wafv1alpha1.RuleSource
		require.NoError(b, k8sClient.Get(ctx, key, &source))
		generation := source.Generation
		source.Spec.Rules += "\nSecRule ARGS \"@contains generation-change\" \"id:9999999,phase:2,deny,status:403\""
		require.NoError(b, k8sClient.Update(ctx, &source))
		var updated wafv1alpha1.RuleSource
		require.NoError(b, k8sClient.Get(ctx, key, &updated))
		require.Greater(b, updated.Generation, generation)
		resourceVersion, generation := updated.ResourceVersion, updated.Generation
		waitForCachedObject(b, cacheReader, key, &wafv1alpha1.RuleSource{}, func(obj client.Object) bool {
			current := obj.(*wafv1alpha1.RuleSource)
			return current.ResourceVersion == resourceVersion && current.Generation == generation
		})
	case "RuleData":
		key := client.ObjectKey{Namespace: namespace, Name: "bench-data"}
		var data wafv1alpha1.RuleData
		require.NoError(b, k8sClient.Get(ctx, key, &data))
		require.Contains(b, data.Spec.Files, "blocked.data")
		generation := data.Generation
		data.Spec.Files["blocked.data"] += "generation-change.example\n"
		require.NoError(b, k8sClient.Update(ctx, &data))
		var updated wafv1alpha1.RuleData
		require.NoError(b, k8sClient.Get(ctx, key, &updated))
		require.Greater(b, updated.Generation, generation)
		resourceVersion, generation := updated.ResourceVersion, updated.Generation
		waitForCachedObject(b, cacheReader, key, &wafv1alpha1.RuleData{}, func(obj client.Object) bool {
			current := obj.(*wafv1alpha1.RuleData)
			return current.ResourceVersion == resourceVersion && current.Generation == generation
		})
	default:
		b.Fatalf("unknown input kind %q", kind)
	}
}

func updateBenchmarkSourceMetadata(b *testing.B, ctx context.Context, cacheReader client.Reader, namespace, iteration string) {
	b.Helper()
	sourceKey := client.ObjectKey{Namespace: namespace, Name: "source-00"}
	var source wafv1alpha1.RuleSource
	require.NoError(b, k8sClient.Get(ctx, sourceKey, &source))
	generation := source.Generation
	if source.Annotations == nil {
		source.Annotations = make(map[string]string)
	}
	source.Annotations["benchmark.coraza.io/iteration"] = iteration
	require.NoError(b, k8sClient.Update(ctx, &source))
	resourceVersion := source.ResourceVersion
	waitForCachedObject(b, cacheReader, sourceKey, &wafv1alpha1.RuleSource{}, func(obj client.Object) bool {
		current := obj.(*wafv1alpha1.RuleSource)
		return current.ResourceVersion == resourceVersion && current.Generation == generation
	})
}

func createBenchmarkNamespace(b *testing.B, ctx context.Context, name string) {
	b.Helper()
	require.NoError(b, k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
}

func benchmarkRuleSource(source int) string {
	var rules strings.Builder
	rules.Grow(24 << 10)
	for rule := 0; rules.Len() < 24<<10; rule++ {
		fmt.Fprintf(&rules,
			"SecRule REQUEST_URI \"@rx ^/probe/%02d/%06d/[a-f0-9]{12}$\" \"id:%d,phase:1,deny,status:403\"\n",
			source, rule, 100000+source*1000+rule)
	}
	return rules.String()
}

func waitForEngineInputsCache(
	t testing.TB,
	reader client.Reader,
	engine *wafv1alpha1.Engine,
	ruleSet *wafv1alpha1.RuleSet,
	sources []wafv1alpha1.SourceReference,
	data *wafv1alpha1.RuleData,
	contentHash string,
) {
	t.Helper()
	waitForCachedObject(t, reader, client.ObjectKeyFromObject(engine), &wafv1alpha1.Engine{})
	waitForCachedObject(t, reader, client.ObjectKeyFromObject(ruleSet), &wafv1alpha1.RuleSet{}, func(obj client.Object) bool {
		current := obj.(*wafv1alpha1.RuleSet)
		return current.Status.AcceptedContent != nil &&
			current.Status.AcceptedContent.ObservedGeneration == current.Generation &&
			current.Status.AcceptedContent.ContentHash == contentHash
	})
	for _, source := range sources {
		waitForCachedObject(t, reader, client.ObjectKey{Namespace: engine.Namespace, Name: source.Name}, &wafv1alpha1.RuleSource{})
	}
	waitForCachedObject(t, reader, client.ObjectKeyFromObject(data), &wafv1alpha1.RuleData{})
}

func waitForEngineFinalizer(t testing.TB, reader client.Reader, engine *wafv1alpha1.Engine) {
	t.Helper()
	waitForCachedObject(t, reader, client.ObjectKeyFromObject(engine), &wafv1alpha1.Engine{}, func(obj client.Object) bool {
		return len(obj.GetFinalizers()) != 0
	})
}

func waitForEngineReady(t testing.TB, reader client.Reader, engine *wafv1alpha1.Engine) {
	t.Helper()
	waitForCachedObject(t, reader, client.ObjectKeyFromObject(engine), &wafv1alpha1.Engine{}, func(obj client.Object) bool {
		current := obj.(*wafv1alpha1.Engine)
		return current.Status != nil && current.Status.AcceptedConfiguration != nil &&
			isConditionCurrent(current.Status.Conditions, conditionReady, reasonConfigurationAccepted, current.Generation)
	})
}

func waitForCachedObject(
	t testing.TB,
	reader client.Reader,
	key client.ObjectKey,
	obj client.Object,
	matches ...func(client.Object) bool,
) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := reader.Get(context.Background(), key, obj); err == nil {
			if len(matches) == 0 || matches[0](obj) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cache did not observe %T %s/%s", obj, key.Namespace, key.Name)
}

type engineClientOperationCounts struct {
	cacheGets      int64
	cacheLists     int64
	ruleSourceGets int64
	ruleDataGets   int64
	apiGets        int64
	apiCreates     int64
	apiUpdates     int64
	apiPatches     int64
	apiDeletes     int64
	statusPatches  int64
}

func (c *engineClientOperationCounts) reset() {
	*c = engineClientOperationCounts{}
}

func (c *engineClientOperationCounts) report(b *testing.B, iterations int) {
	b.ReportMetric(float64(c.cacheGets)/float64(iterations), "cache_gets/op")
	b.ReportMetric(float64(c.cacheLists)/float64(iterations), "cache_lists/op")
	b.ReportMetric(float64(c.ruleSourceGets)/float64(iterations), "rule_source_gets/op")
	b.ReportMetric(float64(c.ruleDataGets)/float64(iterations), "rule_data_gets/op")
	b.ReportMetric(float64(c.apiGets)/float64(iterations), "api_gets/op")
	b.ReportMetric(float64(c.apiCreates)/float64(iterations), "api_creates/op")
	b.ReportMetric(float64(c.apiUpdates)/float64(iterations), "api_updates/op")
	b.ReportMetric(float64(c.apiPatches)/float64(iterations), "api_patches/op")
	b.ReportMetric(float64(c.apiDeletes)/float64(iterations), "api_deletes/op")
	b.ReportMetric(float64(c.statusPatches)/float64(iterations), "status_patches/op")
	b.ReportMetric(float64(c.apiGets+c.apiCreates+c.apiUpdates+c.apiPatches+c.apiDeletes+c.statusPatches)/float64(iterations), "api_calls/op")
}

type engineBenchmarkClient struct {
	client.Client
	cache  client.Reader
	counts *engineClientOperationCounts
}

func (c *engineBenchmarkClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, unstructuredObject := obj.(*unstructured.Unstructured); unstructuredObject {
		c.counts.apiGets++
		return c.Client.Get(ctx, key, obj, opts...)
	}
	c.counts.cacheGets++
	switch obj.(type) {
	case *wafv1alpha1.RuleSource:
		c.counts.ruleSourceGets++
	case *wafv1alpha1.RuleData:
		c.counts.ruleDataGets++
	}
	return c.cache.Get(ctx, key, obj, opts...)
}

func (c *engineBenchmarkClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.counts.cacheLists++
	return c.Client.List(ctx, list, opts...)
}

func (c *engineBenchmarkClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.counts.apiCreates++
	return c.Client.Create(ctx, obj, opts...)
}

func (c *engineBenchmarkClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.counts.apiUpdates++
	return c.Client.Update(ctx, obj, opts...)
}

func (c *engineBenchmarkClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.counts.apiPatches++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *engineBenchmarkClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.counts.apiDeletes++
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *engineBenchmarkClient) Status() client.StatusWriter {
	return &engineBenchmarkStatusWriter{
		SubResourceWriter: c.Client.Status(),
		counts:            c.counts,
	}
}

type engineBenchmarkStatusWriter struct {
	client.SubResourceWriter
	counts *engineClientOperationCounts
}

func (w *engineBenchmarkStatusWriter) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.SubResourcePatchOption,
) error {
	w.counts.statusPatches++
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
