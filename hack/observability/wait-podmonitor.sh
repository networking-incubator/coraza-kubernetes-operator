#!/usr/bin/env bash
# Wait until the operator provisions the per-Engine dataplane PodMonitor.
set -euo pipefail

: "${KIND_CLUSTER_NAME:=coraza-kubernetes-operator-integration}"
: "${DEMO_NAMESPACE:=integration-tests}"
: "${ENGINE_NAME:=coraza}"
: "${PODMONITOR_WAIT_TIMEOUT:=180}"

KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"
PODMONITOR_NAME="coraza-engine-${ENGINE_NAME}-dataplane"

kubectl_ctx() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

kubectl_ctx cluster-info &>/dev/null || {
  echo "ERROR: cluster ${KUBE_CONTEXT} not reachable" >&2
  exit 1
}

echo "=== Waiting for PodMonitor ${DEMO_NAMESPACE}/${PODMONITOR_NAME} (up to ${PODMONITOR_WAIT_TIMEOUT}s) ==="
deadline=$(($(date +%s) + PODMONITOR_WAIT_TIMEOUT))
found=false

while [ "$(date +%s)" -lt "${deadline}" ]; do
  if kubectl_ctx -n "${DEMO_NAMESPACE}" get podmonitor "${PODMONITOR_NAME}" &>/dev/null; then
    echo "PodMonitor ${PODMONITOR_NAME} exists"
    found=true
    break
  fi
  sleep 5
done

if [ "${found}" != "true" ]; then
  echo "ERROR: PodMonitor ${PODMONITOR_NAME} not found within ${PODMONITOR_WAIT_TIMEOUT}s" >&2
  echo "Operator flags:" >&2
  kubectl_ctx -n coraza-system get deploy coraza-kubernetes-operator \
    -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}' 2>/dev/null >&2 || true
  echo "Engines:" >&2
  kubectl_ctx -n "${DEMO_NAMESPACE}" get engine -o wide 2>/dev/null >&2 || true
  exit 1
fi
