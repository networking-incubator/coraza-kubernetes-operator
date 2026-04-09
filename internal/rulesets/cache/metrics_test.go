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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
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

// End-to-end: real server mux + instrumentHandler records metrics (same wiring as production).
func TestServer_InstrumentedMuxRecordsRequestMetrics(t *testing.T) {
	requestsTotal.Reset()
	inFlightRequests.Reset()

	cache := NewRuleSetCache()
	cache.Put("k", "rules", nil)
	logger := utils.NewTestLogger(t)
	srv := NewServer(cache, ":0", logger, nil)

	req := httptest.NewRequest(http.MethodGet, "/rules/k", nil)
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, float64(1), testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "200")))
	assert.Equal(t, float64(0), testutil.ToFloat64(inFlightRequests.WithLabelValues("rules")))
}

// Regression: GET /rules/latest routes to handleGetRules("latest"), not handleLatest; metrics must use handler=rules.
func TestServer_GETRulesLatestPathUsesRulesHandlerLabel(t *testing.T) {
	requestsTotal.Reset()
	inFlightRequests.Reset()

	cache := NewRuleSetCache()
	cache.Put("latest", "rules", nil)
	logger := utils.NewTestLogger(t)
	srv := NewServer(cache, ":0", logger, nil)

	req := httptest.NewRequest(http.MethodGet, "/rules/latest", nil)
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Equal(t, float64(1), testutil.ToFloat64(requestsTotal.WithLabelValues("rules", "GET", "200")))
	assert.Equal(t, float64(0), testutil.ToFloat64(requestsTotal.WithLabelValues("latest", "GET", "200")))
	assert.Equal(t, float64(0), testutil.ToFloat64(inFlightRequests.WithLabelValues("rules")))
}
