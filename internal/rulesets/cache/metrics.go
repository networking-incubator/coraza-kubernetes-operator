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
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "coraza_cache_server_requests_total",
			Help: "Total number of HTTP requests handled by the cache server.",
		},
		[]string{"handler", "method", "code"},
	)

	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "coraza_cache_server_request_duration_seconds",
			Help:    "Duration of HTTP requests handled by the cache server in seconds.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"handler", "method", "code"},
	)

	inFlightRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "coraza_cache_server_in_flight_requests",
			Help: "Number of in-flight HTTP requests currently being handled by the cache server.",
		},
		[]string{"handler"},
	)
)

func init() {
	metrics.Registry.MustRegister(requestsTotal, requestDuration, inFlightRequests)
}

// instrumentHandler wraps an http.Handler to record RED metrics.
func instrumentHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler := handlerLabel(r.URL.Path)

		inFlightRequests.WithLabelValues(handler).Inc()
		defer inFlightRequests.WithLabelValues(handler).Dec()

		sc := &statusCapture{ResponseWriter: w}
		start := time.Now()

		defer func() {
			if p := recover(); p != nil {
				code := "500"
				if sc.written {
					code = strconv.Itoa(sc.status())
				}
				elapsed := time.Since(start).Seconds()
				requestsTotal.WithLabelValues(handler, r.Method, code).Inc()
				requestDuration.WithLabelValues(handler, r.Method, code).Observe(elapsed)
				panic(p)
			}
		}()

		next.ServeHTTP(sc, r)

		code := strconv.Itoa(sc.status())
		elapsed := time.Since(start).Seconds()

		requestsTotal.WithLabelValues(handler, r.Method, code).Inc()
		requestDuration.WithLabelValues(handler, r.Method, code).Observe(elapsed)
	})
}

// handlerLabel derives a short handler label matching handleRules routing: trim
// the "/rules/" prefix, then treat paths with suffix "/latest" as the latest
// handler (same as strings.CutSuffix(path, "/latest") in handleRules). This
// differs from a naive URL suffix check: GET /rules/latest is full-rules routing
// (cache key "latest"), not the latest-metadata endpoint.
func handlerLabel(urlPath string) string {
	if !strings.HasPrefix(urlPath, "/rules/") {
		return "rules"
	}
	path := strings.TrimPrefix(urlPath, "/rules/")
	if _, ok := strings.CutSuffix(path, "/latest"); ok {
		return "latest"
	}
	return "rules"
}

// statusCapture wraps http.ResponseWriter to record the response status code.
type statusCapture struct {
	http.ResponseWriter
	code    int
	written bool
}

// WriteHeader records the first status code and forwards to the wrapped ResponseWriter.
func (sc *statusCapture) WriteHeader(code int) {
	if !sc.written {
		sc.code = code
		sc.written = true
	}
	sc.ResponseWriter.WriteHeader(code)
}

func (sc *statusCapture) Write(b []byte) (int, error) {
	if !sc.written {
		sc.code = http.StatusOK
		sc.written = true
	}
	return sc.ResponseWriter.Write(b)
}

// status returns the captured HTTP status code, defaulting to 200 if
// WriteHeader was never called.
func (sc *statusCapture) status() int {
	if !sc.written {
		return http.StatusOK
	}
	return sc.code
}

// Unwrap supports http.ResponseController by exposing the underlying writer.
func (sc *statusCapture) Unwrap() http.ResponseWriter {
	return sc.ResponseWriter
}
