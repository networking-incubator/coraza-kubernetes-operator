---
title: "ECDS Plugin Contract"
linkTitle: "ECDS Plugin Contract"
weight: 40
description: "How the Coraza Kubernetes Operator delivers rules and data files to the coraza-rs envoy-coraza plugin."
---

This page is the shared contract between the Coraza Kubernetes Operator (CKO) and the
`envoy-coraza` Proxy-Wasm filter built from
[`coraza-rs`](https://github.com/networking-incubator/coraza-rs). It is versioned
independently of either repository. `schema_version` gates compatibility.

## Runtime configuration JSON

The ECDS resource carries this JSON in `PluginConfig.configuration`
(`google.protobuf.StringValue`):

```json
{
  "schema_version": 2,
  "engine": "checkout",
  "namespace": "shop",
  "engine_uid": "11111111-1111-4111-8111-111111111111",
  "driver_type": "wasm",
  "empty_rules_policy": "Reject",
  "failure_policy": "fail",
  "enable_filter_state_logs": true,
  "directives_map": {
    "default": ["SecRuleEngine On\nSecRule ARGS \"@pmFromFile blocked.txt\" \"id:1001,phase:2,deny,status:403\""]
  },
  "default_directives": "default",
  "rule_data": {
    "blocked.txt": "attack\n"
  }
}
```

| Field | Type | Rules |
| --- | --- | --- |
| `schema_version` | integer | Must be `2`. It is the operator's contract marker; `envoy-coraza` ignores unknown keys. |
| `engine`, `namespace` | string | Engine identity; informational. |
| `engine_uid` | string | UID of the Engine; prevents recreate/snapshot confusion. |
| `driver_type` | string | `wasm`. |
| `empty_rules_policy` | string | `Reject` (default) or `Allow`. The operator rejects an empty `directives_map` policy unless this is `Allow`. |
| `failure_policy` | string | `fail` or `allow`; the operator renders the matching native WASM `failure_policy` (`FAIL_CLOSED` or `FAIL_OPEN`). |
| `enable_filter_state_logs` | boolean | Read by `envoy-coraza`; mirrors `Engine.spec.observability.mode == Enabled`. |
| `directives_map` | object(string -> array of strings) | One policy, `default`, holding the complete assembled SecLang text. The plugin compiles every configured policy. |
| `default_directives` | string | Always `default`; the policy used for every authority. |
| `rule_data` | object(string -> string) | Merged RuleData files, last-listed wins, carried as UTF-8 text (not base64). The plugin exposes them to `Include`, `@pmFromFile`, and the file-based operators. |

Rules for both sides:

- No token, token path, polling interval, HTTP rule-server address, TLS material, or
  per-pod identifier appears in the payload.
- `envoy-coraza` validates and compiles the configuration in `on_configure`. A
  configuration that fails to parse or compile is rejected; the proxy keeps its
  previous configuration, or its bootstrap policy, and the operator logs the retry
  budget.
- Failure policy mapping: `fail` -> native `FAIL_CLOSED`; `allow` -> native
  `FAIL_OPEN`. The operator also records the rendered bootstrap policy in the
  EnvoyFilter so a proxy is never left without a policy.
- Size limits are enforced before delivery: the plugin caps one configuration at
  8 MiB, with at most 1024 `rule_data` files, 1 MiB per file, and 8 MiB of rule data
  total. The operator defaults to 6 MiB of RuleData and 1024 files, rejects larger
  flags at startup, and rejects a rendered payload above the 8 MiB configuration cap.
  These caps are not part of the payload.
- `SecRequestBodyAccess On` requires an explicit `SecRequestBodyLimit` at or
  below 16 MiB: the engine default (128 MiB) exceeds the WASM ceiling. If
  `SecRequestBodyInMemoryLimit` is set, it must be `0` or at least the request
  limit, or the body would spill to a file.
- An explicitly allowed empty ruleset is an intentional no-op WAF, not a missing
  configuration.
- Release artifacts: raw `plugin.wasm`, `plugin.wasm.sha256` (standard
  `sha256sum` output, `<hex>  plugin.wasm`), `plugin.manifest.yaml`
  (`schema_version`, module name, file, sha256, coraza-rs commit, coraza crate
  version, Proxy-Wasm SDK revision, embedded CRS release). CKO bundles exactly
  these bytes and manifest.

## Rule text references

Rule text may reference only what the plugin can resolve. The operator validates these
references before an Engine accepts the configuration:

- `Include` targets: a RuleData file name, or the embedded CRS tree in a
  CRS-enabled build (`make wasm.crs`, or the published artifact `make demo` downloads). The release bundle does
  not embed CRS. In a CRS-enabled module, `Include crs/crs-setup.conf`,
  `Include crs/rules/*.conf`, and a literal `crs/rules/<file>.conf` are accepted.
  The previous Go plugin's `@` aliases (`@recommended-conf`, `@demo-conf`,
  `@ftw-conf`, `@crs-setup-conf`, `@owasp_crs/...`) are **not** resolved by
  `envoy-coraza`, and every other glob is rejected before delivery with a clear
  error. You can also use `kubectl coraza generate coreruleset` to turn a
  CoreRuleSet checkout into one RuleSource per file plus a RuleData object for the
  `.data` files; the generated RuleSet concatenates the files and needs no
  includes.
  The argument may be unquoted or wrapped in double quotes: Coraza strips every
  leading and trailing `"` from a wholly quoted argument of at least three bytes,
  and nothing else. A single-quoted or half-quoted target is rejected.
  A non-glob target is trimmed like Coraza trims the name it opens, so extra spaces
  or a tab before the name resolve to that file and its content is scanned.
- A bare RuleData file name in `Include` is resolved from the RuleSet's merged
  RuleData, so the operator checks it at two points. Per-RuleSource validation has no
  data files and defers the name: the RuleSource is `Validated`, not degraded. The
  RuleSet's aggregate compilation supplies the merged files and rejects the RuleSet
  when the name is absent (`Degraded`, `InvalidRuleSet`). An `Include` of a RuleData
  file scans the included file's content as rule text too, recursively.
- `@pmFromFile` / `@pmf` targets: a RuleData file name, or, in a CRS-enabled
  build, the embedded CRS data files under `crs/...`. A leading `./` is ignored, as Coraza path-cleans the
  argument.
- Directives that open or create host files during compilation are rejected:
  `SecDebugLog`, `SecUploadDir`, and `SecUploadKeepFiles` with any value other than
  `Off`. This covers rule text and any RuleData file the rules `Include` (an included
  file is parsed as rule text). `@pmFromFile` data is not SecLang and is not scanned.
  `SecDebugLogLevel`, `SecAuditLogType`, `SecAuditLogFormat`, and `SecAuditLogParts`
  are accepted. `SecAuditLog` is rejected: this WASM build has no audit sink, and a
  policy that names one fails to configure while the Engine reports `Ready`.
- The previous Go plugin's `@owasp_crs` aliases are gone. A CRS-enabled build
  exposes the tree under coraza-rs's native `crs/` namespace (pinned CRS release,
  recorded in the manifest); the release bundle does not. Rules that used
  `@owasp_crs/...` must be rewritten to `crs/...` and served a CRS-enabled module.

Known trades of this contract:

- `Include` targets are matched by form, not by existence: a bare RuleData name is
  accepted per RuleSource and checked by aggregate RuleSet validation against the
  merged files.
- The operator rejects unsupported `@` aliases and glob forms at render time. A
  RuleSet that used `@owasp_crs/...` with the previous plugin must be rewritten to
  `crs/...` (or regenerated).

## Cold start

A proxy caches the module by `sha256` and fetches it only when it first applies a
configuration. A proxy whose cache is empty rejects that first push and never asks for the
resource again on its own, so the operator ends the stream with a retryable `Unavailable`
(at most ten times per version within a 20-second window, per stream). Envoy
resubscribes after the fetch has finished, applies
the same configuration, and acknowledges it; the module is not sent again after that. A
proxy that keeps rejecting retains its previous working configuration, or its bootstrap
policy if it never had one. With a fail-open bootstrap it may allow traffic without WAF;
with fail-closed it denies. The operator logs `ECDS retry budget exhausted`.

## ECDS resource shape

One `TypedExtensionConfig` per Engine, named
`coraza.waf.<namespace>.<engine-name>.<engine-uid>`:

```yaml
"@type": type.googleapis.com/envoy.config.core.v3.TypedExtensionConfig
name: coraza.waf.shop.checkout.11111111-1111-4111-8111-111111111111
typed_config:
  "@type": type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm
  config:
    name: coraza.waf.shop.checkout.11111111-1111-4111-8111-111111111111
    root_id: coraza.waf.shop.checkout.11111111-1111-4111-8111-111111111111
    failure_policy: FAIL_CLOSED
    vm_config:
      vm_id: coraza-11111111-1111-4111-8111-111111111111
      runtime: envoy.wasm.runtime.v8
      code:
        remote:
          # The bundled module is served by the operator; a custom
          # spec.driver.wasm.url module replaces this URL.
          http_uri:
            uri: https://coraza-operator.coraza-system.svc.cluster.local:18080/wasm/<64hex>.wasm
            cluster: coraza_wasm
            timeout: 10s
          sha256: <64hex>
    configuration:
      "@type": type.googleapis.com/google.protobuf.StringValue
      value: "<runtime configuration JSON>"
```

The wire profile is streaming state-of-the-world `ExtensionConfigDiscoveryService`
scoped to one resource per stream. Delta and ADS are out of scope.

## Versions and hashes

- Resource identity includes the Engine UID, so a recreated Engine never reuses a
  deleted Engine's cached configuration.
- The ECDS `version_info` is the engine-version hash, not the hash of the rendered
  resource: `sha256:<64 lowercase hexadecimal characters>`. A NACK retry republishes
  the rejected snapshot as `sha256:<hash>.retry<N>` and appends the same suffix to the
  WASM `vm_id`; treat the suffix as part of the version string.
- `ruleSetHash = sha256(canonical(rule text + sorted filename list + raw bytes))`.
- The Engine version hashes schema and rendering versions, Engine identity, the RuleSet
  UID and content hash, failure and empty-rules policies, observability, binary URL and
  WASM digest, and VM settings.
- No timestamp, Kubernetes `resourceVersion`, nonce, replica identity, token, or CA
  material enters a content hash. Rule text is byte-sensitive: whitespace and comment
  changes change the version.
- Versions are opaque, not monotonic. Reverting to identical effective content may
  reproduce an older version.
- A replica publishes only when it reconstructs the exact bytes whose version matches
  the Engine's accepted configuration.
