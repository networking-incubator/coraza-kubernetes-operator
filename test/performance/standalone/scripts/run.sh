#!/usr/bin/env bash
# One measured run against the standalone rig.
#
#   scripts/run.sh baseline   # B0, no WAF filter        (envoy-baseline.yaml)
#   scripts/run.sh minimal    # WAF on, ~2 rules         (envoy-waf.yaml)
#   scripts/run.sh crs        # WAF on, full OWASP CRS    (envoy-crs.yaml)
#   scripts/run.sh waf        # alias for `minimal`
#
# Knobs (env vars):
#   QPS=500 DURATION=20s CONNECTIONS=8 PATH_=/ PAYLOAD_KB=0 RUN_IDX=1 scripts/run.sh crs
#
# Methodology (mirrors ../ARCHITECTURE.md §9): validity-probe the WAF is actually
# active, discard a warm-up window, then measure steady state with an open-model
# constant-QPS generator (HDR histogram). Report WAF cost as a DELTA vs baseline.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${here}"

MODE="${1:-minimal}"
# An attack that the mode's ruleset must block (proves the filter is really on).
ATTACK_PATH="/admin"
EXPECT_ATTACK="403"
case "${MODE}" in
  baseline)       export ENVOY_CONFIG="envoy-baseline.yaml"; EXPECT_ATTACK="200" ;;
  minimal|waf)    MODE="minimal"; export ENVOY_CONFIG="envoy-waf.yaml" ;;
  crs)            export ENVOY_CONFIG="envoy-crs.yaml"
                  # XSS in a query arg -> CRS 941xxx (critical, score 5) -> blocked at PL1.
                  ATTACK_PATH="/?q=<script>alert(1)</script>" ;;
  *) echo "usage: $0 {baseline|minimal|crs}" >&2; exit 2 ;;
esac

QPS="${QPS:-500}"
DURATION="${DURATION:-20s}"
CONNECTIONS="${CONNECTIONS:-8}"
REQ_PATH="${PATH_:-/}"          # PATH_ (not PATH) to avoid clobbering $PATH
WARMUP="${WARMUP:-5s}"
PAYLOAD_KB="${PAYLOAD_KB:-0}"
RUN_IDX="${RUN_IDX:-1}"

ts="$(date -u +%Y%m%d-%H%M%S)"
json="/results/${MODE}__run${RUN_IDX}__${ts}.json"     # path inside fortio container
host_json="results/${MODE}__run${RUN_IDX}__${ts}.json" # same file on the host
mkdir -p results

echo "== bringing up envoy (${ENVOY_CONFIG}) + backend =="
[[ -f coraza.wasm ]] || { echo "coraza.wasm missing — run scripts/extract-wasm.sh first" >&2; exit 1; }
docker compose up -d --wait backend envoy >/dev/null 2>&1
# CRS compiles ~900 rules in the WASM VM on first request; give it a moment.
sleep 2

echo "== waiting for envoy =="
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null "http://localhost:9901/ready" 2>/dev/null && break
  sleep 1
done

echo "== validity probe =="
benign_code="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:10000/")"
attack_code="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:10000${ATTACK_PATH}")"
echo "   GET /                 -> ${benign_code}"
echo "   GET ${ATTACK_PATH}    -> ${attack_code} (expect ${EXPECT_ATTACK})"
[[ "${benign_code}" == "200" ]] || { echo "FAIL: benign request not 200 — misconfigured" >&2; exit 1; }
if [[ "${MODE}" != "baseline" ]]; then
  [[ "${attack_code}" == "${EXPECT_ATTACK}" ]] || { echo "FAIL: attack not blocked — WAF not active, run invalid" >&2; exit 1; }
fi

payload_args=()
[[ "${PAYLOAD_KB}" -gt 0 ]] && payload_args=(-payload-size "$((PAYLOAD_KB * 1024))")
url="http://envoy:10000${REQ_PATH}"

echo "== warm-up (${WARMUP}, discarded) =="
docker compose run --rm fortio \
  load -qps "${QPS}" -t "${WARMUP}" -c "${CONNECTIONS}" -quiet "${payload_args[@]}" "${url}" >/dev/null 2>&1 || true

echo "== measured run [${MODE} #${RUN_IDX}]: qps=${QPS} t=${DURATION} c=${CONNECTIONS} path=${REQ_PATH} payload=${PAYLOAD_KB}KB =="
docker compose run --rm fortio \
  load -qps "${QPS}" -t "${DURATION}" -c "${CONNECTIONS}" \
       -p 50,90,95,99,99.9 -json "${json}" -quiet "${payload_args[@]}" "${url}" >/dev/null

summary="results/${MODE}__run${RUN_IDX}__${ts}.summary.txt"
python3 - "${host_json}" "${MODE}" > "${summary}" <<'PY'
import json, sys
data = json.load(open(sys.argv[1])); tag = sys.argv[2]
h = data["DurationHistogram"]
pct = {round(p["Percentile"],1): p["Value"]*1000 for p in h.get("Percentiles", [])}
codes = data.get("RetCodes", {})
total = sum(codes.values()) or 1
blocked = codes.get("403", 0)  # WAF block, not an error
err = 100*(total - codes.get("200", 0) - blocked)/total
print(f"{tag}: qps={data['ActualQPS']:.0f} "
      f"p50={pct.get(50,0):.2f} p90={pct.get(90,0):.2f} p95={pct.get(95,0):.2f} "
      f"p99={pct.get(99,0):.2f} p99.9={pct.get(99.9,0):.2f} ms  "
      f"blocked={100*blocked/total:.2f}% err={err:.2f}% codes={codes}")
PY
cat "${summary}"
