#!/usr/bin/env bash
# Baseline-vs-WAF comparison across rulesets, repeated for a stable median.
#
#   scripts/suite.sh                       # baseline minimal crs, 3 runs each
#   REPEATS=5 scripts/suite.sh baseline crs
#   QPS=1000 DURATION=30s scripts/suite.sh
#
# Runs baseline FIRST (ARCHITECTURE.md §9.2), then each WAF ruleset, REPEATS times.
# Everything lands in results/ tagged <mode>__run<N>__<ts>.json for the dashboard.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${here}"

MODES=("${@:-}")
[[ -z "${MODES[*]}" ]] && MODES=(baseline minimal crs)
REPEATS="${REPEATS:-3}"

[[ -f coraza.wasm ]] || ./scripts/extract-wasm.sh

for mode in "${MODES[@]}"; do
  for i in $(seq 1 "${REPEATS}"); do
    echo; echo "################ ${mode} — run ${i}/${REPEATS} ################"
    RUN_IDX="${i}" ./scripts/run.sh "${mode}"
  done
done

echo; echo "################ all runs ################"
cat results/*.summary.txt 2>/dev/null | sort || true
echo
if command -v python3 >/dev/null 2>&1; then
  echo ">> generating dashboard..."
  if python3 scripts/report.py; then
    echo ">> dashboard: $(pwd)/dashboard.html (self-contained — open in any browser)"
  else
    echo "!! report generation failed; raw results are in results/" >&2
  fi
else
  echo "!! python3 not found — skipping dashboard (raw results in results/)" >&2
fi
