---
title: "Observability demo on KIND"
linkTitle: "Observability demo"
weight: 46
description: "Run Prometheus, Grafana, and Coraza control-plane dashboards on a local KIND cluster."
---

This guide walks through the local observability demo: Prometheus Operator, Grafana,
Coraza operator metrics scraping, and the bundled control-plane dashboards.

## Prerequisites

- Docker (or Podman where supported by KIND)
- `kubectl`, `helm`, `kind`, `curl`, `go`, `jq`
- A completed [KIND cluster setup]({{< relref "install-kubernetes-helm" >}}) via `make cluster.kind`

The observability demo **does not** recreate the KIND cluster. It layers monitoring on
top of an existing cluster.

## Quick start

```bash
# 1. Create the KIND cluster (Istio, Gateway, operator) — unchanged from other guides
make cluster.kind

# 2. Deploy Prometheus, enable operator monitoring, seed demo workload
make observability.demo

# 3. Open Grafana (port-forward)
make observability.grafana.port-forward
```

Open [http://localhost:3000](http://localhost:3000):

| Field | Value |
|-------|-------|
| User | `admin` |
| Password | `coraza-demo` (demo only — change in production) |

Dashboards appear under folder **Coraza WAF**:

- **Coraza Operator — Overview** — health summary, validation rates/latency, reconciliation, cache RED/USE, Kubernetes API & workqueue
- **Coraza Operator — Resources** — per-namespace CR drill-down with condition tables

Run `make observability.grafana.url` to print credentials and dashboard UIDs.

### Overview dashboard sections

| Section | What to look for |
|---------|------------------|
| Health summary | Recording-rule stats (engines/rulesets not ready, cache hit ratio). **Cache hit ratio** shows no data when Envoy is not polling the cache (idle). |
| Validation | `coraza_*_validations_total` rates and latency — spikes on bad rule edits |
| Cache RED / USE | Request rates, auth failures, size vs limit, cache Put duration |
| Kubernetes API & workqueue | `rest_client_*` pressure and controller queue retries |

## What `make observability.demo` does

1. **`observability.prometheus.deploy`** — installs [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) in `monitoring`, configures Grafana dashboard sidecar (`searchNamespace: ALL`), and grants Prometheus RBAC to scrape the operator's authenticated `/metrics` endpoint.
2. **`observability.operator.monitoring`** — Helm upgrade enabling `metrics.serviceMonitor`, `metrics.prometheusRule`, and `metrics.grafanaDashboard` with labels matching the Prometheus release.
3. **`observability.demo.workload`** — applies `config/samples` into `integration-tests` and sends HTTP traffic through `coraza-gateway` to populate cache and CR metrics.

Prometheus/Grafana config: `config/observability/`. Demo orchestration is `make observability.*`; only traffic seeding uses `hack/observability/seed-traffic.sh`.

## Production deployment

For production clusters with an existing Prometheus Operator installation:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack   # match your Prometheus release label
  prometheusRule:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack
  grafanaDashboard:
    enabled: true
    folder: "Coraza WAF"
```

Ensure Prometheus can authenticate to `/metrics` — see [Monitoring with Prometheus]({{< relref "monitoring-prometheus" >}}).

Import dashboards manually by copying JSON from
`charts/coraza-kubernetes-operator/dashboards/` if you do not use the Grafana sidecar.

## Simulating alerts (manual)

The demo script does **not** intentionally degrade resources. To exercise
`PrometheusRule` alerts, validation metrics, and red overview stats manually:

### Validation metrics (Overview → Validation)

Apply a RuleSource with invalid SecLang syntax:

```bash
kubectl apply -f - <<'EOF'
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: bad-rules
  namespace: integration-tests
spec:
  rules: |
    SecDefaultActionXPTO "INVALID"
EOF
```

Within one or two scrape intervals, **RuleSource validation rate** should show an
`invalid` series and **RuleSources degraded** in the health row should rise.
Delete the object to clear the signal:

```bash
kubectl delete rulesource bad-rules -n integration-tests
```

### CorazaEngineNotReady

```bash
kubectl apply -f - <<'EOF'
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: broken-engine
  namespace: integration-tests
spec:
  ruleSet:
    name: default-ruleset
  target:
    type: Gateway
    name: nonexistent-gateway
    provider: Istio
  failurePolicy: fail
  driver:
    type: wasm
    wasm: {}
EOF
```

Wait ~5 minutes. The **Engines not ready** stat and `CorazaEngineNotReady` alert should fire.
Cleanup:

```bash
kubectl delete engine broken-engine -n integration-tests
```

### CorazaRuleSetNotReady

Create a RuleSet referencing a missing RuleSource:

```bash
kubectl apply -f - <<'EOF'
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: broken-ruleset
  namespace: integration-tests
spec:
  sources:
    - name: does-not-exist
EOF
```

### CorazaReconcileErrorRateHigh

Apply a RuleSource with invalid SecLang syntax (validation may mark it degraded and
drive reconcile errors depending on timing).

### CorazaCacheSizeHigh

Populate the cache with many distinct RuleSets or temporarily reduce the operator
`--cache-max-size` manager flag (not yet exposed as a Helm value) to approach the
configured limit. Monitor `coraza_cache_size_bytes / coraza_cache_config_max_size_bytes`
on the Overview dashboard.

## Cleanup

```bash
make observability.prometheus.undeploy
```

This removes the `monitoring` namespace and Prometheus RBAC. The Coraza operator and
KIND cluster remain (`make clean.cluster.kind` destroys the cluster).

## Makefile reference

| Target | Description |
|--------|-------------|
| `observability.demo` | Full demo orchestration |
| `observability.prometheus.deploy` | Install kube-prometheus-stack only |
| `observability.prometheus.undeploy` | Remove monitoring stack |
| `observability.operator.monitoring` | Enable operator scrape + dashboards |
| `observability.demo.workload` | Apply demo CRs and seed traffic |
| `observability.grafana.port-forward` | Forward Grafana to localhost:3000 |
| `observability.dashboard.generate` | Regenerate dashboard JSON (Go generator) |
| `observability.dashboard.test` | Run generator unit and golden parity tests |
| `observability.dashboard.validate` | Go tests + chart JSON lint (metric refs, size budget) |

## Troubleshooting

**Grafana shows empty panels**

- Confirm Prometheus target `coraza-system/coraza-kubernetes-operator/0` is UP in
  Prometheus → Status → Targets.
- Verify the operator Helm release has `metrics.serviceMonitor.enabled=true`.
- Wait 1–2 scrape intervals (30s) after seeding traffic.

**Dashboards not in Grafana**

- Check ConfigMap `coraza-kubernetes-operator-dashboards` exists in `coraza-system`
  with label `grafana_dashboard=1`.
- Confirm Grafana sidecar logs in the `monitoring` namespace.

**401 on metrics scrape**

- Apply `config/observability/prometheus-rbac.yaml` (done automatically by
  `observability.prometheus.deploy`).
