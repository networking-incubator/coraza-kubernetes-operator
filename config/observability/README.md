# Local observability stack (KIND demo)

Static config for the optional Prometheus/Grafana/Loki layer used by `make observability.demo`.

| File | Purpose |
|------|---------|
| `kube-prometheus-stack-values.yaml` | Helm values for [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) |
| `grafana-loki-datasource-values.yaml` | Grafana Loki datasource (merged when `OBSERVABILITY_DATAPLANE=1`) |
| `loki-values.yaml` | [grafana/loki](https://github.com/grafana/loki/tree/main/production/helm/loki) SingleBinary demo config |
| `promtail-values.yaml` | [grafana/promtail](https://github.com/grafana/helm-charts/tree/main/charts/promtail) — ships Gateway `coraza_waf_blocked_request` JSON to Loki |
| `prometheus-rbac.yaml` | Lets Prometheus scrape the operator's authenticated `/metrics` endpoint |

Orchestration: `make observability.demo` (Prometheus + Loki/Promtail + per-Engine PodMonitor + contract WASM).

Default contract-mode WASM: `oci://docker.io/rpkatz/wasmplugin:met5` (`CORAZA_DEMO_WASM_IMAGE`).
