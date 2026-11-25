# Bundled WASM assets

This package embeds the `envoy-coraza` module built from
[`coraza-rs`](https://github.com/networking-incubator/coraza-rs). The operator
serves it to Gateways over its own asset route (`/wasm/<digest>.wasm`); nothing
is pulled from an OCI registry or extracted at runtime.

| File | Committed content |
|---|---|
| `plugin.wasm` | Dev fixture: the 8-byte valid empty WASM module (`\0asm\1\0\0\0`). |
| `plugin.manifest.yaml` | Manifest for the committed bytes (`schema_version: 1`, module name, file, `sha256`, build metadata). |

`Load` + `VerifyAll` are run on every replica at startup and back the
`xds-assets` ready check; `ResolveDefault` returns the verified module bytes.

## Dev fixture vs. release

The fixture exists only so that `go build`/`go test` always have embeddable
bytes. **The real bundled module is never committed.** The release workflow and
`make demo` replace both files with published `coraza-rs` assets and verify the
result. Local source builds remain available through `make wasm.plugin` and
`make wasm.crs`; the release image job does not compile Rust.

`make build.image` and the ordinary `make cluster.kind` path embed the committed
fixture unless `wasm.bundle` was run first. That image is suitable for
build/test scaffolding, **not for WAF enforcement**.

## Building the module

```bash
make wasm.release                # published artifact (what `make demo` uses)
# or build the pinned commit:
make wasm.plugin                 # builds the pinned coraza-rs commit
make wasm.bundle WASM_MODULE=.cache/wasm/envoy_coraza.wasm
make wasm.verify
```

- `wasm.plugin` clones/updates the pinned `CORAZA_RS_VERSION` commit, builds
  `envoy-coraza` with Cargo for `wasm32-wasip1`, and links the WASI stubs from
  `hack/wasi-stub.wat` with the pinned Binaryen release. The stubs are required
  on Envoy releases before 1.40 (Istio 1.31 ships Envoy 1.39) and are harmless
  on newer releases. The output stays in `.cache/`.
- **The release bundle does not embed CRS.** `make wasm.crs` (and the
  `coraza-rs-crs.wasm` artifact `make demo` downloads) fetches the CRS release
  pinned in coraza-rs's `crs.pin` and builds with `embed-crs`; `crs/...`
  includes only work with that module. The bundle
  manifest records `crs_version: none` for the release build and the pinned
  release (for example `v4.30.0`) for the CRS-enabled build.
- Pass `CORAZA_RS_DIR=/path/to/coraza-rs` to build a local checkout; the target
  reads it through `git archive` and does not modify it.
- `wasm.bundle` copies `WASM_MODULE` to `internal/assets/plugin.wasm` and
  rewrites `plugin.manifest.yaml` with the recomputed lowercase SHA-256. The
  manifest records the coraza-rs commit, the coraza crate version, the
  Proxy-Wasm SDK revision, and the embedded CRS release (`crs_version`).
- `wasm.verify` recomputes the digest of the bundled file and fails if it
  disagrees with the manifest. With `WASM_VERIFY_RELEASE=1`, it also checks the
  published digest, rejects the dev fixture, and verifies that the configured
  coraza-rs tag resolves to `CORAZA_RS_VERSION`. This checks pin alignment, not
  the published module's build provenance.

`make test` does not invoke these build targets. The CI release-artifact check
uses a separate assets directory; `make wasm.bundle` or `make wasm.release`
with the default `ASSETS_DIR` replaces the fixture, so do not commit those
generated files.

## Release artifacts

The release workflow downloads the digest-pinned non-CRS `coraza-rs.wasm`,
bundles it into the operator image, and publishes `plugin.wasm`,
`plugin.wasm.sha256` and `plugin.manifest.yaml` alongside the other release
assets. It checks that the upstream release tag resolves to the Makefile's
`CORAZA_RS_VERSION` pin but does not attest that the published bytes were built
from that commit.

## Limits

`envoy-coraza` caps one plugin configuration at 8 MiB, with at most 1024
`rule_data` files, 1 MiB per file, and 8 MiB of rule data total. The operator
rejects a rendered payload above the configuration cap and defaults to 6 MiB of
RuleData and 1024 files.

The manifest describes a single module; multi-module rollover (overlapping
versions during an upgrade) is post-MVP. Custom external module hosts must be
plain HTTPS without redirects, and the host must present a certificate chain
signed by the operator xDS CA (`--xds-ca-path`) over TLS 1.3; public-CA hosts
are not trusted yet. GitHub release asset URLs redirect and must not be used as
an Engine `url`.
