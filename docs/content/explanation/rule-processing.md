---
title: "Rule Processing"
linkTitle: "Rule Processing"
weight: 20
description: "How rules are aggregated, compiled, validated, accepted, and published."
---

This page explains the lifecycle of firewall rules from **RuleSource** / **RuleData** to enforcement in the WASM plugin.

## Rule aggregation

A **RuleSet** lists **RuleSource** names in `spec.sources`. Each RuleSource stores SecLang in `spec.rules`. The operator fetches each RuleSource in list order and concatenates the strings into one aggregate body (with newlines between fragments).

The order matters because SecLang directives are evaluated sequentially. Engine configuration directives (such as `SecRuleEngine On`) must appear before detection rules.

## SecLang compilation

The aggregated rule body is compiled using the [Coraza](https://github.com/corazawaf/coraza) engine. Compilation performs:

- Syntax validation of all directives.
- Resolution of rule dependencies (such as chain rules).
- Linking of data files when `@pmFromFile` or similar directives are used.

If compilation fails, the RuleSet enters a `Degraded` state with reason `InvalidRuleSet`, and the error message is included in the condition. The failed content is not accepted for delivery.

## Data files

Rules that use `@pmFromFile` reference pattern files by name. The operator loads file content from **RuleData** objects listed in the RuleSet `spec.data` field. Each RuleData `spec.files` entry maps a filename to its text; when multiple RuleData objects are listed, **later** entries in `spec.data` win for the same filename.

The merged files are exposed to the Coraza compiler via an in-memory virtual filesystem (nothing is written to node disk).

## Cluster rule policy

Cluster administrators can narrow what tenants may use in rule text below what `envoy-coraza` supports, with the `rulePolicy` Helm values (manager flags `--rule-denied-operators`, `--rule-denied-directives`, `--rule-max-request-body-limit`, `--rule-response-body-access`). See [Helm Chart Values]({{< relref "../reference/helm-values#cluster-rule-policy" >}}). The policy can only deny: it never allows anything the plugin does not support.

The policy applies to new or changed RuleSet content: the tenant rule text and every RuleData file it `Include`s. The CRS tree bundled in a CRS-enabled module (`crs/...`) is not scanned. A tightened policy, including across an operator restart, applies to the next change of a RuleSet's content: content the RuleSet already accepted is not checked against it again, and RuleSets whose accepted content it would refuse are not reported. Returning to the last accepted content is accepted. The directive and `Include` guard, the body-limit checks and the gateway-parity checks run on every reconcile, also for content the RuleSet already accepted; the Coraza compile runs when the merged content, the generation or readiness changes, and the policy only when the merged content differs from the content the RuleSet last accepted. A violation marks the RuleSet `Degraded` with reason `InvalidRuleSet` and a message such as `@pmFromFile is not allowed by the cluster rule policy`; the last accepted configuration keeps serving. The policy is enforced on the RuleSet, so a RuleSource can show `Validated` while the RuleSet that uses it reports `InvalidRuleSet`.

Operator names and directive names are matched case-insensitively. An operator alias is denied with its operator (`pmf` with `pmFromFile`, `ipMatchF` with `ipMatchFromFile`, `geoLookup` with `unconditionalMatch`). Items of a `SecDataset` block are data and are not matched; a backtick block opened by any other directive is rejected. A `ctl` action in `SecRule`, `SecAction`, `SecDefaultAction` or `SecRuleUpdateActionById` (where Coraza and `envoy-coraza` take the action list from different arguments, both are checked) is judged like the directive with the same effect:

| `ctl` action | Directive |
|---|---|
| `auditEngine` | `SecAuditEngine` |
| `auditLogParts` | `SecAuditLogParts` |
| `debugLogLevel` | `SecDebugLogLevel` |
| `requestBodyAccess` | `SecRequestBodyAccess` |
| `requestBodyLimit` | `SecRequestBodyLimit` |
| `responseBodyAccess` | `SecResponseBodyAccess` |
| `responseBodyLimit` | `SecResponseBodyLimit` |
| `ruleEngine` | `SecRuleEngine` |
| `ruleRemoveById` | `SecRuleRemoveById` |
| `ruleRemoveByMsg` | `SecRuleRemoveByMsg` |
| `ruleRemoveByTag` | `SecRuleRemoveByTag` |
| `ruleRemoveTargetById` | `SecRuleUpdateTargetById` |
| `ruleRemoveTargetByTag` | `SecRuleUpdateTargetByTag` |
| `ruleRemoveTargetByMsg` | `SecRuleUpdateTargetById` or `SecRuleUpdateTargetByTag` |

Denying a directive denies its `ctl` action. `ctl:responseBodyAccess=On` is rejected when response body access is `Denied`, and, when the request body maximum is below 16 MiB, `ctl:requestBodyLimit` above it is rejected. When the request body maximum is below 16 MiB, `ctl:requestBodyAccess=On` needs an explicit `SecRequestBodyLimit` in the tenant rule text, because without one `envoy-coraza` inspects up to 16 MiB.

Only the operator should have write access to `rulesets/status`; the policy reads `status.acceptedContent` to tell accepted content from new content.

## Unsupported rule detection

After compilation, the operator checks for rules that are known to be unsupported in the current execution environment. In WASM mode, unsupported rules fall into two categories:

| Tier | Meaning |
|------|---------|
| **Incompatible** | Rules that do not function in WASM mode. They fail silently and provide no protection. |
| **Redundant** | Rules that work, but where Envoy handles the same attack cases before they reach the WAF. |

When unsupported rules are detected:

- The RuleSet is marked `Degraded` with reason `UnsupportedRules`.
- The condition message lists each unsupported rule ID, its category, and a brief description.
- The rejected content is **not accepted for delivery**. The last accepted ECDS snapshot stays published.
- The RuleSet is not automatically requeued. The user must remove or replace the unsupported rules to produce a new valid revision.

This behavior can be overridden with the annotation `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"`. The issues are still logged and reported in the status, but the RuleSet is accepted normally.

## Rule publication

When rules are successfully compiled and validated, the operator renders an ECDS snapshot for every Engine bound to the RuleSet and streams it to Envoy. The snapshot version is the hash of the Engine-version input, not of the payload bytes: the schema and rendering versions, the Engine identity, the RuleSet UID and content hash, the failure and empty-rules policies, observability flags, the WASM binary URL and digest, and the VM settings. A rule or RuleData change moves it through the RuleSet content hash; an Engine or delivery setting change moves it too.

Envoy applies a new snapshot in place: Gateway pods do not restart and the module bytes are not sent again. See [Istio WASM Integration]({{< relref "/explanation/istio-wasm-integration" >}}) for the delivery details.

## Last-known-good behavior

If a RuleSet update introduces unsupported rules, the new revision is rejected and the last accepted snapshot stays applied. The plugin continues to enforce the last-known-good rules until the issue is resolved and a new valid revision is accepted and published.

If the update fails aggregate validation, the RuleSet becomes `Degraded` with `InvalidRuleSet` and the Engine keeps its accepted configuration, so the last accepted snapshot stays published and an already-running proxy keeps enforcing the previous rules. Content the delivery contract refuses (host-file directives, unsupported includes, or a body limit the plugin would reject) instead reports `RejectedContent`: there the Engine clears its accepted configuration, because the previously accepted content can no longer be delivered. Missing RuleData or RuleSource inputs are a wait state and keep the last accepted configuration as well, and so does content past a delivery limit or the rule limit (`ContentTooLarge`), which is refused before it is compiled. Content that compiled, or failed to, is not compiled again until it changes.

These paths keep a bad rule update from replacing a working WAF with an empty one, but they do not guarantee WAF coverage for proxies that start while the desired state is invalid.
