#!/usr/bin/env bash
# Print structured coraza_waf_blocked_request JSON lines from Gateway pod logs.
set -euo pipefail

: "${KIND_CLUSTER_NAME:=coraza-kubernetes-operator-integration}"
: "${DEMO_NAMESPACE:=integration-tests}"
: "${GATEWAY_NAME:=coraza-gateway}"
: "${BLOCKED_LOG_TAIL:=200}"

KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"

kubectl_ctx() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

command -v jq &>/dev/null || { echo "ERROR: jq required" >&2; exit 1; }
kubectl_ctx cluster-info &>/dev/null || {
  echo "ERROR: cluster ${KUBE_CONTEXT} not reachable" >&2
  exit 1
}

selector="gateway.networking.k8s.io/gateway-name=${GATEWAY_NAME}"
pod="$(kubectl_ctx -n "${DEMO_NAMESPACE}" get pod -l "${selector}" \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [ -z "${pod}" ]; then
  echo "ERROR: no Running Gateway pod for ${DEMO_NAMESPACE}/${GATEWAY_NAME}" >&2
  exit 1
fi

echo "=== WAF blocked-request logs (${DEMO_NAMESPACE}/${pod}, last ${BLOCKED_LOG_TAIL} lines) ==="
raw="$(kubectl_ctx -n "${DEMO_NAMESPACE}" logs "${pod}" --tail="${BLOCKED_LOG_TAIL}" 2>/dev/null || true)"
if [ -z "${raw}" ]; then
  echo "No log lines returned."
  exit 0
fi

found=0
while IFS= read -r line; do
  case "${line}" in
    *coraza_waf_blocked_request*)
      json="$(echo "${line}" | sed -n 's/.*\({"event":"coraza_waf_blocked_request".*}\).*/\1/p')"
      if [ -n "${json}" ] && echo "${json}" | jq -e . >/dev/null 2>&1; then
        echo "${json}" | jq -c .
        found=$((found + 1))
      else
        echo "${line}"
        found=$((found + 1))
      fi
      ;;
  esac
done <<< "${raw}"

if [ "${found}" -eq 0 ]; then
  echo "No coraza_waf_blocked_request events in the last ${BLOCKED_LOG_TAIL} lines."
  echo "Hint: contract-mode WASM + blocked traffic required; some gateways suppress Wasm Info logs."
  exit 1
fi

echo "=== ${found} blocked-request log event(s) ==="
