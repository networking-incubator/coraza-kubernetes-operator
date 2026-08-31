Unable to open session log file "/home/rzago/.cache/starship/session_1122210391562010.log": Os { code: 30, kind: ReadOnlyFilesystem, message: "Read-only file system" }!
#!/usr/bin/env bash
# Patch Sail Istio CR meshConfig with an OTel ALS provider (examples only).
# Coraza never reconciles MeshConfig.
#
# ALS shape matches OpenShift CIO #1555 (FILTER_STATE).
#
# kubectl patch --type=merge replaces whole arrays (RFC 7386), so we can't
# just merge-patch a single-entry extensionProviders array: that would delete
# any other providers already configured on the CR. Instead, read the
# current list, replace only the entry named ${PROVIDER} (or add it), and
# patch with the full merged list.
set -euo pipefail

ISTIO_NAME="${ISTIO_NAME:-coraza}"
SERVICE="${SERVICE:-central-waf-als-collector.coraza-central-waf-telemetry.svc.cluster.local}"
PORT="${PORT:-4317}"
PROVIDER="${PROVIDER:-waf-log-collector}"

kubectl get istio "$ISTIO_NAME" >/dev/null

# CIO openshift/cluster-ingress-operator#1555 (Envoy filter state from coraza-proxy-wasm PR #20+).
labels_json='{
    "start_time": "%START_TIME%",
    "namespace": "%ENVIRONMENT(POD_NAMESPACE)%",
    "gateway": "%ENVIRONMENT(ISTIO_META_WORKLOAD_NAME)%",
    "pod_name": "%ENVIRONMENT(POD_NAME)%",
    "method": "%REQ(:METHOD)%",
    "path": "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%",
    "response_code": "%RESPONSE_CODE%",
    "response_flags": "%RESPONSE_FLAGS%",
    "route_name": "%ROUTE_NAME%",
    "authority": "%REQ(:AUTHORITY)%",
    "duration": "%DURATION%",
    "client_ip": "%REQ(X-FORWARDED-FOR)%",
    "request_id": "%REQ(X-REQUEST-ID)%",
    "sni_hostname": "%REQUESTED_SERVER_NAME%",
    "waf_event": "%FILTER_STATE(wasm.io.coraza.waf.event:PLAIN)%",
    "rule_id": "%FILTER_STATE(wasm.io.coraza.waf.rule_id:PLAIN)%",
    "action": "%FILTER_STATE(wasm.io.coraza.waf.action:PLAIN)%",
    "phase": "%FILTER_STATE(wasm.io.coraza.waf.phase:PLAIN)%",
    "waf_status": "%FILTER_STATE(wasm.io.coraza.waf.status:PLAIN)%",
    "severity": "%FILTER_STATE(wasm.io.coraza.waf.severity:PLAIN)%",
    "category": "%FILTER_STATE(wasm.io.coraza.waf.category:PLAIN)%"
}'

new_provider=$(jq -n \
  --arg name "$PROVIDER" \
  --arg service "$SERVICE" \
  --argjson port "$PORT" \
  --argjson labels "$labels_json" \
  '{
    name: $name,
    envoyOtelAls: {
      service: $service,
      port: $port,
      logFormat: {
        labels: $labels
      }
    }
  }')

existing_providers=$(kubectl get istio "$ISTIO_NAME" -o jsonpath='{.spec.values.meshConfig.extensionProviders}')
if [ -z "$existing_providers" ] || [ "$existing_providers" = "null" ]; then
  existing_providers="[]"
fi

merged_providers=$(jq -c --argjson new "$new_provider" \
  '[.[] | select(.name != $new.name)] + [$new]' \
  <<<"$existing_providers")

patch=$(jq -n --argjson providers "$merged_providers" \
  '{spec: {values: {meshConfig: {extensionProviders: $providers}}}}')

kubectl patch istio "$ISTIO_NAME" --type=merge -p "$patch"

echo "Patched Istio/${ISTIO_NAME} meshConfig.extensionProviders[${PROVIDER}] -> ${SERVICE}:${PORT}"
echo "Roll out istiod if needed, then enable observability on the Engine."
