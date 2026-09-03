#!/usr/bin/env bash
# Preflight checks for KIND WAF ALS telemetry (Sail Istio + upstream OTEL).
set -euo pipefail

: "${ISTIO_NAME:=coraza}"
: "${COLLECTOR_NS:=coraza-central-waf-telemetry}"
: "${COLLECTOR_NAME:=central-waf-als}"
: "${TELEMETRY_NS:=integration-tests}"
: "${TELEMETRY_NAME:=coraza-engine-waf-engine-telemetry}"
: "${ENGINE_NS:=integration-tests}"
: "${ENGINE_NAME:=waf-engine}"
: "${GATEWAY_NAME:=coraza-gateway}"
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

echo "=== KIND WAF ALS telemetry preflight ==="

check "kubectl context" kubectl cluster-info >/dev/null

check "OTEL CRD" kubectl api-resources --api-group=opentelemetry.io 2>/dev/null | grep -q opentelemetrycollectors

check "Telemetry CRD" kubectl api-resources --api-group=telemetry.istio.io 2>/dev/null | grep -q telemetries

check "WasmPlugin CRD" kubectl api-resources --api-group=extensions.istio.io 2>/dev/null | grep -q wasmplugins

check "Istio CR ${ISTIO_NAME}" kubectl get istio "${ISTIO_NAME}" >/dev/null

if kubectl get istio "${ISTIO_NAME}" -o json 2>/dev/null | jq -e '.spec.values.meshConfig.extensionProviders[]? | select(.name=="waf-log-collector")' >/dev/null; then
  echo "OK   mesh extensionProvider waf-log-collector"
else
  echo "FAIL mesh extensionProvider waf-log-collector missing (run apply-meshconfig.sh or kind/apply-kind.sh)"
  fail=1
fi

if kubectl rollout status -n "${COLLECTOR_NS}" "deploy/${COLLECTOR_NAME}-collector" --timeout=5s >/dev/null 2>&1; then
  echo "OK   Deployment/${COLLECTOR_NAME}-collector available"
else
  echo "FAIL Deployment/${COLLECTOR_NAME}-collector not available in ${COLLECTOR_NS}"
  fail=1
fi

check "Gateway ${TELEMETRY_NS}/${GATEWAY_NAME}" \
  kubectl get gateway -n "${TELEMETRY_NS}" "${GATEWAY_NAME}" >/dev/null

expected_collector="${COLLECTOR_NAME}-collector.${COLLECTOR_NS}.svc.cluster.local:4317"
if kubectl get gatewayclass istio -o jsonpath='{.metadata.annotations.internal\.do-not-use\.openshift\.io/waf-otel-collector}' 2>/dev/null | grep -qx "${expected_collector}"; then
  echo "OK   GatewayClass/istio WAF collector endpoint declaration"
else
  echo "FAIL GatewayClass/istio WAF collector endpoint must be ${expected_collector}"
  fail=1
fi

if kubectl get telemetry -n "${TELEMETRY_NS}" "${TELEMETRY_NAME}" >/dev/null 2>&1; then
  if kubectl get telemetry -n "${TELEMETRY_NS}" "${TELEMETRY_NAME}" -o yaml | grep -q 'waf-log-collector'; then
    echo "OK   Telemetry/${TELEMETRY_NAME} uses waf-log-collector"
  else
    echo "FAIL Telemetry/${TELEMETRY_NAME} missing waf-log-collector provider"
    fail=1
  fi
else
  echo "WARN Telemetry/${TELEMETRY_NAME} not found in ${TELEMETRY_NS}"
fi

if kubectl get wasmplugin -n "${ENGINE_NS}" "${WASM_PLUGIN_NAME}" >/dev/null 2>&1; then
  if kubectl get wasmplugin -n "${ENGINE_NS}" "${WASM_PLUGIN_NAME}" -o jsonpath='{.spec.pluginConfig.enable_filter_state_logs}' 2>/dev/null | grep -q true; then
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
