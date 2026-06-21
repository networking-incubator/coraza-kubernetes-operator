---
title: "Observability demo on KIND"
linkTitle: "Observability demo"
weight: 46
description: "Run Prometheus, Grafana, Loki, and Coraza control-plane and dataplane dashboards on a local KIND cluster."
---

This guide walks through the local observability demo: Prometheus Operator, Grafana, Loki,
Coraza operator metrics scraping, bundled control-plane dashboards, and **coraza_waf_*** dataplane
metrics from Envoy Gateway pods.

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

# 2. Deploy Prometheus, Loki, enable operator monitoring, seed demo workload
make observability.demo

The demo **rebuilds and reloads the operator image** into KIND so per-Engine PodMonitor
provisioning and contract-mode WASM flags are active. Re-running is safe.

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
- **Coraza WAF — Dataplane** — live `coraza_waf_*` request, block, rule-hit, anomaly metrics and Loki block logs

Run `make observability.grafana.url` to print credentials and dashboard UIDs.

### Dataplane demo requirements

`make observability.demo` enables:

- **Per-Engine PodMonitor** (operator-managed) scraping Envoy `:15090/stats/prometheus` with flat-stat label decode
- **Contract-mode WASM** via `CORAZA_DEMO_WASM_IMAGE` (default `oci://docker.io/rpkatz/wasmplugin:met5`)
- **Loki + Promtail** for Gateway pod logs (structured `coraza_waf_blocked_request` JSON)

Override the WASM image:

```bash
make observability.demo CORAZA_DEMO_WASM_IMAGE=oci://docker.io/rpkatz/wasmplugin:met5
```

Inspect WAF block logs from the Gateway pod:

```bash
make observability.logs.show
```

Loki query (Grafana Explore):

```logql
{namespace=~"$namespace", engine=~"$engine", event="coraza_waf_blocked_request"}
```

During **FTW conformance** (`make test.conformance`), select the generated namespace
(e.g. `crs-conformance-<id>`) and engine `conformance-engine` in the dataplane dashboard
dropdowns. CRS blocks emit ModSecurity-style audit lines; Promtail derives the `category`
label from the first `attack-*` rule tag (e.g. `xss`, `injection_php`).

Control-plane only (no dataplane scrape or contract WASM):

```bash
make OBSERVABILITY_DATAPLANE=0 observability.demo.run
```

### Overview dashboard sections

| Section | What to look for |
|---------|------------------|
| Health summary | Recording-rule stats (engines/rulesets not ready, cache hit ratio). **Cache hit ratio** shows no data when Envoy is not polling the cache (idle). |
| Validation | `coraza_*_validations_total` rates and latency — spikes on bad rule edits |
| Cache RED / USE | Request rates, auth failures, size vs limit, cache Put duration |
| Kubernetes API & workqueue | `rest_client_*` pressure and controller queue retries |

## What `make observability.demo` does

1. **`observability.prometheus.deploy`** — kube-prometheus-stack with Grafana sidecar; provisions the **Loki datasource** when dataplane mode is on.
2. **`observability.loki.deploy`** — **Loki + Promtail** (`loki-values.yaml`, `promtail-values.yaml`).
3. **`observability.operator.monitoring`** — operator ServiceMonitor, PrometheusRule, Grafana dashboards; dataplane mode enables **per-Engine PodMonitor** and contract WASM (`met5`).
4. **`observability.demo.workload`** — applies `config/samples`, seeds traffic, waits for Prometheus metrics and Loki block logs, prints sample JSON via `observability.logs.show`.

Prometheus/Grafana/Loki config: `config/observability/`. Scripts: `hack/observability/*.sh`.

## Production deployment

For production clusters with an existing Prometheus Operator installation:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack
  prometheusRule:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack
  grafanaDashboard:
    enabled: true
    folder: "Coraza WAF"

# Per-Engine PodMonitor — preferred for any Gateway + Engine pair.
dataplanePodMonitor:
  enabled: true
  additionalLabels:
    release: kube-prometheus-stack
```

Keep `metrics.podMonitor.enabled=false` when using `dataplanePodMonitor` to avoid duplicate scrapes.

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
