---
title: "Architecture"
linkTitle: "Architecture"
weight: 10
description: "How the operator's two controllers, shared cache, and data flow work together."
---

The Coraza Kubernetes Operator uses a two-controller architecture with a shared in-memory cache to deliver firewall rules to WASM plugins running in Envoy sidecars.

## High-Level Overview

The operator manages two custom resources:

- **RuleSet** -- Aggregates SecLang from **RuleSource** resources (and optional **RuleData** for `@pmFromFile` files) and compiles them into a cached, validated ruleset.
- **Engine** -- Attaches a RuleSet to one or more Gateways by deploying a Coraza WASM plugin into Envoy.

The data flows through these components:

```
RuleSource / RuleData
       |
       v
RuleSetReconciler  --compile and validate-->  ECDS resource (per Engine)
                                                    |
                                                    v
                                   EnvoyFilter + WASM plugin in Envoy
                                                    |
                                                    v
                                            Traffic filtering
```

## Two Controllers

### RuleSetReconciler

The RuleSet controller watches **RuleSet** resources and the **RuleSource** and **RuleData** objects they reference (via field indexes in the watched namespace). When any of these change, it:

1. Loads each **RuleSource** in `spec.sources` order and concatenates `spec.rules`.
2. If `spec.data` is set, loads each **RuleData** and merges `spec.files` (later objects override duplicate filenames).
3. Compiles and validates the rules using the Coraza engine.
4. Checks for rules that are unsupported in the current execution environment (such as WASM mode).
5. On success, records the accepted content hash in the RuleSet status.
6. Updates the RuleSet status conditions.

If compilation fails or unsupported rules are detected, the new revision is rejected. The last accepted record may remain available for status and audit, while the Engine's distributable snapshot follows the accepted-record rules described in [Rule processing]({{< relref "/explanation/rule-processing" >}}).

### EngineReconciler

The Engine controller watches Engine resources, their referenced RuleSets, Gateways, and pods. When an Engine is created or updated, it:

1. Verifies that the referenced RuleSet is in a Ready state.
2. Discovers Gateway pods that match the Engine's target name (via the GEP-1762 label selector).
3. Creates or updates an Istio EnvoyFilter that inserts the WASM filter and publishes the accepted RuleSet over ECDS (see [Istio WASM Integration]({{< relref "/explanation/istio-wasm-integration" >}})).
4. Creates a NetworkPolicy to allow Gateway pods to reach the operator's ECDS listener.
5. Authorizes the Gateway's ECDS subscription: the Gateway authenticates with its own pod ServiceAccount token (validated by TokenReview), and the operator maps that ServiceAccount to the Engine's target Gateway.
6. Updates the Engine status with matched Gateways and conditions.

Dataplane `coraza_waf_*` metrics use Istio OpenTelemetry access logs and a platform-owned central collector. For an observability-enabled Engine, the reconciler creates the Gateway-scoped Telemetry; it does not create OTC sidecars or patch Gateways for telemetry inject.

## ECDS and Asset Serving

The active delivery path is ECDS, not the legacy HTTP rule server. The operator serves ECDS and the digest-addressed WASM asset route on one TLS listener (port 18080 by default). The retained `/rules` cache-server implementation is not started in the default deployment.

The ECDS serving plane is built per replica. It reconstructs accepted content from Kubernetes state, verifies the accepted hashes, and serves only matching snapshots. See [Istio WASM Integration]({{< relref "/explanation/istio-wasm-integration" >}}) for the transport and attachment details.

The ECDS path does not depend on CKO-created ServiceEntry or DestinationRule resources. Gateway access is controlled by the Engine NetworkPolicy and the platform's network policy/egress controls.

## Controller Manager

Both controllers are initialized in a shared controller manager (`internal/controller/manager.go`). The manager provides:

- A shared Kubernetes client and cache.
- Leader election support for high availability.
- Health and readiness probes.
- The metrics endpoint.

The RuleSetCache instance is created once and shared between both controllers.
