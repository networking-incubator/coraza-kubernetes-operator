#!/usr/bin/env bash
#
# suite.sh - Run the canonical baseline-vs-WAF comparison for one scenario:
#   1. WAF off      (B0 baseline)
#   2. WAF on/minimal (WASM overhead floor)
#   3. WAF on/medium  (typical policy)
#
# Report WAF cost as the delta between each run and the WAF-off baseline.
#
#   suite.sh <scenario-file> [rulesets...]
#   suite.sh scenarios/benign-fixed.env minimal medium
#
# Env:
#   COLLECT=true          Also pull Prometheus metrics around each run.
#   SETTLE=25s            Wait after toggling the WAF for the WASM poll to apply.
#   EXPECT_ATTACK_BLOCK=true  Validate attack-block scenarios (auto for attack-*).
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../lib.sh
source ./lib.sh
preflight

SCENARIO_FILE="${1:?usage: suite.sh <scenario-file> [rulesets...]}"; shift || true
RULESETS=("$@"); [[ ${#RULESETS[@]} -eq 0 ]] && RULESETS=(minimal medium)
SETTLE="${SETTLE:-25s}"
COLLECT="${COLLECT:-false}"

is_attack_scenario() { [[ "$(basename "${SCENARIO_FILE}")" == attack-* ]]; }

run_one() {
  local tag="$1"; local expect_block="$2"
  local start end
  start="$(date -u +%s)"
  EXPECT_BLOCK="${expect_block}" ./scripts/run.sh "${SCENARIO_FILE}" "${tag}"
  end="$(date -u +%s)"
  if [[ "${COLLECT}" == "true" ]]; then
    ./scripts/collect.sh "${start}" "${end}" "${tag}" || warn "metric collection failed for ${tag}"
  fi
}

# --- 1. baseline: WAF off ---------------------------------------------------
log "=== Baseline: WAF OFF ==="
./scripts/waf.sh off
log "Settling ${SETTLE} for WasmPlugin removal"; sleep "${SETTLE%s}"
if is_attack_scenario; then run_one "waf-off" "false"; else run_one "waf-off" ""; fi

# --- 2..N: WAF on per ruleset ----------------------------------------------
for rs in "${RULESETS[@]}"; do
  log "=== WAF ON: ${rs} ==="
  ./scripts/waf.sh on "${rs}"
  log "Settling ${SETTLE} for the WasmPlugin to apply to Envoy (and poll, if cache enabled)"; sleep "${SETTLE%s}"
  if is_attack_scenario; then run_one "waf-${rs}" "true"; else run_one "waf-${rs}" ""; fi
done

echo
SCN="$(basename "${SCENARIO_FILE}" .env)"
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
