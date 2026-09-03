# Central Istio WAF telemetry on KIND (FILTER_STATE / CIO-shaped ALS)

KIND fixtures mirror the OpenShift path (`../openshift/`) except MeshConfig is
patched via `../apply-meshconfig.sh` on the Sail `Istio` CR (not CIO).

Requires `make cluster.kind.otel` (Istio + OTEL operator + `coraza-gateway` in
`integration-tests`). Rebuild/load the operator image from this branch before
validating the Engine-managed Telemetry:

```bash
make build.image cluster.load-images

# Optional, but required to populate the Grafana dashboard.
make observability.prometheus.deploy observability.operator.monitoring
```

Opcionalmente, publique a imagem no GHCR e reconfigure o deployment para usar
essa tag antes do teste:

```bash
export IMAGE_TAG=telemetry-engine-$(git rev-parse --short HEAD)
export OPERATOR_IMAGE=ghcr.io/rafaelvzago/coraza-kubernetes-operator:${IMAGE_TAG}
echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_USERNAME" --password-stdin
make build.image CONTROLLER_MANAGER_CONTAINER_IMAGE="$OPERATOR_IMAGE"
docker push "$OPERATOR_IMAGE"
make deploy CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE=ghcr.io/rafaelvzago/coraza-kubernetes-operator CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG="$IMAGE_TAG"
kubectl -n coraza-system rollout status deployment/coraza-kubernetes-operator --timeout=300s
```

## Manifest index

| File | Purpose |
|------|---------|
| `apply-kind.sh` | Apply collector + mesh + GatewayClass declaration + WAF |
| `20-collector-servicemonitor.yaml` | Scrape the collector's `coraza_waf_*` metrics when Prometheus Operator is installed |
| `31-waf-workload.yaml` | Echo + HTTPRoute (Gateway from `make cluster.kind`) |
| `40-waf-rules.yaml` | RuleSource + RuleSet |
| `41-waf-engine.yaml` | Observability-enabled Engine; operator creates Telemetry |
| `12-otel-collector-debug-logging.yaml` | Optional OTLP log dump to collector stdout |
| `50-validate-traffic.sh` | Smoke test 200/403 |
| `55-generate-traffic-and-logs.sh` | Traffic + gateway logs + metrics |

Shared with parent dir: `../00-namespace.yaml`, `../10-otel-collector.yaml`,
`../apply-meshconfig.sh`.

## Quick start

```bash
# From repo root (once per machine / after operator code changes)
make cluster.kind.otel
make build.image cluster.load-images

# Pipeline
cd charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry/kind
./apply-kind.sh
./55-generate-traffic-and-logs.sh
```

When the Prometheus stack is installed, wait at least 30 seconds after
generating traffic for both the gateway PodMonitor and the collector
ServiceMonitor to be scraped before opening the `Coraza WAF` dashboard.

Integration test (same pipeline, ephemeral namespace):

```bash
make test.integration TEST_ARGS='-run TestCentralALSMetricsPipeline -v'
```

Preflight: `hack/observability/validate-kind-waf-telemetry.sh` (from repo root).

## vs OpenShift

| | KIND (this dir) | OpenShift (`../openshift/`) |
|---|-----------------|----------------------------|
| MeshConfig | `../apply-meshconfig.sh` | CIO GatewayClass annotation |
| Gateway | `coraza-gateway` / `integration-tests` | `waf-gateway` / `waf-telemetry` |
| GatewayClass | `istio` (from `make cluster.kind`) | `openshift-default` |
| OTEL collector image | Upstream contrib (`10-otel-collector.yaml`) | RH pinned (`openshift/10-…`) |
