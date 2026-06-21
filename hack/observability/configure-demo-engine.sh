#!/usr/bin/env bash
# Force demo Engine reconciliation with contract-mode WASM (bumps spec.generation).
set -euo pipefail

: "${KIND_CLUSTER_NAME:=coraza-kubernetes-operator-integration}"
: "${DEMO_NAMESPACE:=integration-tests}"
: "${ENGINE_NAME:=coraza}"
: "${CORAZA_DEMO_WASM_IMAGE:=oci://docker.io/rpkatz/wasmplugin:met5}"

KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"

kubectl_ctx() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

kubectl_ctx cluster-info &>/dev/null || {
  echo "ERROR: cluster ${KUBE_CONTEXT} not reachable" >&2
  exit 1
}

if ! kubectl_ctx -n "${DEMO_NAMESPACE}" get engine "${ENGINE_NAME}" &>/dev/null; then
  echo "Engine ${DEMO_NAMESPACE}/${ENGINE_NAME} not found; skipping WASM patch"
  exit 0
fi

echo "=== Configuring demo Engine ${DEMO_NAMESPACE}/${ENGINE_NAME} with WASM ${CORAZA_DEMO_WASM_IMAGE} ==="
kubectl_ctx -n "${DEMO_NAMESPACE}" patch engine "${ENGINE_NAME}" --type=merge \
  -p "{\"spec\":{\"driver\":{\"wasm\":{\"image\":\"${CORAZA_DEMO_WASM_IMAGE}\"}}}}"
