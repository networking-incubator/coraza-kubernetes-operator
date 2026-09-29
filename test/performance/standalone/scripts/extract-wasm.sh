#!/usr/bin/env bash
# Extract the coraza-proxy-wasm module from its OCI image into ./coraza.wasm.
#
# Vanilla Envoy (unlike istio-agent) cannot pull an oci:// reference, so we copy
# the .wasm blob out of the image and load it as a local file. We deliberately
# use the SAME image the operator deploys by default, so this standalone rig
# measures the exact production binary (the fork build accepts static
# `directives_map` config in addition to its cache-polling mode).
set -euo pipefail

# Keep in sync with internal/defaults/defaults.go (DefaultCorazaWasmOCIReference,
# minus the oci:// scheme prefix which is Istio-only).
WASM_IMAGE="${WASM_IMAGE:-ghcr.io/networking-incubator/coraza-proxy-wasm:64800ac102924d1205097ba6efc95fbd5a99f9d3}"
WASM_PATH_IN_IMAGE="${WASM_PATH_IN_IMAGE:-/plugin.wasm}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${here}/coraza.wasm"

echo ">> pulling ${WASM_IMAGE} (cross-arch is fine — we only copy a blob, never run it)"
docker pull -q "${WASM_IMAGE}" >/dev/null

echo ">> extracting ${WASM_PATH_IN_IMAGE} -> ${out}"
id="$(docker create "${WASM_IMAGE}" 2>/dev/null || docker create --entrypoint /bin/true "${WASM_IMAGE}")"
trap 'docker rm "${id}" >/dev/null 2>&1 || true' EXIT
docker cp "${id}:${WASM_PATH_IN_IMAGE}" "${out}"

echo ">> extracted $(du -h "${out}" | cut -f1) to ${out}"
