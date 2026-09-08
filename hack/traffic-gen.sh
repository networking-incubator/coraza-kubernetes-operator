#!/usr/bin/env bash
# Traffic generator for local Coraza WAF testing.
# Sends a mix of clean and attack requests to the gateway, then loops.
#
# Usage:
#   ./hack/traffic-gen.sh [GATEWAY_IP] [INTERVAL_SECONDS]
#
# Defaults:
#   GATEWAY_IP = 172.20.255.127 (MetalLB VIP from make cluster.kind)
#   INTERVAL   = 1s between rounds

set -euo pipefail

GW="${1:-172.20.255.127}"
INTERVAL="${2:-1}"
HOST="echo.integration-tests.svc.cluster.local"
BASE="http://${GW}"

PASS=0
BLOCK=0
ERR=0

send() {
  local label="$1"; shift
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' "$@" 2>/dev/null || echo "000")
  case "$code" in
    2*|3*) PASS=$((PASS+1));  printf '  %-38s → PASS  (%s)\n'  "$label" "$code" ;;
    403)   BLOCK=$((BLOCK+1)); printf '  %-38s → BLOCK (403)\n' "$label" ;;
    *)     ERR=$((ERR+1));    printf '  %-38s → ERR   (%s)\n'  "$label" "$code" ;;
  esac
}

echo "Traffic generator → ${BASE}  (Host: ${HOST})"
echo "Ctrl-C to stop. Interval: ${INTERVAL}s"

ROUND=0
while true; do
  ROUND=$((ROUND+1))
  printf '\n── Round %-3d ──────────────────────────────────\n' "$ROUND"

  # --- clean requests ---
  send "GET /"                        -H "Host: $HOST" "${BASE}/"
  send "GET /health"                  -H "Host: $HOST" "${BASE}/health"
  send "GET /api/v1/items?page=2"     -H "Host: $HOST" "${BASE}/api/v1/items?page=2"
  send "POST /api/data"               -H "Host: $HOST" -X POST \
    -H 'Content-Type: application/json' -d '{"key":"value"}' "${BASE}/api/data"

  # --- SQLi matching the loaded rule (rule id:1001) ---
  send "SQLi: 1' OR '1'='1"           -H "Host: $HOST" \
    "${BASE}/?user=1'+OR+'1'%3d'1"
  send "SQLi: in POST body"           -H "Host: $HOST" -X POST \
    -d "user=1' OR '1'='1&pass=x" "${BASE}/login"

  # --- other attack patterns (may not be blocked with minimal ruleset) ---
  send "SQLi: UNION SELECT"           -H "Host: $HOST" \
    "${BASE}/?id=1+UNION+SELECT+1,2,3--"
  send "XSS: script tag"              -H "Host: $HOST" \
    "${BASE}/?q=%3Cscript%3Ealert%281%29%3C%2Fscript%3E"
  send "LFI: etc/passwd"              -H "Host: $HOST" \
    "${BASE}/../../../../etc/passwd"
  send "CMDi: semicolon"              -H "Host: $HOST" \
    "${BASE}/?cmd=ls%3Bcat+%2Fetc%2Fpasswd"
  send "UA: sqlmap"                   -H "Host: $HOST" \
    -H "User-Agent: sqlmap/1.7.2" "${BASE}/"

  printf '\nTotals  pass=%-4d block=%-4d err=%-4d\n' "$PASS" "$BLOCK" "$ERR"
  sleep "$INTERVAL"
done
