#!/usr/bin/env bash
# Phase A: deploy OTC sidecar and annotate Gateway for injection.
# Prereqs: make cluster.kind (includes OTel Operator from ticket 01)
set -euo pipefail

NS="integration-tests"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "=== Step 1: Apply OTC sidecar CR ==="
kubectl apply -f "$SCRIPT_DIR/otc-sidecar.yaml"

echo "=== Step 2: Patch Gateway for sidecar injection ==="
kubectl patch gateway coraza-gateway -n "$NS" --type merge \
  --patch-file "$SCRIPT_DIR/gateway-patch.yaml"

echo "=== Step 3: Wait for Gateway pod rollout ==="
# Istio recreates the Deployment when infrastructure annotations change.
# Wait for the new pod with the sidecar container.
sleep 5
kubectl rollout status deployment -n "$NS" -l gateway.networking.k8s.io/gateway-name=coraza-gateway --timeout=120s

echo "=== Step 4: Verify sidecar injection ==="
POD=$(kubectl get pod -n "$NS" -l gateway.networking.k8s.io/gateway-name=coraza-gateway -o jsonpath='{.items[0].metadata.name}')
CONTAINERS=$(kubectl get pod "$POD" -n "$NS" -o jsonpath='{.spec.containers[*].name}')
echo "Pod: $POD"
echo "Containers: $CONTAINERS"

if echo "$CONTAINERS" | grep -q "otc-container"; then
  echo "OK: OTC sidecar injected"
else
  echo "FAIL: OTC sidecar not found in pod containers"
  exit 1
fi

echo ""
echo "=== Step 5: Verify metrics export ==="
echo "Run in another terminal:"
echo "  kubectl port-forward $POD 9090:9090 -n $NS"
echo "Then:"
echo "  curl -s localhost:9090/metrics | grep -E '(coraza_waf_|waf_filter_)'"
echo ""
echo "Phase A setup complete."
