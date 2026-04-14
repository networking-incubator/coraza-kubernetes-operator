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

The operator exposes RED (Rate, Errors, Duration) metrics for the RuleSet cache server:

| Metric | Type | Description |
|--------|------|-------------|
| `coraza_cache_server_requests_total` | Counter | Total number of requests. Labels: `handler`, `method`, `code`. |
| `coraza_cache_server_request_duration_seconds` | Histogram | Request duration in seconds. Labels: `handler`, `method`, `code`. |
| `coraza_cache_server_in_flight_requests` | Gauge | Number of in-flight requests. Labels: `handler`. |

The `handler` label has two values:

- `rules` -- requests for the full compiled ruleset
- `latest` -- requests for the latest ruleset metadata
