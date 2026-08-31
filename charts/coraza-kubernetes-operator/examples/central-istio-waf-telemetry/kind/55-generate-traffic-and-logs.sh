Unable to open session log file "/home/rzago/.cache/starship/session_2746119224310161.log": Os { code: 30, kind: ReadOnlyFilesystem, message: "Read-only file system" }!
#!/usr/bin/env bash
# Generate allow/block traffic and observe WAF + ALS telemetry signals on KIND.
set -euo pipefail

NS="${NS:-integration-tests}"
GW="${GW:-coraza-gateway}"
GW_SVC="${GW_SVC:-${GW}-istio}"
GW_DEPLOY="${GW_DEPLOY:-${GW}-istio}"
COLLECTOR_NS="${COLLECTOR_NS:-coraza-central-waf-telemetry}"
COLLECTOR_NAME="${COLLECTOR_NAME:-central-waf-als}"
ALLOW_REQUESTS="${ALLOW_REQUESTS:-5}"
BLOCK_REQUESTS="${BLOCK_REQUESTS:-10}"
USE_LB="${USE_LB:-0}"
SHOW_COLLECTOR_DEBUG="${SHOW_COLLECTOR_DEBUG:-0}"

metric_count() {
  local url="$1" pattern="$2"
  curl -sf "${url}" 2>/dev/null | grep -E "^${pattern}" | awk '{sum += $2} END {print sum+0}'
}

echo "=== 1) Gateway target ==="
if [[ "${USE_LB}" == "1" ]]; then
  HOST="$(kubectl get gateway -n "${NS}" "${GW}" -o jsonpath='{.status.addresses[0].value}')"
  if [[ -z "${HOST}" ]]; then
    echo "ERROR: Gateway has no external address; set USE_LB=0 for port-forward mode" >&2
    exit 1
  fi
  BASE="http://${HOST}"
  echo "LB: ${BASE}"
else
  kubectl port-forward -n "${NS}" "svc/${GW_SVC}" 8080:80 >/tmp/kind-waf-traffic-pf.log 2>&1 &
  PF_PID=$!
  trap 'kill ${PF_PID} 2>/dev/null || true; kill ${MET_PF} 2>/dev/null || true' EXIT
  sleep 2
  BASE="http://127.0.0.1:8080"
  echo "Port-forward svc/${GW_SVC} -> localhost:8080"
fi

echo ""
echo "=== 2) Baseline collector metrics (:9090) ==="
kubectl port-forward -n "${COLLECTOR_NS}" "svc/${COLLECTOR_NAME}-collector" 19090:9090 >/tmp/kind-waf-metrics-pf.log 2>&1 &
MET_PF=$!
sleep 2
BEFORE_BLOCK="$(metric_count 'http://127.0.0.1:19090/metrics' 'coraza_waf_blocked_requests_total' || true)"
BEFORE_REQ="$(metric_count 'http://127.0.0.1:19090/metrics' 'coraza_waf_requests_total' || true)"
BEFORE_BLOCK="${BEFORE_BLOCK:-0}"
BEFORE_REQ="${BEFORE_REQ:-0}"
echo "coraza_waf_blocked_requests_total (sum): ${BEFORE_BLOCK}"
echo "coraza_waf_requests_total (sum):         ${BEFORE_REQ}"

echo ""
echo "=== 3) Generate traffic ==="
for i in $(seq 1 "${ALLOW_REQUESTS}"); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/?allow=${i}")"
  echo "allow ${i}: HTTP ${code}"
done
for i in $(seq 1 "${BLOCK_REQUESTS}"); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/?q=attack&n=${i}")"
  echo "block ${i}: HTTP ${code}"
done

sleep 3

echo ""
echo "=== 4) Gateway logs (Coraza deny + access) ==="
kubectl logs -n "${NS}" "deploy/${GW_DEPLOY}" --tail=80 \
  | grep -iE 'Coraza: Access denied|attack| 403 ' || echo "(no matching lines in last 80 log lines)"

echo ""
echo "=== 5) Collector metrics after traffic (:9090) ==="
AFTER_BLOCK="$(metric_count 'http://127.0.0.1:19090/metrics' 'coraza_waf_blocked_requests_total' || true)"
AFTER_REQ="$(metric_count 'http://127.0.0.1:19090/metrics' 'coraza_waf_requests_total' || true)"
AFTER_BLOCK="${AFTER_BLOCK:-0}"
AFTER_REQ="${AFTER_REQ:-0}"
echo "coraza_waf_blocked_requests_total (sum): ${AFTER_BLOCK} (delta: $((AFTER_BLOCK - BEFORE_BLOCK)))"
echo "coraza_waf_requests_total (sum):         ${AFTER_REQ} (delta: $((AFTER_REQ - BEFORE_REQ)))"
echo ""
curl -sf 'http://127.0.0.1:19090/metrics' 2>/dev/null | grep -E '^coraza_waf_' || true

kill "${MET_PF}" 2>/dev/null || true
MET_PF=""

echo ""
echo "=== 6) Collector OTLP intake (internal :8888) ==="
kubectl port-forward -n "${COLLECTOR_NS}" "svc/${COLLECTOR_NAME}-collector-monitoring" 18888:8888 >/tmp/kind-waf-mon-pf.log 2>&1 &
MET_PF=$!
sleep 2
curl -sf 'http://127.0.0.1:18888/metrics' 2>/dev/null \
  | grep -E 'otelcol_receiver_accepted_log_records_total|otelcol_exporter_sent_metric_points_total' || true

if [[ "${SHOW_COLLECTOR_DEBUG}" == "1" ]]; then
  echo ""
  echo "=== 7) Collector pod logs (last 40 lines) ==="
  echo "Note: ALS payloads are OTLP logs, not stdout, unless debug exporter is enabled."
  kubectl logs -n "${COLLECTOR_NS}" "deploy/${COLLECTOR_NAME}-collector" --tail=40
fi

echo ""
echo "=== done ==="
echo "Tip: enable OTLP log dump: kubectl apply -f $(dirname "$0")/12-otel-collector-debug-logging.yaml"
echo "Tip: SHOW_COLLECTOR_DEBUG=1 $0  # include collector pod stdout (after debug CR applied)"
echo "Tip: USE_LB=1 $0                 # use MetalLB external IP instead of port-forward"
