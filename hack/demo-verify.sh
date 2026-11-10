#!/usr/bin/env bash
# Assert the demo Gateway passes normal traffic and blocks the demo attacks.
# Called by `make demo.verify`; KIND_CLUSTER_NAME and DEMO_NAMESPACE come from
# the environment.
set -euo pipefail

: "${KIND_CLUSTER_NAME:?KIND_CLUSTER_NAME is required}"
: "${DEMO_NAMESPACE:?DEMO_NAMESPACE is required}"

source "$(dirname $(readlink -f $0))/curl_wrapper.sh"

ctx="kind-$KIND_CLUSTER_NAME"
addr=""
deadline=$(( $(date +%s) + 300 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  addr="$(kubectl --context "$ctx" -n "$DEMO_NAMESPACE" get gateway coraza-gateway \
    -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
  [ -n "$addr" ] && break
  sleep 5
done
[ -n "$addr" ] || { echo "ERROR: Gateway coraza-gateway has no address" >&2; exit 1; }

echo "=== Sending demo traffic to http://$addr/ ==="
allowed=""; blocked=""; data=""
deadline=$(( $(date +%s) + 240 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  allowed="$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://$addr/" || true)"
  blocked="$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://$addr/?q=evilmonkey" || true)"
  data="$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://$addr/?q=evildata" || true)"
  [ "$allowed" = "200" ] && [ "$blocked" = "403" ] && [ "$data" = "403" ] && break
  sleep 5
done

echo "  allowed  GET /                      -> $allowed (want 200)"
echo "  blocked  GET /?q=evilmonkey         -> $blocked (want 403)"
echo "  blocked  GET /?q=evildata           -> $data (want 403, @pmFromFile RuleData)"
if [ "$allowed" != "200" ] || [ "$blocked" != "403" ] || [ "$data" != "403" ]; then
  echo "ERROR: the WAF is not enforcing (is the WASM module loaded?)" >&2
  exit 1
fi
echo "=== Demo verified: rules from the cluster block the request ==="

if [[ $CONTAINER_TOOL =~ podman ]]; then
  echo "(i) Info: if using podman, consider sourcing hack/curl_wrapper.sh"
  echo "    source hack/curl_wrapper.sh"
  echo "    curl -s -o /dev/null -w '%{http_code}' http://$addr/"
  echo "    curl_restore  # to revert the wrapper override"
fi
