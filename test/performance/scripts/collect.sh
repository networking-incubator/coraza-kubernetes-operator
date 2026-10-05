#!/usr/bin/env bash
#
# collect.sh - Pull control-plane and data-plane metrics from Prometheus for a
# given time window and save them alongside the load results. Best-effort:
# requires a reachable Prometheus (see PROM_* vars in lib.sh).
#
#   collect.sh <start_epoch> <end_epoch> [TAG]
#
# Tip: run.sh prints window_utc; or wrap a run with:
#   START=$(date -u +%s); scripts/run.sh ...; END=$(date -u +%s)
#   scripts/collect.sh "$START" "$END" mytag
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../lib.sh
source ./lib.sh
require_cmd curl
preflight

START="${1:?usage: collect.sh <start_epoch> <end_epoch> [TAG]}"
END="${2:?usage: collect.sh <start_epoch> <end_epoch> [TAG]}"
TAG="${3:-untagged}"
STEP="${STEP:-15s}"

mkdir -p "${RESULTS_DIR}"
out="${RESULTS_DIR}/metrics__${TAG}__$(date -u +%Y%m%dT%H%M%SZ).jsonl"

# Curated PromQL: data-plane (WAF) + control-plane (cache + reconcile).
declare -a QUERIES=(
  'waf_rps_by_outcome|sum by (outcome) (rate(coraza_waf_requests_total[1m]))'
  'waf_active_rule_count|max(coraza_waf_plugin_rule_count)'
  'waf_anomaly_p95|histogram_quantile(0.95, sum by (le) (rate(coraza_waf_request_anomaly_score_bucket[1m])))'
  'waf_plugin_loads|sum by (status) (coraza_waf_plugin_loads_total)'
  'cache_req_rate|sum by (handler,code) (rate(coraza_cache_server_requests_total[1m]))'
  'cache_latency_p99|histogram_quantile(0.99, sum by (le,handler) (rate(coraza_cache_server_request_duration_seconds_bucket[1m])))'
  'cache_inflight|max by (handler) (coraza_cache_server_in_flight_requests)'
  'cache_size_bytes|max(coraza_cache_size_bytes)'
  'reconcile_rate|sum by (controller,result) (rate(controller_runtime_reconcile_total[1m]))'
  'workqueue_depth|max by (name) (workqueue_depth)'
)

log "Port-forwarding Prometheus svc/${PROM_SVC} (${PROM_NAMESPACE}) -> localhost:${PROM_PORT}"
"${CLI}" port-forward -n "${PROM_NAMESPACE}" "svc/${PROM_SVC}" "${PROM_PORT}:${PROM_PORT}" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" >/dev/null 2>&1 || true' EXIT
for _ in $(seq 1 20); do
  curl -sf "http://localhost:${PROM_PORT}/-/ready" >/dev/null 2>&1 && break
  sleep 1
done

log "Querying ${#QUERIES[@]} expressions over [${START}, ${END}] step=${STEP}"
: > "${out}"
for entry in "${QUERIES[@]}"; do
  name="${entry%%|*}"; expr="${entry#*|}"
  resp="$(curl -sG "http://localhost:${PROM_PORT}/api/v1/query_range" \
    --data-urlencode "query=${expr}" \
    --data-urlencode "start=${START}" \
    --data-urlencode "end=${END}" \
    --data-urlencode "step=${STEP}" 2>/dev/null || echo '{}')"
  printf '{"metric":"%s","query":%s,"result":%s}\n' \
    "${name}" "$(printf '%s' "${expr}" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')" \
    "${resp}" >> "${out}"
  status="$(printf '%s' "${resp}" | python3 -c 'import json,sys;
try: print(json.load(sys.stdin).get("status","?"))
except Exception: print("parse-error")' 2>/dev/null)"
  echo "  ${name}: ${status}"
done

ok "Metrics written to ${out}"
warn "If queries returned empty, confirm the ServiceMonitor (:8443) and PodMonitor (:15090) are enabled and scraped."
