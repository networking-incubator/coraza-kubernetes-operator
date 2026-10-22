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
if [[ -z "${benign_code}" || -z "${attack_code}" ]]; then
  err "probe for '/' or '/?q=attack' returned no HTTP status (target unreachable?). Aborting."
  exit 1
fi

# Probe the profile's ACTUAL request shape. The two probes above only cover a
# bare GET, so a profile-specific rejection (e.g. CRS 920420 blocking every
# POST) would otherwise slip through and be measured as if it were valid.
if [[ -n "${LOAD_PROFILE:-}" ]]; then
  shape_args=()
  [[ -n "${METHOD}"       ]] && shape_args+=(-X "${METHOD}")
  [[ -n "${CONTENT_TYPE}" ]] && shape_args+=(-content-type "${CONTENT_TYPE}")
  [[ -n "${PAYLOAD_FILE}" ]] && shape_args+=(-payload-file "${PAYLOAD_FILE}")
  [[ -n "${PAYLOAD}"      ]] && shape_args+=(-payload "${PAYLOAD}")
  [[ "${PAYLOAD_SIZE}" -gt 0 ]] && shape_args+=(-payload-size "${PAYLOAD_SIZE}")
  if [[ ${#HEADERS[@]} -gt 0 ]]; then
    for _h in "${HEADERS[@]}"; do shape_args+=(-H "${_h}"); done
  fi
  # ${arr[@]+"${arr[@]}"} (not "${arr[@]}"): bash 3.2 (macOS /bin/bash) treats
  # an empty array as unbound under `set -u` and aborts here for any profile
  # that leaves METHOD/CONTENT_TYPE/PAYLOAD_FILE/PAYLOAD/HEADERS all unset.
  shape_code="$(probe_status ${shape_args[@]+"${shape_args[@]}"} "${url}")"
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
# Run the measured load as a one-off Job rather than streaming it back over a
# single long-lived 'kubectl exec' connection: some client<->API-server network
# paths reset multi-minute exec streams (observed consistently at ~120s with
# zero bytes in flight), which aborts every run with DURATION>=~120s. Polling
# Job status with short, independent API calls needs no connection to stay
# open for the run's duration, and 'kubectl logs' on an already-finished Job
# is a single bounded fetch rather than a live stream.
job_name="perf-run-$(date +%s)-${RANDOM}"

# Render one argv element as a YAML single-quoted scalar, so values containing
# ':', '{' or spaces (URLs, -H headers) cannot be reparsed as YAML structure.
yaml_quote() { printf "'%s'" "${1//\'/\'\'}"; }

# -quiet drops fortio's info-level progress logging, which 'kubectl logs'
# would otherwise merge with the JSON result (stdout+stderr are combined).
# The plain-text summary report isn't suppressed by -quiet, so it still
# precedes the JSON in the combined log; extracted below with sed.
job_cmd=(fortio "${fortio_args[@]}" -t "${DURATION}" -quiet -json - "${url}")
job_cmd_yaml=""
for _arg in "${job_cmd[@]}"; do
  job_cmd_yaml+=$'\n            - '"$(yaml_quote "${_arg}")"
done

# Applied as YAML rather than 'kubectl create job' so the injection annotation
# and backoffLimit are set when the pod is created, not patched in afterwards.
"${CLI}" apply -n "${NAMESPACE}" -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
spec:
  backoffLimit: 0
  template:
    metadata:
      annotations:
        # Keep the load generator OUT of the Istio mesh, matching
        # manifests/loadgen.yaml. An injected sidecar would add its own latency
        # and CPU to every measured request, and would also stop the Job from
        # ever reporting success, since istio-proxy does not exit on its own.
        sidecar.istio.io/inject: "false"
    spec:
      restartPolicy: Never
      containers:
        - name: fortio
          image: ${LOADGEN_IMAGE}
          # 'command', not 'args': the fortio image's ENTRYPOINT is already
          # fortio, so args would append and yield 'fortio fortio load ...'.
          # This matches what 'kubectl create job -- ...' used to generate.
          command:${job_cmd_yaml}
          volumeMounts:
            - name: payloads
              mountPath: /payloads
              readOnly: true
      volumes:
        # Same mount as manifests/loadgen.yaml, so a PAYLOAD_FILE profile
        # resolves to the identical path (/payloads/...) in the shape probe
        # (which runs in the loadgen pod) and in this measured-run Job.
        # 'optional' keeps profiles that send no body runnable before
        # setup.sh has built the ConfigMap.
        - name: payloads
          configMap:
            name: ${PAYLOADS_CONFIGMAP}
            optional: true
EOF

# DURATION is a Go duration ("60s", "2m", "4h", "1m30s"). Stripping non-digits
# would read "4h" as 4 seconds and abort a soak run after the 90s buffer.
duration_seconds() {
  local spec="$1" total=0 num unit
  [[ "${spec}" =~ ^([0-9]+[hms])+$ ]] || return 1
  while [[ "${spec}" =~ ^([0-9]+)([hms])(.*)$ ]]; do
    num="${BASH_REMATCH[1]}"; unit="${BASH_REMATCH[2]}"; spec="${BASH_REMATCH[3]}"
    case "${unit}" in
      h) total=$(( total + num * 3600 ));;
      m) total=$(( total + num * 60 ));;
      s) total=$(( total + num ));;
    esac
  done
  printf '%s' "${total}"
}

if ! dur_secs="$(duration_seconds "${DURATION}")"; then
  err "cannot parse DURATION='${DURATION}' (expected a Go duration such as 60s, 2m or 4h)"
  "${CLI}" delete job "${job_name}" -n "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
  exit 1
fi
deadline=$(( $(date +%s) + dur_secs + 90 ))
while true; do
  read -r succeeded failed <<<"$("${CLI}" get job "${job_name}" -n "${NAMESPACE}" \
    -o jsonpath='{.status.succeeded}{" "}{.status.failed}' 2>/dev/null || true)"
  [[ "${succeeded:-0}" == "1" ]] && break
  if [[ "${failed:-0}" == "1" ]]; then
    err "measured run Job '${job_name}' failed; see: ${CLI} logs -n ${NAMESPACE} job/${job_name}"
    exit 1
  fi
  if (( $(date +%s) > deadline )); then
    err "measured run Job '${job_name}' did not complete within ${dur_secs}s+90s buffer. Aborting."
    "${CLI}" delete job "${job_name}" -n "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
    exit 1
  fi
  sleep 10
done
# fortio's plain-text report precedes the JSON in the combined log and isn't
# itself valid JSON; keep from the top-level '{' through the matching
# unindented closing '}' (fortio pretty-prints with a 2-space indent, so only
# the outermost brace sits at column 0).
"${CLI}" logs -n "${NAMESPACE}" "job/${job_name}" | sed -n '/^{/,/^}/p' > "${raw_json}"
"${CLI}" delete job "${job_name}" -n "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
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
