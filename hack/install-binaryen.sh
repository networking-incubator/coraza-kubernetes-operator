#!/usr/bin/env bash
# Download the pinned Binaryen release and print its bin directory on stdout.
# Used for wasm-as/wasm-merge when building the envoy-coraza artifact for Envoy
# releases before 1.40 (see hack/wasi-stub.wat).
set -euo pipefail

VERSION="${BINARYEN_VERSION:-123}"
CACHE="${BINARYEN_DIR:-.cache/binaryen}"

log() { echo "install-binaryen: $*" >&2; }

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$os/$arch" in
  linux/x86_64) platform="x86_64-linux" ;;
  linux/aarch64 | linux/arm64) platform="aarch64-linux" ;;
  darwin/x86_64) platform="x86_64-macos" ;;
  darwin/arm64) platform="arm64-macos" ;;
  *)
    log "unsupported platform $os/$arch; install binaryen $VERSION and run wasm-as/wasm-merge from PATH"
    exit 1
    ;;
esac

dest="$CACHE/version_$VERSION"
if [ -x "$dest/bin/wasm-as" ] && [ -x "$dest/bin/wasm-merge" ]; then
  echo "$dest/bin"
  exit 0
fi

tarball="binaryen-version_${VERSION}-${platform}.tar.gz"
url="https://github.com/WebAssembly/binaryen/releases/download/version_${VERSION}/${tarball}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

log "downloading binaryen $VERSION ($platform)"
curl -fsSL "$url" -o "$tmp/$tarball"
curl -fsSL "$url.sha256" -o "$tmp/$tarball.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$tmp" && sha256sum --check --strict "$tarball.sha256" >/dev/null)
else
  (cd "$tmp" && shasum -a 256 -c "$tarball.sha256" >/dev/null)
fi

rm -rf "$dest"
mkdir -p "$dest"
tar -xzf "$tmp/$tarball" -C "$dest" --strip-components=1
log "installed into $dest"
echo "$dest/bin"
