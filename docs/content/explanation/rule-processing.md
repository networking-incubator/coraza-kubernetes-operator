---
title: "Rule Processing"
linkTitle: "Rule Processing"
weight: 20
description: "How rules are aggregated, compiled, validated, and cached."
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

If the update fails aggregate validation, the RuleSet becomes `Degraded` with `InvalidRuleSet`. The Engine clears its accepted configuration and stops distributing that snapshot; an already-running proxy keeps the WAF it has, but a new or reconnecting proxy has only its bootstrap policy until the RuleSet is fixed and re-accepted. Missing RuleData or RuleSource inputs can be a wait state and keep the last accepted configuration instead.

These paths keep a bad rule update from replacing a working WAF with an empty one, but they do not guarantee WAF coverage for proxies that start while the desired state is invalid.
