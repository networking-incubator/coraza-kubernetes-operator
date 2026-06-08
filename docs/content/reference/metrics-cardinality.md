---
title: "Metrics Cardinality Reference"
linkTitle: "Metrics Cardinality"
weight: 50
description: "Cardinality reference and metricRelabelings examples for the Coraza operator."
---

## Overview

Budget target: < 5,000 operator-side series for a 50-engine cluster. Gauges with
`_info` suffix follow the info-metric pattern (constant value=1, identity labels only);
numeric observations are separate gauges. Never use `_total` suffix on gauges (reserved
for counters by Prometheus convention).

## Control-Plane Metrics (operator /metrics on :8443)

### Cache Server (existing — `internal/rulesets/cache/metrics.go`)

| Metric | Type | Labels | Worst-case series/cluster | Notes |
|--------|------|--------|--------------------------|-------|
| `coraza_cache_server_requests_total` | counter | `handler`, `method`, `code` | ~6 | `handler` ∈ {`rules`, `latest`} |
| `coraza_cache_server_request_duration_seconds` | histogram | `handler`, `method`, `code` | ~6 × 11 buckets | |
| `coraza_cache_server_in_flight_requests` | gauge | `handler` | ~2 | |
| `coraza_cache_size_bytes` | gauge | (none) | 1 | |
| `coraza_cache_instances` | gauge | (none) | 1 | |
| `coraza_cache_total_entries` | gauge | (none) | 1 | |
| `coraza_cache_config_max_size_bytes` | gauge | (none) | 1 | |
| `coraza_cache_gc_pruned_entries_total` | counter | `reason` | 2 | `reason` ∈ {`age`, `size`} |
| `coraza_cache_gc_size_limit_exceeded_total` | counter | (none) | 1 | |

### Controller Observability (PR #397 — `internal/controller/metrics.go`)

> **Note:** The metrics in this section are defined in PR #397, which is not yet merged into `main`. These metrics do not exist in the current codebase. This section applies only once PR #397 is merged.

| Metric | Type | Labels | Worst-case series/cluster | Notes |
|--------|------|--------|--------------------------|-------|
| `coraza_engine_info` | gauge=1 | `namespace`, `name`, `target_name`, `target_type`, `driver_type`, `ruleset_name`, `failure_policy` | 1 per Engine | Info pattern |
| `coraza_engine_condition` | gauge | `namespace`, `name`, `condition` | 4 per Engine (Ready/Progressing/Degraded/Accepted) | |
| `coraza_engines` | gauge | `namespace` | 1 per namespace | |
| `coraza_ruleset_info` | gauge=1 | `namespace`, `name` | 1 per RuleSet | Info pattern |
| `coraza_ruleset_condition` | gauge | `namespace`, `name`, `condition` | 3 per RuleSet (Ready/Progressing/Degraded) | |
| `coraza_rulesets` | gauge | `namespace` | 1 per namespace | |
| `coraza_ruleset_sources` | gauge | `namespace`, `name` | 1 per RuleSet | |
| `coraza_ruleset_data_files` | gauge | `namespace`, `name` | 1 per RuleSet | |

### Controller-Runtime Built-ins (not `coraza_` prefixed)

These metrics are emitted automatically by controller-runtime and are not configurable
by this chart. They appear in the same scrape job as the `coraza_` metrics.

| Metric | Notes |
|--------|-------|
| `controller_runtime_reconcile_total{controller, result}` | ~4 series per controller |
| `controller_runtime_reconcile_errors_total{controller}` | ~2 series |
| `workqueue_depth{name}` | ~2 series |

Many more standard operator observability metrics are emitted. Refer to the
[controller-runtime documentation](https://book.kubebuilder.io/reference/metrics-reference)
for the full list.

## Worked Example: 50-engine cluster

```
50 Engines × (1 info + 4 conditions)                              =  250 Engine series
50 Engines grouped in 5 namespaces: 5 namespace aggregates         =    5 series
50 RuleSets × (1 info + 3 conditions + 1 sources + 1 data_files)  =  300 RuleSet series
50 RuleSets in 5 namespaces: 5 namespace aggregates              =    5 series
Cache server: ~98 series
                                                                   ─────────────────
Total coraza_* operator series:                                    ~658
```

Well within the 5,000 series budget.

## What NOT to use as label values

### Rule IDs on the operator side

The operator never sees per-rule decisions — those live in Envoy after the WAF driver
processes traffic. Adding `rule_id` labels on the operator side would require
data-plane scraping (see [Data-Plane Metrics](#data-plane-metrics) below). Rule-level
cardinality explodes quickly: CRS alone contains thousands of rules.

### IP addresses

IP addresses are unbounded cardinality and violate standard Prometheus cardinality
policy. Never use client IPs, source IPs, or any IP-derived value as a label.

### Numeric counts as label values

Use separate gauge metrics rather than encoding counts in label values. For example,
`coraza_ruleset_sources{namespace="...", name="..."}` carrying the numeric count is
correct. A label like `source_count="5"` on a parent metric is not — each distinct
count creates a new time series and the label conveys no structural identity.

### Raw error messages

Use error type or reason codes, never free-form error strings. Free-form strings have
effectively unbounded cardinality (every unique message is a new series) and are
difficult to query reliably.

## Data-Plane Metrics (coraza_waf_* — emitted by WAF driver in Envoy)

Data-plane metrics are emitted directly from the Coraza WASM driver running inside
Envoy sidecars. They are NOT scraped from the operator.

For the full specification of data-plane metric names, labels, and cardinality
constraints (including the top-N rule limit that bounds per-rule series), see the driver metrics contract documentation (available once the `feat/metrics-driver-contract` branch is merged).

**Key differences from operator metrics:**

| Property | Operator metrics | Data-plane metrics |
|----------|------------------|--------------------|
| Source | operator `/metrics` on `:8443` | Envoy admin API or PodMonitor |
| Scrape target | ServiceMonitor on operator pod | PodMonitor on Gateway pods |
| Per-rule detail | No — operator never sees rule decisions | Yes — bounded by top-N limit |
| Worst-case (10-engine cluster) | ~585 series | ~7,000 series |

## Reducing Cardinality with metricRelabelings

The Helm chart exposes `metrics.serviceMonitor.metricRelabelings` to drop or transform
series before they are stored in your TSDB. The following examples can be pasted into
`values.yaml`.

**Example 1 — drop all info metrics in large multi-tenant clusters (keep only conditions + counts):**

```yaml
metrics:
  serviceMonitor:
    metricRelabelings:
      - sourceLabels: [__name__]
        regex: 'coraza_(engine|ruleset)_info'
        action: drop
```

This reduces series by 1 per Engine and 1 per RuleSet. Useful when you have many
short-lived resources and do not need the identity labels captured in info metrics.

**Example 2 — keep only `coraza_` prefixed metrics (drop controller-runtime built-ins from this scrape job):**

```yaml
metrics:
  serviceMonitor:
    metricRelabelings:
      - sourceLabels: [__name__]
        regex: 'coraza_.*'
        action: keep
```

This is useful when controller-runtime built-ins are already collected by a
cluster-wide scrape job and you want to avoid duplication.

**Example 3 — drop high-cardinality cache histogram (keep only counter + gauge):**

```yaml
metrics:
  serviceMonitor:
    metricRelabelings:
      - sourceLabels: [__name__]
        regex: 'coraza_cache_server_request_duration_seconds.*'
        action: drop
```

The histogram generates ~84 series (6 label combinations × 14 series each: n explicit buckets + 1 `+Inf` bucket + `_count` + `_sum`). Dropping it
reduces operator-side series by ~84 while retaining the counter for request rate
calculations.
