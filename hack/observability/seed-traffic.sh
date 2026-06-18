#!/usr/bin/env bash
# Port-forward to coraza-gateway and send HTTP traffic for dashboard metrics.
set -euo pipefail

: "${KIND_CLUSTER_NAME:=coraza-kubernetes-operator-integration}"
: "${DEMO_NAMESPACE:=integration-tests}"
: "${GATEWAY_NAME:=coraza-gateway}"
: "${GW_WAIT_TIMEOUT:=300s}"
: "${HTTP_WAIT_TIMEOUT:=120}"

KUBE_CONTEXT="kind-${KIND_CLUSTER_NAME}"

kubectl_ctx() {
  kubectl --context "${KUBE_CONTEXT}" "$@"
}

pick_free_port() {
  if [ -n "${OBSERVABILITY_GW_PORT:-}" ]; then
    echo "${OBSERVABILITY_GW_PORT}"
    return
  fi
  if command -v python3 &>/dev/null; then
    python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
    return
  fi
  local port=29100
  while ss -tln 2>/dev/null | grep -q ":${port} "; do
    port=$((port + 1))
  done
  echo "${port}"
}

command -v curl &>/dev/null || { echo "ERROR: curl required" >&2; exit 1; }
kubectl_ctx cluster-info &>/dev/null || {
  echo "ERROR: KIND cluster context ${KUBE_CONTEXT} is not reachable. Run: make cluster.kind" >&2
  exit 1
}

echo "=== Waiting for Gateway ${GATEWAY_NAME} to be Programmed ==="
if ! kubectl_ctx -n "${DEMO_NAMESPACE}" wait --for=condition=Programmed \
  "gateway/${GATEWAY_NAME}" --timeout="${GW_WAIT_TIMEOUT}" 2>/dev/null; then
  echo "ERROR: Gateway ${DEMO_NAMESPACE}/${GATEWAY_NAME} not Programmed within ${GW_WAIT_TIMEOUT}" >&2
  exit 1
fi

echo "=== Waiting for Gateway pod Ready ==="
if ! kubectl_ctx -n "${DEMO_NAMESPACE}" wait --for=condition=Ready \
  pod -l "gateway.networking.k8s.io/gateway-name=${GATEWAY_NAME}" \
  --timeout="${GW_WAIT_TIMEOUT}" 2>/dev/null; then
  echo "ERROR: Gateway pod for ${GATEWAY_NAME} not Ready within ${GW_WAIT_TIMEOUT}" >&2
  exit 1
fi

gw_pod="$(kubectl_ctx -n "${DEMO_NAMESPACE}" get pod \
  -l "gateway.networking.k8s.io/gateway-name=${GATEWAY_NAME}" \
  --field-selector=status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [ -z "${gw_pod}" ]; then
  echo "ERROR: Gateway pod not found in ${DEMO_NAMESPACE}" >&2
  exit 1
fi

pf_port="$(pick_free_port)"
pf_pid=""
pf_target="pod/${gw_pod}"
cleanup() {
  if [ -n "${pf_pid}" ] && kill -0 "${pf_pid}" 2>/dev/null; then
    kill "${pf_pid}" 2>/dev/null || true
    wait "${pf_pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

start_port_forward() {
  if [ -n "${pf_pid}" ] && kill -0 "${pf_pid}" 2>/dev/null; then
    kill "${pf_pid}" 2>/dev/null || true
    wait "${pf_pid}" 2>/dev/null || true
  fi
  kubectl_ctx -n "${DEMO_NAMESPACE}" port-forward "${pf_target}" "${pf_port}:80" >/dev/null 2>&1 &
  pf_pid=$!
  sleep 1
}

echo "=== Port-forwarding ${pf_target} -> localhost:${pf_port} ==="
start_port_forward
if ! kill -0 "${pf_pid}" 2>/dev/null; then
  echo "ERROR: port-forward to ${gw_pod} failed to start (is localhost:${pf_port} in use?)" >&2
  exit 1
fi

base="http://127.0.0.1:${pf_port}"
echo "=== Waiting for HTTP via Gateway (up to ${HTTP_WAIT_TIMEOUT}s) ==="
http_deadline=$(($(date +%s) + HTTP_WAIT_TIMEOUT))
http_ready=false
while [ "$(date +%s)" -lt "${http_deadline}" ]; do
  if ! kill -0 "${pf_pid}" 2>/dev/null; then
    echo "port-forward exited; restarting..." >&2
    start_port_forward
  fi
  if curl -sf "${base}/echo" -o /dev/null; then
    http_ready=true
    break
  fi
  sleep 2
done
if [ "${http_ready}" != "true" ]; then
  echo "ERROR: gateway HTTP not reachable at ${base}/echo within ${HTTP_WAIT_TIMEOUT}s" >&2
  exit 1
fi

echo "=== Seeding HTTP traffic through Gateway ==="
ok=0
fail=0

curl_once() {
  if curl -sf "$1" -o /dev/null; then
    ok=$((ok + 1))
  else
    fail=$((fail + 1))
  fi
}

for i in $(seq 1 20); do
  curl_once "${base}/echo?n=${i}"
  curl_once "${base}/"
done
for path in "/?evilmonkey=1" "/?q=1'%20OR%201=1--" "/?id=union+select+1"; do
  curl_once "${base}${path}"
done

echo "Traffic seeding finished: ${ok} succeeded, ${fail} failed (gateway via port-forward ${pf_port})"
if [ "${ok}" -eq 0 ]; then
  echo "ERROR: all HTTP requests failed; gateway may be unreachable" >&2
  exit 1
fi
if [ "${fail}" -gt 0 ]; then
  echo "WARNING: ${fail} request(s) failed (non-fatal)" >&2
fi
