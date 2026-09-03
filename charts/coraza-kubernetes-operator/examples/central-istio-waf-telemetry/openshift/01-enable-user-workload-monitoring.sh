#!/usr/bin/env bash
# Enable OpenShift User Workload Monitoring without replacing existing config.
# This is an explicit cluster-admin action; apply-openshift.sh never invokes it.
set -euo pipefail

namespace="${MONITORING_NAMESPACE:-openshift-monitoring}"
configmap="${MONITORING_CONFIGMAP:-cluster-monitoring-config}"

command -v jq >/dev/null || {
  echo "ERROR: jq is required to build a safe ConfigMap merge patch" >&2
  exit 1
}

current="$(oc get configmap "${configmap}" -n "${namespace}" \
  -o jsonpath='{.data.config\.yaml}')"

if grep -Eq '^enableUserWorkload:[[:space:]]*true([[:space:]]*(#.*)?)?$' <<<"${current}"; then
  echo "OpenShift User Workload Monitoring is already enabled."
  exit 0
fi

if grep -Eq '^enableUserWorkload:' <<<"${current}"; then
  updated="$(sed -E 's/^enableUserWorkload:[[:space:]]*(true|false)([[:space:]]*(#.*)?)?$/enableUserWorkload: true\2/' <<<"${current}")"
else
  updated="${current}"$'\n\nenableUserWorkload: true'
fi

patch="$(jq -n --arg config "${updated}" '{data: {"config.yaml": $config}}')"
oc patch configmap "${configmap}" -n "${namespace}" --type=merge --patch "${patch}"

echo "Enabled OpenShift User Workload Monitoring."
