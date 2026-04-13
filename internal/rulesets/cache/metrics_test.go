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

package cache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/networking-incubator/coraza-kubernetes-operator/test/utils"
)

func TestMetricsRegistered(t *testing.T) {
	registry := metrics.Registry

	problems, err := testutil.GatherAndLint(registry,
		"coraza_cache_server_requests_total",
		"coraza_cache_server_request_duration_seconds",
		"coraza_cache_server_in_flight_requests",
	)
	require.NoError(t, err)
	assert.Empty(t, problems, "metric lint problems")
}

func TestHandlerLabel(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/rules/ns/name/latest", "latest"},
		{"/rules/ns/name", "rules"},
		{"/rules/", "rules"},
		{"/rules/x/latest", "latest"},
		// Matches handleRules: path "latest" does not have suffix "/latest", so GetRules("latest").
		{"/rules/latest", "rules"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, handlerLabel(tt.path))
		})
	}
}

func TestStatusCapture_ExplicitWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: rec}

	sc.WriteHeader(http.StatusNotFound)
	assert.Equal(t, http.StatusNotFound, sc.status())
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestStatusCapture_ImplicitOK(t *testing.T) {
	rec := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: rec}

	_, err := sc.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, sc.status())
}

func TestStatusCapture_DefaultWithoutWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: rec}

	assert.Equal(t, http.StatusOK, sc.status())
}

func TestStatusCapture_WriteHeaderCalledOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: rec}

	sc.WriteHeader(http.StatusBadRequest)
	sc.WriteHeader(http.StatusOK) // second call should not change captured code
	assert.Equal(t, http.StatusBadRequest, sc.status())
}

func TestStatusCapture_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: rec}

	assert.Equal(t, rec, sc.Unwrap())
}

func TestInstrumentHandler_RequestCounter(t *testing.T) {
	requestsTotal.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := instrumentHandler(inner)

	req := httptest.NewRequest(http.MethodGet, "/rules/ns/name", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	val := testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "200"))
	assert.Equal(t, float64(1), val)
}

func TestInstrumentHandler_LatestEndpoint(t *testing.T) {
	requestsTotal.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := instrumentHandler(inner)

	req := httptest.NewRequest(http.MethodGet, "/rules/ns/name/latest", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	val := testutil.ToFloat64(requestsTotal.WithLabelValues("latest", "GET", "200"))
	assert.Equal(t, float64(1), val)
}

func TestInstrumentHandler_ErrorResponse(t *testing.T) {
	requestsTotal.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	handler := instrumentHandler(inner)

	req := httptest.NewRequest(http.MethodGet, "/rules/ns/name", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	val := testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "404"))
	assert.Equal(t, float64(1), val)
}

func TestInstrumentHandler_DurationObserved(t *testing.T) {
	requestDuration.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := instrumentHandler(inner)

	req := httptest.NewRequest(http.MethodGet, "/rules/ns/name", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	count := testutil.CollectAndCount(requestDuration)
	assert.Greater(t, count, 0, "histogram should have at least one metric family")
}

func TestInstrumentHandler_InFlightGauge(t *testing.T) {
	inFlightRequests.Reset()

	gaugeChecked := make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		val := testutil.ToFloat64(inFlightRequests.WithLabelValues("rules"))
		assert.Equal(t, float64(1), val, "in-flight should be 1 while request is active")
		close(gaugeChecked)
		w.WriteHeader(http.StatusOK)
	})
	handler := instrumentHandler(inner)

	req := httptest.NewRequest(http.MethodGet, "/rules/ns/name", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	<-gaugeChecked

	val := testutil.ToFloat64(inFlightRequests.WithLabelValues("rules"))
	assert.Equal(t, float64(0), val, "in-flight should be 0 after request completes")
}

func TestInstrumentHandler_MetricMetadata(t *testing.T) {
	expected := `# HELP coraza_cache_server_requests_total Total number of HTTP requests handled by the cache server.
# TYPE coraza_cache_server_requests_total counter
`
	requestsTotal.Reset()
	err := testutil.CollectAndCompare(requestsTotal, strings.NewReader(expected))
	assert.NoError(t, err, "requestsTotal metadata mismatch")
}

// Adversarial: non-GET methods must record real HTTP method and status (e.g. 405), not assume GET.
func TestInstrumentHandler_NonGETMethodAndStatus(t *testing.T) {
	requestsTotal.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})
	handler := instrumentHandler(inner)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/rules/ns/name", nil)
			handler.ServeHTTP(httptest.NewRecorder(), req)
			val := testutil.ToFloat64(requestsTotal.WithLabelValues("rules", method, "405"))
			assert.Equal(t, float64(1), val, "counter for %s", method)
			requestsTotal.Reset()
		})
	}
}

// Adversarial: paths containing "/latest" in the middle must not be labeled "latest" (suffix-only rule).
func TestHandlerLabel_LatestOnlyAsPathSuffix(t *testing.T) {
	assert.Equal(t, "rules", handlerLabel("/rules/a/latest/b"))
	assert.Equal(t, "latest", handlerLabel("/rules/a/latest"))
	// Single-segment "latest" is not CutSuffix(path, "/latest") in handleRules; label must be "rules".
	assert.Equal(t, "rules", handlerLabel("/rules/latest"))
}

func TestHandlerLabel_UnicodePath(t *testing.T) {
	assert.Equal(t, "latest", handlerLabel("/rules/ns/"+string([]rune{0x540d, 0x524d})+"/latest"))
}

// Adversarial: many overlapping requests must not leak in-flight gauge.
func TestInstrumentHandler_ConcurrentInFlightReturnsToZero(t *testing.T) {
	inFlightRequests.Reset()
	requestsTotal.Reset()

	const n = 48
	start := make(chan struct{})
	entered := make(chan struct{}, n)
	var wg sync.WaitGroup
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-start
		w.WriteHeader(http.StatusOK)
	})
	handler := instrumentHandler(inner)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/rules/concurrent", nil)
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	for i := 0; i < n; i++ {
		<-entered
	}
	close(start)
	wg.Wait()

	assert.Equal(t, float64(0), testutil.ToFloat64(inFlightRequests.WithLabelValues("rules")))
	assert.Equal(t, float64(n), testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "200")))
}

// Adversarial: panic in inner handler must still run deferred in-flight decrement (no gauge leak).
func TestInstrumentHandler_PanicDecrementsInFlight(t *testing.T) {
	inFlightRequests.Reset()
	requestsTotal.Reset()
	requestDuration.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("forced panic for middleware teardown")
	})
	handler := instrumentHandler(inner)

	req := httptest.NewRequest(http.MethodGet, "/rules/x", nil)
	defer func() {
		r := recover()
		require.NotNil(t, r)
		assert.Equal(t, float64(0), testutil.ToFloat64(inFlightRequests.WithLabelValues("rules")))
		assert.Equal(t, float64(1), testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "500")))
		assert.Greater(t, testutil.CollectAndCount(requestDuration), 0)
	}()
	handler.ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("expected panic")
}

// Panic after a status was written: metrics use that status, not 500.
func TestInstrumentHandler_PanicAfterWriteHeaderUsesWrittenCode(t *testing.T) {
	inFlightRequests.Reset()
	requestsTotal.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		panic("after header")
	})
	handler := instrumentHandler(inner)

	req := httptest.NewRequest(http.MethodGet, "/rules/x", nil)
	defer func() {
		r := recover()
		require.NotNil(t, r)
		assert.Equal(t, float64(1), testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "418")))
	}()
	handler.ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("expected panic")
}

// ---------------------------------------------------------------------------
// USE metrics
// ---------------------------------------------------------------------------

// newUSEMetricsRegistry returns an isolated prometheus.Registry and RuleSetCache
// with USE metrics registered for that registry (no shared package-level cache).
func newUSEMetricsRegistry(t *testing.T) (*prometheus.Registry, *RuleSetCache) {
	t.Helper()
	reg := prometheus.NewRegistry()
	c := NewRuleSetCache()
	require.NoError(t, RegisterUSEMetrics(reg, c, DefaultGC()))
	return reg, c
}

const (
	gcMetricEventuallyTimeout = 5 * time.Second
	gcMetricEventuallyTick    = 10 * time.Millisecond
)

// startGCLoopForTest runs rungc in the background and registers cleanup that
// cancels the context and waits for the goroutine to exit.
func startGCLoopForTest(t *testing.T, srv *ruleSetCacheServer) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.rungc(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func TestUSEMetricsRegistered(t *testing.T) {
	reg, c := newUSEMetricsRegistry(t)
	// Populate at least one cache key so coraza_cache_entries emits a sample.
	c.Put("use-lint/x", "rules", nil)

	problems, err := testutil.GatherAndLint(reg,
		MetricCacheSizeBytes,
		MetricCacheInstances,
		MetricCacheEntries,
		MetricCacheConfigMaxSizeBytes,
		MetricCacheGCPrunedEntriesTotal,
		MetricCacheGCSizeLimitExceeded,
	)
	require.NoError(t, err)
	assert.Empty(t, problems, "USE metric lint problems")
}

func TestUSEMetrics_SizeBytesReflectsCache(t *testing.T) {
	reg, c := newUSEMetricsRegistry(t)
	before := gatherGaugeValue(t, reg, MetricCacheSizeBytes)
	c.Put("use-size/a", "some rules for size test", nil)

	after := gatherGaugeValue(t, reg, MetricCacheSizeBytes)
	assert.Greater(t, after, before, "size_bytes should increase after Put")
}

func TestUSEMetrics_InstancesReflectsCache(t *testing.T) {
	reg, c := newUSEMetricsRegistry(t)
	before := gatherGaugeValue(t, reg, MetricCacheInstances)
	c.Put("use-instances/unique-key", "rules", nil)

	after := gatherGaugeValue(t, reg, MetricCacheInstances)
	assert.Equal(t, before+1, after, "instances should increase by 1 after adding a new key")
}

func TestUSEMetrics_ConfigMaxSizeBytesIsConstant(t *testing.T) {
	reg, _ := newUSEMetricsRegistry(t)
	val := gatherGaugeValue(t, reg, MetricCacheConfigMaxSizeBytes)
	assert.Equal(t, float64(DefaultGC().MaxSize), val)
}

func TestUSEMetrics_CacheEntriesPerKey(t *testing.T) {
	c := NewRuleSetCache()
	collector := newCacheEntriesCollector(c)

	c.Put("ns/foo", "v1", nil)
	c.Put("ns/foo", "v2", nil)
	c.Put("ns/bar", "v1", nil)

	ch := make(chan prometheus.Metric, 10)
	collector.Collect(ch)
	close(ch)

	byKey := map[string]float64{}
	for m := range ch {
		var d dto.Metric
		require.NoError(t, m.Write(&d))
		for _, lp := range d.GetLabel() {
			if lp.GetName() == "cache_key" {
				byKey[lp.GetValue()] = d.GetGauge().GetValue()
			}
		}
	}
	assert.Equal(t, float64(2), byKey["ns/foo"])
	assert.Equal(t, float64(1), byKey["ns/bar"])
}

func TestUSEMetrics_CacheEntriesCollectorEmitsNoStaleKeys(t *testing.T) {
	c := NewRuleSetCache()
	collector := newCacheEntriesCollector(c)

	c.Put("ns/temp", "rules", nil)
	ch := make(chan prometheus.Metric, 10)
	collector.Collect(ch)
	close(ch)
	assert.Len(t, drainMetrics(ch), 1, "should emit one sample for one key")

	// Empty cache produces zero samples (no stale labels).
	c2 := NewRuleSetCache()
	collector2 := newCacheEntriesCollector(c2)
	ch2 := make(chan prometheus.Metric, 10)
	collector2.Collect(ch2)
	close(ch2)
	assert.Empty(t, drainMetrics(ch2), "empty cache should emit zero samples")
}

func TestUSEMetrics_GCPrunedByAge(t *testing.T) {
	gcPrunedEntriesTotal.Reset()

	c := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	gc := &GarbageCollectionConfig{
		GCInterval: 50 * time.Millisecond,
		MaxAge:     100 * time.Millisecond,
		MaxSize:    1024 * 1024 * 1024,
	}
	srv := NewServer(c, ":0", logger, gc, newNoopTokenReview())

	c.Put("ns/a", "old", nil)
	c.Put("ns/a", "new", nil)
	c.SetEntryTimestamp("ns/a", 0, time.Now().Add(-200*time.Millisecond))

	startGCLoopForTest(t, srv)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(gcPrunedEntriesTotal.WithLabelValues(PruneReasonAge)) >= 1
	}, gcMetricEventuallyTimeout, gcMetricEventuallyTick, "age prune counter should increment")
}

func TestUSEMetrics_GCPrunedBySize(t *testing.T) {
	gcPrunedEntriesTotal.Reset()

	c := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	gc := &GarbageCollectionConfig{
		GCInterval: 50 * time.Millisecond,
		MaxAge:     24 * time.Hour,
		MaxSize:    10,
	}
	srv := NewServer(c, ":0", logger, gc, newNoopTokenReview())

	c.Put("ns/a", "old version with enough bytes", nil)
	c.Put("ns/a", "new version", nil)

	startGCLoopForTest(t, srv)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(gcPrunedEntriesTotal.WithLabelValues(PruneReasonSize)) >= 1
	}, gcMetricEventuallyTimeout, gcMetricEventuallyTick, "size prune counter should increment")
}

func TestRegisterUSEMetrics_IdempotentWhenCacheAndGCMatch(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewRuleSetCache()
	gc := DefaultGC()
	require.NoError(t, RegisterUSEMetrics(reg, c, gc))
	require.NoError(t, RegisterUSEMetrics(reg, c, gc))
}

func TestRegisterUSEMetrics_ConflictWhenCacheDiffers(t *testing.T) {
	reg := prometheus.NewRegistry()
	require.NoError(t, RegisterUSEMetrics(reg, NewRuleSetCache(), DefaultGC()))
	err := RegisterUSEMetrics(reg, NewRuleSetCache(), DefaultGC())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUSEMetricsRegistryConflict))
}

func TestRegisterUSEMetrics_ConflictWhenGCDiffers(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewRuleSetCache()
	require.NoError(t, RegisterUSEMetrics(reg, c, DefaultGC()))
	err := RegisterUSEMetrics(reg, c, GarbageCollectionConfig{
		GCInterval: DefaultGC().GCInterval,
		MaxAge:     DefaultGC().MaxAge,
		MaxSize:    DefaultGC().MaxSize + 1,
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUSEMetricsRegistryConflict))
}

// Adversarial: cache_key label must tolerate non-ASCII instance paths (same class of input as HTTP paths).
func TestUSEMetrics_CacheEntriesCollector_UnicodeCacheKey(t *testing.T) {
	c := NewRuleSetCache()
	collector := newCacheEntriesCollector(c)
	key := "ns/" + string([]rune{0x540d, 0x524d})
	c.Put(key, "rules", nil)

	ch := make(chan prometheus.Metric, 10)
	collector.Collect(ch)
	close(ch)

	var found bool
	for _, m := range drainMetrics(ch) {
		var d dto.Metric
		require.NoError(t, m.Write(&d))
		for _, lp := range d.GetLabel() {
			if lp.GetName() == "cache_key" && lp.GetValue() == key {
				found = true
				assert.Equal(t, float64(1), d.GetGauge().GetValue())
			}
		}
	}
	assert.True(t, found, "expected a sample for unicode cache_key %q", key)
}

func TestUSEMetrics_GCSizeLimitExceeded(t *testing.T) {
	before := testutil.ToFloat64(gcSizeLimitExceededTotal)

	c := NewRuleSetCache()
	logger := utils.NewTestLogger(t)
	gc := &GarbageCollectionConfig{
		GCInterval: 50 * time.Millisecond,
		MaxAge:     24 * time.Hour,
		MaxSize:    5,
	}
	srv := NewServer(c, ":0", logger, gc, newNoopTokenReview())

	c.Put("ns/big", "this string is definitely longer than 5 bytes", nil)

	startGCLoopForTest(t, srv)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(gcSizeLimitExceededTotal) > before
	}, gcMetricEventuallyTimeout, gcMetricEventuallyTick,
		"size-limit-exceeded counter should increment when latest entry exceeds MaxSize")
}

// gatherGaugeValue gathers the named gauge metric from a prometheus.Gatherer.
func gatherGaugeValue(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := g.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == name {
			require.NotEmpty(t, mf.GetMetric(), "metric %s has no samples", name)
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func drainMetrics(ch <-chan prometheus.Metric) []prometheus.Metric {
	out := make([]prometheus.Metric, 0)
	for m := range ch {
		out = append(out, m)
	}
	return out
}

// End-to-end: real server mux + instrumentHandler records metrics (same wiring as production).
func TestServer_InstrumentedMuxRecordsRequestMetrics(t *testing.T) {
	requestsTotal.Reset()
	inFlightRequests.Reset()

	cache := NewRuleSetCache()
	cache.Put("default/test-instance", "rules", nil)
	logger := utils.NewTestLogger(t)
	srv := NewServer(cache, ":0", logger, nil, testTokenReview())

	req := authenticatedRequest("/rules/default/test-instance")
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, float64(1), testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "200")))
	assert.Equal(t, float64(0), testutil.ToFloat64(inFlightRequests.WithLabelValues("rules")))

	req = authenticatedRequest("/rules/default/test-instance/latest")
	rec = httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, float64(1), testutil.ToFloat64(requestsTotal.WithLabelValues("latest", "GET", "200")))
	assert.Equal(t, float64(1), testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "200")))
}
