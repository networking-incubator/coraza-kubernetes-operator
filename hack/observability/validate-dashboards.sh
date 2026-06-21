#!/usr/bin/env bash
# Validate committed chart dashboard JSON (syntax, required fields, metric refs, size).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DASH_DIR="${ROOT_DIR}/charts/coraza-kubernetes-operator/dashboards"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

require_cmd() {
  command -v "$1" &>/dev/null || { echo "ERROR: $1 required" >&2; exit 1; }
}

require_cmd jq

echo "=== Dashboard JSON syntax ==="
for f in "${DASH_DIR}"/*.json; do
  if jq empty "${f}" 2>/dev/null; then
    pass "$(basename "${f}") valid JSON"
  else
    fail "$(basename "${f}") invalid JSON"
  fi
done

echo "=== Dashboard required fields ==="
for f in "${DASH_DIR}"/*.json; do
  uid="$(jq -r '.uid // empty' "${f}")"
  title="$(jq -r '.title // empty' "${f}")"
  if [ -n "${uid}" ] && [ -n "${title}" ]; then
    pass "$(basename "${f}") has uid and title"
  else
    fail "$(basename "${f}") missing uid or title"
  fi
done

echo "=== PromQL metric references ==="
OVERVIEW="${DASH_DIR}/coraza-operator-overview.json"
RESOURCES="${DASH_DIR}/coraza-operator-resources.json"
DATAPLANE="${DASH_DIR}/coraza-waf-dataplane.json"

# Collect expr fields from panel targets (nested rows included), not arbitrary JSON text.
collect_exprs() {
  jq -r '[.. | objects | .expr? | select(type == "string" and length > 0)] | unique | .[]' "$1"
}

check_metric_in_exprs() {
  local file="$1"
  local metric="$2"
  local label="$3"
  if collect_exprs "${file}" | grep -qF "${metric}"; then
    pass "${label} references ${metric}"
  else
    fail "${label} missing reference to ${metric}"
  fi
}

for metric in \
  coraza_cache_size_bytes \
  coraza:engines_not_ready:count \
  coraza_rulesource_validations_total \
  coraza_ruleset_validations_total \
  coraza_cache_set_duration_seconds \
  rest_client_requests_total; do
  check_metric_in_exprs "${OVERVIEW}" "${metric}" "overview"
done

check_metric_in_exprs "${OVERVIEW}" controller_runtime_reconcile_total "overview"
check_metric_in_exprs "${RESOURCES}" controller_runtime_reconcile_total "resources"

for metric in coraza_engine_condition coraza_ruleset_condition; do
  check_metric_in_exprs "${RESOURCES}" "${metric}" "resources"
done

for metric in \
  coraza_waf_requests_total \
  coraza_waf_blocked_requests_total \
  coraza_waf_rule_hits_total \
  coraza_waf_request_anomaly_score_bucket \
  coraza_waf_plugin_rule_count; do
  check_metric_in_exprs "${DATAPLANE}" "${metric}" "dataplane"
done

if collect_exprs "${DATAPLANE}" | grep -q 'event="coraza_waf_blocked_request"'; then
  pass "dataplane uses event label for blocked-request Loki query"
else
  fail "dataplane missing event=coraza_waf_blocked_request Loki query"
fi

echo "=== ConfigMap size budget ==="
# Per-file limit (Kubernetes ConfigMap value size). Combined budget leaves headroom
# for Helm template keys and etcd's ~1.5MiB ConfigMap object limit.
MAX_CONFIGMAP_COMBINED_BYTES=1048576
total=0
for f in "${DASH_DIR}"/*.json; do
  size=$(wc -c < "${f}")
  total=$((total + size))
  if [ "${size}" -lt 524288 ]; then
    pass "$(basename "${f}") size ${size} bytes (<512KiB)"
  else
    fail "$(basename "${f}") too large: ${size} bytes"
  fi
done
pass "combined dashboard JSON size ${total} bytes"
# Helm embeds JSON as indented YAML (~20% overhead vs raw files).
helm_budget=$((MAX_CONFIGMAP_COMBINED_BYTES * 80 / 100))
if [ "${total}" -gt "${MAX_CONFIGMAP_COMBINED_BYTES}" ]; then
  fail "combined dashboard JSON ${total} bytes exceeds safe ConfigMap budget (${MAX_CONFIGMAP_COMBINED_BYTES})"
elif [ "${total}" -gt "${helm_budget}" ]; then
  fail "combined dashboard JSON ${total} bytes leaves little headroom for Helm YAML embedding (budget ${helm_budget})"
else
  pass "combined dashboard JSON within safe ConfigMap budget (${MAX_CONFIGMAP_COMBINED_BYTES}, helm headroom ${helm_budget})"
fi

echo
echo "Results: ${PASS} passed, ${FAIL} failed"
if [ "${FAIL}" -gt 0 ]; then
  exit 1
fi
