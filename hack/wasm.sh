#!/usr/bin/env bash
# Build, download, bundle, and verify the envoy-coraza WASM module.
#
#   hack/wasm.sh plugin    build from the pinned coraza-rs commit
#   hack/wasm.sh release   download and bundle a published artifact
#   hack/wasm.sh bundle    bundle WASM_MODULE into the assets directory
#   hack/wasm.sh verify    check the bundled module against its manifest
#
# Makefile targets pass the inputs through the environment.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/.." && pwd)"

work=""
cleanup() {
  # The EXIT trap must not fail the script when there is nothing to remove.
  if [ -n "${work:-}" ]; then
    rm -rf "$work"
  fi
}
trap cleanup EXIT

die() { echo "wasm: $*" >&2; exit 1; }

# GNU coreutils first; macOS ships shasum instead.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

require() { [ -n "${!1:-}" ] || die "$1 is required"; }

build_plugin() {
  require CORAZA_RS_REPO
  require CORAZA_RS_VERSION
  require CORAZA_RS_DIR
  require WASM_MODULE_OUT
  require WASI_STUB
  require BINARYEN_VERSION
  require BINARYEN_DIR

  local wasm_crs="${WASM_CRS:-0}"
  local default_dir="$repo_root/.cache/coraza-rs"

  if [ -n "${WASM_MODULE:-}" ]; then
    echo "wasm.plugin: reusing $WASM_MODULE"
    return 0
  fi

  if [ "$CORAZA_RS_DIR" = "$default_dir" ]; then
    if [ ! -e "$default_dir/.git" ]; then
      echo "wasm.plugin: cloning $CORAZA_RS_REPO"
      git clone --quiet "$CORAZA_RS_REPO" "$default_dir"
    fi
    if ! git -C "$default_dir" rev-parse --verify --quiet "$CORAZA_RS_VERSION^{commit}" >/dev/null; then
      echo "wasm.plugin: fetching $CORAZA_RS_VERSION"
      git -C "$default_dir" fetch --quiet --all --tags
    fi
  fi
  if ! git -C "$CORAZA_RS_DIR" rev-parse --verify --quiet "$CORAZA_RS_VERSION^{commit}" >/dev/null; then
    die "pinned coraza-rs commit $CORAZA_RS_VERSION is not available in $CORAZA_RS_REPO; pass CORAZA_RS_DIR=<checkout>"
  fi

  work="$(mktemp -d)"
  # git archive exports the commit, so uncommitted edits in CORAZA_RS_DIR are not built.
  git -C "$CORAZA_RS_DIR" archive --format=tar "$CORAZA_RS_VERSION" | tar -x -C "$work"

  if [ "$wasm_crs" = "1" ]; then
    echo "wasm.plugin: fetching the pinned OWASP CRS"
    ( cd "$work" && ./envoy-coraza/crs-fetch.sh )
  fi

  echo "wasm.plugin: building $CORAZA_RS_VERSION with Rust"
  local cargo_args=(build --locked --release --target wasm32-wasip1 -p envoy-coraza)
  if [ "$wasm_crs" = "1" ]; then
    cargo_args+=(--features envoy-coraza/embed-crs)
  fi
  ( cd "$work" &&
    rustup target add wasm32-wasip1 >/dev/null &&
    cargo "${cargo_args[@]}" )

  local module="$work/target/wasm32-wasip1/release/envoy_coraza.wasm"
  [ -f "$module" ] || die "build did not produce $module"

  local bin
  bin="$(BINARYEN_VERSION="$BINARYEN_VERSION" BINARYEN_DIR="$BINARYEN_DIR" "$here/install-binaryen.sh")"
  "$bin/wasm-as" "$WASI_STUB" -o "$work/wasi-stub.wasm"
  mkdir -p "$(dirname "$WASM_MODULE_OUT")"
  "$bin/wasm-merge" "$work/wasi-stub.wasm" wasi_snapshot_preview1 "$module" envoy-coraza \
    -o "$WASM_MODULE_OUT" --rename-export-conflicts --all-features
  echo "wasm.plugin: $WASM_MODULE_OUT"
}

bundle() {
  require ASSETS_DIR
  require WASM_MODULE_NAME
  require WASM_GIT_SHA
  require WASM_CORAZA_VERSION
  require WASM_PROXY_WASM_SDK_VERSION

  # Resolve the embedded CRS release from the commit being bundled. The
  # checkout may sit on another branch, so its working tree would be wrong.
  if [ -z "${WASM_CRS_VERSION:-}" ]; then
    if [ "${WASM_CRS:-0}" = "1" ]; then
      require CORAZA_RS_DIR
      require CORAZA_RS_VERSION
      WASM_CRS_VERSION="$(git -C "$CORAZA_RS_DIR" show "$CORAZA_RS_VERSION:envoy-coraza/crs.pin" 2>/dev/null | awk 'NR==1{print $1}' || true)"
      [ -n "$WASM_CRS_VERSION" ] || die "cannot read crs.pin from $CORAZA_RS_VERSION in $CORAZA_RS_DIR"
    else
      WASM_CRS_VERSION=none
    fi
  fi

  [ -n "${WASM_MODULE:-}" ] || die "WASM_MODULE is required: path to a raw plugin.wasm"
  [ -f "$WASM_MODULE" ] || die "WASM_MODULE not found: $WASM_MODULE"

  local dest="$ASSETS_DIR/plugin.wasm"
  if [ ! "$WASM_MODULE" -ef "$dest" ]; then
    cp "$WASM_MODULE" "$dest"
  fi
  local sum
  sum="$(sha256 "$dest")"

  # Metadata is written as YAML plain scalars: git SHAs and versions are safe unquoted.
  printf 'schema_version: 1\nmodule: %s\nfile: plugin.wasm\nsha256: %s\ngit_sha: %s\ncoraza_version: %s\nproxy_wasm_sdk_version: %s\ncrs_version: %s\n' \
    "$WASM_MODULE_NAME" "$sum" "$WASM_GIT_SHA" "$WASM_CORAZA_VERSION" "$WASM_PROXY_WASM_SDK_VERSION" "$WASM_CRS_VERSION" \
    > "$ASSETS_DIR/plugin.manifest.yaml"

  echo "wasm.bundle: $sum $dest"
}

verify() {
  require ASSETS_DIR

  local manifest="$ASSETS_DIR/plugin.manifest.yaml"
  local want got
  want="$(awk '/^sha256:/{print $2; exit}' "$manifest")"
  [ -n "$want" ] || die "no sha256 in $manifest"
  got="$(sha256 "$ASSETS_DIR/plugin.wasm")"
  [ "$want" = "$got" ] || die "digest mismatch: manifest $want, $ASSETS_DIR/plugin.wasm $got"
  echo "wasm.verify: $got $ASSETS_DIR/plugin.wasm ok"
}

release() {
  require WASM_RELEASE_VERSION
  require WASM_RELEASE_BASE
  require WASM_RELEASE_DIR
  require WASM_RELEASE_ASSET
  require WASM_RELEASE_SHA256

  local out="$WASM_RELEASE_DIR/$WASM_RELEASE_ASSET"
  if [ ! -f "$out" ] || [ "$(sha256 "$out")" != "$WASM_RELEASE_SHA256" ]; then
    echo "wasm.release: downloading $WASM_RELEASE_VERSION/$WASM_RELEASE_ASSET"
    mkdir -p "$WASM_RELEASE_DIR"
    curl -fsSL --retry 3 -o "$out.tmp" "$WASM_RELEASE_BASE/$WASM_RELEASE_ASSET"
    mv "$out.tmp" "$out"
  fi

  local got
  got="$(sha256 "$out")"
  [ "$got" = "$WASM_RELEASE_SHA256" ] || die "digest mismatch for $WASM_RELEASE_ASSET: got $got, want $WASM_RELEASE_SHA256"
  WASM_MODULE="$out" bundle
}

case "${1:-}" in
  plugin) build_plugin ;;
  release) release ;;
  bundle) bundle ;;
  verify) verify ;;
  *)
    die "usage: $0 {plugin|release|bundle|verify}"
    ;;
esac
