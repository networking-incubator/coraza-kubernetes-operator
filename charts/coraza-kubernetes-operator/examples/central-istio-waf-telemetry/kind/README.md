# Central Istio WAF telemetry on KIND (FILTER_STATE / CIO-shaped ALS)

KIND fixtures mirror the OpenShift path (`../openshift/`) except MeshConfig is
patched via `../apply-meshconfig.sh` on the Sail `Istio` CR (not CIO).

Requires `make cluster.kind.otel` (Istio + OTEL operator + `coraza-gateway` in
`integration-tests`). That target also installs an initial operator deployment.
For the validation, update that deployment to the PR 463 image you want to
test. If `OPERATOR_IMAGE` already points to an image in an external registry,
use it directly; no local build or `kind load docker-image` is required:

```bash
# OPERATOR_IMAGE must be an image reference including its tag.
# For example, it may be exported by the CI job that built PR 463.
test -n "${OPERATOR_IMAGE:-}" || { echo "Set OPERATOR_IMAGE to the PR 463 image first"; exit 1; }
IMAGE_REPOSITORY="${OPERATOR_IMAGE%:*}"
IMAGE_TAG="${OPERATOR_IMAGE##*:}"

helm upgrade --install coraza-kubernetes-operator \
  charts/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace \
  --set createNamespace=false \
  --set image.repository="$IMAGE_REPOSITORY" \
  --set image.tag="$IMAGE_TAG" \
  --set image.pullPolicy=Always \
  --set istio.revision=coraza
kubectl -n coraza-system rollout status deployment/coraza-kubernetes-operator --timeout=300s

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
| `../00-namespace.yaml` | Central collector namespace and its NetworkPolicy |
| `../10-otel-collector.yaml` | Sidecarless central ALS collector |
| `20-collector-servicemonitor.yaml` | Scrape the collector's `coraza_waf_*` metrics when Prometheus Operator is installed |
| `31-waf-workload.yaml` | Echo + HTTPRoute (Gateway from `make cluster.kind`) |
| `40-waf-rules.yaml` | RuleSource + RuleSet |
| `41-waf-engine.yaml` | Observability-enabled Engine; operator creates Telemetry |
| `12-otel-collector-debug-logging.yaml` | Optional OTLP log dump to collector stdout |
| `50-validate-traffic.sh` | Smoke test 200/403 |
| `55-generate-traffic-and-logs.sh` | Traffic + gateway logs + metrics |

Shared with parent dir: `../00-namespace.yaml`, `../10-otel-collector.yaml`,
`../apply-meshconfig.sh`.

## Deploy the example

Run these commands from the repository root. Every Kubernetes resource is
applied explicitly from its versioned YAML file. Shell scripts are reserved for
complementary actions: patching Sail's existing Istio CR without replacing its
other extension providers, validating traffic, and generating traffic.

```bash
# 0. Create the KIND cluster with Istio, Gateway, an initial Coraza operator,
# and the OpenTelemetry Operator. Skip this when it has already been run.
make cluster.kind.otel

# If OPERATOR_IMAGE is already set to a PR 463 image in an external registry,
# use it directly. No local build and no `kind load docker-image` are needed.
# `image.pullPolicy=Always` prevents KIND from reusing a local image with the
# same tag.
test -n "${OPERATOR_IMAGE:-}" || { echo "Set OPERATOR_IMAGE to the PR 463 image first"; exit 1; }
IMAGE_REPOSITORY="${OPERATOR_IMAGE%:*}"
IMAGE_TAG="${OPERATOR_IMAGE##*:}"
helm upgrade --install coraza-kubernetes-operator \
  charts/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace \
  --set createNamespace=false \
  --set image.repository="$IMAGE_REPOSITORY" \
  --set image.tag="$IMAGE_TAG" \
  --set image.pullPolicy=Always \
  --set istio.revision=coraza
kubectl -n coraza-system rollout status deployment/coraza-kubernetes-operator --timeout=300s
kubectl -n coraza-system get deployment coraza-kubernetes-operator \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\\n"}'
# Expected: the value of $OPERATOR_IMAGE

# Paths for the remaining commands.
REPO_ROOT="$(git rev-parse --show-toplevel)"
EX="$REPO_ROOT/charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry"
KIND="$EX/kind"

# 1. Platform-owned central ALS collector. This collector has no sidecar.
kubectl apply -f "$EX/00-namespace.yaml"
kubectl apply -f "$EX/10-otel-collector.yaml"
kubectl rollout status -n coraza-central-waf-telemetry \
  deployment/central-waf-als-collector --timeout=300s

# 2. Sail MeshConfig. This is a patch, not a YAML apply: the script preserves
# any existing extensionProviders and adds or replaces only waf-log-collector.
ISTIO_NAME=coraza PROVIDER=waf-log-collector "$EX/apply-meshconfig.sh"

# 3. Make the collector endpoint available to the GatewayClass.
kubectl annotate gatewayclass istio \
  'internal.do-not-use.openshift.io/waf-otel-collector=central-waf-als-collector.coraza-central-waf-telemetry.svc.cluster.local:4317' \
  --overwrite

# 4. Optional, but needed to display coraza_waf_* in the local Grafana stack.
make observability.prometheus.deploy observability.operator.monitoring
kubectl apply -f "$KIND/20-collector-servicemonitor.yaml"

# 5. WAF-protected workload and rules.
kubectl apply -f "$KIND/31-waf-workload.yaml"
kubectl wait -n integration-tests gateway/coraza-gateway \
  --for=condition=Programmed --timeout=180s
kubectl wait -n integration-tests pod \
  -l gateway.networking.k8s.io/gateway-name=coraza-gateway \
  --for=condition=Ready --timeout=180s
kubectl apply -f "$KIND/40-waf-rules.yaml"
kubectl apply -f "$KIND/41-waf-engine.yaml"
kubectl wait -n integration-tests engine/waf-engine \
  --for=condition=Ready --timeout=180s
```

Confirm that PR 463 created the expected central-ALS resources before sending
traffic:

```bash
kubectl get telemetry -n integration-tests coraza-engine-waf-engine-telemetry \
  -o jsonpath='{.spec.accessLogging[0].providers[0].name}{"\\n"}'
# Expected: waf-log-collector

kubectl get wasmplugin -n integration-tests coraza-engine-waf-engine \
  -o jsonpath='{.spec.pluginConfig.enable_filter_state_logs}{"\\n"}'
# Expected: true
```

Run the scripts only after the manifests are applied:

```bash
# Smoke test, then generate enough traffic for metrics and Grafana.
"$KIND/50-validate-traffic.sh"
"$KIND/55-generate-traffic-and-logs.sh"

# The preflight script checks the assembled configuration. It does not apply resources.
hack/observability/validate-kind-waf-telemetry.sh
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
