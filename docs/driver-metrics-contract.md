# WAF Driver Metrics Contract

## Overview

The coraza-kubernetes-operator is split into two distinct planes:

```
+----------------------------------+      +------------------------------------+
|         CONTROL PLANE            |      |          DATA PLANE                |
|  (Kubernetes operator)           |      |  (WAF driver in Envoy sidecar)     |
|                                  |      |                                    |
|  - Watches CRD state             |      |  - Intercepts HTTP requests        |
|  - Manages rule cache server     |      |  - Executes Coraza WAF evaluation  |
|  - Reconciles Engine/RuleSet     |      |  - Emits per-request decisions     |
|  - Applies WasmPlugin/SSA        |      |  - Exposes Prometheus metrics      |
|                                  |      |                                    |
|  Metrics: operator internals     |      |  Metrics: THIS document            |
|  (controller-runtime defaults)   |      |  (coraza_waf_* namespace)          |
+----------------------------------+      +------------------------------------+
         |                                          ^
         | injects engine+namespace labels          |
         | via WasmPlugin pluginConfig JSON         |
         +------------------------------------------+
```

The operator (control plane) exposes its own metrics — reconcile durations, queue depths, cache hit rates — via the standard controller-runtime metrics endpoint, collected by the ServiceMonitor Helm template.

This document defines what the **data plane MUST emit**: the `coraza_waf_*` Prometheus metrics that a WAF driver produces at request-processing time, independent of any Kubernetes API interactions.

## Applicability

Every driver implementation — WASM, Dynamic Module, or any future type — MUST emit all metrics in this contract.

Drivers that do not implement this contract cannot be merged into the coraza-kubernetes-operator project. The implementation checklist in the final section is the mandatory pre-merge gate.

## Label Injection

The operator will inject `engine` and `namespace` labels into the driver at load time via the WasmPlugin `pluginConfig` JSON field:

```json
{"engine": "my-engine", "namespace": "my-ns"}
```

> **Not yet implemented:** The operator does not currently inject `engine` and `namespace` into `pluginConfig`. This injection will be added in a future PR alongside the first driver implementation. Until then, driver prototypes should use hardcoded test values for these fields.

The driver MUST read these fields at initialization and apply them as Prometheus labels to **all** emitted metrics. Drivers that fail to read `pluginConfig` at startup MUST log an error and refuse to process traffic — a driver emitting metrics without the `engine` and `namespace` labels cannot be correlated to a specific Engine CRD instance, defeating the purpose of multi-tenant observability.

## Mandatory Metrics

All metrics listed below MUST be implemented. Metric names are exact — Prometheus performs case-sensitive, exact-string matching.

### coraza_waf_requests_total

Type: counter

Description: Total WAF-evaluated HTTP requests, partitioned by outcome.

Labels:
- `engine` — Engine CRD name (injected from pluginConfig)
- `namespace` — Engine CRD namespace (injected from pluginConfig)
- `driver_type` — one of `wasm`, `dynamic_module`
- `outcome` — one of `pass`, `block`, `detect`, `redirect`, `error`

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
- `engine` — injected from pluginConfig
- `namespace` — injected from pluginConfig
- `driver_type` — one of `wasm`, `dynamic_module`
- `rule_id` — numeric rule ID string (e.g., `"941100"`) or `"other"` for overflow
- `severity` — one of `CRITICAL`, `ERROR`, `WARNING`, `NOTICE`, `INFO`
- `outcome` — one of `block`, `detect`, `pass`

**IMPORTANT — Cardinality Bound:** Drivers MUST emit only the top-N rules by cumulative hit count (N ≤ 200). All rules beyond the top-N MUST be aggregated under `rule_id="other"`. Without this limit this metric is unbounded: CoreRuleSet alone has approximately 700 rules, multiplied by 3 outcome values, multiplied by the number of Engine instances. At 10 engines that is 21,000 time series from a single metric.

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
- `engine` — injected from pluginConfig
- `namespace` — injected from pluginConfig
- `driver_type` — one of `wasm`, `dynamic_module`

Buckets: `0, 5, 10, 15, 20, 30, 40, 50, 75, 100, +Inf`

Use cases:
- **Threshold tuning** — identify the score distribution before tightening `tx.anomaly_scoring_threshold`
- **False-positive detection** — legitimate traffic accumulating non-zero scores indicates rule tuning is needed
- **Attack intensity tracking** — shifts in p95/p99 signal campaign changes
- **Paranoia level impact** — compare score distributions before and after changing `tx.paranoia_level`

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

Description: Current count of rule override directives in effect, partitioned by type. The value reflects the live state after the most recent plugin load — it is set once at load time, not incremented per request.

Labels:
- `engine` — injected from pluginConfig
- `namespace` — injected from pluginConfig
- `driver_type` — one of `wasm`, `dynamic_module`
- `rule_id` — numeric rule ID string of the overridden rule
- `type` — one of `disabled`, `action_changed`, `tag_removed`, `threshold_changed`

**Timing:** This gauge MUST be set **once at plugin load** per override directive. It MUST NOT be modified per request. On plugin reload the gauge values are reset to reflect the new configuration — a gauge correctly represents the current override state even if overrides are removed between loads.

**Why gauge, not counter:** Rule overrides are a load-time configuration snapshot, not a monotonically increasing event stream. A counter cannot decrease when overrides are removed on reload, making the old value misleading. A gauge accurately reflects the current WAF posture at any point in time.

**Security note:** A change in this metric signals a WAF posture change — a rule was disabled or its action was altered. Operators SHOULD configure an alert on posture changes between scrapes:
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
- `engine` — injected from pluginConfig
- `namespace` — injected from pluginConfig
- `driver_type` — one of `wasm`, `dynamic_module`
- `status` — one of `success`, `failure`

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
- `engine` — injected from pluginConfig
- `namespace` — injected from pluginConfig
- `driver_type` — one of `wasm`, `dynamic_module`

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
- `engine` — injected from pluginConfig
- `namespace` — injected from pluginConfig
- `driver_type` — one of `wasm`, `dynamic_module`
- `category` — attack category (see CRS tag mapping below)
- `severity` — one of `CRITICAL`, `ERROR`, `WARNING`, `NOTICE`, `INFO`

**Severity note:** INFO-severity rules do not produce blocking actions in standard Coraza/CRS configurations. However, `SecRuleUpdateActionById` can elevate an INFO rule's action to `block`. If the driver encounters a block attributed to an INFO-severity rule, it MUST emit the counter with `severity="INFO"` — it MUST NOT silently remap to `NOTICE` or drop the increment.

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

The dominant cardinality risk is `coraza_waf_rule_hits_total`. The top-N bound with `rule_id="other"` overflow is the primary mitigation and is non-negotiable. In deployments with many Gateway replicas, consider reducing N below 200 to keep total Prometheus series within budget.

## Implementation Checklist

A driver PR MUST satisfy all of the following before merge:

- [ ] All 7 metrics implemented with the exact names defined in this document
- [ ] `engine` and `namespace` labels injected from `pluginConfig` JSON at initialization
- [ ] `coraza_waf_rule_hits_total` bounded by top-N (N≤200) with overflow aggregated to `rule_id="other"`
- [ ] `coraza_waf_rule_overrides` set (gauge) at load time, not per request; value reset on plugin reload to reflect new configuration
- [ ] Metric names match exactly (Prometheus is case-sensitive, exact-match)
- [ ] Integration test validates that all 7 metrics appear after a test HTTP request is processed
- [ ] `promtool check metrics` passes on the raw exposition output from the driver
- [ ] Cardinality budget is documented for any label dimension not in this spec (if extensions are proposed)
- [ ] Driver refuses to process traffic if `pluginConfig` is missing or malformed, with a logged error

## Scraping Configuration

The `coraza_waf_*` metrics are exposed on the Envoy stats port (`http-envoy-prom`, port 15090) at `/stats/prometheus` on Gateway pods. The operator Helm chart provides an opt-in PodMonitor to collect these metrics.

To enable scraping, set the following in your Helm values:

```yaml
metrics:
  podMonitor:
    enabled: true
    gatewaySelector:
      gateway.networking.k8s.io/gateway-name: my-gateway
```

See `charts/coraza-kubernetes-operator/values.yaml` for the full PodMonitor configuration reference, including `interval`, `scrapeTimeout`, and `metricRelabelings`.

The PodMonitor enforces a mandatory cardinality guard: only time series matching `coraza_waf_.*` are kept. This prevents ingesting Envoy's thousands of internal stats alongside WAF metrics.

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
