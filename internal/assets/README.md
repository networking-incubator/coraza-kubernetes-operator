# Bundled WASM assets

This package embeds the coraza-proxy-wasm module that the operator serves to
Gateways over its own asset route (`/wasm/<digest>.wasm`). The image carries the
raw bytes; nothing is pulled from an OCI registry or extracted at runtime.

| File | Committed content |
|---|---|
| `plugin.wasm` | Dev fixture: the 8-byte valid empty WASM module (`\0asm\1\0\0\0`). |
| `plugin.manifest.yaml` | Manifest for the committed bytes (`schema_version: 1`, module name, file, `sha256`, build metadata). |

`Load` + `VerifyAll` are run on every replica at startup and back the
`xds-assets` ready check; `ResolveDefault` returns the verified module bytes.

## Dev fixture vs. release

The fixture exists only so that `go build`/`go test` always have embeddable
bytes. Release images must not ship it: the release workflow replaces both files
with the pinned sibling release module before the image build and then verifies
the result.

```
make wasm.bundle WASM_MODULE=/path/to/plugin.wasm \
  WASM_GIT_SHA=<sha> WASM_CORAZA_VERSION=<version> WASM_PROXY_WASM_SDK_VERSION=<version>
make wasm.verify
```

- `wasm.bundle` copies `WASM_MODULE` (required) to `internal/assets/plugin.wasm`
  and rewrites `plugin.manifest.yaml` with the recomputed lowercase SHA-256.
- `wasm.verify` recomputes the digest of the bundled file and fails if it
  disagrees with the manifest. It runs in the release workflow and can be run
  locally at any time; it passes on the committed fixture.

Neither target runs as part of `make test`, and the committed fixture is left
untouched unless `wasm.bundle` is invoked explicitly.

## Release artifacts

The release workflow downloads `plugin.wasm` from the pinned coraza-proxy-wasm
release (repository variables `CORAZA_PROXY_WASM_VERSION`,
`CORAZA_PROXY_WASM_SHA256`, `CORAZA_PROXY_WASM_GIT_SHA`), checks the digest,
bundles it, and publishes `plugin.wasm`, `plugin.wasm.sha256` and
`plugin.manifest.yaml` alongside the other release assets.

## Limits

The manifest describes a single module; multi-module rollover (overlapping
versions during an upgrade) is post-MVP. GitHub release asset URLs redirect and
must not be used as an Engine `url`, and custom external module hosts are
post-MVP too; both would be served from a non-redirecting HTTPS host instead.
