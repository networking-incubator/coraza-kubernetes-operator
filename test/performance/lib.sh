#!/usr/bin/env bash
#
# lib.sh - shared config + helpers for the Coraza WAF performance-testing harness.
# Sourced by the scripts under scripts/; not meant to be run directly.
#
set -euo pipefail

# --- paths ------------------------------------------------------------------
PT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${PT_ROOT}/manifests"
RULESETS_DIR="${MANIFESTS_DIR}/rulesets"
SCENARIOS_DIR="${PT_ROOT}/scenarios"
RESULTS_DIR="${RESULTS_DIR:-${PT_ROOT}/results}"

# --- platform / cluster -----------------------------------------------------
PLATFORM="${PLATFORM:-kubernetes}"
case "${PLATFORM}" in
  kubernetes|k8s) PLATFORM="kubernetes"; CLI="${CLI:-kubectl}"; GATEWAY_CLASS="${GATEWAY_CLASS:-istio}";;
  openshift|ocp)  PLATFORM="openshift";  CLI="${CLI:-oc}";      GATEWAY_CLASS="${GATEWAY_CLASS:-openshift-default}";;
  *) echo "ERROR: unknown PLATFORM='${PLATFORM}'" >&2; exit 1;;
esac

# --- test topology ----------------------------------------------------------
NAMESPACE="${NAMESPACE:-waf-perf}"
GATEWAY_NAME="${GATEWAY_NAME:-perf-gateway}"
GATEWAY_SVC="${GATEWAY_SVC:-${GATEWAY_NAME}-${GATEWAY_CLASS}}"
# In-cluster URL the load generator hits (keeps client<->gateway hop internal).
TARGET_URL="${TARGET_URL:-http://${GATEWAY_SVC}.${NAMESPACE}.svc.cluster.local:80}"

# Load generator (fortio: open-model, HDR histograms -> no coordinated omission).
LOADGEN_IMAGE="${LOADGEN_IMAGE:-fortio/fortio:latest}"
LOADGEN_NAME="${LOADGEN_NAME:-loadgen}"

# WAF resource names.
ENGINE_NAME="${ENGINE_NAME:-perf-engine}"
RULESET_NAME="${RULESET_NAME:-perf-ruleset}"

# Observability (optional metric collection via collect.sh).
PROM_NAMESPACE="${PROM_NAMESPACE:-monitoring}"
PROM_SVC="${PROM_SVC:-prometheus-k8s}"
PROM_PORT="${PROM_PORT:-9090}"

WAIT_TIMEOUT="${WAIT_TIMEOUT:-180s}"

# --- logging ----------------------------------------------------------------
if [[ -t 1 ]]; then
  _B="\033[1;34m"; _G="\033[1;32m"; _Y="\033[1;33m"; _R="\033[1;31m"; _X="\033[0m"
else _B=""; _G=""; _Y=""; _R=""; _X=""; fi
log()  { echo -e "${_B}==>${_X} $*"; }
ok()   { echo -e "${_G}✓${_X} $*"; }
warn() { echo -e "${_Y}!${_X} $*" >&2; }
err()  { echo -e "${_R}✗${_X} $*" >&2; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || { err "missing command: $1"; exit 1; }; }

preflight() {
  require_cmd "${CLI}"
  "${CLI}" cluster-info >/dev/null 2>&1 || { err "cannot reach cluster via '${CLI}'"; exit 1; }
}

# Exec a command in the load generator pod.
loadgen_exec() {
  "${CLI}" exec -n "${NAMESPACE}" "deploy/${LOADGEN_NAME}" -- "$@"
}
