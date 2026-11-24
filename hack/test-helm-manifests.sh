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

# expect_render_fail PATTERN ARGS...: rendering with ARGS must fail with an
# error that matches PATTERN.
expect_render_fail() {
  local pattern=$1 render_err
  shift
  if render_err=$(render "$@"); then
    fail "$* should fail the render"
  elif grep -qE -- "$pattern" <<<"$render_err"; then
    pass "$* rejected with descriptive error"
  else
    fail "Render with $* failed without matching '$pattern': $render_err"
  fi
}

# ── Test 1: default render (no optional features) ────────────────────────────
section "Default render"
out=$(render)
echo "$out" | grep -q "kind: ServiceAccount" && pass "ServiceAccount present" || fail "ServiceAccount missing"
echo "$out" | grep -q "kind: PrometheusRule" && fail "PrometheusRule should not be present by default" || pass "PrometheusRule absent (correct)"
echo "$out" | grep -q "kind: PodMonitor" && fail "PodMonitor should not be present by default" || pass "PodMonitor absent (correct)"
echo "$out" | grep -q "kind: ServiceMonitor" && fail "ServiceMonitor should not be present by default" || pass "ServiceMonitor absent when disabled (correct)"

# ── Test 1b: operator memory sizing ──────────────────────────────────────────
section "Operator memory sizing"
out=$(render -s templates/deployment.yaml)
echo "$out" | grep -A4 "^ *limits:" | grep -q "memory: 1Gi" && pass "memory limit defaults to 1Gi" || fail "memory limit default is not 1Gi"
echo "$out" | grep -A4 "^ *requests:" | grep -q "memory: 512Mi" && pass "memory request defaults to 512Mi" || fail "memory request default is not 512Mi"
echo "$out" | grep -A3 "name: MEMORY_LIMIT_BYTES" | grep -q "resource: limits.memory" &&
  pass "MEMORY_LIMIT_BYTES comes from limits.memory" || fail "MEMORY_LIMIT_BYTES is not set from limits.memory"
echo "$out" | grep -A1 "resourceName: memory" | grep -q "restartPolicy: RestartContainer" &&
  pass "a memory resize restarts the container" || fail "memory resizePolicy is not RestartContainer"

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
for alert in CorazaXDSCertificateExpiringSoon CorazaXDSCertificateExpired; do
  echo "$out" | grep -q "alert: $alert$" && pass "$alert alert present" || fail "$alert alert missing"
done
echo "$out" | grep -q "coraza_xds_serving_cert_not_after_seconds" && echo "$out" | grep -q "coraza_xds_ca_not_after_seconds" &&
  pass "certificate expiry alerts read both gauges" || fail "certificate expiry alerts do not read both gauges"

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

# ── Rule policy: defaults pass only the ceiling and response access ──────────
section "Rule policy defaults"
out=$(render)
echo "$out" | grep -q -- "--rule-max-request-body-limit=16777216" && pass "default request body ceiling passed" || fail "default request body ceiling missing"
echo "$out" | grep -q -- "--rule-response-body-access=Allowed" && pass "default response body access passed" || fail "default response body access missing"
echo "$out" | grep -q -- "--rule-denied-" && fail "no denied lists should be passed by default" || pass "no denied lists by default"
echo "$out" | grep -qF -- '- "--rule-max-rules=20000"' && pass "default rule limit passed" || fail "default rule limit missing"

# ── Rule policy: lists and limits render as manager flags ────────────────────
section "Rule policy values"
out=$(render --set 'rulePolicy.deniedOperators={pmFromFile,rx}' \
  --set 'rulePolicy.deniedDirectives={SecRuleEngine,Include}' \
  --set rulePolicy.maxRequestBodyLimit=1048576 \
  --set rulePolicy.responseBodyAccess=Denied \
  --set rulePolicy.maxRules=500)
echo "$out" | grep -qF -- '- "--rule-denied-operators=pmFromFile,rx"' && pass "denied operators joined and quoted" || fail "denied operators missing"
echo "$out" | grep -qF -- '- "--rule-denied-directives=SecRuleEngine,Include"' && pass "denied directives joined and quoted" || fail "denied directives missing"
echo "$out" | grep -qF -- '- "--rule-max-request-body-limit=1048576"' && pass "request body ceiling rendered as a quoted integer" || fail "request body ceiling not rendered as a quoted integer"
echo "$out" | grep -qF -- '- "--rule-response-body-access=Denied"' && pass "response body access quoted" || fail "response body access missing"
echo "$out" | grep -qF -- '- "--rule-max-rules=500"' && pass "rule limit rendered as a quoted integer" || fail "rule limit not rendered as a quoted integer"

# ── Rule policy: list entries with spaces or '#' stay one quoted argument ────
section "Rule policy entries are quoted"
out=$(render --set 'rulePolicy.deniedDirectives={Sec Rule,#x}')
echo "$out" | grep -qF -- '- "--rule-denied-directives=Sec Rule,#x"' && pass "entries with spaces and '#' rendered whole" || fail "entries with spaces and '#' not rendered whole"

# ── Rule policy: invalid body limit or response access fails ─────────────────
section "Rule policy values are validated"
for value in "--set rulePolicy.maxRequestBodyLimit=1.5" "--set-string rulePolicy.maxRequestBodyLimit=16Mi" \
  "--set rulePolicy.maxRequestBodyLimit=0" "--set rulePolicy.maxRequestBodyLimit=16777217" \
  "--set rulePolicy.responseBodyAccess=null" "--set rulePolicy.responseBodyAccess=denied" \
  "--set rulePolicy.deniedOperators=pm" "--set rulePolicy.deniedDirectives=SecRuleEngine" \
  "--set rulePolicy.maxRules=0" "--set rulePolicy.maxRules=1.5" "--set-string rulePolicy.maxRules=many"; do
  key=${value#* rulePolicy.}
  key=${key%%=*}
  # shellcheck disable=SC2086
  expect_render_fail "rulePolicy.$key must be" $value
done

# ── Rule policy: list entries must be strings ────────────────────────────────
section "Rule policy list entries are validated"
for value in "rulePolicy.deniedOperators[0]=1" "rulePolicy.deniedDirectives[0]=true"; do
  expect_render_fail "${value%%\[*} entries must be strings" --set "$value"
done

# ── Rule policy: empty list entries are dropped ──────────────────────────────
section "Rule policy empty entries"
out=$(render --set 'rulePolicy.deniedOperators={}' --set 'rulePolicy.deniedDirectives={}')
echo "$out" | grep -q -- "--rule-denied-" && fail "empty lists should pass no denied flags" || pass "empty lists pass no denied flags"
out=$(render --set 'rulePolicy.deniedOperators={pm,}')
echo "$out" | grep -qF -- '- "--rule-denied-operators=pm"' && pass "empty entries dropped from a list" || fail "empty entries not dropped from a list"

# ── Test 18: serving certificate generated by default ────────────────────────
section "Serving certificate generated by default"
out=$(render)
echo "$out" | grep -qE "^  name: .+-serving-cert$" && pass "generated serving Secret present" || fail "generated serving Secret missing"
echo "$out" | grep -q "^  ca.crt:" && pass "generated Secret carries ca.crt" || fail "generated Secret missing ca.crt"
echo "$out" | grep -q "^  tls.key:" && pass "generated Secret carries tls.key" || fail "generated Secret missing tls.key"

# ── Test 19: generated certificate lifetime is 365 days ──────────────────────
section "Generated certificate lifetime"
if command -v openssl &>/dev/null; then
  crt_b64=$(echo "$out" | awk '/^  tls.crt:/{print $2}')
  if [ -n "$crt_b64" ]; then
    # -checkend is portable; `date -d` is GNU-only and fails on macOS.
    crt=$(printf '%s' "$crt_b64" | openssl base64 -d -A)
    if printf '%s' "$crt" | openssl x509 -noout -checkend $((360 * 86400)) >/dev/null 2>&1 &&
      ! printf '%s' "$crt" | openssl x509 -noout -checkend $((370 * 86400)) >/dev/null 2>&1; then
      pass "generated certificate valid ~365 days"
    else
      fail "generated certificate lifetime is not ~365 days"
    fi
  else
    fail "generated certificate not found in render"
  fi
else
  echo
  echo "SKIP: openssl not found — cannot check certificate lifetime"
fi

# ── Test 20: tls.generate=false renders no serving certificate or key ────────
section "tls.generate=false omits serving certificate"
out=$(render --set tls.generate=false)
if echo "$out" | grep -q "^kind: Secret"; then
  fail "serving Secret should be absent with tls.generate=false"
else
  pass "serving Secret absent with tls.generate=false"
fi
if echo "$out" | grep -q "^  tls.key:"; then
  fail "private key rendered with tls.generate=false"
else
  pass "no private key rendered with tls.generate=false"
fi

# ── Test 21: tls.existingSecret suppresses generation ────────────────────────
section "tls.existingSecret suppresses generation"
out=$(render --set tls.existingSecret=my-serving-cert)
if echo "$out" | grep -q "^kind: Secret"; then
  fail "chart should not create a serving Secret when existingSecret is set"
else
  pass "no serving Secret with tls.existingSecret set"
fi
echo "$out" | grep -q "secretName: my-serving-cert" && pass "deployment mounts existingSecret" || fail "deployment does not reference existingSecret"

# ── Test 22: operator egress restriction is off by default ───────────────────
section "NetworkPolicy egress disabled by default"
out=$(render -s templates/networkpolicy.yaml)
echo "$out" | grep -q "^    - Egress" && fail "Egress policyType should be absent by default" || pass "Egress policyType absent by default"
echo "$out" | grep -q "^  egress:" && fail "egress rules should be absent by default" || pass "egress rules absent by default"

# ── Test 22b: null networkPolicy maps still render the ingress policy ────────
section "NetworkPolicy with null values"
for value in "networkPolicy=null" "networkPolicy.egress=null" "networkPolicy.egress.apiServer=null" "networkPolicy.egress.enabled=null"; do
  if out=$(render -s templates/networkpolicy.yaml --set "$value"); then
    echo "$out" | grep -q "port: 8081" && ! echo "$out" | grep -q "^  egress:" &&
      pass "$value renders the ingress policy without egress" || fail "$value did not render the ingress policy alone"
  else
    fail "$value should render: $out"
  fi
done

# ── Test 23: egress restricted to the configured API server addresses ────────
section "NetworkPolicy egress with apiServer.cidrs"
out=$(render -s templates/networkpolicy.yaml --set networkPolicy.egress.enabled=true \
  --set-json 'networkPolicy.egress.apiServer.cidrs=[" 10.0.0.1/32 ","fd00::1/128"]')
echo "$out" | grep -q "^    - Egress" && pass "Egress policyType added" || fail "Egress policyType missing"
echo "$out" | grep -qF 'cidr: "10.0.0.1/32"' && echo "$out" | grep -qF 'cidr: "fd00::1/128"' && pass "API server cidrs trimmed and quoted" || fail "API server cidrs missing"
echo "$out" | grep -q "port: 6443" && echo "$out" | grep -q "port: 443" && pass "API server ports rendered" || fail "API server ports missing"
echo "$out" | grep -q "0.0.0.0/0" && fail "egress must not allow all addresses" || pass "no allow-all egress rule"

# ── Test 24: egress enabled without addresses fails under helm template ──────
section "NetworkPolicy egress without cidrs fails"
expect_render_fail "networkPolicy.egress.apiServer.cidrs" -s templates/networkpolicy.yaml --set networkPolicy.egress.enabled=true

# ── Test 25: egress needs at least one valid port, with explicit cidrs too ───
section "NetworkPolicy egress ports are required"
for ports in "networkPolicy.egress.apiServer.ports=null" "networkPolicy.egress.apiServer.ports={}" \
  "networkPolicy.egress.apiServer.ports={443.7}" "networkPolicy.egress.apiServer.ports=443"; do
  expect_render_fail "networkPolicy.egress.apiServer.ports" -s templates/networkpolicy.yaml \
    --set networkPolicy.egress.enabled=true --set 'networkPolicy.egress.apiServer.cidrs={10.0.0.1/32}' --set "$ports"
done

# ── Test 26: explicit egress cidrs need a non-zero prefix length ─────────────
section "NetworkPolicy egress cidrs are validated"
for cidr in "10.0.0.1" "0.0.0.0/0" "::/0" "10.0.0.1/00" "::/00" "10.0.0.1/33" "fd00::1/129" "256.0.0.1/32"; do
  expect_render_fail "networkPolicy.egress.apiServer.cidrs entries must be" -s templates/networkpolicy.yaml \
    --set networkPolicy.egress.enabled=true --set "networkPolicy.egress.apiServer.cidrs={$cidr}"
done

# ── Test 27: egress values must have the right type ──────────────────────────
section "NetworkPolicy egress value types are validated"
expect_render_fail "networkPolicy.egress.enabled must be true or false" -s templates/networkpolicy.yaml \
  --set networkPolicy.egress.enabled=true --set-string networkPolicy.egress.enabled=false
expect_render_fail "networkPolicy.egress.apiServer.cidrs must be a list" -s templates/networkpolicy.yaml \
  --set networkPolicy.egress.enabled=true --set networkPolicy.egress.apiServer.cidrs=10.0.0.1/32

# ── Test 28: xDS request and write bounds ────────────────────────────────────
section "xDS request and write bounds"
out=$(render)
grep -qxE -- " *- --xds-max-request-bytes=262144" <<<"$out" && pass "default request size limit passed" || fail "default --xds-max-request-bytes missing"
grep -qxE -- " *- --xds-write-timeout=30s" <<<"$out" && pass "default write timeout passed" || fail "default --xds-write-timeout missing"
out=$(render --set xds.maxRequestBytes=1048576 --set xds.writeTimeout=45s)
grep -qxE -- " *- --xds-max-request-bytes=1048576" <<<"$out" && pass "request size limit override rendered as an integer" || fail "--xds-max-request-bytes override missing"
grep -qxE -- " *- --xds-write-timeout=45s" <<<"$out" && pass "write timeout override passed" || fail "--xds-write-timeout override missing"

# ── Test 29: xDS per-source connection cap ───────────────────────────────────
section "xDS per-source connection cap"
out=$(render)
grep -qxE -- " *- --xds-max-connections-per-source=0" <<<"$out" && pass "default per-source connection share passed" || fail "default --xds-max-connections-per-source missing"
out=$(render --set xds.maxConnectionsPerSource=4096)
grep -qxE -- " *- --xds-max-connections-per-source=4096" <<<"$out" && pass "per-source connection cap override passed" || fail "--xds-max-connections-per-source override missing"

# ── tls.bootstrap hands the certificate to the operator ─────────────────────
# OLM bundles and GitOps renderers cannot run the generate branch; the operator
# creates the Secret itself, so the chart must stay out of the way and the
# mount must be optional or the pod never starts.
section "tls.bootstrap=true"
out=$(render --set tls.bootstrap=true)
if echo "$out" | grep -q "^kind: Secret"; then
  fail "chart should not create a serving Secret when tls.bootstrap=true"
else
  pass "no serving Secret with tls.bootstrap=true"
fi
echo "$out" | grep -q -- "--xds-cert-secret=${RELEASE}-coraza-kubernetes-operator-serving-cert" \
  && pass "manager told which Secret to provision" || fail "--xds-cert-secret missing"
echo "$out" | grep -q "optional: true" && pass "serving cert mount is optional" || fail "serving cert mount is not optional"
echo "$out" | grep -q "resourceNames:" && pass "bootstrap Role restricts get/update by name" || fail "bootstrap Role is not name-restricted"

# ── tls.bootstrap honours existingSecret ────────────────────────────────────
section "tls.bootstrap=true with existingSecret"
out=$(render --set tls.bootstrap=true --set tls.existingSecret=my-serving-cert)
echo "$out" | grep -q -- "--xds-cert-secret=my-serving-cert" \
  && pass "manager provisions the named Secret" || fail "--xds-cert-secret does not use existingSecret"
echo "$out" | grep -q "secretName: my-serving-cert" \
  && pass "deployment mounts existingSecret" || fail "deployment does not mount existingSecret"

# ── bootstrap is off by default ─────────────────────────────────────────────
section "tls.bootstrap defaults to off"
out=$(render)
if echo "$out" | grep -q -- "--xds-cert-secret"; then
  fail "--xds-cert-secret should not be set by default"
else
  pass "--xds-cert-secret absent by default"
fi
if echo "$out" | grep -q "optional: true"; then
  fail "serving cert mount should be required by default"
else
  pass "serving cert mount required by default"
fi
if echo "$out" | grep -q "resourceNames:"; then
  fail "bootstrap Role should not render by default"
else
  pass "bootstrap Role absent by default"
fi

# ── a custom caName is rejected under bootstrap ─────────────────────────────
# The operator always writes the CA under ca.crt, so --xds-ca-path would point
# at a file that never appears.
section "tls.bootstrap rejects a custom caName"
if render_err=$(render --set tls.bootstrap=true --set tls.caName=root.crt 2>&1); then
  fail "a custom caName should have failed the render under bootstrap"
else
  echo "$render_err" | grep -q "tls.caName must be ca.crt" \
    && pass "custom caName rejected with a descriptive error" \
    || fail "render failed but the error did not mention caName: $render_err"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
