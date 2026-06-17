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
out=$(render --set metrics.enabled=true --set metrics.serviceMonitor.enabled=true)
echo "$out" | grep -q "kind: ServiceMonitor" && pass "ServiceMonitor rendered" || fail "ServiceMonitor missing"
echo "$out" | grep -q "honorLabels: true" && pass "ServiceMonitor honorLabels enabled" || fail "ServiceMonitor honorLabels missing"
echo "$out" | grep -q 'action: labeldrop' && echo "$out" | grep -qF '^(job|instance)$' && pass "ServiceMonitor drops reserved scrape labels" || fail "ServiceMonitor labeldrop guard missing"
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
render_err=""
if ! render_err=$(render --set metrics.enabled=true --set metrics.podMonitor.enabled=true 2>&1); then
  if echo "$render_err" | grep -q "gatewaySelector"; then
    pass "Empty gatewaySelector rejected with descriptive error"
  else
    fail "Render failed but error did not mention gatewaySelector: $render_err"
  fi
else
  fail "Empty gatewaySelector should have caused a render failure"
fi

# ── Test 7: PodMonitor disabled when metrics.enabled=false ───────────────────
section "PodMonitor respects metrics.enabled gate"
out=$(render --set metrics.enabled=false --set metrics.podMonitor.enabled=true \
  --set 'metrics.podMonitor.gatewaySelector.app=my-gateway')
echo "$out" | grep -q "kind: PodMonitor" && fail "PodMonitor should be gated on metrics.enabled" || pass "PodMonitor correctly gated"

# ── Test 8: additionalLabels merged into PrometheusRule ──────────────────────
section "PrometheusRule additionalLabels"
out=$(render --set metrics.enabled=true --set metrics.prometheusRule.enabled=true \
  --set 'metrics.prometheusRule.additionalLabels.release=kube-prometheus-stack')
echo "$out" | grep -q "release: kube-prometheus-stack" && pass "additionalLabels merged" || fail "additionalLabels not merged"

# ── Test 9: PodMonitor port name override ────────────────────────────────────
section "PodMonitor portName override"
out=$(render --set metrics.podMonitor.enabled=true \
  --set 'metrics.podMonitor.gatewaySelector.app=my-gateway' \
  --set metrics.podMonitor.portName=custom-stats-port)
echo "$out" | grep -q "custom-stats-port" && pass "Custom portName rendered" || fail "Custom portName not rendered"

# ── Test 10: PrometheusRule custom rules injection ───────────────────────────
section "PrometheusRule custom rules"
out=$(render --set metrics.enabled=true --set metrics.prometheusRule.enabled=true \
  --set-json 'metrics.prometheusRule.rules=[{"alert":"CustomAlert","expr":"up == 0","for":"1m","labels":{"severity":"info"},"annotations":{"summary":"custom"}}]')
echo "$out" | grep -q "CustomAlert" && pass "Custom alert rule injected" || fail "Custom alert rule missing"

# ── Test 11: PodMonitor additionalLabels ─────────────────────────────────────
section "PodMonitor additionalLabels"
out=$(render --set metrics.podMonitor.enabled=true \
  --set 'metrics.podMonitor.gatewaySelector.app=my-gateway' \
  --set 'metrics.podMonitor.additionalLabels.release=kube-prometheus-stack')
echo "$out" | grep -q "release: kube-prometheus-stack" && pass "PodMonitor additionalLabels merged" || fail "PodMonitor additionalLabels not merged"

# ── Test 12: PodMonitor metricRelabelings injection ──────────────────────────
section "PodMonitor metricRelabelings"
out=$(render --set metrics.podMonitor.enabled=true \
  --set 'metrics.podMonitor.gatewaySelector.app=my-gateway' \
  --set-json 'metrics.podMonitor.metricRelabelings=[{"sourceLabels":["__name__"],"regex":"coraza_waf_requests_total","action":"drop"}]')
echo "$out" | grep -q "coraza_waf_requests_total" && pass "User metricRelabelings injected" || fail "User metricRelabelings missing"
echo "$out" | grep -qF 'coraza_waf_.*' && pass "Mandatory cardinality guard still present" || fail "Mandatory cardinality guard missing"

# ── Test 12b: EnvoyFilter stats_tags ─────────────────────────────────────────
section "EnvoyFilter stats_tags"
out=$(render --set metrics.envoyStatsTags.enabled=true --set 'metrics.envoyStatsTags.gatewaySelector.app=my-gateway')
echo "$out" | grep -q "kind: EnvoyFilter" && pass "EnvoyFilter rendered" || fail "EnvoyFilter missing"
echo "$out" | grep -q "tag_name: engine" && pass "engine stats_tag present" || fail "engine stats_tag missing"

# ── Test 14: Grafana dashboard ConfigMap ─────────────────────────────────────
section "Grafana dashboard ConfigMap"
dash_tmp=$(mktemp /tmp/coraza-dashboard-helm-XXXXXX.yaml)
render --set metrics.enabled=true --set metrics.grafanaDashboard.enabled=true \
  --set metrics.prometheusRule.enabled=true \
  -s templates/grafana-dashboard-configmap.yaml > "${dash_tmp}"
grep -q "kind: ConfigMap" "${dash_tmp}" && pass "Dashboard ConfigMap rendered" || fail "Dashboard ConfigMap missing"
grep -q "grafana_dashboard" "${dash_tmp}" && pass "grafana_dashboard label present" || fail "grafana_dashboard label missing"
grep -q "coraza-operator-overview.json" "${dash_tmp}" && pass "Overview dashboard embedded" || fail "Overview dashboard missing"
grep -q "coraza-operator-resources.json" "${dash_tmp}" && pass "Resources dashboard embedded" || fail "Resources dashboard missing"
grep -q "grafana_folder" "${dash_tmp}" && pass "grafana_folder annotation present" || fail "grafana_folder annotation missing"
rm -f "${dash_tmp}"

# ── Test 15: ServiceMonitor additionalLabels ─────────────────────────────────
section "ServiceMonitor additionalLabels"
out=$(render --set metrics.enabled=true --set metrics.serviceMonitor.enabled=true \
  --set 'metrics.serviceMonitor.additionalLabels.release=kube-prometheus-stack')
echo "$out" | grep -q "release: kube-prometheus-stack" && pass "ServiceMonitor additionalLabels merged" || fail "ServiceMonitor additionalLabels not merged"

# ── Test 16: Grafana dashboard gated on metrics.enabled ──────────────────────
section "Grafana dashboard respects metrics.enabled gate"
out=$(render --set metrics.enabled=false --set metrics.grafanaDashboard.enabled=true)
echo "$out" | grep -q "coraza-operator-overview.json" && fail "Dashboard should be gated on metrics.enabled" || pass "Dashboard correctly gated"

# ── Test 16b: Grafana dashboard requires PrometheusRule ───────────────────────
section "Grafana dashboard requires prometheusRule"
out=$(render --set metrics.enabled=true --set metrics.grafanaDashboard.enabled=true \
  --set metrics.prometheusRule.enabled=false)
echo "$out" | grep -q "coraza-operator-overview.json" && fail "Dashboard should require prometheusRule.enabled" || pass "Dashboard correctly gated on prometheusRule"

# ── Test 17: promtool check rules (optional) ─────────────────────────────────
if command -v promtool &>/dev/null; then
  section "promtool check rules"
  tmpfile=$(mktemp /tmp/prometheusrule-XXXXXX.yaml)
  # Render only the PrometheusRule template and extract spec.groups with
  # dedented indentation so promtool sees a valid rules file.
  render --set metrics.enabled=true --set metrics.prometheusRule.enabled=true \
    -s templates/prometheusrule.yaml \
    | sed -n '/^  groups:/,$ { s/^  //; p; }' > "$tmpfile"
  if promtool check rules "$tmpfile" &>/dev/null; then
    pass "promtool check rules passed"
  else
    fail "promtool check rules failed"
  fi
  rm -f "$tmpfile"
else
  echo
  echo "SKIP: promtool not found — install prometheus to validate rules"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
