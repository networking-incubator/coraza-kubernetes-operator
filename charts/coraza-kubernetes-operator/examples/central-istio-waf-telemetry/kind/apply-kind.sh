Unable to open session log file "/home/rzago/.cache/starship/session_6009157993740115.log": Os { code: 30, kind: ReadOnlyFilesystem, message: "Read-only file system" }!
#!/usr/bin/env bash
# Apply KIND WAF ALS telemetry example end-to-end (Sail Istio + upstream OTEL).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EX_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
COLLECTOR_NS="${COLLECTOR_NS:-coraza-central-waf-telemetry}"
COLLECTOR_NAME="${COLLECTOR_NAME:-central-waf-als}"
WAF_NS="${WAF_NS:-integration-tests}"
GW="${GW:-coraza-gateway}"
ISTIO_NAME="${ISTIO_NAME:-coraza}"
SKIP_MESHCONFIG="${SKIP_MESHCONFIG:-0}"
REQUIRE_KIND_GATEWAY="${REQUIRE_KIND_GATEWAY:-1}"

step() { echo ""; echo "==> $*"; }

if ! kubectl api-resources --api-group=opentelemetry.io 2>/dev/null | grep -q opentelemetrycollectors; then
  echo "ERROR: OpenTelemetryCollector CRD missing. Run: make cluster.kind.otel" >&2
  exit 1
fi

if [[ "${REQUIRE_KIND_GATEWAY}" == "1" ]]; then
  if ! kubectl get gateway -n "${WAF_NS}" "${GW}" >/dev/null 2>&1; then
    echo "ERROR: Gateway ${WAF_NS}/${GW} not found. Run: make cluster.kind.otel" >&2
    exit 1
  fi
fi

step "Collector namespace + central OTEL collector"
kubectl apply -f "${EX_DIR}/00-namespace.yaml"
kubectl apply -f "${EX_DIR}/10-otel-collector.yaml"
kubectl rollout status -n "${COLLECTOR_NS}" "deploy/${COLLECTOR_NAME}-collector" --timeout=300s

if [[ "${SKIP_MESHCONFIG}" != "1" ]]; then
  step "MeshConfig extensionProvider waf-log-collector (FILTER_STATE ALS)"
  chmod +x "${EX_DIR}/apply-meshconfig.sh"
  ISTIO_NAME="${ISTIO_NAME}" PROVIDER=waf-log-collector \
    "${EX_DIR}/apply-meshconfig.sh"
else
  echo "SKIP_MESHCONFIG=1: assuming mesh extensionProvider already configured"
fi

step "GatewayClass collector declaration"
kubectl annotate gatewayclass istio \
  "internal.do-not-use.openshift.io/waf-otel-collector=${COLLECTOR_NAME}-collector.${COLLECTOR_NS}.svc.cluster.local:4317" \
  --overwrite

step "Echo + HTTPRoute (Gateway ${GW} from make cluster.kind)"
kubectl apply -f "${SCRIPT_DIR}/31-waf-workload.yaml"
kubectl wait -n "${WAF_NS}" "gateway/${GW}" --for=condition=Programmed --timeout=180s
kubectl wait -n "${WAF_NS}" pod -l "gateway.networking.k8s.io/gateway-name=${GW}" \
  --for=condition=Ready --timeout=180s

step "RuleSource + RuleSet + Engine"
kubectl apply -f "${SCRIPT_DIR}/40-waf-rules.yaml"
kubectl apply -f "${SCRIPT_DIR}/41-waf-engine.yaml"
kubectl wait -n "${WAF_NS}" "engine/waf-engine" --for=condition=Ready --timeout=180s

step "Done"
echo "Validate: ${SCRIPT_DIR}/50-validate-traffic.sh"
echo "Metrics:  kubectl -n ${COLLECTOR_NS} port-forward svc/${COLLECTOR_NAME}-collector 9090:9090"
echo "Preflight: hack/observability/validate-kind-waf-telemetry.sh (from repo root)"
echo "Integration: make test.integration TEST_ARGS='-run TestCentralALSMetricsPipeline -v'"
