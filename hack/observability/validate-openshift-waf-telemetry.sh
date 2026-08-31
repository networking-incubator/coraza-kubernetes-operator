Unable to open session log file "/home/rzago/.cache/starship/session_5286870014349264.log": Os { code: 30, kind: ReadOnlyFilesystem, message: "Read-only file system" }!
#!/usr/bin/env bash
# Preflight checks for OpenShift WAF ALS telemetry (OSSM + RH OTEL + CIO).
set -euo pipefail

: "${GATEWAYCLASS:=openshift-default}"
: "${COLLECTOR_NS:=coraza-central-waf-telemetry}"
: "${COLLECTOR_NAME:=central-waf-als}"
: "${TELEMETRY_NS:=waf-telemetry}"
: "${TELEMETRY_NAME:=coraza-waf-als}"
: "${ENGINE_NS:=waf-telemetry}"
: "${ENGINE_NAME:=waf-engine}"
: "${WASM_PLUGIN_NAME:=coraza-engine-${ENGINE_NAME}}"

fail=0

check() {
  local label="$1"
  shift
  if "$@"; then
    echo "OK   ${label}"
  else
    echo "FAIL ${label}"
    fail=1
  fi
}

echo "=== OpenShift WAF ALS telemetry preflight ==="

check "oc session" oc whoami

check "GatewayClass ${GATEWAYCLASS}" oc get gatewayclass "${GATEWAYCLASS}" >/dev/null

check "RH OTEL CRD" oc api-resources --api-group=opentelemetry.io 2>/dev/null | grep -q opentelemetrycollectors

check "Telemetry CRD" oc api-resources --api-group=telemetry.istio.io 2>/dev/null | grep -q telemetries

check "WasmPlugin CRD" oc api-resources --api-group=extensions.istio.io 2>/dev/null | grep -q wasmplugins

annotation=$(oc get gatewayclass "${GATEWAYCLASS}" \
  -o jsonpath='{.metadata.annotations.internal\.do-not-use\.openshift\.io/waf-otel-collector}' 2>/dev/null || true)
if [ -n "${annotation}" ]; then
  echo "OK   GatewayClass waf-otel-collector annotation=${annotation}"
else
  echo "FAIL GatewayClass waf-otel-collector annotation missing"
  fail=1
fi

if oc get istio -A -o json 2>/dev/null | jq -e '.items[].spec.values.meshConfig.extensionProviders[]? | select(.name=="waf-log-collector")' >/dev/null; then
  echo "OK   mesh extensionProvider waf-log-collector"
else
  echo "WARN mesh extensionProvider waf-log-collector not found (CIO may still be reconciling)"
fi

if oc rollout status -n "${COLLECTOR_NS}" "deploy/${COLLECTOR_NAME}-collector" --timeout=5s >/dev/null 2>&1; then
  echo "OK   Deployment/${COLLECTOR_NAME}-collector available"
else
  echo "FAIL Deployment/${COLLECTOR_NAME}-collector not available in ${COLLECTOR_NS}"
  fail=1
fi

if oc get telemetry -n "${TELEMETRY_NS}" "${TELEMETRY_NAME}" >/dev/null 2>&1; then
  if oc get telemetry -n "${TELEMETRY_NS}" "${TELEMETRY_NAME}" -o yaml | grep -q 'waf-log-collector'; then
    echo "OK   Telemetry/${TELEMETRY_NAME} uses waf-log-collector"
  else
    echo "FAIL Telemetry/${TELEMETRY_NAME} missing waf-log-collector provider"
    fail=1
  fi
else
  echo "WARN Telemetry/${TELEMETRY_NAME} not found in ${TELEMETRY_NS}"
fi

if oc get wasmplugin -n "${ENGINE_NS}" "${WASM_PLUGIN_NAME}" >/dev/null 2>&1; then
  if oc get wasmplugin -n "${ENGINE_NS}" "${WASM_PLUGIN_NAME}" -o jsonpath='{.spec.pluginConfig.enable_filter_state_logs}' 2>/dev/null | grep -q true; then
    echo "OK   WasmPlugin enable_filter_state_logs=true"
  else
    echo "FAIL WasmPlugin ${WASM_PLUGIN_NAME} missing enable_filter_state_logs=true"
    fail=1
  fi
else
  echo "WARN WasmPlugin/${WASM_PLUGIN_NAME} not found in ${ENGINE_NS}"
fi

echo "=== done (exit ${fail}) ==="
exit "${fail}"
