#!/usr/bin/env bash
# Apply OpenShift WAF ALS telemetry example through Prometheus scrape targets.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EX_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
COLLECTOR_NS="${COLLECTOR_NS:-coraza-central-waf-telemetry}"
COLLECTOR_NAME="${COLLECTOR_NAME:-central-waf-als}"
WAF_NS="${WAF_NS:-openshift-ingress}"

SKIP_OPERATOR="${SKIP_OPERATOR:-0}"
SKIP_GATEWAYCLASS="${SKIP_GATEWAYCLASS:-0}"
BUILD_OPERATOR_IMAGE="${BUILD_OPERATOR_IMAGE:-1}"

step() { echo ""; echo "==> $*"; }

require_user_workload_monitoring() {
  if oc get namespace openshift-user-workload-monitoring >/dev/null 2>&1; then
    return
  fi

  echo "ERROR: OpenShift User Workload Monitoring is not enabled." >&2
  echo "Run ${SCRIPT_DIR}/01-enable-user-workload-monitoring.sh as cluster-admin," >&2
  echo "then wait for openshift-user-workload-monitoring before applying this example." >&2
  exit 1
}

step "Prerequisite: OpenShift User Workload Monitoring"
require_user_workload_monitoring

step "GatewayClass openshift-default"
if [[ "${SKIP_GATEWAYCLASS}" != "1" ]]; then
  oc apply -f "${SCRIPT_DIR}/05-gatewayclass-openshift.yaml"
fi

step "Collector namespace + central OTEL collector"
oc apply -f "${EX_DIR}/00-namespace.yaml"
oc apply -f "${SCRIPT_DIR}/10-otel-collector-openshift.yaml"
oc rollout status -n "${COLLECTOR_NS}" "deploy/${COLLECTOR_NAME}-collector" --timeout=300s

step "Plaintext OTLP route to sidecarless collector"
oc apply -f "${SCRIPT_DIR}/11-collector-destinationrule.yaml"

step "CIO GatewayClass annotation (cluster-admin)"
chmod +x "${SCRIPT_DIR}/annotate-gatewayclass.sh" "${SCRIPT_DIR}/verify-mesh-waf-collector.sh"
"${SCRIPT_DIR}/annotate-gatewayclass.sh"
sleep 15
"${SCRIPT_DIR}/verify-mesh-waf-collector.sh"

if [[ "${SKIP_OPERATOR}" != "1" ]]; then
  step "Coraza operator (Helm)"
  BUILD_IMAGE="${BUILD_OPERATOR_IMAGE}" "${SCRIPT_DIR}/install-operator-openshift.sh"
else
  echo "SKIP_OPERATOR=1: assuming Coraza operator already installed"
fi

step "Echo + Gateway + HTTPRoute in openshift-ingress"
oc apply -f "${SCRIPT_DIR}/31-waf-workload.yaml"
oc wait -n "${WAF_NS}" "gateway/waf-gateway" --for=condition=Programmed --timeout=180s
oc wait -n "${WAF_NS}" pod -l "gateway.networking.k8s.io/gateway-name=waf-gateway" \
  --for=condition=Ready --timeout=180s

step "RuleSource + RuleSet + Engine"
oc apply -f "${SCRIPT_DIR}/40-waf-rules.yaml"
oc apply -f "${SCRIPT_DIR}/41-waf-engine.yaml"
oc wait -n "${WAF_NS}" "engine/waf-engine" --for=condition=Ready --timeout=180s

step "Gateway-scoped Telemetry (waf-log-collector ALS)"

step "OpenShift User Workload Monitoring targets"
oc apply -f "${SCRIPT_DIR}/60-prometheus-monitors.yaml"

step "Done"
echo "Validate: ${SCRIPT_DIR}/50-validate-traffic.sh"
echo "Metrics:   oc -n ${COLLECTOR_NS} port-forward svc/${COLLECTOR_NAME}-collector 9090:9090"
echo "Monitors:  ${SCRIPT_DIR}/60-prometheus-monitors.yaml"
echo "Preflight: hack/observability/validate-openshift-waf-telemetry.sh (from repo root)"
