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

# Captured before the defaults below assign EXPECT_STATUS, so this holds only a
# value the caller actually passed. A blocking profile declares 403, but the
# same profile must expect 200 on the WAF-off baseline, so suite.sh overrides
# it there and that override has to outrank the profile.
_env_expect_status="${EXPECT_STATUS:-}"
[[ -f "${SCENARIO_FILE}" ]] || { err "scenario file not found: ${SCENARIO_FILE}"; exit 1; }

# --- scenario defaults (overridden by the scenario file) --------------------
SCENARIO_NAME="$(basename "${SCENARIO_FILE}" .env)"
QPS="0"                 # 0 = max throughput (find the knee)
DURATION="60s"
CONNS="8"
WARMUP="${WARMUP:-10s}"

# --- request shape (a load profile may override all of these) ---------------
METHOD=""               # empty = fortio default (GET, or POST when a body is set)
REQ_PATH="/"
CONTENT_TYPE=""
PAYLOAD_FILE=""         # path INSIDE the loadgen pod, e.g. /payloads/json-1kb.json
PAYLOAD=""              # inline literal body
PAYLOAD_SIZE="0"        # >0 makes fortio send a body of this many random bytes
HEADERS=()              # extra request headers, each "Key: Value"
EXPECT_STATUS="200"     # status the shape probe must see, else the run aborts

# shellcheck disable=SC1090
source "${SCENARIO_FILE}"

# A load profile defines the request SHAPE; the scenario defines load intensity
# and duration. Sourced second so the profile wins on any overlap.
if [[ -n "${LOAD_PROFILE:-}" ]]; then
  if [[ -f "${LOAD_PROFILES_DIR}/${LOAD_PROFILE}.env" ]]; then
    lp_file="${LOAD_PROFILES_DIR}/${LOAD_PROFILE}.env"
  elif [[ -f "${LOAD_PROFILE}" ]]; then
    lp_file="${LOAD_PROFILE}"
  else
    err "unknown load profile '${LOAD_PROFILE}' (no ${LOAD_PROFILES_DIR}/${LOAD_PROFILE}.env and not a file)"
    exit 1
  fi
  # shellcheck disable=SC1090
  source "${lp_file}"
  # Fold the profile into the scenario name so runs with different request
  # shapes land in separate result groups and are never pooled into one median.
  SCENARIO_NAME="${SCENARIO_NAME}+$(basename "${lp_file}" .env)"
  log "Load profile: $(basename "${lp_file}" .env) ($(basename "${lp_file}"))"
fi
[[ -n "${_env_expect_status}" ]] && EXPECT_STATUS="${_env_expect_status}"

mkdir -p "${RESULTS_DIR}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
base="${RESULTS_DIR}/${SCENARIO_NAME}__${TAG}__${stamp}"
raw_json="${base}.json"
summary="${base}.summary.txt"

# --- build fortio args ------------------------------------------------------
fortio_args=(load -qps "${QPS}" -c "${CONNS}" -p "50,75,90,95,99,99.9" -allow-initial-errors)
[[ -n "${METHOD}"       ]] && fortio_args+=(-X "${METHOD}")
[[ -n "${CONTENT_TYPE}" ]] && fortio_args+=(-content-type "${CONTENT_TYPE}")
[[ -n "${PAYLOAD_FILE}" ]] && fortio_args+=(-payload-file "${PAYLOAD_FILE}")
[[ -n "${PAYLOAD}"      ]] && fortio_args+=(-payload "${PAYLOAD}")
[[ "${PAYLOAD_SIZE}" -gt 0 ]] && fortio_args+=(-payload-size "${PAYLOAD_SIZE}")
if [[ ${#HEADERS[@]} -gt 0 ]]; then
  for _h in "${HEADERS[@]}"; do fortio_args+=(-H "${_h}"); done
fi
url="${TARGET_URL}${REQ_PATH}"

# Fail fast if the profile names a payload that is not in the ConfigMap, rather
# than letting fortio send an empty body and silently measuring the wrong thing.
# Checked via the API, not in-pod: the fortio image is distroless (no test/ls).
if [[ -n "${PAYLOAD_FILE}" ]]; then
  _key="$(basename "${PAYLOAD_FILE}")"
  _esc="${_key//./\\.}"
  # Captured, not piped to grep -q: grep exits on the first line and kubectl
  # then dies of SIGPIPE mid-write, which pipefail reports as failure. That
  # only shows up once a payload exceeds the pipe buffer (~64 KB).
  _val="$("${CLI}" get configmap "${PAYLOADS_CONFIGMAP}" -n "${NAMESPACE}" \
          -o "jsonpath={.data['${_esc}']}{.binaryData['${_esc}']}" 2>/dev/null || true)"
  if [[ -z "${_val}" ]]; then
    err "payload '${_key}' missing from configmap/${PAYLOADS_CONFIGMAP} in ${NAMESPACE}."
    err "Build it with: manifests/load-profiles/gen-payloads.sh && scripts/setup.sh"
    exit 1
  fi
fi

# --- validity probe ---------------------------------------------------------
# fortio writes response headers to STDERR (>=1.22), so the status line is read
# from stderr only. Reading stdout returns the body and silently yields "".
# Retries: a probe is now a hard gate on the run, and back-to-back kubectl
# exec calls occasionally return nothing. One flaky exec should not discard a
# whole measurement.
probe_status() {
  local out code
  for _ in 1 2 3; do
    out="$(loadgen_exec fortio curl "$@" 2>&1 >/dev/null || true)"
    code="$(printf '%s' "${out}" | grep -oE 'HTTP/[0-9.]+ [0-9]{3}' | head -1 | awk '{print $2}' || true)"
    [[ -n "${code}" ]] && { printf '%s' "${code}"; return 0; }
    sleep 2
  done
  return 0
}

log "Probing WAF state (benign + attack)"
benign_code="$(probe_status "${TARGET_URL}/")"
attack_code="$(probe_status "${TARGET_URL}/?q=attack")"
log "  benign '/' -> ${benign_code:-?}   attack '/?q=attack' -> ${attack_code:-?}"

# Probe the profile's ACTUAL request shape. The two probes above only cover a
# bare GET, so a profile-specific rejection (e.g. CRS 920420 blocking every
# POST) would otherwise slip through and be measured as if it were valid.
if [[ -n "${LOAD_PROFILE:-}" ]]; then
  shape_args=()
  [[ -n "${METHOD}"       ]] && shape_args+=(-X "${METHOD}")
  [[ -n "${CONTENT_TYPE}" ]] && shape_args+=(-content-type "${CONTENT_TYPE}")
  [[ -n "${PAYLOAD_FILE}" ]] && shape_args+=(-payload-file "${PAYLOAD_FILE}")
  [[ -n "${PAYLOAD}"      ]] && shape_args+=(-payload "${PAYLOAD}")
  if [[ ${#HEADERS[@]} -gt 0 ]]; then
    for _h in "${HEADERS[@]}"; do shape_args+=(-H "${_h}"); done
  fi
  shape_code="$(probe_status "${shape_args[@]}" "${url}")"
  log "  shape probe '${REQ_PATH}' -> ${shape_code:-?} (expected ${EXPECT_STATUS:-200})"
  if [[ "${shape_code}" != "${EXPECT_STATUS:-200}" ]]; then
    err "load profile '${LOAD_PROFILE}' returned '${shape_code}', expected '${EXPECT_STATUS:-200}'."
    err "Set EXPECT_STATUS in the profile if this status is intended, otherwise"
    err "the run would measure rejected requests instead of real work. Aborting."
    exit 1
  fi
fi
if [[ -n "${EXPECT_BENIGN_STATUS:-}" && "${benign_code}" != "${EXPECT_BENIGN_STATUS}" ]]; then
  err "EXPECT_BENIGN_STATUS=${EXPECT_BENIGN_STATUS} but benign probe returned '${benign_code}' (backend unreachable or misconfigured?). Aborting."
  exit 1
fi
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
log "Measured run: scenario='${SCENARIO_NAME}' tag='${TAG}' qps=${QPS} conns=${CONNS} dur=${DURATION}"
log "  shape: method=${METHOD:-GET(auto)} path='${REQ_PATH}' body=${PAYLOAD_FILE:-${PAYLOAD:+inline}}${PAYLOAD_FILE:+ }${PAYLOAD_SIZE}B ct=${CONTENT_TYPE:-none} hdrs=${#HEADERS[@]}"
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
