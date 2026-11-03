#!/usr/bin/env bash
# Verify CIO propagated waf-log-collector (check openshift-ingress values ConfigMap).
set -euo pipefail

GATEWAYCLASS="${GATEWAYCLASS:-openshift-default}"
ANNOTATION_KEY="internal.do-not-use.openshift.io/waf-otel-collector"

echo "=== GatewayClass annotation ==="
oc get gatewayclass "${GATEWAYCLASS}" \
  -o "jsonpath={.metadata.annotations.${ANNOTATION_KEY//./\\.}}{'\n'}" 2>/dev/null || true

echo "=== openshift-ingress/values-openshift-gateway ==="
oc get configmap -n openshift-ingress values-openshift-gateway \
  -o jsonpath='{.data.merged-values}' \
  | python3 -c "
import sys, yaml, json
raw = sys.stdin.read()
try:
    d = yaml.safe_load(raw) or {}
except Exception:
    d = json.loads(raw) if raw.strip().startswith('{') else {}
eps = d.get('meshConfig', {}).get('extensionProviders', [])
names = [p.get('name') for p in eps]
print('extensionProviders:', names if names else 'NONE')
for p in eps:
    if p.get('name') == 'waf-log-collector':
        als = p.get('envoyOtelAls', {})
        print('waf-log-collector service:', als.get('service'), 'port:', als.get('port'))
"
