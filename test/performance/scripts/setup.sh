#!/usr/bin/env bash
#
# setup.sh - Provision the performance-test target: namespace, backend, load
# generator, Gateway, and HTTPRoute. Does NOT enable the WAF (see waf.sh).
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../lib.sh
source ./lib.sh
preflight

log "Creating namespace '${NAMESPACE}'"
"${CLI}" create namespace "${NAMESPACE}" --dry-run=client -o yaml | "${CLI}" apply -f -

log "Deploying backend (echo) and load generator (fortio)"
"${CLI}" apply -n "${NAMESPACE}" -f "${MANIFESTS_DIR}/backend.yaml"
"${CLI}" apply -n "${NAMESPACE}" -f "${MANIFESTS_DIR}/loadgen.yaml"

log "Deploying Gateway '${GATEWAY_NAME}' (class '${GATEWAY_CLASS}') and HTTPRoute"
GATEWAY_NAME="${GATEWAY_NAME}" GATEWAY_CLASS="${GATEWAY_CLASS}" \
  envsubst < "${MANIFESTS_DIR}/gateway.yaml" | "${CLI}" apply -n "${NAMESPACE}" -f -

log "Waiting for workloads and Gateway to be ready"
"${CLI}" rollout status -n "${NAMESPACE}" deploy/echo --timeout="${WAIT_TIMEOUT}"
"${CLI}" rollout status -n "${NAMESPACE}" deploy/"${LOADGEN_NAME}" --timeout="${WAIT_TIMEOUT}"
"${CLI}" wait -n "${NAMESPACE}" "gateway/${GATEWAY_NAME}" --for=condition=Programmed --timeout="${WAIT_TIMEOUT}"

log "Smoke test: load generator -> gateway"
if loadgen_exec fortio curl -quiet "${TARGET_URL}/" >/dev/null 2>&1; then
  ok "Load generator can reach the gateway."
else
  warn "Smoke test failed; check '${CLI} get gateway,httproute,pods -n ${NAMESPACE}'"
fi

ok "Target provisioned. Enable the WAF with: scripts/waf.sh on <minimal|medium>"
