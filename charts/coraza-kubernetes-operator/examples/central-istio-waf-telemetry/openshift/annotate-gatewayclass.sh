#!/usr/bin/env bash
# Point CIO at the central WAF ALS collector Service (OpenShift cluster-ingress-operator).
# Requires cluster-admin. Coraza never reconciles MeshConfig.
set -euo pipefail

GATEWAYCLASS="${GATEWAYCLASS:-openshift-default}"
COLLECTOR_NS="${COLLECTOR_NS:-coraza-central-waf-telemetry}"
COLLECTOR_NAME="${COLLECTOR_NAME:-central-waf-als}"
PORT="${PORT:-4317}"
ANNOTATION_KEY="internal.do-not-use.openshift.io/waf-otel-collector"
ENDPOINT="${COLLECTOR_NAME}-collector.${COLLECTOR_NS}.svc.cluster.local:${PORT}"

oc get gatewayclass "${GATEWAYCLASS}" >/dev/null

oc annotate gatewayclass "${GATEWAYCLASS}" \
  "${ANNOTATION_KEY}=${ENDPOINT}" \
  --overwrite

echo "Annotated GatewayClass/${GATEWAYCLASS} ${ANNOTATION_KEY}=${ENDPOINT}"
echo "Wait for ingress-operator reconcile, then verify waf-log-collector in mesh extensionProviders."
