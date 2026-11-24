---
title: "ECDS Plugin Contract"
linkTitle: "ECDS Plugin Contract"
weight: 40
description: "How the Coraza Kubernetes Operator delivers rules and data files to the coraza-rs envoy-coraza plugin."
---

This page is the shared contract between the Coraza Kubernetes Operator (CKO) and the
`envoy-coraza` Proxy-Wasm filter built from
[`coraza-rs`](https://github.com/networking-incubator/coraza-rs). It is versioned
independently of either repository. `schema_version` is an operator-side marker;
`envoy-coraza` ignores it and does not use it to gate compatibility.

## Runtime configuration JSON

The ECDS resource carries the runtime configuration in
`PluginConfig.configuration` (`google.protobuf.StringValue`) as a
`config_gzip_b64` envelope: the JSON document below gzip-compressed into one
member and encoded as standard padded base64. The envelope is the only
top-level key; the plugin rejects it combined with any other key or nested
inside itself. The gzip member is rendered with a zero header (no timestamp,
name, comment or extra field) at a fixed compression level, so every operator
replica produces identical bytes for the same configuration. The plugin's
8 MiB configuration cap bounds both the encoded `PluginConfig.configuration`
and the decompressed document; the operator checks the decompressed document
before wrapping and the encoded envelope at publication, and the gRPC message
limit (16 MiB by default) bounds the ECDS response carrying the envelope.
Decoded, the document is:

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
| `schema_version` | integer | CKO emits `2` as its contract marker. `envoy-coraza` ignores this key and does not reject other values. |
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
  previous configuration, or its bootstrap policy. The operator republishes NACKed
  configurations with exponential backoff.
- Failure policy mapping: `fail` -> native `FAIL_CLOSED`; `allow` -> native
  `FAIL_OPEN`. The operator also records the rendered bootstrap policy in the
  EnvoyFilter so a proxy is never left without a policy.
- The operator checks the supplied RuleData file count and aggregate bytes (defaults:
  1024 files and 6 MiB), and rejects a rendered configuration over the plugin's 8 MiB
  configuration cap. The plugin separately caps its compiled `rule_data` at 1024 files,
  1 MiB per file, and 8 MiB total.
- In a CRS-enabled build, `envoy-coraza` adds the embedded CRS files to `rule_data` before
  compiling and counts them against those same file and byte limits. This reduces the
  capacity left for user-supplied RuleData; the operator's pre-delivery checks do not
  subtract the embedded files, so a payload can pass operator checks and still be
  rejected by the plugin. Verify proxy acceptance: Engine `Ready=True` reflects operator
  acceptance, not proxy acknowledgement. Check the Gateway
  Envoy logs for NACK/configuration errors and the operator log for
  `ECDS configuration accepted by proxy`.
- `SecRequestBodyAccess On` needs an explicit `SecRequestBodyLimit` at or below
  16 MiB: the engine default (128 MiB) exceeds the WASM ceiling and the plugin
  refuses the configuration. CKO rejects a visible `SecRequestBodyLimit` above the
  cap, but cannot see directives inside `crs/` includes, so set the limit in your
  own rule text. If `SecRequestBodyInMemoryLimit` is set, it must be `0` or at
  least the request limit, or the body would spill to a file.
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
  `SecDebugLogLevel`, `SecAuditLogFormat`, and `SecAuditLogParts` are accepted.
  Only `SecAuditLogType Serial` is supported; `Concurrent`, `Syslog`, and `Https`
  are rejected. `SecAuditLog` is rejected: this WASM build has no audit sink, and a
  policy that names one fails to configure while the Engine reports `Ready`.
- Operators `envoy-coraza` is built without are rejected: `@inspectFile`, `@rbl`, and
  `@validateSchema`, negated or not, in rule text or in an included RuleData file. The
  RuleSet validation rejects them with `InvalidRuleSet`. A RuleSource that uses one
  usually reports `InvalidRules` first, unless its per-source check stops earlier at a
  RuleData reference it cannot resolve on its own.
- Rule text that Coraza, which validates it, and `envoy-coraza`, which runs it, would
  read differently is rejected, in rule text and in any RuleData file the rules
  `Include`:
  - a `SecRule` operator that starts with whitespace before `@` or `!` (`" @rbl x"`):
    `envoy-coraza` trims the operator, so it would run the named operator or negation
    where Coraza reads an `@rx` pattern;
  - `SecIgnoreRuleCompilationErrors On`: Coraza would skip a rule it cannot compile
    instead of reporting it, and `envoy-coraza` refuses a configuration in which a
    directive was skipped;
  - directive arguments in double quotes that are empty or start or end with another
    double quote (`SecAction ""id:1,phase:1,pass""`): Coraza removes every leading and
    trailing quote, while `envoy-coraza` removes one pair;
  - a line of 65536 bytes or more: Coraza stops reading rule text there, so split long
    rules with a trailing backslash;
  - a directive name with a non-ASCII letter: directive names are ASCII, and the two
    engines lowercase other letters differently;
  - a backtick block other than a `SecDataset` item list closed by a line holding just
    a backtick: both engines pass the lines of a block, the rest of its closing line,
    and any lines that closing line continues to the directive that opened it;
  - an action list in `SecRule`, `SecAction`, `SecDefaultAction` or
    `SecRuleUpdateActionById` with unbalanced double quotes, or a
    `SecRuleUpdateActionById` action list with a double quote, escaped or not,
    inside its enclosing pair: for `SecRuleUpdateActionById`, `envoy-coraza` reads only
    the part after the last one;
  - `SecRuleUpdateActionById`, `SecRuleUpdateTargetById` and `SecRuleUpdateTargetByTag`
    arguments that the two engines split differently: write the last argument as one
    field, in one pair of double quotes or in none, with no other double quote (an
    unquoted last argument must not end with a quote, escaped or not);
  - a `SecRuleUpdateActionById` that lists another ID or range after a single rule ID:
    Coraza stops at the first single ID, while `envoy-coraza` updates every ID and
    range listed.

  The RuleSet reports these as `InvalidRuleSet`. A non-ASCII directive name and a
  backtick block other than a closed `SecDataset` item list are also rejected on the
  RuleSource (`InvalidRules`), before Coraza compiles its rules. The directive and
  `Include` guard, the body-limit checks and these checks run on every reconcile,
  also for content the RuleSet already accepted; the Coraza compile runs when the
  merged content, the generation or readiness changes. Accepted content that fails
  these checks reports `InvalidRuleSet` and keeps its accepted record, so the Engine
  keeps its accepted configuration.
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
configuration. A proxy whose cache is empty rejects that first push, starts the fetch, and
never asks for the resource again on its own. Envoy also accepts, without building it, a
configuration whose bytes are those it last received, so the operator never sends a proxy
the bytes it rejected: it gives them a new tag (see [Versions and hashes](#versions-and-hashes)),
ends the stream with a retryable `Unavailable`, and sends the configuration again under the
new tag when the proxy resubscribes, after the fetch has finished. Each resolvable NACK is
retried without an attempt or time limit. The delay is the proxy's own: it starts at 2
seconds, doubles with each rejection in a row of the same content, is capped at 30 seconds,
and ends at the proxy's next acknowledgement. New content ends the delay at once and starts
it again from 2 seconds. A proxy that keeps rejecting retains its previous working
configuration, or its bootstrap policy if it never had one. With a fail-open bootstrap it
may allow traffic without WAF; with fail-closed it denies. Persistent NACKs can be diagnosed
from the operator retry logs and Gateway Envoy logs.

## Revocation

When a token or its authorization is revoked, the operator ends the stream with
`PermissionDenied`, or `Unauthenticated` for a token that no longer validates. A stream
revoked while a response is being written ends with an HTTP/2 `RST_STREAM` instead, so the
rest of that response is not sent; Envoy retries the stream and admission then refuses it.

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
    allow_on_headers_stop_iteration: true
    vm_config:
      vm_id: coraza-11111111-1111-4111-8111-111111111111
      runtime: envoy.wasm.runtime.v8
      nack_on_code_cache_miss: true
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
      value: "{\"config_gzip_b64\": \"<padded base64 of one gzip member holding the runtime configuration JSON>\"}"
```

The wire profile is streaming state-of-the-world `ExtensionConfigDiscoveryService`
scoped to one resource per stream. Delta and ADS are out of scope.

## Versions and hashes

- Resource identity includes the Engine UID, so a recreated Engine never reuses a
  deleted Engine's cached configuration.
- The content version of an Engine is the engine-version hash, not the hash of the
  rendered resource: `sha256:<64 lowercase hexadecimal characters>`. What a proxy is sent
  carries a tag after it. The ECDS `version_info` is `sha256:<hash>.<epoch>-<generation>`,
  and the WASM `vm_id` is the rendered `vm_id` with `.<epoch>-<generation>` appended. The
  epoch is 16 lowercase hexadecimal characters chosen at random when an operator process
  starts, and the generation is a decimal counter per Engine that starts at 0, so no two
  processes send the same tag. The tag is never part of a content hash, the accepted
  configuration, a status field or a readiness check, and the plugin configuration bytes never
  carry it; `envoy-coraza` reads only those bytes, so it never sees the tag.
- Envoy builds a filter only from bytes that differ from the bytes it last received, and
  accepts identical bytes without building them. A rejected configuration is therefore
  retried under another tag, never resent. The operator records, for each proxy, the tag it
  sent last, whether the proxy acknowledged it, and how many times in a row the proxy has
  rejected a configuration. It moves the Engine to the next generation when a proxy rejects
  the current tag, or would be sent a tag it did not acknowledge, whether or not the
  rejection reached it. The records are bounded (four per admitted stream) and only a response
  writes one, so a stream that sends rejections alone fills none. Losing a record restarts
  that proxy's back-off, and when the record held the Engine's current tag unacknowledged,
  the Engine moves to its next generation.
- A proxy that connects again reports the version it last accepted. When that is a tag of
  the Engine's current content, from any process, the operator answers the first request of
  the stream with those bytes again. That is one full response, which counts against the
  response budget, and Envoy skips building bytes it already built, so a Gateway that moves
  to another replica, or finds the operator restarted, keeps its filter, however many tags
  the Engine's Gateways hold. Any other version is sent the current tag
  of the process it connects to: a version not written in the form above (including the
  bare hashes and `.retry<N>` versions of earlier releases), a version of other content,
  and a generation this process has not reached.
- A tag another process minted carries only the content the proxy reported. When the
  operator publishes new content, the proxies that hold such a tag are sent it under the
  operator's own current tag, because the process that minted the tag may send the same tag
  with the same bytes: a proxy that rejected them here would be sent them there again, and
  Envoy accepts bytes it already received without building them. A proxy that reports such a
  tag after the new content was published is replayed exactly the bytes it reports, not moved
  to the operator's own tag with the proxies that held the tag before: Gateways that move
  between replicas while the content is published build their filter once for each content,
  also when the content returns to an earlier version, as long as they were not sent other
  content in between. A Gateway that last received other bytes (for example, it rejected
  newer content before the Engine returned to an earlier version) builds the earlier
  content again, because Envoy builds any bytes that differ from the last ones it received.
- A tag is a few words. The operator holds the rendered configuration of an Engine once,
  and produces the bytes sent under a tag when it sends them, so the tags in use cost no
  more than the streams that hold them, and a tag is dropped with its last stream.
  Publishing new content decodes the configuration once, whatever the number of tags.
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
