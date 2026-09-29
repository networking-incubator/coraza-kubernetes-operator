#!/usr/bin/env bash
#
# waf.sh - Toggle the WAF on/off for baseline-vs-WAF comparison.
#
#   waf.sh on <minimal|medium|/path/to/ruleset.yaml>   # apply ruleset + engine
#   waf.sh off                                          # remove engine (WAF off)
#   waf.sh status                                       # show WAF resource state
#
# Removing the Engine (off) is the B0 baseline: traffic flows through the same
# Gateway/Envoy path with no WASM filter attached.
#
# Env knobs:
#   FAILURE_POLICY=fail|allow        Engine failure policy (default: fail).
#   POLL_CACHE=true                  Enable cache polling (ruleSetCacheServer);
#                                    default is static-embedded rules.
#   POLL_INTERVAL_SECONDS=15         Poll cadence when POLL_CACHE=true.
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../lib.sh
source ./lib.sh
preflight

FAILURE_POLICY="${FAILURE_POLICY:-fail}"
case "${FAILURE_POLICY}" in
  fail|allow) ;;
  *) err "invalid FAILURE_POLICY='${FAILURE_POLICY}' (must be 'fail' or 'allow')"; exit 1;;
esac

# POLL_CACHE=true injects ruleSetCacheServer (cache-poll model); default is
# static-embedded rules. POLL_INTERVAL_SECONDS tunes the poll cadence.
POLL_CACHE="${POLL_CACHE:-false}"
POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS:-15}"
if [[ "${POLL_CACHE}" == "true" ]]; then
  RULESET_CACHE_SERVER_BLOCK=$'  ruleSetCacheServer:\n    pollIntervalSeconds: '"${POLL_INTERVAL_SECONDS}"
else
  RULESET_CACHE_SERVER_BLOCK=""
fi

resolve_ruleset() {
  local sel="$1"
  case "${sel}" in
    minimal) echo "${RULESETS_DIR}/minimal.yaml";;
    medium)  echo "${RULESETS_DIR}/medium.yaml";;
    *)       [[ -f "${sel}" ]] && echo "${sel}" || { err "unknown ruleset '${sel}'"; exit 1; };;
  esac
}

cmd="${1:-}"; shift || true
case "${cmd}" in
  on)
    ruleset_file="$(resolve_ruleset "${1:-minimal}")"
    log "Applying ruleset from $(basename "${ruleset_file}")"
    "${CLI}" apply -n "${NAMESPACE}" -f "${ruleset_file}"

    log "Waiting for RuleSet '${RULESET_NAME}' to be Ready"
    "${CLI}" wait -n "${NAMESPACE}" "ruleset/${RULESET_NAME}" --for=condition=Ready --timeout="${WAIT_TIMEOUT}"

    log "Applying Engine '${ENGINE_NAME}' (failurePolicy=${FAILURE_POLICY}, cache-poll=${POLL_CACHE})"
    ENGINE_NAME="${ENGINE_NAME}" RULESET_NAME="${RULESET_NAME}" GATEWAY_NAME="${GATEWAY_NAME}" \
      FAILURE_POLICY="${FAILURE_POLICY}" RULESET_CACHE_SERVER_BLOCK="${RULESET_CACHE_SERVER_BLOCK}" \
      envsubst '${ENGINE_NAME} ${RULESET_NAME} ${GATEWAY_NAME} ${FAILURE_POLICY} ${RULESET_CACHE_SERVER_BLOCK}' \
      < "${MANIFESTS_DIR}/engine.yaml" | "${CLI}" apply -n "${NAMESPACE}" -f -

    log "Waiting for Engine '${ENGINE_NAME}' to be Ready"
    "${CLI}" wait -n "${NAMESPACE}" "engine/${ENGINE_NAME}" --for=condition=Ready --timeout="${WAIT_TIMEOUT}"

    warn "Allow Istio to apply the WasmPlugin to the Gateway Envoy (plus one poll"
    warn "interval ~15s if the cache is enabled) before measuring; run.sh validity-checks."
    ok "WAF enabled."
    ;;
  off)
    log "Removing Engine '${ENGINE_NAME}' (WAF off / baseline)"
    "${CLI}" delete -n "${NAMESPACE}" engine "${ENGINE_NAME}" --ignore-not-found
    warn "Allow the WasmPlugin to be removed from Envoy before measuring baseline."
    ok "WAF disabled."
    ;;
  status)
    "${CLI}" get -n "${NAMESPACE}" engine,ruleset,rulesource 2>/dev/null || true
    "${CLI}" get -n "${NAMESPACE}" wasmplugin 2>/dev/null || true
    ;;
  *)
    echo "usage: waf.sh {on <ruleset>|off|status}" >&2; exit 1;;
esac
