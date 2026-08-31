# WAF Driver Metrics Contract

## Overview

The coraza-kubernetes-operator is split into two distinct planes:

```
+----------------------------------+      +------------------------------------+
|         CONTROL PLANE            |      |          DATA PLANE                |
|  (Kubernetes operator)           |      |  (WAF driver in Envoy / Gateway)   |
|                                  |      |                                    |
|  - Watches CRD state             |      |  - Intercepts HTTP requests        |
|  - Manages rule cache server     |      |  - Executes Coraza WAF evaluation  |
|  - Reconciles Engine/RuleSet     |      |  - Emits structured WAF JSON logs  |
|  - Applies WasmPlugin/SSA        |      |  - Optional legacy waf_filter_*    |
|                                  |      |                                    |
|  Metrics: operator internals     |      |  Contract metrics: THIS document  |
|  (controller-runtime defaults)   |      |  (coraza_waf_* via central ALS)    |
+----------------------------------+      +------------------------------------+
```

The operator (control plane) exposes its own metrics - reconcile durations, queue depths, cache hit rates - via the standard controller-runtime metrics endpoint, collected by the ServiceMonitor Helm template.

This document defines the **data-plane contract**: the `coraza_waf_*` Prometheus metric names and labels that must be available end-to-end. The data-plane structured-log path is structured logs -> OpenTelemetry Collector -> Prometheus; the central ALS path is Istio ALS -> platform-owned collector -> Prometheus (see "Central Istio ALS path" below). Neither is an operator path: the operator does not emit or transport `coraza_waf_*` series itself. Drivers MAY also emit the same series directly (for example via Envoy stats).

## Applicability

Every driver implementation - WASM, Dynamic Module, or any future type - MUST make all metrics in this contract available end-to-end (directly or via a documented collector transform from driver logs).

Drivers that do not implement this contract cannot be merged into the coraza-kubernetes-operator project. The implementation checklist in the final section is the mandatory pre-merge gate.

## Label Tenancy

Every `coraza_waf_*` series MUST carry these required labels:

| Label | Meaning |
|---|---|
| `engine` | Engine CRD name |
| `namespace` | Engine CRD namespace |
| `driver_type` | one of `wasm`, `dynamic_module` |

`gateway` (Gateway CR name, typically `Engine.spec.target.name`) is **optional / supplemental**. Emitters MAY include it for operational correlation. It MUST NOT be treated as a required contract label, and cardinality guidance below does not assume it is always present.

### Central Istio ALS path

WasmPlugin `pluginConfig` carries `engine`, `namespace`, and `driver_type`. Coraza stamps those (and outcome / block fields) for Istio OpenTelemetry ALS; a platform-owned central collector materializes baseline `coraza_waf_*` series. The operator reconciles the Gateway-scoped Telemetry when Engine observability is enabled, using the Istio provider named by the target GatewayClass `internal.do-not-use.openshift.io/waf-otel-collector` annotation; it does not reconcile MeshConfig or that collector.

The central ALS path is baseline-only and does not by itself satisfy this contract.

## Mandatory Metrics

All metrics listed below MUST be implemented. Metric names are exact - Prometheus performs case-sensitive, exact-string matching.

### coraza_waf_requests_total

Type: counter

Description: Total WAF-evaluated HTTP requests, partitioned by outcome.

Labels:
- `engine` - Engine CRD name
- `namespace` - Engine CRD namespace
- `driver_type` - one of `wasm`, `dynamic_module`
- `outcome` - one of `pass`, `block`, `detect`, `redirect`, `error`

Prometheus text format example:
```
# HELP coraza_waf_requests_total Total WAF-evaluated HTTP requests by outcome.
# TYPE coraza_waf_requests_total counter
coraza_waf_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",outcome="pass"} 1024
coraza_waf_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",outcome="block"} 42
coraza_waf_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",outcome="detect"} 8
coraza_waf_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",outcome="redirect"} 3
coraza_waf_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",outcome="error"} 1
```

### coraza_waf_rule_hits_total

Type: counter

Description: Total rule match events, partitioned by rule ID, severity, and outcome.

Labels:
- `engine`
- `namespace`
- `driver_type` - one of `wasm`, `dynamic_module`
- `rule_id` - numeric rule ID string (e.g., `"941100"`) or `"other"` for overflow
- `severity` - one of `CRITICAL`, `ERROR`, `WARNING`, `NOTICE`, `INFO`
- `outcome` - one of `block`, `detect`, `pass`

**IMPORTANT - Cardinality Bound:** Drivers MUST emit only the top-N rules by cumulative hit count (N ≤ 200). All rules beyond the top-N MUST be aggregated under `rule_id="other"`. Without this limit this metric is unbounded: CoreRuleSet alone has approximately 700 rules, multiplied by 3 outcome values, multiplied by the number of Engine instances. At 10 engines that is 21,000 time series from a single metric.

The top-N window is computed per engine+namespace tuple and SHOULD be reset on plugin reload.

Prometheus text format example:
```
# HELP coraza_waf_rule_hits_total Total rule match events by rule ID, severity, and outcome.
# TYPE coraza_waf_rule_hits_total counter
coraza_waf_rule_hits_total{engine="gw-waf",namespace="prod",driver_type="wasm",rule_id="941100",severity="CRITICAL",outcome="block"} 17
coraza_waf_rule_hits_total{engine="gw-waf",namespace="prod",driver_type="wasm",rule_id="942100",severity="ERROR",outcome="detect"} 9
coraza_waf_rule_hits_total{engine="gw-waf",namespace="prod",driver_type="wasm",rule_id="other",severity="ERROR",outcome="detect"} 2341
```

### coraza_waf_request_anomaly_score

Type: histogram

Description: Distribution of per-request anomaly scores after full transaction evaluation.

Labels:
- `engine`
- `namespace`
- `driver_type` - one of `wasm`, `dynamic_module`

Buckets: `0, 5, 10, 15, 20, 30, 40, 50, 75, 100, +Inf`

Use cases:
- **Threshold tuning** - identify the score distribution before tightening `tx.anomaly_scoring_threshold`
- **False-positive detection** - legitimate traffic accumulating non-zero scores indicates rule tuning is needed
- **Attack intensity tracking** - shifts in p95/p99 signal campaign changes
- **Paranoia level impact** - compare score distributions before and after changing `tx.paranoia_level`

Prometheus text format example:
```
# HELP coraza_waf_request_anomaly_score Distribution of per-request anomaly scores.
# TYPE coraza_waf_request_anomaly_score histogram
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="0"} 890
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="5"} 950
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="10"} 970
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="15"} 980
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="20"} 990
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="30"} 998
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="40"} 999
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="50"} 1000
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="75"} 1000
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="100"} 1000
coraza_waf_request_anomaly_score_bucket{engine="gw-waf",namespace="prod",driver_type="wasm",le="+Inf"} 1000
coraza_waf_request_anomaly_score_sum{engine="gw-waf",namespace="prod",driver_type="wasm"} 4321
coraza_waf_request_anomaly_score_count{engine="gw-waf",namespace="prod",driver_type="wasm"} 1000
```

### coraza_waf_rule_overrides

Type: gauge

Description: Current count of rule override directives in effect, partitioned by type. The value reflects the live state after the most recent plugin load - it is set once at load time, not incremented per request.

Labels:
- `engine`
- `namespace`
- `driver_type` - one of `wasm`, `dynamic_module`
- `rule_id` - numeric rule ID string of the overridden rule
- `type` - one of `disabled`, `action_changed`, `tag_removed`, `threshold_changed`

**Timing:** This gauge MUST be set **once at plugin load** per override directive. It MUST NOT be modified per request. On plugin reload the gauge values are reset to reflect the new configuration - a gauge correctly represents the current override state even if overrides are removed between loads.

**Why gauge, not counter:** Rule overrides are a load-time configuration snapshot, not a monotonically increasing event stream. A counter cannot decrease when overrides are removed on reload, making the old value misleading. A gauge accurately reflects the current WAF posture at any point in time.

**Security note:** A change in this metric signals a WAF posture change - a rule was disabled or its action was altered. Operators SHOULD configure an alert on posture changes between scrapes:
```
changes(coraza_waf_rule_overrides[1h]) > 0
```

This alert fires when the gauge value changes (override added or removed), not on every scrape. Use `sum by (engine, namespace, driver_type)` if you want to alert on the total override count exceeding a threshold:
```
sum by (engine, namespace, driver_type) (coraza_waf_rule_overrides) > 10
```

Prometheus text format example:
```
# HELP coraza_waf_rule_overrides Current count of rule override directives in effect by type.
# TYPE coraza_waf_rule_overrides gauge
coraza_waf_rule_overrides{engine="gw-waf",namespace="prod",driver_type="wasm",rule_id="941100",type="disabled"} 1
coraza_waf_rule_overrides{engine="gw-waf",namespace="prod",driver_type="wasm",rule_id="942150",type="action_changed"} 1
```

### coraza_waf_plugin_loads_total

Type: counter

Description: Total plugin load attempts, partitioned by status.

Labels:
- `engine`
- `namespace`
- `driver_type` - one of `wasm`, `dynamic_module`
- `status` - one of `success`, `failure`

Prometheus text format example:
```
# HELP coraza_waf_plugin_loads_total Total plugin load attempts by status.
# TYPE coraza_waf_plugin_loads_total counter
coraza_waf_plugin_loads_total{engine="gw-waf",namespace="prod",driver_type="wasm",status="success"} 3
coraza_waf_plugin_loads_total{engine="gw-waf",namespace="prod",driver_type="wasm",status="failure"} 1
```

### coraza_waf_plugin_rule_count

Type: gauge

Description: Number of active rules after all override directives (SecRuleRemoveById, SecRuleUpdateActionById, etc.) have been applied. This reflects the live rule count, not the total rules in the ruleset before overrides.

Labels:
- `engine`
- `namespace`
- `driver_type` - one of `wasm`, `dynamic_module`

Prometheus text format example:
```
# HELP coraza_waf_plugin_rule_count Number of active rules after override directives are applied.
# TYPE coraza_waf_plugin_rule_count gauge
coraza_waf_plugin_rule_count{engine="gw-waf",namespace="prod",driver_type="wasm"} 698
```

### coraza_waf_blocked_requests_total

Type: counter

Description: Blocked requests partitioned by attack category and severity. Derived from CRS tags on the matching rule.

Labels:
- `engine`
- `namespace`
- `driver_type` - one of `wasm`, `dynamic_module`
- `category` - attack category (see CRS tag mapping below)
- `severity` - one of `CRITICAL`, `ERROR`, `WARNING`, `NOTICE`, `INFO`

**Severity note:** INFO-severity rules do not produce blocking actions in standard Coraza/CRS configurations. However, `SecRuleUpdateActionById` can elevate an INFO rule's action to `block`. If the driver encounters a block attributed to an INFO-severity rule, it MUST emit the counter with `severity="INFO"` - it MUST NOT silently remap to `NOTICE` or drop the increment.

**CRS tag to category mapping:**

| CRS tag | `category` label value |
|---|---|
| `attack-sqli` | `sqli` |
| `attack-xss` | `xss` |
| `attack-rce` | `rce` |
| `attack-lfi` | `lfi` |
| `attack-rfi` | `rfi` |
| `attack-command-injection` | `command_injection` |
| `attack-protocol` | `protocol_attack` |
| `attack-session-fixation` | `session_fixation` |
| `attack-java` | `java_attack` |
| (none of the above) | `other` |

When a blocking transaction matches multiple rules with different categories, the driver MUST emit one counter increment per distinct category present on the blocking rule chain.

Prometheus text format example:
```
# HELP coraza_waf_blocked_requests_total Blocked requests by attack category and severity.
# TYPE coraza_waf_blocked_requests_total counter
coraza_waf_blocked_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",category="sqli",severity="CRITICAL"} 12
coraza_waf_blocked_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",category="xss",severity="ERROR"} 5
coraza_waf_blocked_requests_total{engine="gw-waf",namespace="prod",driver_type="wasm",category="other",severity="WARNING"} 3
```

## Cardinality Budget Table

> **Per-pod vs. per-engine:** Values below count unique label combinations per unique `(engine, namespace)` tuple. Prometheus stores separate time series per scrape target (pod). Multiply each value by the number of Gateway pod replicas to get the total series stored in Prometheus. For example, with 3 replicas per engine, `coraza_waf_rule_hits_total` worst case is 30,150 × 3 = 90,450 series. Consider this when sizing Prometheus storage and setting the top-N limit (see `coraza_waf_rule_hits_total` below).

| Metric | Worst-case series (10 engines, per pod) | Mitigation strategy |
|---|---|---|
| `coraza_waf_requests_total` | 10 × 5 outcomes × 2 driver types = 100 | Fixed label set; no unbounded dimension |
| `coraza_waf_rule_hits_total` | 10 × 201 rule slots × 5 severities × 3 outcomes × 2 driver types = 60,300 | top-N bound (N≤200) with `rule_id="other"` overflow; lower N in multi-replica deployments |
| `coraza_waf_request_anomaly_score` | 10 × (11 buckets + sum + count) × 2 driver types = 260 | Fixed bucket set; bounded by driver types |
| `coraza_waf_rule_overrides` | 10 × (overrides per engine) × 4 types × 2 driver types; overrides are operator-controlled | Gauge reflects current state; alert on `changes()`, not cardinality |
| `coraza_waf_plugin_loads_total` | 10 × 2 driver types × 2 statuses = 40 | Fixed label set |
| `coraza_waf_plugin_rule_count` | 10 × 2 driver types = 20 | Gauge; no unbounded dimension |
| `coraza_waf_blocked_requests_total` | 10 × 9 categories × 5 severities × 2 driver types = 900 | Fixed category and severity set |

The dominant cardinality risk is `coraza_waf_rule_hits_total`. Keep the top-N bound with `rule_id="other"` overflow. With many Gateway replicas, lower N below 200 if the series budget requires it.

## Structured log events

The WASM driver emits warning-level JSON logs that collectors turn into `coraza_waf_*` series:

| Log `event` | Primary contract coverage |
|---|---|
| `coraza_waf_request` | `coraza_waf_requests_total` (`outcome`), `coraza_waf_request_anomaly_score` (`anomaly_score`) |
| `coraza_waf_blocked_request` | `coraza_waf_blocked_requests_total` (`category`, `severity`, `rule_id`) |
| `coraza_waf_plugin_load` | `coraza_waf_plugin_loads_total` (`status`), `coraza_waf_plugin_rule_count`, `coraza_waf_rule_overrides` |

`coraza_waf_rule_hits_total` (top-N) is not fully covered by per-request logs yet; enrich logs or aggregate in the collector in a follow-up.

Legacy Envoy stats (`waf_filter_tx_total`, interruption counters) can still be scraped from Envoy, but they do not satisfy the `coraza_waf_*` contract names.

## Implementation Checklist

A driver / observability PR MUST satisfy all of the following before merge:

- [ ] All metrics listed above available end-to-end with the exact names defined in this document
- [ ] Required labels `engine`, `namespace`, and `driver_type` present on every `coraza_waf_*` series (`gateway` optional only)
- [ ] `coraza_waf_rule_hits_total` bounded by top-N (N≤200) with overflow aggregated to `rule_id="other"` (when that metric is materialized)
- [ ] Load-time posture (`rule_overrides`, `plugin_rule_count`) reflected after plugin load/reload
- [ ] Metric names match exactly (Prometheus is case-sensitive, exact-match)
- [ ] Integration test validates contract metrics (or equivalent collector export) after a test HTTP request
- [ ] `promtool check metrics` passes on the exposition output (driver or collector)
- [ ] Cardinality budget is documented for any label dimension not in this spec (if extensions are proposed)

## Scraping Configuration

Scrape a platform-owned OpenTelemetry Collector that materializes `coraza_waf_*` from Istio ALS attributes. `plugin_load` remains unsupported via access logs.

Gateway pods also expose Envoy stats on `http-envoy-prom` (port **15090**; in-pod admin is often **15000**). The Helm chart ships an opt-in PodMonitor that keeps only `coraza_waf_.*` when those series appear on Envoy. Do not use EnvoyFilter `stats_tags` for tenancy labels.

```yaml
metrics:
  podMonitor:
    enabled: true
    gatewaySelector:
      gateway.networking.k8s.io/gateway-name: my-gateway
```

See `charts/coraza-kubernetes-operator/values.yaml` for PodMonitor knobs. Istio `Telemetry` (`telemetry.istio.io`) configures mesh access logs and Istio metrics; ALS->metrics conversion stays on the platform collector path.

## Example Prometheus Queries

### Top 5 blocked rule IDs (last 5 minutes)

```promql
topk(5,
  sum by (rule_id) (
    rate(coraza_waf_rule_hits_total{outcome="block"}[5m])
  )
)
```

### WAF block rate per engine (requests per second)

```promql
sum by (engine, namespace) (
  rate(coraza_waf_requests_total{outcome="block"}[5m])
)
```

### Anomaly score p95 per engine

```promql
histogram_quantile(0.95,
  sum by (engine, namespace, le) (
    rate(coraza_waf_request_anomaly_score_bucket[5m])
  )
)
```

### Engines with changed override posture (any override added or removed in the last hour)

```promql
changes(coraza_waf_rule_overrides[1h]) > 0
```

### WAF error rate (fraction of requests resulting in evaluation errors)

```promql
sum by (engine, namespace) (
  rate(coraza_waf_requests_total{outcome="error"}[5m])
)
/
sum by (engine, namespace) (
  rate(coraza_waf_requests_total[5m])
)
```
