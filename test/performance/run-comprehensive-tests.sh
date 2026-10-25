#!/usr/bin/env bash
#
# run-comprehensive-tests.sh - Run complete performance test matrix
#
# Runs all combinations of:
#   - 3 Load Profiles: baseline-get, args-heavy, headers-heavy
#   - 4 WAF Rulesets: off, minimal, medium, crs
#   - 1 Scenario: fixed-500qps
#   = 12 total test combinations
#
# Then generates comprehensive HTML report with all visualizations.
#
# Prerequisites:
#   - Connected to Kubernetes cluster
#   - RuleSets deployed (manifests/rulesets/*.yaml)
#   - Detection-only mode enabled (SecRuleEngine DetectionOnly)
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

# shellcheck source=./lib.sh
source ./lib.sh
preflight

# Join args with ", " — "${arr[*]}" can only join on a single IFS character.
join_list() { local out="$1"; shift; local x; for x in "$@"; do out+=", ${x}"; done; printf '%s' "${out}"; }

# Test configuration
SCENARIO_FILE="scenarios/fixed-500qps.env"
PROFILES=("baseline-get" "args-heavy" "headers-heavy")
RULESETS=("minimal" "medium" "crs")

# Verify scenario file exists
if [[ ! -f "${SCENARIO_FILE}" ]]; then
    echo "❌ Error: Scenario file not found: ${SCENARIO_FILE}"
    exit 1
fi

# Verify load profile files exist
for profile in "${PROFILES[@]}"; do
    profile_file="manifests/load-profiles/${profile}.env"
    if [[ ! -f "${profile_file}" ]]; then
        echo "❌ Error: Load profile not found: ${profile_file}"
        exit 1
    fi
done

# Read the rate/duration out of the scenario rather than restating them: the
# banner claimed "60s" long after the scenario moved to 120s, and a wrong
# duration in the log makes every archived run report ambiguous.
scenario_name="$(basename "${SCENARIO_FILE}" .env)"
# shellcheck disable=SC1090
read -r scenario_qps scenario_duration scenario_conns < <(
    source "${SCENARIO_FILE}"; echo "${QPS:-?} ${DURATION:-?} ${CONNS:-?}"
)

echo "═══════════════════════════════════════════════════════════════"
echo "  COMPREHENSIVE PERFORMANCE TEST SUITE"
echo "═══════════════════════════════════════════════════════════════"
echo ""
echo "📋 Configuration:"
echo "  Profiles: $(join_list "${PROFILES[@]}")"
echo "  Rulesets: $(join_list off "${RULESETS[@]}")"
echo "  Scenario: ${scenario_name} (${scenario_qps} QPS, ${scenario_duration}, ${scenario_conns} connections)"
echo "  Total tests: $(( ${#PROFILES[@]} * (${#RULESETS[@]} + 1) )) combinations"
echo ""
echo "  Detection-only mode: Expected (0% error rate)"
echo "  Repeats: 1 per cell — indicative only; the methodology wants >=3."
echo "           Re-run this script and take medians before reporting."
echo ""

echo "✅ All prerequisites verified"
echo ""
echo "───────────────────────────────────────────────────────────────"
echo ""

# Track start time
test_start=$(date +%s)
tests_run=0
tests_total=$((${#PROFILES[@]} * (${#RULESETS[@]} + 1)))  # +1 for 'off'

echo "🚀 Starting test execution (${tests_total} tests)"
echo ""

# Run tests for each profile
for profile in "${PROFILES[@]}"; do
    echo "═══════════════════════════════════════════════════════════════"
    echo "  PROFILE: ${profile}"
    echo "═══════════════════════════════════════════════════════════════"
    echo ""

    # Run suite.sh with the current profile
    # This runs: off, minimal, medium, crs
    export LOAD_PROFILE="${profile}"
    export COLLECT=false  # Disable Prometheus collection for faster execution
    export SETTLE=30s     # Wait for WASM plugin to reload

    echo "Running: LOAD_PROFILE=${profile} ./scripts/suite.sh ${SCENARIO_FILE} ${RULESETS[*]}"
    echo ""

    if LOAD_PROFILE="${profile}" ./scripts/suite.sh "${SCENARIO_FILE}" "${RULESETS[@]}"; then
        tests_run=$((tests_run + ${#RULESETS[@]} + 1))
        echo ""
        echo "✅ Profile ${profile} completed (${tests_run}/${tests_total} tests done)"
    else
        echo ""
        echo "❌ Error running tests for profile: ${profile}"
        exit 1
    fi

    echo ""

    # Brief pause between profiles
    if [[ "${profile}" != "${PROFILES[-1]}" ]]; then
        echo "⏱️  Pausing 15s between profiles..."
        sleep 15
    fi
    echo ""
done

test_end=$(date +%s)
test_duration=$((test_end - test_start))

echo "═══════════════════════════════════════════════════════════════"
echo "  TEST EXECUTION COMPLETE"
echo "═══════════════════════════════════════════════════════════════"
echo ""
echo "  Tests completed: ${tests_run}/${tests_total}"
echo "  Duration: ${test_duration}s ($((test_duration / 60))m $((test_duration % 60))s)"
echo ""

# Verify results
echo "───────────────────────────────────────────────────────────────"
echo "  VALIDATING RESULTS"
echo "───────────────────────────────────────────────────────────────"
echo ""

result_count=$(find results -name "fixed-500qps+*__waf-*__*.json" -type f ! -size 0 | wc -l | tr -d ' ')
echo "  JSON result files: ${result_count}"

if [[ ${result_count} -ge ${tests_total} ]]; then
    echo "  ✅ All tests produced results"
else
    echo "  ⚠️  Expected ${tests_total} results, found ${result_count}"
fi

echo ""

# Check for errors (403s in detection-only mode)
echo "  Checking error rates..."
error_found=false

for result_file in results/fixed-500qps+*__waf-*__*.json; do
    if [[ ! -f "${result_file}" ]] || [[ ! -s "${result_file}" ]]; then
        continue
    fi

    error_rate=$(python3 -c "
import json
import sys
try:
    with open('${result_file}') as f:
        data = json.load(f)
    codes = data.get('RetCodes', {})
    total = sum(codes.values())
    errors = sum(v for k, v in codes.items() if k != '200')
    if total > 0:
        rate = (errors / total) * 100
        if rate > 0:
            print(f'{rate:.1f}')
            sys.exit(1)
except:
    pass
" 2>/dev/null || echo "ERROR")

    if [[ "${error_rate}" == "ERROR" ]] || [[ -n "${error_rate}" ]]; then
        echo "    ⚠️  $(basename ${result_file}): ${error_rate}% errors"
        error_found=true
    fi
done

if [[ "${error_found}" == "false" ]]; then
    echo "  ✅ All tests have 0% error rate (detection-only working)"
else
    echo "  ⚠️  Some tests have errors - check detection-only mode configuration"
fi

echo ""
echo "───────────────────────────────────────────────────────────────"
echo "  GENERATING COMPREHENSIVE HTML REPORT"
echo "───────────────────────────────────────────────────────────────"
echo ""

if command -v python3 >/dev/null 2>&1; then
    if python3 generate-report.py; then
        echo ""
        echo "✅ Comprehensive report generated: results/performance-report.html"
        echo ""
        echo "  View report:"
        echo "    open results/performance-report.html"
        echo ""
        echo "  File size: $(du -h results/performance-report.html | awk '{print $1}')"
    else
        echo "❌ Error generating comprehensive report"
        exit 1
    fi
else
    echo "❌ python3 not found - cannot generate report"
    exit 1
fi

echo "═══════════════════════════════════════════════════════════════"
echo "  ✅ COMPREHENSIVE TEST SUITE COMPLETE"
echo "═══════════════════════════════════════════════════════════════"
echo ""
echo "📊 Summary:"
echo "  Tests run: ${tests_run}/${tests_total}"
echo "  Results: ${result_count} JSON files"
echo "  Report: results/performance-report.html"
echo "  Duration: ${test_duration}s"
echo ""

exit 0
