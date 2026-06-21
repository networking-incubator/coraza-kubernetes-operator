#!/usr/bin/env bash
# Wait until Loki indexes coraza_waf_blocked_request lines from Gateway pods.
set -euo pipefail

: "${KIND_CLUSTER_NAME:=coraza-kubernetes-operator-integration}"
: "${DEMO_NAMESPACE:=integration-tests}"
: "${MONITORING_NAMESPACE:=monitoring}"
: "${KUBE_PROM_STACK_RELEASE:=kube-prometheus-stack}"
: "${LOKI_WAIT_TIMEOUT:=180}"

KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"
LOKI_SVC="${LOKI_RELEASE:-loki}"

kubectl_ctx() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

command -v curl &>/dev/null || { echo "ERROR: curl required" >&2; exit 1; }
command -v jq &>/dev/null || { echo "ERROR: jq required" >&2; exit 1; }

pick_free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

pf_port="$(pick_free_port)"
pf_pid=""
cleanup() {
  if [ -n "${pf_pid}" ] && kill -0 "${pf_pid}" 2>/dev/null; then
    kill "${pf_pid}" 2>/dev/null || true
    wait "${pf_pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

echo "=== Waiting for WAF block logs in Loki (up to ${LOKI_WAIT_TIMEOUT}s) ==="
kubectl_ctx -n "${MONITORING_NAMESPACE}" port-forward "svc/${LOKI_SVC}" "${pf_port}:3100" >/dev/null 2>&1 &
pf_pid=$!
sleep 2

	query='{namespace="'${DEMO_NAMESPACE}'", event="coraza_waf_blocked_request"}'
deadline=$(($(date +%s) + LOKI_WAIT_TIMEOUT))
found=false

while [ "$(date +%s)" -lt "${deadline}" ]; do
  result="$(curl -sfG "http://127.0.0.1:${pf_port}/loki/api/v1/query_range" \
    --data-urlencode "query=${query}" \
    --data-urlencode "limit=1" \
    --data-urlencode "start=$(($(date +%s) - 3600))000000000" \
    --data-urlencode "end=$(date +%s)000000000" 2>/dev/null \
    | jq -r '.data.result | length' 2>/dev/null || echo 0)"
  if [ "${result}" != "0" ] && [ -n "${result}" ]; then
    echo "WAF block logs visible in Loki (streams: ${result})"
    found=true
    break
  fi
  sleep 5
done

if [ "${found}" != "true" ]; then
  echo "ERROR: coraza_waf_blocked_request not found in Loki within ${LOKI_WAIT_TIMEOUT}s" >&2
  echo "Fallback query (unstructured):" >&2
  fallback='{namespace="'${DEMO_NAMESPACE}'", container="istio-proxy"} |= "coraza_waf_blocked_request"'
  curl -sfG "http://127.0.0.1:${pf_port}/loki/api/v1/query_range" \
    --data-urlencode "query=${fallback}" \
    --data-urlencode "limit=1" \
    --data-urlencode "start=$(($(date +%s) - 3600))000000000" \
    --data-urlencode "end=$(date +%s)000000000" 2>/dev/null \
    | jq -r '.data.result | length' 2>/dev/null >&2 || true
  echo "Try: make observability.logs.show (Gateway pod logs)" >&2
  exit 1
fi
