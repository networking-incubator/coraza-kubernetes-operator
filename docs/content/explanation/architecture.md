---
title: "Architecture"
linkTitle: "Architecture"
weight: 10
description: "How the operator's two controllers, shared cache, and data flow work together."
---

The Coraza Kubernetes Operator uses a two-controller architecture with a shared in-memory cache to deliver firewall rules to WASM plugins running in Envoy sidecars.

## High-Level Overview

The operator manages two custom resources:

- **RuleSet** -- Aggregates SecLang rules from ConfigMaps and compiles them into a cached, validated ruleset.
- **Engine** -- Attaches a RuleSet to one or more Gateways by deploying a Coraza WASM plugin into Envoy.

The data flows through these components:

```
ConfigMaps/Secrets
       |
       v
RuleSetReconciler  --compile and validate-->  RuleSetCache (HTTP server)
                                                    |
                                                    v
                                          WASM plugin in Envoy (polls cache)
                                                    |
                                                    v
                                            Traffic filtering
```

## Two Controllers

### RuleSetReconciler

The RuleSet controller watches RuleSet resources and their referenced ConfigMaps and Secrets. When any of these change, it:

1. Reads the rules from each referenced ConfigMap in order.
2. If a data Secret is referenced, loads the data files.
3. Compiles and validates the rules using the Coraza engine.
4. Checks for rules that are unsupported in the current execution environment (such as WASM mode).
5. On success, stores the compiled ruleset in the shared cache.
6. Updates the RuleSet status conditions.

If compilation fails or unsupported rules are detected, the new revision is rejected. Any previously cached valid revision continues to be served until the issues are resolved.

### EngineReconciler

The Engine controller watches Engine resources, their referenced RuleSets, Gateways, and pods. When an Engine is created or updated, it:

1. Verifies that the referenced RuleSet is in a Ready state.
2. Discovers Gateway pods that match the Engine's workload selector.
3. Creates or updates an Istio WasmPlugin resource via server-side apply, configuring the WASM plugin to poll the cache server for the specified RuleSet.
4. Creates a NetworkPolicy to allow Gateway pods to reach the cache server.
5. Manages a ServiceAccount token for authenticated cache access.
6. Updates the Engine status with matched Gateways and conditions.

## RuleSet Cache Server

The cache is an in-memory, versioned HTTP server that runs within the operator process on port 18080 (configurable). It serves two endpoints:

| Endpoint | Purpose |
|----------|---------|
| `GET /rules/{namespace/name}` | Returns the full compiled ruleset as a JSON `RuleSetEntry`. |
| `GET /rules/{namespace/name}/latest` | Returns metadata (UUID and timestamp) about the latest cached version. |

The cache keys are the `namespace/name` of the RuleSet resource. Cache entries are garbage-collected based on:

- **Maximum age** (`--cache-max-age`, default 24 hours) -- entries older than this are removed.
- **Maximum size** (`--cache-max-size`, default 100 MB) -- when the total cache size exceeds this, the oldest entries are evicted.
- **GC interval** (`--cache-gc-interval`, default 5 minutes) -- how often the garbage collector runs.

## Istio Prerequisites

When the `--operator-name` flag is set, the operator creates a ServiceEntry and DestinationRule at startup via server-side apply. These resources make the cache server discoverable within the Istio mesh so that WASM plugins running in Gateway pods can reach it.

## Controller Manager

Both controllers are initialized in a shared controller manager (`internal/controller/manager.go`). The manager provides:

- A shared Kubernetes client and cache.
- Leader election support for high availability.
- Health and readiness probes.
- The metrics endpoint.

The RuleSetCache instance is created once and shared between both controllers.
