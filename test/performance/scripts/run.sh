#!/usr/bin/env bash
#
# run.sh - Run one load scenario with fortio from inside the cluster and record
# results (raw JSON + a parsed summary line).
#
#   run.sh <scenario-file> [TAG]
#
# TAG labels the output (e.g. "waf-off", "waf-medium") so runs are comparable.
# A scenario file is a sourced env fragment; see scenarios/*.env.
#
# Environment knobs:
#   EXPECT_BLOCK=true|false  Assert the attack probe is/ isn't blocked (403).
#   WARMUP=10s               Warm-up load discarded before the measured run.
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../lib.sh
source ./lib.sh
preflight

SCENARIO_FILE="${1:?usage: run.sh <scenario-file> [TAG]}"
TAG="${2:-untagged}"
[[ -f "${SCENARIO_FILE}" ]] || { err "scenario file not found: ${SCENARIO_FILE}"; exit 1; }

# --- scenario defaults (overridden by the scenario file) --------------------
SCENARIO_NAME="$(basename "${SCENARIO_FILE}" .env)"
QPS="0"                 # 0 = max throughput (find the knee)
DURATION="60s"
CONNS="8"
REQ_PATH="/"
PAYLOAD_SIZE="0"        # >0 makes fortio send a POST body of this many bytes
WARMUP="${WARMUP:-10s}"
# shellcheck disable=SC1090
source "${SCENARIO_FILE}"

mkdir -p "${RESULTS_DIR}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
base="${RESULTS_DIR}/${SCENARIO_NAME}__${TAG}__${stamp}"
raw_json="${base}.json"
summary="${base}.summary.txt"

# --- build fortio args ------------------------------------------------------
fortio_args=(load -qps "${QPS}" -c "${CONNS}" -p "50,75,90,95,99,99.9")
[[ "${PAYLOAD_SIZE}" -gt 0 ]] && fortio_args+=(-payload-size "${PAYLOAD_SIZE}")
url="${TARGET_URL}${REQ_PATH}"

# --- validity probe ---------------------------------------------------------
log "Probing WAF state (benign + attack)"
benign_code="$(loadgen_exec fortio curl -quiet "${TARGET_URL}/" 2>/dev/null | awk '/^HTTP\//{print $2; exit}')"
attack_code="$(loadgen_exec fortio curl -quiet "${TARGET_URL}/?q=attack" 2>/dev/null | awk '/^HTTP\//{print $2; exit}')"
log "  benign '/' -> ${benign_code:-?}   attack '/?q=attack' -> ${attack_code:-?}"
if [[ -n "${EXPECT_BLOCK:-}" ]]; then
  if [[ "${EXPECT_BLOCK}" == "true" && "${attack_code}" != "403" ]]; then
    err "EXPECT_BLOCK=true but attack probe returned '${attack_code}' (WAF not active?). Aborting."
    exit 1
  fi
  if [[ "${EXPECT_BLOCK}" == "false" && "${attack_code}" == "403" ]]; then
    err "EXPECT_BLOCK=false but attack probe was blocked (WAF still active?). Aborting."
    exit 1
  fi
  ok "WAF state matches EXPECT_BLOCK=${EXPECT_BLOCK}"
fi

# --- warm-up (discarded) ----------------------------------------------------
log "Warm-up ${WARMUP} (discarded)"
loadgen_exec fortio "${fortio_args[@]}" -t "${WARMUP}" -quiet "${url}" >/dev/null 2>&1 || true

# --- measured run -----------------------------------------------------------
log "Measured run: scenario='${SCENARIO_NAME}' tag='${TAG}' qps=${QPS} conns=${CONNS} dur=${DURATION} payload=${PAYLOAD_SIZE}B"
log "  target: ${url}"
run_start="$(date -u +%s)"
loadgen_exec fortio "${fortio_args[@]}" -t "${DURATION}" -json - "${url}" > "${raw_json}"
run_end="$(date -u +%s)"

# --- parse summary ----------------------------------------------------------
python3 - "${raw_json}" "${SCENARIO_NAME}" "${TAG}" "${run_start}" "${run_end}" > "${summary}" <<'PY'
import json, sys
raw, scenario, tag, t0, t1 = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
d = json.load(open(raw))
h = d.get("DurationHistogram", {})
pct = {str(p["Percentile"]): p["Value"]*1000 for p in h.get("Percentiles", [])}  # ms
codes = d.get("RetCodes", {})
total = sum(codes.values()) or 1
# 403 = WAF block (expected for attack traffic) — report separately, not as error.
blocked = sum(v for k, v in codes.items() if str(k) == "403")
errors = sum(v for k, v in codes.items() if not str(k).startswith("2") and str(k) != "403")
def g(p): return f'{pct.get(p, float("nan")):.2f}'
print(f"scenario={scenario} tag={tag}")
print(f"window_utc={t0}..{t1}")
print(f"actual_qps={d.get('ActualQPS', 0):.1f} requested_qps={d.get('RequestedQPS','max')}")
print(f"count={h.get('Count',0)} avg_ms={h.get('Avg',0)*1000:.2f} min_ms={h.get('Min',0)*1000:.2f} max_ms={h.get('Max',0)*1000:.2f}")
print(f"p50_ms={g('50')} p90_ms={g('90')} p95_ms={g('95')} p99_ms={g('99')} p99.9_ms={g('99.9')}")
print(f"ret_codes={codes} blocked_pct={100*blocked/total:.3f} error_pct={100*errors/total:.3f}")
PY

ok "Results:"
cat "${summary}"
echo
log "Raw JSON:      ${raw_json}"
log "Summary:       ${summary}"
warn "Correlate this window with control-plane (:8443) and data-plane (:15090) metrics via scripts/collect.sh."
