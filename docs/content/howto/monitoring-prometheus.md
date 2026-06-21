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

## Dataplane (WAF driver) metrics

Gateway administrators can monitor WAF traffic decisions without shell access to Envoy pods. The Coraza WASM driver emits `coraza_waf_*` metrics on each Gateway pod's Envoy prometheus port (`http-envoy-prom`, port **15090**) at `/stats/prometheus`.

The operator injects `engine` and `namespace` into the WasmPlugin `pluginConfig` so metrics are labeled per Engine CRD. See the [driver metrics contract](https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/docs/driver-metrics-contract.md) for the full metric catalog.

### Scraping Gateway pods

Prefer the operator-managed PodMonitor (one per Engine, automatic gateway selector):

```yaml
# values.yaml
dataplanePodMonitor:
  enabled: true
  additionalLabels:
    release: kube-prometheus-stack   # match your Prometheus release label
```

Alternatively, enable the static Helm PodMonitor for a fixed gateway selector (not per-Engine):

```yaml
# values.yaml
metrics:
  podMonitor:
    enabled: true
    gatewaySelector:
      gateway.networking.k8s.io/gateway-name: my-gateway
```

Do **not** enable both `dataplanePodMonitor` and `metrics.podMonitor` — that causes duplicate scrapes.

Both paths include **metricRelabelings** that decode flat Envoy stat names into Prometheus labels (required on Istio 1.21+ Gateway workloads).

### Example dataplane queries

Block rate per engine:

```promql
sum by (engine, namespace) (
  rate(coraza_waf_requests_total{outcome="block"}[5m])
)
```

Top blocked rule IDs:

```promql
topk(5,
  sum by (rule_id) (
    rate(coraza_waf_rule_hits_total{outcome="block"}[5m])
  )
)
```

### Blocked request logs

When a request is interrupted in contract mode, the WASM driver emits structured JSON via `proxywasm.LogWarn` with `event=coraza_waf_blocked_request` (rule ID, client IP, URI, truncated matched data). Lines appear in **Gateway pod logs** (`wasm log …` prefix).

Collect with Promtail → Loki (see [Observability demo]({{< relref "observability-demo" >}})) or `kubectl logs` on Gateway pods.
