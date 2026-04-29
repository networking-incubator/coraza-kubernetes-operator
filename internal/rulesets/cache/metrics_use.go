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
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrUSEMetricsRegistryConflict is returned when RegisterUSEMetrics is called
// again for the same *prometheus.Registry with a different RuleSetCache or
// GarbageCollectionConfig than the first successful registration for that registry.
var ErrUSEMetricsRegistryConflict = errors.New("USE metrics already registered for this registry with a different RuleSetCache or GarbageCollectionConfig")

// Prometheus metric name constants for the RuleSet cache.
const (
	MetricCacheSizeBytes            = "coraza_cache_size_bytes"
	MetricCacheInstances            = "coraza_cache_instances"
	MetricCacheTotalEntries         = "coraza_cache_total_entries"
	MetricCacheConfigMaxSizeBytes   = "coraza_cache_config_max_size_bytes"
	MetricCacheGCPrunedEntriesTotal = "coraza_cache_gc_pruned_entries_total"
	MetricCacheGCSizeLimitExceeded  = "coraza_cache_gc_size_limit_exceeded_total"
)

// PruneReason values label the garbage-collection prune counter.
const (
	PruneReasonAge  = "age"
	PruneReasonSize = "size"
)

type useMetricsRegistration struct {
	cache *RuleSetCache
	gc    GarbageCollectionConfig
}

// gcPrunedEntriesTotal and gcSizeLimitExceededTotal are package-level singletons
// shared across all RegisterUSEMetrics calls. They are incremented by the GC loop
// in server.go regardless of which registry is active. Tests must call
// gcPrunedEntriesTotal.Reset() / gcSizeLimitExceededTotal.Reset() in t.Cleanup
// to prevent cross-test counter accumulation.
var (
	gcPrunedEntriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricCacheGCPrunedEntriesTotal,
			Help: "Total number of cache entries pruned by the garbage collector.",
		},
		[]string{"reason"},
	)

	gcSizeLimitExceededTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricCacheGCSizeLimitExceeded,
			Help: "Total number of GC cycles where cache size still exceeded the configured maximum after pruning.",
		},
		[]string{},
	)

	registerMu sync.Mutex
	// registeredUSEMetricsByPromRegistry records the first successful registration
	// per *prometheus.Registry so repeat calls can be checked for idempotency.
	registeredUSEMetricsByPromRegistry = map[*prometheus.Registry]useMetricsRegistration{}
)

// RegisterUSEMetrics registers USE-method (Utilization/Saturation/Errors) metrics
// for the RuleSet cache on the given Registerer.
//
// For a *prometheus.Registry, a second call with the same RuleSetCache pointer
// and equal GarbageCollectionConfig returns nil (idempotent). A second call with
// the same registry but a different cache or GC config returns
// ErrUSEMetricsRegistryConflict. The GC counters are shared collector instances
// and are registered with each Registerer passed to this function.
//
// Registerers that are not *prometheus.Registry are not tracked for idempotency;
// each call to Register may return an error if a metric name is already in use.
func RegisterUSEMetrics(reg prometheus.Registerer, c *RuleSetCache, gc GarbageCollectionConfig) error {
	registerCollectors := func() error {
		collectors := []prometheus.Collector{
			prometheus.NewGaugeFunc(
				prometheus.GaugeOpts{
					Name: MetricCacheSizeBytes,
					Help: "Current total payload size of the cache in bytes.",
				},
				func() float64 { return float64(c.TotalSize()) },
			),
			prometheus.NewGaugeFunc(
				prometheus.GaugeOpts{
					Name: MetricCacheInstances,
					Help: "Number of distinct cache keys (instances) stored in the cache.",
				},
				func() float64 { return float64(c.Len()) },
			),
			prometheus.NewGaugeFunc(
				prometheus.GaugeOpts{
					Name: MetricCacheTotalEntries,
					Help: "Total number of stored entry revisions across all cache keys.",
				},
				func() float64 { return float64(c.TotalEntries()) },
			),
			prometheus.NewGaugeFunc(
				prometheus.GaugeOpts{
					Name: MetricCacheConfigMaxSizeBytes,
					Help: "Configured maximum cache size in bytes.",
				},
				func() float64 { return float64(gc.MaxSize) },
			),
			gcPrunedEntriesTotal,
			gcSizeLimitExceededTotal,
		}
		for _, col := range collectors {
			if err := reg.Register(col); err != nil {
				return err
			}
		}
		return nil
	}

	if r, ok := reg.(*prometheus.Registry); ok {
		registerMu.Lock()
		defer registerMu.Unlock()

		if prev, exists := registeredUSEMetricsByPromRegistry[r]; exists {
			match := prev.cache == c && prev.gc == gc
			if match {
				return nil
			}
			return fmt.Errorf("%w", ErrUSEMetricsRegistryConflict)
		}

		if err := registerCollectors(); err != nil {
			return err
		}
		registeredUSEMetricsByPromRegistry[r] = useMetricsRegistration{cache: c, gc: gc}
		return nil
	}

	return registerCollectors()
}
