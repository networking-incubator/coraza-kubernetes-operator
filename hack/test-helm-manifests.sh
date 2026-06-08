#!/usr/bin/env bash
set -euo pipefail

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)/charts/coraza-kubernetes-operator"
RELEASE="coraza-test"
NAMESPACE="coraza-system"
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS+1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
section() { echo; echo "=== $1 ==="; }

require_cmd() {
  if ! command -v "$1" &>/dev/null; then
    echo "ERROR: $1 is required but not found" >&2
    exit 1
  fi
}

require_cmd helm

render() {
  helm template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" "$@" 2>&1
}

# ── Test 1: default render (no optional features) ────────────────────────────
section "Default render"
out=$(render)
echo "$out" | grep -q "kind: ServiceAccount" && pass "ServiceAccount present" || fail "ServiceAccount missing"
echo "$out" | grep -q "kind: PrometheusRule" && fail "PrometheusRule should not be present by default" || pass "PrometheusRule absent (correct)"
echo "$out" | grep -q "kind: PodMonitor" && fail "PodMonitor should not be present by default" || pass "PodMonitor absent (correct)"
echo "$out" | grep -q "kind: ServiceMonitor" && fail "ServiceMonitor should not be present by default" || pass "ServiceMonitor absent when disabled (correct)"

# ── Test 2: ServiceMonitor enabled ───────────────────────────────────────────
section "ServiceMonitor enabled"
out=$(render --set metrics.serviceMonitor.enabled=true)
echo "$out" | grep -q "kind: ServiceMonitor" && pass "ServiceMonitor rendered" || fail "ServiceMonitor missing"
echo "$out" | grep -q "monitoring.coreos.com" && pass "monitoring.coreos.com API group" || fail "Wrong API group"

# ── Test 3: PrometheusRule enabled ───────────────────────────────────────────
section "PrometheusRule enabled"
out=$(render --set metrics.enabled=true --set metrics.prometheusRule.enabled=true)
echo "$out" | grep -q "kind: PrometheusRule" && pass "PrometheusRule rendered" || fail "PrometheusRule missing"
echo "$out" | grep -q "CorazaEngineNotReady" && pass "CorazaEngineNotReady alert present" || fail "CorazaEngineNotReady alert missing"
echo "$out" | grep -q "coraza:engines_not_ready:count" && pass "Recording rule present" || fail "Recording rule missing"

# ── Test 4: PrometheusRule disabled when metrics.enabled=false ───────────────
section "PrometheusRule respects metrics.enabled gate"
out=$(render --set metrics.enabled=false --set metrics.prometheusRule.enabled=true)
echo "$out" | grep -q "kind: PrometheusRule" && fail "PrometheusRule should be gated on metrics.enabled" || pass "PrometheusRule correctly gated"

# ── Test 5: PodMonitor with valid selector ────────────────────────────────────
section "PodMonitor with gatewaySelector"
out=$(render --set metrics.podMonitor.enabled=true --set 'metrics.podMonitor.gatewaySelector.app=my-gateway')
echo "$out" | grep -q "kind: PodMonitor" && pass "PodMonitor rendered" || fail "PodMonitor missing"
echo "$out" | grep -q "coraza_waf_" && pass "cardinality filter present" || fail "cardinality filter missing"

# ── Test 6: PodMonitor with empty selector fails ──────────────────────────────
section "PodMonitor rejects empty gatewaySelector"
render_out=$(render --set metrics.podMonitor.enabled=true || true)
if echo "$render_out" | grep -q "gatewaySelector"; then
  pass "Empty gatewaySelector rejected with descriptive error"
else
  fail "Empty gatewaySelector should fail with descriptive error"
fi

# ── Test 7: additionalLabels merged into PrometheusRule ──────────────────────
section "PrometheusRule additionalLabels"
out=$(render --set metrics.enabled=true --set metrics.prometheusRule.enabled=true \
  --set 'metrics.prometheusRule.additionalLabels.release=prometheus')
echo "$out" | grep -q "release: prometheus" && pass "additionalLabels merged" || fail "additionalLabels not merged"

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
