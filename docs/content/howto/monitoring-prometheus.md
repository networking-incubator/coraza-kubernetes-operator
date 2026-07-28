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

Counters and histograms are emitted during Coraza validation in the RuleSource and RuleSet reconcilers. The `outcome` label is `valid`, `invalid`, or (RuleSource only) `skipped`. A `valid` outcome means Coraza parsing succeeded — it does not imply the resource is Ready.

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

When the Helm chart's `metrics.prometheusRule.enabled` value is true, bundled alerts cover validation failure rates, cache hit ratio, and authentication failures on the cache server.

## Data-plane WAF metrics (Gateway / WASM)

Control-plane metrics above are from the operator. Data-plane WAF observability uses the [`coraza_waf_*` contract](https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/docs/driver-metrics-contract.md).

### OpenTelemetry Collector sidecar (log → metrics)

An OTel Collector sidecar injected into the Gateway pod reads WASM plugin logs from container log files (via `file_log` receiver with hostPath `/var/log/pods`) and materializes Prometheus counters using the `count` connector.

**Proven today** (with the default WASM image):

| Metric | Source | Labels |
|--------|--------|--------|
| `coraza_waf_rule_hits_from_logs_total` | Coraza text warnings on stderr | `engine`, `namespace`, `driver_type`, `rule_id`, `severity`, `category` |
| `waf_filter_tx_total` | Envoy stats `/stats/prometheus` | `instance`, `job` |

**Requires WASM plugin with structured JSON events** (not yet in the default image):

| Metric | Source |
|--------|--------|
| `coraza_waf_requests_total` | `coraza_waf_request` JSON event |
| `coraza_waf_blocked_requests_total` | `coraza_waf_blocked_request` JSON event |
| `coraza_waf_plugin_loads_total` | `coraza_waf_plugin_load` JSON event |

Start from the chart example:

- [`charts/coraza-kubernetes-operator/examples/otel-collector-sidecar.yaml`](https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/charts/coraza-kubernetes-operator/examples/otel-collector-sidecar.yaml)

Annotate the Gateway pod template with `sidecar.opentelemetry.io/inject: "coraza-gw-sidecar"`. The sidecar requires the [OpenTelemetry Operator](https://opentelemetry.io/docs/kubernetes/operator/) CRD. On OpenShift, JWT-protect the collector’s `:9090` exporter and scrape with a `PodMonitor` + `bearerTokenSecret`.

### Transitional: scrape Envoy stats

Gateway Envoy exposes legacy `waf_filter_*` series on `/stats/prometheus` (port **15090** `http-envoy-prom`). The OTC sidecar config already scrapes this endpoint and filters for WAF-related metrics. This path does **not** replace the contract log→metrics design and should not depend on EnvoyFilter `stats_tags`.

### Istio Telemetry

Istio `Telemetry` (`telemetry.istio.io`) controls mesh access logging and tracing. It cannot capture WASM `proxy_log()` events — those go to Envoy’s application log (stderr), not the access log subsystem. Use OTC (or another collector) for WAF observability.
