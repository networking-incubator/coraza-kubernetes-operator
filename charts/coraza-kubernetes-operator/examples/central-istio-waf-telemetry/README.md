# Central Istio WAF telemetry (examples)

MeshConfig, Telemetry, and OpenTelemetry Collector fixtures that turn Coraza ALS attributes into baseline `coraza_waf_*` Prometheus metrics.

The Coraza operator does not reconcile these. Apply them yourself (or via mesh/platform GitOps).

Shared collector/namespace/MeshConfig scripts live in this directory. **How to run a test:**

| Environment | Directory | Entry |
|-------------|-----------|--------|
| KIND | [`kind/`](kind/) | [`kind/README.md`](kind/README.md): explicit `kubectl apply -f` commands, or `TestCentralALSMetricsPipeline` |
| OpenShift | [`openshift/`](openshift/) | `openshift/apply-openshift.sh` (OSSM + RH OTEL + CIO; not GA) |

Ownership, SCC/mTLS gaps, and a Grafana dashboard suggestion: [gist](https://gist.github.com/rafaelvzago/35440655260569dfbafa6bae31a72781) (working notes, not product docs).

## Prerequisites

- Istio (Sail) with a Coraza-protected Gateway and WasmPlugin with **filter-state logging**
  (`enable_filter_state_logs: true`; `coraza-proxy-wasm` PR #20+, for example `e20b40ca`)
- OpenTelemetry Operator CRDs (for the example `OpenTelemetryCollector`)
- Network path from Gateway proxies to the collector OTLP gRPC port

WASM image for tests and OpenShift examples:

`oci://ghcr.io/networking-incubator/coraza-proxy-wasm:e20b40ca25e3c50f212999e3decfde5503e630c3`

## Apply (shared pieces)

The KIND guide lists the complete command sequence. Its Kubernetes resources
are applied explicitly from their YAML files; helper scripts are used only for
the MeshConfig patch and traffic validation/generation. The equivalent core
commands are:

```bash
# 1) Namespace + central collector (no hostPath)
kubectl apply -f 00-namespace.yaml
kubectl apply -f 10-otel-collector.yaml

# 2) MeshConfig extensionProviders (FILTER_STATE / CIO-shaped ALS labels)
ISTIO_NAME=coraza ./apply-meshconfig.sh

# 3) Declare the collector on the GatewayClass
kubectl annotate gatewayclass istio \
  internal.do-not-use.openshift.io/waf-otel-collector=central-waf-als-collector.coraza-central-waf-telemetry.svc.cluster.local:4317 \
  --overwrite

# 4) Enable spec.observability.mode: Enabled on the Engine.
# The operator creates the Gateway-scoped Telemetry automatically.
```

Integration test: `make cluster.kind.otel` then:

```bash
KIND_CLUSTER_NAME=coraza-kubernetes-operator-integration \
go test -tags=integration ./test/integration/... -run TestCentralALSMetricsPipeline -v -count=1
```

Scrape: `kubectl -n coraza-central-waf-telemetry port-forward svc/central-waf-als-collector 9090:9090`

## Metrics (baseline-only)

This example is **baseline-only**: it implements 2 of the 7 metrics mandatory
under [`docs/driver-metrics-contract.md`](../../../../docs/driver-metrics-contract.md).
It does not by itself satisfy the full data-plane contract.

| Metric | Source |
|---|---|
| `coraza_waf_requests_total` | ALS `outcome` + D1 labels |
| `coraza_waf_blocked_requests_total` | ALS block + `severity` / `category` |

Required D1 labels: `engine`, `namespace`, `driver_type`. On the default FILTER_STATE path, `namespace` comes from `%ENVIRONMENT(POD_NAMESPACE)%` and `driver_type` defaults to `wasm` in the collector. `engine` is `unknown` until WASM filter state (or ALS labels) carry the Engine name; the operator still writes `engine` / `namespace` / `driver_type` on WasmPlugin `pluginConfig`, and `TestCentralALSMetricsPipeline` asserts `namespace` and `driver_type` on a block series, not `engine`.

### Not implemented by this example

- `coraza_waf_rule_hits_total` (top-N with `rule_id="other"` overflow), `coraza_waf_request_anomaly_score`, `coraza_waf_rule_overrides`, `coraza_waf_plugin_loads_total`, `coraza_waf_plugin_rule_count` — get these from the WASM driver's structured logs per the [driver metrics contract](../../../../docs/driver-metrics-contract.md), not from this collector. A per-rule `rule_id` metric needs a top-N/allow-list bound this collector pipeline cannot express safely (see [metrics-cardinality.md](../../../../docs/content/reference/metrics-cardinality.md)), so it is intentionally omitted here rather than shipped unbounded.
- `coraza_waf_plugin_loads_total` / `plugin_load` is a lifecycle signal on the proxy log path. These examples do not synthesize it from access logs.

## Volume / filtering (#62)

On Istio 1.30.3:

- **Default (OpenShift / CIO-aligned):** ALS labels use `%FILTER_STATE(wasm.io.coraza.waf.*:PLAIN)%`
  from `coraza-proxy-wasm` filter state (interruption / block path today).
- Do not use Telemetry CEL on `response.headers[...]` (LDS reject).
- Prefer Gateway-scoped Telemetry and collector conditions that drop `outcome` `unknown` / `-`.
