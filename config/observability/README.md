# Local observability stack (KIND demo)

Static config for the optional Prometheus/Grafana layer used by `make observability.demo`.

| File | Purpose |
|------|---------|
| `kube-prometheus-stack-values.yaml` | Helm values for [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) |
| `prometheus-rbac.yaml` | Lets Prometheus scrape the operator's authenticated `/metrics` endpoint |

Orchestration: `make observability.demo` (all steps are Makefile targets; traffic seeding uses `hack/observability/seed-traffic.sh`).
