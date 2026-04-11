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
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus metric name constants for the RuleSet cache.
const (
	MetricCacheSizeBytes            = "coraza_cache_size_bytes"
	MetricCacheInstances            = "coraza_cache_instances"
	MetricCacheEntries              = "coraza_cache_entries"
	MetricCacheConfigMaxSizeBytes   = "coraza_cache_config_max_size_bytes"
	MetricCacheGCPrunedEntriesTotal = "coraza_cache_gc_pruned_entries_total"
	MetricCacheGCSizeLimitExceeded  = "coraza_cache_gc_size_limit_exceeded_total"
)

// PruneReason values label the garbage-collection prune counter.
const (
	PruneReasonAge  = "age"
	PruneReasonSize = "size"
)

var (
	gcPrunedEntriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: MetricCacheGCPrunedEntriesTotal,
			Help: "Total number of cache entries pruned by the garbage collector.",
		},
		[]string{"reason"},
	)

	gcSizeLimitExceededTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: MetricCacheGCSizeLimitExceeded,
			Help: "Total number of GC cycles where cache size still exceeded the configured maximum after pruning.",
		},
	)

	registerOnce sync.Once
)

// cacheEntriesCollector implements prometheus.Collector to emit per-cache-key
// entry counts on every scrape, avoiding stale label sets when keys are removed.
type cacheEntriesCollector struct {
	cache *RuleSetCache
	desc  *prometheus.Desc
}

func newCacheEntriesCollector(c *RuleSetCache) *cacheEntriesCollector {
	return &cacheEntriesCollector{
		cache: c,
		desc: prometheus.NewDesc(
			MetricCacheEntries,
			"Number of stored entry revisions per cache key.",
			[]string{"cache_key"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (c *cacheEntriesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector.
func (c *cacheEntriesCollector) Collect(ch chan<- prometheus.Metric) {
	for _, key := range c.cache.ListKeys() {
		count := c.cache.CountEntries(key)
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), key)
	}
}

// RegisterUSEMetrics registers USE-method (Utilization/Saturation/Errors) metrics
// for the RuleSet cache. Safe to call multiple times; registration happens at most
// once per process via sync.Once.
func RegisterUSEMetrics(reg prometheus.Registerer, c *RuleSetCache, gc GarbageCollectionConfig) {
	registerOnce.Do(func() {
		reg.MustRegister(
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
					Name: MetricCacheConfigMaxSizeBytes,
					Help: "Configured maximum cache size in bytes.",
				},
				func() float64 { return float64(gc.MaxSize) },
			),
			newCacheEntriesCollector(c),
			gcPrunedEntriesTotal,
			gcSizeLimitExceededTotal,
		)
	})
}
