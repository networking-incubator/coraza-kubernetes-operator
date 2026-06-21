#!/usr/bin/env bash
# Validate Promtail regex patterns against representative Gateway log lines.
set -euo pipefail

pass=0
fail=0

check() {
  local name="$1"
  local pattern="$2"
  local line="$3"
  if echo "${line}" | grep -Eq "${pattern}"; then
    echo "  PASS: ${name}"
    pass=$((pass + 1))
  else
    echo "  FAIL: ${name}" >&2
    fail=$((fail + 1))
  fi
}

extract_field() {
  local line="$1"
  local pattern="$2"
  echo "${line}" | sed -n "${pattern}" | head -1
}

extract_attack_tag() {
  echo "${line}" | sed -n 's/.*\[tag "attack-\([^"]*\)"\].*/\1/p' | head -1
}

extract_category() {
  local line="$1"
  if echo "${line}" | grep -q '"category"'; then
    echo "${line}" | sed -n 's/.*"category":"\([^"]*\)".*/\1/p' | tr '-' '_'
    return
  fi
  local tag
  tag="$(extract_attack_tag "${line}")"
  if [ -n "${tag}" ]; then
    echo "${tag}" | tr '-' '_'
    return
  fi
  if echo "${line}" | grep -q 'Coraza: Access denied.*\[id "'; then
    echo "other"
  fi
}

is_waf_block_line() {
  case "$1" in
    *coraza_waf_blocked_request*|*Coraza:\ Access\ denied*) return 0 ;;
    *) return 1 ;;
  esac
}

should_drop_unrelated() {
  ! is_waf_block_line "$1"
}

normalize_event() {
  local line="$1"
  if is_waf_block_line "${line}"; then
    echo "coraza_waf_blocked_request"
  fi
}

json_line='2026-06-19T13:23:29.428384Z	warning	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1152	wasm log integration-tests.coraza-engine-coraza: {"event":"coraza_waf_blocked_request","engine":"coraza","namespace":"integration-tests","rule_id":3001,"severity":"CRITICAL","category":"other","phase":"http_request_headers","action":"deny","status":403,"client_ip":"127.0.0.1","method":"GET","uri":"/?evilmonkey=1"}	thread=45'

audit_line='2026-06-19T13:23:29.428297Z	critical	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1158	wasm log integration-tests.coraza-engine-coraza: [client "127.0.0.1"] Coraza: Access denied (phase 2). Evil Monkey Detected [file "_inline_"] [line "46"] [id "3001"] [severity "critical"] [uri "/?evilmonkey=1"] [tag "monkey-attack"]	thread=45'

crs_audit='2026-06-19T12:00:00.000000Z	critical	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1158	wasm log crs-conformance-ad1e41.coraza-engine-conformance-engine: [client "10.0.0.1"] Coraza: Access denied (phase 1). XSS Attack [id "941100"] [severity "critical"] [uri "/test"] [tag "application-multi"] [tag "attack-xss"] [tag "OWASP_CRS"]	thread=12'

crs_injection='2026-06-19T12:00:00.000000Z	critical	envoy wasm wasm log integration-tests.coraza-engine-coraza: Coraza: Access denied [id "942100"] [tag "attack-injection-php"] [tag "OWASP_CRS"]'

json_hyphen_category='2026-06-19T13:23:29.428384Z	warning	envoy wasm external/envoy/source/extensions/common/wasm/context.cc:1152	wasm log integration-tests.coraza-engine-coraza: {"event":"coraza_waf_blocked_request","engine":"coraza","namespace":"integration-tests","rule_id":942100,"severity":"CRITICAL","category":"injection-php","phase":"http_request_body","action":"deny","status":403,"client_ip":"10.0.0.1","method":"POST","uri":"/login"}	thread=45'

echo "=== Promtail pattern checks ==="
check "JSON block event" \
  '.*\{"event":"coraza_waf_blocked_request".*\}.*' \
  "${json_line}"
check "Audit block event" \
  'Coraza: Access denied.*\[id "[^"]+"\]' \
  "${audit_line}"
check "CRS attack-xss tag" \
  '\[tag "attack-xss"\]' \
  "${crs_audit}"
check "First CRS attack tag" \
  '\[tag "attack-xss"\]' \
  "${crs_audit}"
check "CRS attack-injection-php tag" \
  '\[tag "attack-injection-php"\]' \
  "${crs_injection}"
check "Audit client_ip" \
  '\[client "[^"]+"\]' \
  "${audit_line}"
echo "=== Promtail filter checks (LogQL !~ — RE2 drop stage cannot use lookahead) ==="
unrelated_line='2026-06-19T13:23:29.428384Z info envoy upstream some unrelated line'
if should_drop_unrelated "${unrelated_line}"; then
  echo "  PASS: WAF line filter drops unrelated lines"
  pass=$((pass + 1))
else
  echo "  FAIL: WAF line filter drops unrelated lines" >&2
  fail=$((fail + 1))
fi
if is_waf_block_line "${json_line}"; then
  echo "  PASS: JSON block line passes WAF filter"
  pass=$((pass + 1))
else
  echo "  FAIL: JSON block line should pass WAF filter" >&2
  fail=$((fail + 1))
fi
if is_waf_block_line "${crs_audit}"; then
  echo "  PASS: CRS audit line passes WAF filter"
  pass=$((pass + 1))
else
  echo "  FAIL: CRS audit line should pass WAF filter" >&2
  fail=$((fail + 1))
fi

echo "=== Category derivation checks ==="
got="$(extract_category "${json_line}")"
[ "${got}" = "other" ] && echo "  PASS: JSON category" && pass=$((pass + 1)) || { echo "  FAIL: JSON category (got ${got})" >&2; fail=$((fail + 1)); }
got="$(extract_category "${audit_line}")"
[ "${got}" = "other" ] && echo "  PASS: custom rule audit defaults to other" && pass=$((pass + 1)) || { echo "  FAIL: custom rule audit (got ${got})" >&2; fail=$((fail + 1)); }
got="$(extract_category "${crs_audit}")"
[ "${got}" = "xss" ] && echo "  PASS: CRS audit attack-xss -> xss" && pass=$((pass + 1)) || { echo "  FAIL: CRS audit attack-xss (got ${got})" >&2; fail=$((fail + 1)); }
got="$(extract_category "${crs_injection}")"
[ "${got}" = "injection_php" ] && echo "  PASS: CRS audit attack-injection-php -> injection_php" && pass=$((pass + 1)) || { echo "  FAIL: CRS audit injection (got ${got})" >&2; fail=$((fail + 1)); }
got="$(extract_category "${json_hyphen_category}")"
[ "${got}" = "injection_php" ] && echo "  PASS: JSON hyphenated category injection-php -> injection_php" && pass=$((pass + 1)) || { echo "  FAIL: JSON hyphenated category (got ${got})" >&2; fail=$((fail + 1)); }

echo "=== Event normalization checks ==="
got="$(normalize_event "${json_line}")"
[ "${got}" = "coraza_waf_blocked_request" ] && echo "  PASS: JSON event label" && pass=$((pass + 1)) || { echo "  FAIL: JSON event label (got ${got})" >&2; fail=$((fail + 1)); }
got="$(normalize_event "${crs_audit}")"
[ "${got}" = "coraza_waf_blocked_request" ] && echo "  PASS: audit event label" && pass=$((pass + 1)) || { echo "  FAIL: audit event label (got ${got})" >&2; fail=$((fail + 1)); }

echo "=== Field extraction checks ==="
got="$(extract_field "${audit_line}" 's/.*\[client "\([^"]*\)"\].*/\1/p')"
[ "${got}" = "127.0.0.1" ] && echo "  PASS: audit client_ip" && pass=$((pass + 1)) || { echo "  FAIL: audit client_ip (got ${got})" >&2; fail=$((fail + 1)); }
got="$(extract_field "${json_line}" 's/.*"method":"\([^"]*\)".*/\1/p')"
[ "${got}" = "GET" ] && echo "  PASS: JSON method" && pass=$((pass + 1)) || { echo "  FAIL: JSON method (got ${got})" >&2; fail=$((fail + 1)); }
got="$(extract_field "${json_line}" 's/.*"severity":"\([^"]*\)".*/\1/p' | tr '[:lower:]' '[:upper:]')"
[ "${got}" = "CRITICAL" ] && echo "  PASS: severity uppercase" && pass=$((pass + 1)) || { echo "  FAIL: severity uppercase (got ${got})" >&2; fail=$((fail + 1)); }

if [ "${fail}" -ne 0 ]; then
  echo "Results: ${pass} passed, ${fail} failed" >&2
  exit 1
fi
echo "Results: ${pass} passed, ${fail} failed"
