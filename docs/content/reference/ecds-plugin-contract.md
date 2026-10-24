---
title: "ECDS Plugin Contract"
linkTitle: "ECDS Plugin Contract"
weight: 40
description: "Versioned contract between the Coraza Kubernetes Operator and the coraza-proxy-wasm ECDS consumer."
---

This page is the shared contract between the Coraza Kubernetes Operator (CKO) and the
`coraza-proxy-wasm` plugin. It is versioned independently of either repository.
`schema_version` gates compatibility.

## Runtime configuration JSON

The ECDS resource carries this JSON in `PluginConfig.configuration`
(`google.protobuf.StringValue`):

```json
{
  "schema_version": 1,
  "engine": "checkout",
  "namespace": "shop",
  "engine_uid": "11111111-1111-4111-8111-111111111111",
  "driver_type": "wasm",
  "empty_rules_policy": "Reject",
  "failure_policy": "fail",
  "enable_filter_state_logs": true,
  "rules": "SecRuleEngine On\nSecRule ARGS \"@attack\" \"id:1001,phase:2,deny,status:403\"",
  "data_files": {
    "blocked.txt": "YXR0YWNrCg=="
  }
}
```

| Field | Type | Rules |
| --- | --- | --- |
| `schema_version` | integer | Must be `1`; any other value fails plugin configuration. |
| `engine`, `namespace` | string | Engine identity; informational and metrics labels. |
| `engine_uid` | string | UID of the Engine; prevents recreate/snapshot confusion. |
| `driver_type` | string | `wasm`. |
| `empty_rules_policy` | string | `Reject` (default) or `Allow`. Empty `rules` is rejected unless this is `Allow`. |
| `failure_policy` | string | `fail` or `allow`; maps to the plugin's internal policy and to the native WASM `failure_policy`. |
| `enable_filter_state_logs` | boolean (JSON only) | Mirrors `Engine.spec.observability.mode == Enabled`. |
| `rules` | string | Complete assembled SecLang text, byte-exact. |
| `data_files` | object(string -> base64) | Merged RuleData files, last-listed wins; base64 of raw bytes. |

Rules for both sides:

- No token, token path, polling interval, HTTP rule-server address, TLS material, or
  per-pod identifier appears in the payload.
- Unknown schema versions fail explicitly; the plugin returns
  `OnPluginStartStatusFailed` so Envoy retains the prior configuration.
- The plugin builds synchronously in `OnPluginStart`: validate -> decode base64 ->
  validate filenames -> construct virtual filesystem -> `coraza.NewWAF` -> publish the
  WAF. It performs no network call and starts no timers or tickers.
- Failure policy mapping: `fail` -> native `FAIL_CLOSED` + plugin block-on-error;
  `allow` -> native `FAIL_OPEN` + plugin allow-on-error.
- Size limits are enforced before allocation: `max_payload_bytes`, `max_decoded_bytes`,
  `max_data_files`, `max_filename_len`. Values are operator configuration and are not
  part of this payload.
- An explicitly allowed empty ruleset is an intentional no-op WAF, not a missing
  configuration.
- Release artifacts: raw `plugin.wasm`, `plugin.wasm.sha256` (lowercase hex),
  `plugin.manifest.yaml` (`schema_version`, module name, file, sha256, git SHA,
  Coraza version, proxy-wasm SDK version). CKO bundles exactly these bytes and manifest.

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
- The ECDS `version_info` is the complete-resource content hash:
  `sha256:<64 lowercase hexadecimal characters>`.
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
