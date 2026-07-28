#!/usr/bin/env bash
# Phase B: deploy OTC sidecar with log→metrics pipeline.
# Prereqs: Phase A complete (OTel Operator installed, Gateway running with OTC sidecar)
set -euo pipefail

NS="integration-tests"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "=== Step 1: Update OTC sidecar CR with Phase B config ==="
kubectl apply -f "$SCRIPT_DIR/otc-sidecar-phase-b.yaml"

echo "=== Step 2: Restart Gateway pod to pick up new OTC config ==="
# OTC sidecar config is baked at injection time — pod restart required.
kubectl delete pod -n "$NS" -l gateway.networking.k8s.io/gateway-name=coraza-gateway
sleep 5
kubectl rollout status deployment -n "$NS" -l gateway.networking.k8s.io/gateway-name=coraza-gateway --timeout=120s

echo "=== Step 3: Wait for pod readiness ==="
kubectl wait --for=condition=Ready pod -n "$NS" -l gateway.networking.k8s.io/gateway-name=coraza-gateway --timeout=120s

echo "=== Step 4: Verify sidecar injection ==="
POD=$(kubectl get pod -n "$NS" -l gateway.networking.k8s.io/gateway-name=coraza-gateway -o jsonpath='{.items[0].metadata.name}')
echo "Pod: $POD"

# Check init containers (native sidecar) and regular containers
INIT_CONTAINERS=$(kubectl get pod "$POD" -n "$NS" -o jsonpath='{.spec.initContainers[*].name}')
CONTAINERS=$(kubectl get pod "$POD" -n "$NS" -o jsonpath='{.spec.containers[*].name}')
echo "Init containers: $INIT_CONTAINERS"
echo "Containers: $CONTAINERS"

if echo "$INIT_CONTAINERS $CONTAINERS" | grep -q "otc-container"; then
  echo "OK: OTC sidecar present"
else
  echo "FAIL: OTC sidecar not found"
  exit 1
fi

echo ""
echo "=== Step 5: Generate WAF traffic ==="
echo "Sending normal + SQLi requests..."
sleep 10  # wait for WAF plugin to load rules

# Port-forward and send requests
kubectl port-forward -n "$NS" svc/coraza-gateway-istio 8080:80 &
PF_PID=$!
sleep 3

for i in $(seq 1 3); do
  curl -s -o /dev/null -w "Normal request $i: %{http_code}\n" http://localhost:8080/ -H "Host: echo.integration-tests.svc.cluster.local"
done
for i in $(seq 1 3); do
  curl -s -o /dev/null -w "SQLi request $i: %{http_code}\n" "http://localhost:8080/?id=1%27%20OR%20%271%27%3D%271" -H "Host: echo.integration-tests.svc.cluster.local"
done

kill $PF_PID 2>/dev/null
wait $PF_PID 2>/dev/null || true

echo ""
echo "=== Step 6: Check metrics ==="
echo "Waiting 15s for OTC scrape cycle + log processing..."
sleep 15

kubectl port-forward "$POD" 9090:9090 -n "$NS" &
PF_PID=$!
sleep 3

echo "--- WAF metrics from Envoy stats (Phase A) ---"
curl -s localhost:9090/metrics | grep -E "^waf_filter_" || echo "(none)"

echo ""
echo "--- WAF metrics from logs (Phase B) ---"
curl -s localhost:9090/metrics | grep -E "^coraza_waf_rule_hits" || echo "(none)"

echo ""
echo "--- All coraza/waf metrics ---"
curl -s localhost:9090/metrics | grep -iE "(coraza_waf|waf_filter)" || echo "(none)"

kill $PF_PID 2>/dev/null
wait $PF_PID 2>/dev/null || true

echo ""
echo "=== Step 7: Check OTC logs for errors ==="
kubectl logs "$POD" -n "$NS" -c otc-container --tail=20

echo ""
echo "Phase B setup complete."
