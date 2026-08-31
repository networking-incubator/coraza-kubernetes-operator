Unable to open session log file "/home/rzago/.cache/starship/session_3224438034466325.log": Os { code: 30, kind: ReadOnlyFilesystem, message: "Read-only file system" }!
#!/usr/bin/env bash
# Smoke-test WAF allow/block through coraza-gateway (port-forward or MetalLB).
set -euo pipefail

NS="${NS:-integration-tests}"
GW="${GW:-coraza-gateway}"
GW_SVC="${GW_SVC:-${GW}-istio}"
USE_LB="${USE_LB:-0}"

if [[ "${USE_LB}" == "1" ]]; then
  HOST="$(kubectl get gateway -n "${NS}" "${GW}" -o jsonpath='{.status.addresses[0].value}')"
  if [[ -z "${HOST}" ]]; then
    echo "ERROR: Gateway has no external address; set USE_LB=0 for port-forward mode" >&2
    exit 1
  fi
  BASE="http://${HOST}"
  echo "Using Gateway LB: ${BASE}"
else
  kubectl port-forward -n "${NS}" "svc/${GW_SVC}" 8080:80 >/tmp/kind-waf-pf.log 2>&1 &
  PF_PID=$!
  trap 'kill ${PF_PID} 2>/dev/null || true' EXIT
  sleep 2
  BASE="http://127.0.0.1:8080"
  echo "Port-forward svc/${GW_SVC} -> localhost:8080"
fi

echo -n "allow GET / -> "
curl -s -o /dev/null -w "%{http_code}\n" "${BASE}/"

echo -n "block GET /?q=attack -> "
curl -s -o /dev/null -w "%{http_code}\n" "${BASE}/?q=attack"
