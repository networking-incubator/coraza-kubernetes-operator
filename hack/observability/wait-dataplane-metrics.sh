#!/usr/bin/env bash
# Wait until coraza_waf_* metrics appear in Prometheus after demo traffic seeding.
set -euo pipefail

: "${KIND_CLUSTER_NAME:=coraza-kubernetes-operator-integration}"
: "${DEMO_NAMESPACE:=integration-tests}"
: "${GATEWAY_NAME:=coraza-gateway}"
: "${MONITORING_NAMESPACE:=monitoring}"
: "${KUBE_PROM_STACK_RELEASE:=kube-prometheus-stack}"
: "${DATAPLANE_METRICS_WAIT_TIMEOUT:=180}"

KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"
PROM_SVC="${KUBE_PROM_STACK_RELEASE}-prometheus"

kubectl_ctx() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

command -v curl &>/dev/null || { echo "ERROR: curl required" >&2; exit 1; }
kubectl_ctx cluster-info &>/dev/null || {
  echo "ERROR: KIND cluster context ${KUBE_CONTEXT} is not reachable" >&2
  exit 1
}

pick_free_port() {
  if command -v python3 &>/dev/null; then
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
    return
  fi
  echo 29090
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

echo "=== Waiting for coraza_waf metrics in Prometheus (up to ${DATAPLANE_METRICS_WAIT_TIMEOUT}s) ==="
kubectl_ctx -n "${MONITORING_NAMESPACE}" port-forward "svc/${PROM_SVC}" "${pf_port}:9090" >/dev/null 2>&1 &
pf_pid=$!
sleep 2

query='count(coraza_waf_requests_total{namespace="'${DEMO_NAMESPACE}'"})'
echo "Prometheus query: ${query}"
deadline=$(($(date +%s) + DATAPLANE_METRICS_WAIT_TIMEOUT))
found=false

while [ "$(date +%s)" -lt "${deadline}" ]; do
  if ! kill -0 "${pf_pid}" 2>/dev/null; then
    echo "Prometheus port-forward exited; restarting..." >&2
    kubectl_ctx -n "${MONITORING_NAMESPACE}" port-forward "svc/${PROM_SVC}" "${pf_port}:9090" >/dev/null 2>&1 &
    pf_pid=$!
    sleep 2
  fi
  result="$(curl -sfG "http://127.0.0.1:${pf_port}/api/v1/query" \
    --data-urlencode "query=${query}" 2>/dev/null | jq -r '.data.result[0].value[1] // empty' || true)"
  if [ -n "${result}" ] && [ "${result}" != "0" ]; then
    echo "Dataplane metrics visible in Prometheus (coraza_waf_requests_total series: ${result})"
    found=true
    break
  fi
  sleep 5
done

if [ "${found}" != "true" ]; then
  echo "ERROR: coraza_waf metrics not found within ${DATAPLANE_METRICS_WAIT_TIMEOUT}s" >&2
  echo "Diagnostics:" >&2
  kubectl_ctx -n "${DEMO_NAMESPACE}" get podmonitor 2>/dev/null >&2 || echo "  (no PodMonitors in ${DEMO_NAMESPACE})" >&2
  kubectl_ctx -n coraza-system get deploy coraza-kubernetes-operator \
    -o jsonpath='  operator args: {.spec.template.spec.containers[0].args}{"\n"}' 2>/dev/null >&2 || true
  raw="$(curl -sfG "http://127.0.0.1:${pf_port}/api/v1/query" \
    --data-urlencode 'query=count({__name__=~"coraza_waf_.*"})' 2>/dev/null \
    | jq -r '.data.result[0].value[1] // "none"' || echo "prometheus unreachable")"
  echo "  raw coraza_waf_* series in Prometheus: ${raw}" >&2
  echo "Check: operator image rebuilt (make build.image cluster.load-images), PodMonitor, contract WASM, Gateway pod labels (gateway-name=${GATEWAY_NAME})." >&2
  exit 1
fi
