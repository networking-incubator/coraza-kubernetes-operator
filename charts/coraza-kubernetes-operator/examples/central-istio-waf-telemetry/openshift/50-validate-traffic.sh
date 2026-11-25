#!/usr/bin/env bash
# Smoke-test WAF allow/block through waf-gateway (port-forward or external LB).
set -euo pipefail

NS="${NS:-openshift-ingress}"
GW="${GW:-waf-gateway}"
USE_LB="${USE_LB:-0}"

if [[ "${USE_LB}" == "1" ]]; then
  HOST="$(oc get gateway -n "${NS}" "${GW}" -o jsonpath='{.status.addresses[0].value}')"
  BASE="http://${HOST}"
  echo "Using Gateway LB: ${BASE}"
else
  SVC="${GW}-openshift-default"
  oc port-forward -n "${NS}" "svc/${SVC}" 8080:80 >/tmp/waf-pf.log 2>&1 &
  PF_PID=$!
  trap 'kill ${PF_PID} 2>/dev/null || true' EXIT
  sleep 2
  BASE="http://localhost:8080"
  echo "Port-forward svc/${SVC} -> localhost:8080"
fi

echo -n "allow GET / -> "
curl -s -o /dev/null -w "%{http_code}\n" "${BASE}/"

echo -n "block GET /?q=attack -> "
curl -s -o /dev/null -w "%{http_code}\n" "${BASE}/?q=attack"
