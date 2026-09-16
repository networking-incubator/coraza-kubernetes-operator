#!/usr/bin/env bash
#
# teardown.sh - Remove the performance-test namespace and everything in it.
# Results under results/ are kept.
#
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
# shellcheck source=../lib.sh
source ./lib.sh
preflight

log "Deleting namespace '${NAMESPACE}'"
if [[ "${PLATFORM}" == "openshift" ]]; then
  "${CLI}" delete project "${NAMESPACE}" --ignore-not-found
else
  "${CLI}" delete namespace "${NAMESPACE}" --ignore-not-found
fi
ok "Teardown complete. Results retained in ${RESULTS_DIR}"
