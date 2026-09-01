---
title: "Monitoring with Prometheus"
linkTitle: "Monitoring with Prometheus"
weight: 45
description: "Enable metrics collection and Prometheus monitoring for the operator."
---

The Coraza Kubernetes Operator exposes Prometheus metrics over HTTPS for monitoring the RuleSet cache server.

## Enabling the Metrics Endpoint

Metrics are enabled by default. The endpoint is served over HTTPS on port **8443** with TLS 1.3 and requires authentication via a Kubernetes ServiceAccount token.

To disable metrics:

```yaml
# values.yaml
metrics:
  enabled: false
```

## Enabling the ServiceMonitor

If you use the [Prometheus Operator](https://prometheus-operator.dev/), enable the ServiceMonitor to automatically discover the metrics endpoint:

```yaml
# values.yaml
metrics:
  serviceMonitor:
    enabled: true
```

## Configuring Prometheus RBAC

The metrics endpoint uses Kubernetes authentication. Prometheus must present a valid ServiceAccount token and the ServiceAccount must have permission to access the `/metrics` endpoint.

Create a ClusterRole and ClusterRoleBinding:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: coraza-metrics-reader
rules:
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: coraza-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: coraza-metrics-reader
subjects:
  - kind: ServiceAccount
    name: prometheus
    namespace: monitoring
```

Adjust the ServiceAccount name and namespace to match your Prometheus installation.

## Using User-Provided TLS Certificates

By default, the operator generates a self-signed certificate for the metrics endpoint. To use your own certificate:

1. Create a Secret containing the TLS certificate and key:

   ```bash
   kubectl create secret tls metrics-tls \
     --cert=tls.crt --key=tls.key \
     -n coraza-system
   ```

2. Reference it in the Helm values:

   ```yaml
   metrics:
     certSecret: metrics-tls
     certName: tls.crt
     keyName: tls.key
     caName: ca.crt   # optional: for ServiceMonitor TLS verification
   ```

## Available Metrics

### RuleSet cache server (RED)

| Metric | Type | Description |
|--------|------|-------------|
| `coraza_cache_server_requests_total` | Counter | Total number of requests. Labels: `handler`, `method`, `code`. |
| `coraza_cache_server_request_duration_seconds` | Histogram | Request duration in seconds. Labels: `handler`, `method`, `code`. |
| `coraza_cache_server_in_flight_requests` | Gauge | Number of in-flight requests. Labels: `handler`. |
| `coraza_cache_server_auth_failures_total` | Counter | Authentication failures on the cache HTTP server (invalid or missing bearer token). |

The `handler` label has two values:

- `rules` -- requests for the full compiled ruleset
- `latest` -- requests for the latest ruleset metadata

### Rule validation

Counters and histograms are emitted during Coraza validation in the RuleSource and RuleSet reconcilers. The `outcome` label is `valid`, `invalid`, or (RuleSource only) `skipped`. A `valid` outcome means Coraza parsing succeeded; it does not imply the resource is Ready.

| Metric | Type | Description |
|--------|------|-------------|
| `coraza_rulesource_validations_total` | Counter | RuleSource validation outcomes. Labels: `namespace`, `outcome`. |
| `coraza_rulesource_validation_duration_seconds` | Histogram | RuleSource validation latency. Labels: `namespace`, `outcome` (`valid` or `invalid` only). |
| `coraza_ruleset_validations_total` | Counter | RuleSet aggregate validation outcomes. Labels: `namespace`, `outcome`. |
| `coraza_ruleset_validation_duration_seconds` | Histogram | RuleSet aggregate validation latency. Labels: `namespace`, `outcome`. |

### Cache storage

| Metric | Type | Description |
|--------|------|-------------|
| `coraza_cache_set_duration_seconds` | Histogram | Time to store a compiled RuleSet in the in-memory cache. Labels: `namespace`. |

For controller resource gauges, condition metrics, and cardinality guidance, see [Metrics cardinality reference]({{< relref "../reference/metrics-cardinality" >}}).

## Data-plane WAF metrics (Gateway / WASM)

Control-plane metrics above are from the operator. Data-plane WAF observability uses the [`coraza_waf_*` contract](https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/docs/driver-metrics-contract.md).

### Central Istio ALS to platform collector

The operator does not create per-Engine OpenTelemetryCollector sidecars or patch Gateways for `sidecar.opentelemetry.io/inject`. Dataplane `coraza_waf_*` metrics come from Istio OpenTelemetry access logs (WAF filter state `wasm.io.coraza.waf.*` via `%FILTER_STATE%`) into a platform-owned central collector.

To enable this path:

1. Create the collector and configure the `waf-log-collector` ALS provider in Istio MeshConfig.
2. Add its `host:port` endpoint to the target GatewayClass:

   ```yaml
   metadata:
     annotations:
       internal.do-not-use.openshift.io/waf-otel-collector: central-waf-als-collector.coraza-central-waf-telemetry.svc.cluster.local:4317
   ```

3. Set `spec.observability.mode: Enabled` on the Engine.

Coraza then reconciles the Gateway-scoped Telemetry with `waf-log-collector`; it does not reconcile MeshConfig or the collector.

#### Scrape target

Scrape the central collector Service on port `9090`. Baseline series include `engine`, `namespace`, and `driver_type`. On the default FILTER_STATE path, `namespace` is the Gateway pod namespace and `driver_type` defaults to `wasm`; `engine` is `unknown` until filter state carries the Engine name.

#### Baseline metrics from ALS

| Metric | Notes |
|--------|--------|
| `coraza_waf_requests_total` | `outcome` + D1 labels |
| `coraza_waf_blocked_requests_total` | block `severity` / `category` |

The initial collector pipeline is baseline-only (2 of the 7 [contract](https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/docs/driver-metrics-contract.md) metrics). A per-rule `rule_id` metric is not included: bounding its cardinality safely needs a top-N/allow-list this collector pipeline cannot express. `plugin_load` / `coraza_waf_plugin_loads_total` is likewise not synthesized from access logs (lifecycle stays on the proxy log path).

### Block ratio query

```promql
sum(rate(coraza_waf_requests_total{outcome="block"}[5m]))
/
clamp_min(sum(rate(coraza_waf_requests_total[5m])), 1e-9)
```

Use a short `rate` window (5m to 15m) for a near-term pulse.

### Envoy stats scrape

Gateway Envoy may still expose legacy `waf_filter_*` series on `/stats/prometheus` (port **15090** `http-envoy-prom`). Contract `coraza_waf_*` series come from the central ALS -> collector path. Do not use EnvoyFilter `stats_tags` for tenancy labels.
