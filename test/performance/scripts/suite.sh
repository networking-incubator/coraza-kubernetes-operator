#!/usr/bin/env bash
#
# suite.sh - Run the canonical baseline-vs-WAF comparison for one scenario:
#   1. WAF off      (B0 baseline)
#   2. WAF on, once per ruleset given as an argument
#      (default: minimal = WASM floor, medium = typical policy, crs = OWASP CRS)
#
# Report WAF cost as the delta between each run and the WAF-off baseline.
#
#   suite.sh <scenario-file> [rulesets...]
#   suite.sh scenarios/fixed-500qps.env minimal medium crs
#   LOAD_PROFILE=args-heavy suite.sh scenarios/fixed-500qps.env crs
#
# Env:
#   COLLECT=true          Also pull Prometheus metrics around each run.
#   SETTLE=25s            Wait after toggling the WAF for the WASM poll to apply.
#   A blocking load profile (EXPECT_STATUS=403) is validated automatically;
#   the WAF-off baseline run forces EXPECT_STATUS=200.
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../lib.sh
source ./lib.sh
preflight

SCENARIO_FILE="${1:?usage: suite.sh <scenario-file> [rulesets...]}"; shift || true
# Default covers the full cost curve: WASM floor -> typical policy -> CRS.
# Omitting crs here understated the headline number, since CRS is the dominant
# latency driver and the point of the comparison.
RULESETS=("$@"); [[ ${#RULESETS[@]} -eq 0 ]] && RULESETS=(minimal medium crs)
SETTLE="${SETTLE:-25s}"
COLLECT="${COLLECT:-false}"

run_one() {
  local tag="$1"; local baseline="$2"
  local start end
  start="$(date -u +%s)"
  # On the WAF-off baseline every shape must pass, including blocking profiles
  # that declare EXPECT_STATUS=403 for the WAF-on runs. Overriding here beats
  # the old attack-* filename heuristic: it keys off actual WAF state, and the
  # profile already says what it expects when the WAF is on.
  if [[ "${baseline}" == "baseline" ]]; then
    EXPECT_STATUS=200 ./scripts/run.sh "${SCENARIO_FILE}" "${tag}"
  else
    ./scripts/run.sh "${SCENARIO_FILE}" "${tag}"
  fi
  end="$(date -u +%s)"
  if [[ "${COLLECT}" == "true" ]]; then
    ./scripts/collect.sh "${start}" "${end}" "${tag}" || warn "metric collection failed for ${tag}"
  fi
}

# --- 1. baseline: WAF off ---------------------------------------------------
log "=== Baseline: WAF OFF ==="
./scripts/waf.sh off
log "Settling ${SETTLE} for WasmPlugin removal"; sleep "${SETTLE%s}"
run_one "waf-off" "baseline"

# --- 2..N: WAF on per ruleset ----------------------------------------------
for rs in "${RULESETS[@]}"; do
  log "=== WAF ON: ${rs} ==="
  ./scripts/waf.sh on "${rs}"
  log "Settling ${SETTLE} for the WasmPlugin to apply to Envoy (and poll, if cache enabled)"; sleep "${SETTLE%s}"
  run_one "waf-${rs}" "wafon"
done

echo
# Must match the result group run.sh writes, which folds in the load profile.
SCN="$(basename "${SCENARIO_FILE}" .env)"
if [[ -n "${LOAD_PROFILE:-}" ]]; then
  SCN="${SCN}+$(basename "${LOAD_PROFILE}" .env)"
fi
if command -v python3 >/dev/null 2>&1; then
  log "Generating self-contained HTML report for '${SCN}'"
  if python3 ./scripts/report.py --results "${RESULTS_DIR}" --scenario "${SCN}" \
       --out "${RESULTS_DIR}/${SCN}.html"; then
    ok "Report: ${RESULTS_DIR}/${SCN}.html (self-contained — open in any browser)"
  else
    warn "report generation failed; raw results are still in ${RESULTS_DIR}"
  fi
else
  warn "python3 not found — skipping HTML report (raw results in ${RESULTS_DIR})"
fi

ok "Suite complete. Compare summaries:"
echo "  grep -H . ${RESULTS_DIR}/${SCN}__*.summary.txt"
warn "Report WAF cost as (waf-* p95/p99) minus (waf-off p95/p99), same scenario/session."
