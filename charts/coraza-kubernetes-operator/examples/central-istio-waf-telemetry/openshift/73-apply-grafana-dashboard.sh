#!/usr/bin/env bash
# Deploy the Grafana resources using the chart-maintained Coraza WAF dashboard.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../../.." && pwd)"
NAMESPACE="${GRAFANA_NAMESPACE:-coraza-central-waf-telemetry}"
TOKEN_SECRET="${GRAFANA_THANOS_TOKEN_SECRET:-central-waf-grafana-thanos-token}"
DASHBOARD="${REPO_ROOT}/charts/coraza-kubernetes-operator/dashboards/coraza-waf.json"

if ! oc get secret -n "${NAMESPACE}" "${TOKEN_SECRET}" >/dev/null 2>&1; then
  echo "ERROR: missing Secret ${NAMESPACE}/${TOKEN_SECRET}." >&2
  echo "Create it with the command documented in ${SCRIPT_DIR}/README.md." >&2
  exit 1
fi

oc create configmap -n "${NAMESPACE}" central-waf-grafana-dashboard \
  --from-file=coraza-waf.json="${DASHBOARD}" \
  --dry-run=client -o yaml | oc apply -f -
oc apply -f "${SCRIPT_DIR}/70-grafana-openshift.yaml"
oc apply -f "${SCRIPT_DIR}/71-grafana-datasource.yaml"
oc apply -f "${SCRIPT_DIR}/72-grafana-dashboard.yaml"

echo "Grafana resources applied in ${NAMESPACE}."
echo "Inspect: oc get grafana,grafanadatasource,grafanadashboard -n ${NAMESPACE}"
