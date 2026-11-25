#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
: "${TMPDIR:?TMPDIR must point to a writable runner temp directory}"
work="$(mktemp -d "$TMPDIR/wasm-release-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT

cd "$repo_root"

# The live check uses an isolated assets directory and the actual release pin.
assets="$work/release-assets"
mkdir -p "$assets"
make wasm.release WASM_CRS=0 \
  ASSETS_DIR="$assets" WASM_RELEASE_DIR="$work/release-cache"
make wasm.verify WASM_CRS=0 WASM_VERIFY_RELEASE=1 ASSETS_DIR="$assets"

# Use existing commit objects and local tags for offline verifier failure cases.
source_pin="$(git rev-parse HEAD)"
tag_repo="$work/tag-repo.git"
git init --quiet --bare "$tag_repo"
git --git-dir="$tag_repo" fetch --quiet --no-tags "$repo_root" "$source_pin"
git --git-dir="$tag_repo" config user.name "WASM release test"
git --git-dir="$tag_repo" config user.email "wasm-release-test@example.invalid"

light_tag=v0.0.0-lightweight
annotated_tag=v0.0.0-annotated
git --git-dir="$tag_repo" tag "$light_tag" "$source_pin"
git --git-dir="$tag_repo" tag -a "$annotated_tag" -m "release verifier test" "$source_pin"

write_bundle() {
  local dir="$1"
  local git_sha="$2"
  local crs_version="$3"
  mkdir -p "$dir"
  printf '\0asm\1\0\0\0' > "$dir/plugin.wasm"
  local digest
  digest="$(sha256sum "$dir/plugin.wasm" | cut -d' ' -f1)"
  cat > "$dir/plugin.manifest.yaml" <<EOF
schema_version: 1
module: envoy-coraza
file: plugin.wasm
sha256: $digest
git_sha: $git_sha
coraza_version: test
proxy_wasm_sdk_version: test
crs_version: $crs_version
EOF
}

valid_assets="$work/valid-assets"
dev_assets="$work/dev-assets"
write_bundle "$valid_assets" "$source_pin" none
write_bundle "$dev_assets" dev none
fixture_sha="$(sha256sum "$valid_assets/plugin.wasm" | cut -d' ' -f1)"

verify_local_tag() {
  make wasm.verify WASM_CRS=0 WASM_VERIFY_RELEASE=1 \
    ASSETS_DIR="$valid_assets" \
    CORAZA_RS_REPO="$tag_repo" CORAZA_RS_VERSION="$source_pin" \
    WASM_RELEASE_VERSION="$1" WASM_RELEASE_SHA256="$fixture_sha"
}
verify_local_tag "$light_tag"
verify_local_tag "$annotated_tag"

expect_failure() {
  local description="$1"
  local expected="$2"
  shift 2

  local output
  if output="$("$@" 2>&1)"; then
    echo "wasm release test: $description unexpectedly succeeded" >&2
    exit 1
  fi
  case "$output" in
    *"$expected"*) echo "wasm release test: $description rejected as expected" ;;
    *)
      printf 'wasm release test: %s failed for the wrong reason:\n%s\n' "$description" "$output" >&2
      exit 1
      ;;
  esac
}

expect_failure "development fixture" \
  "release verification refuses the development fixture" \
  make wasm.verify WASM_CRS=0 WASM_VERIFY_RELEASE=1 \
  ASSETS_DIR="$dev_assets" CORAZA_RS_REPO="$tag_repo" \
  CORAZA_RS_VERSION="$source_pin" WASM_RELEASE_VERSION="$light_tag" \
  WASM_RELEASE_SHA256="$fixture_sha"

expect_failure "release digest mismatch" "release digest mismatch" \
  make wasm.verify WASM_CRS=0 WASM_VERIFY_RELEASE=1 \
  ASSETS_DIR="$valid_assets" CORAZA_RS_REPO="$tag_repo" \
  CORAZA_RS_VERSION="$source_pin" WASM_RELEASE_VERSION="$light_tag"

expect_failure "CRS module refusal" "release verification requires the non-CRS module" \
  make wasm.verify WASM_CRS=1 WASM_VERIFY_RELEASE=1 \
  ASSETS_DIR="$valid_assets" CORAZA_RS_REPO="$tag_repo" \
  CORAZA_RS_VERSION="$source_pin" WASM_RELEASE_VERSION="$light_tag" \
  WASM_RELEASE_SHA256="$fixture_sha"

wrong_pin=0000000000000000000000000000000000000000
expect_failure "tag/source pin mismatch" \
  "coraza-rs tag $light_tag resolves to" \
  make wasm.verify WASM_CRS=0 WASM_VERIFY_RELEASE=1 \
  ASSETS_DIR="$valid_assets" CORAZA_RS_REPO="$tag_repo" \
  CORAZA_RS_VERSION="$wrong_pin" WASM_RELEASE_VERSION="$light_tag" \
  WASM_RELEASE_SHA256="$fixture_sha"

missing_tag=v0.0.0-missing
expect_failure "missing upstream tag" \
  "coraza-rs release tag $missing_tag was not found" \
  make wasm.verify WASM_CRS=0 WASM_VERIFY_RELEASE=1 \
  ASSETS_DIR="$valid_assets" CORAZA_RS_REPO="$tag_repo" \
  CORAZA_RS_VERSION="$source_pin" WASM_RELEASE_VERSION="$missing_tag" \
  WASM_RELEASE_SHA256="$fixture_sha"
