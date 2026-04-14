---
title: "Rule Processing"
linkTitle: "Rule Processing"
weight: 20
description: "How rules are aggregated, compiled, validated, and cached."
---

This page explains the lifecycle of firewall rules from ConfigMap to enforcement in the WASM plugin.

## Rule Aggregation

A RuleSet references an ordered list of ConfigMaps. Each ConfigMap must contain a key named `rules` with SecLang directives as its value. The operator reads these ConfigMaps in the specified order and concatenates their contents to form a single rule body.

The order matters because SecLang directives are evaluated sequentially. Engine configuration directives (such as `SecRuleEngine On`) must appear before detection rules.

## SecLang Compilation

The aggregated rule body is compiled using the [Coraza](https://github.com/corazawaf/coraza) engine. Compilation performs:

- Syntax validation of all directives.
- Resolution of rule dependencies (such as chain rules).
- Linking of data files when `@pmFromFile` or similar directives are used.

If compilation fails, the RuleSet enters a `Degraded` state with reason `InvalidRuleSet`, and the error message is included in the condition. The failed revision is not cached.

## Data Files

Rules that use the `@pmFromFile` directive reference external pattern files. The operator loads these files from a Secret of type `coraza/data`. Each key in the Secret corresponds to a filename referenced in the rules, and each value is the file content.

The data files are made available to the Coraza compiler via an in-memory virtual filesystem. This means no files are written to disk.

## Unsupported Rule Detection

After compilation, the operator checks for rules that are known to be unsupported in the current execution environment. In WASM mode, unsupported rules fall into two categories:

| Tier | Meaning |
|------|---------|
| **Incompatible** | Rules that do not function in WASM mode. They fail silently and provide no protection. |
| **Redundant** | Rules that work, but where Envoy handles the same attack cases before they reach the WAF. |

When unsupported rules are detected:

- The RuleSet is marked `Degraded` with reason `UnsupportedRules`.
- The condition message lists each unsupported rule ID, its category, and a brief description.
- The rejected revision is **not cached**. Any previously cached valid revision continues to be served.
- The RuleSet is not automatically requeued. The user must remove or replace the unsupported rules to produce a new valid revision.

This behavior can be overridden with the annotation `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"`. The issues are still logged and reported in the status, but the RuleSet is cached normally.

## Cache Entry Lifecycle

When rules are successfully compiled and validated, the result is stored in the in-memory RuleSet cache. The cache uses the RuleSet's `namespace/name` as the key.

Each cache entry is identified by a UUID that changes with each successful compilation. WASM plugins poll the `/rules/{key}/latest` endpoint to check for a new UUID, and fetch the full ruleset from `/rules/{key}` when a change is detected.

Entries are evicted by the garbage collector based on age and total cache size. See [Architecture]({{< relref "architecture" >}}) for the cache configuration parameters.

## Last-Known-Good Behavior

If a RuleSet update introduces invalid or unsupported rules, the new revision is rejected and the previous valid revision remains in the cache. WASM plugins continue to enforce the last-known-good rules until the issue is resolved and a new valid revision is compiled.

This design ensures that a bad rule update does not leave Gateways unprotected.
