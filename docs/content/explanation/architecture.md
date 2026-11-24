---
title: "Architecture"
linkTitle: "Architecture"
weight: 10
description: "How the operator validates rules, attaches Engines, and publishes accepted configurations."
---

The Coraza Kubernetes Operator validates rule resources, attaches Engines to Gateways, and publishes accepted WAF configurations to Envoy over ECDS.

## High-Level Overview

The operator manages four custom resources:

- **RuleSource** -- Stores SecLang rule text and reports per-fragment validation status.
- **RuleData** -- Stores data files referenced by rules such as `@pmFromFile`.
- **RuleSet** -- Orders RuleSources and optional RuleData, validates the assembled rules, and records the accepted content hash.
- **Engine** -- Attaches a RuleSet-backed Coraza WASM filter to a Gateway.

The data flows through these components:

```
RuleSource / RuleData
       |
       v
RuleSetReconciler --validate--> accepted RuleSet status
                                      |
                                      v
EngineReconciler --render--> EnvoyFilter + accepted Engine configuration
                                      |
                                      v
PublicationReconciler --publish--> ECDS snapshot
                                      |
                                      v
                          WASM filter in Envoy
                                      |
                                      v
                             Traffic filtering
```

## Controllers

### RuleSourceReconciler

The RuleSource controller validates each rule fragment with Coraza and records
whether it is valid, invalid, or skipped. Aggregate RuleSet validation remains
authoritative for rules that depend on RuleData or other fragments.

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
3. Creates or updates an Istio EnvoyFilter that inserts the WASM filter and points it at the accepted ECDS resource (see [Istio WASM Integration]({{< relref "/explanation/istio-wasm-integration" >}})).
4. Creates a NetworkPolicy to allow Gateway pods to reach the operator's ECDS listener.
5. Authorizes the Gateway's ECDS subscription: the Gateway authenticates with its own pod ServiceAccount token (validated by TokenReview), and the operator maps that ServiceAccount to the Engine's target Gateway.
6. Updates the Engine status with matched Gateways and conditions.

Dataplane `coraza_waf_*` metrics use Istio OpenTelemetry access logs and a platform-owned central collector. For an observability-enabled Engine, the reconciler creates the Gateway-scoped Telemetry; it does not create OTC sidecars or patch Gateways for telemetry inject.

### PublicationReconciler

Each operator replica reconstructs accepted Engine configurations from Kubernetes
resources and status, verifies their hashes, publishes matching snapshots to its
local xDS store, and authorizes Gateway pod ServiceAccounts. It does not write
status.

## ECDS and Asset Serving

The operator serves ECDS and the digest-addressed WASM asset route on one TLS listener (port 18080 by default).

The ECDS serving plane is built per replica. It reconstructs accepted content from Kubernetes state, verifies the accepted hashes, and serves only matching snapshots. See [Istio WASM Integration]({{< relref "/explanation/istio-wasm-integration" >}}) for the transport and attachment details.

The ECDS path does not depend on CKO-created ServiceEntry or DestinationRule resources. Gateway access is controlled by the Engine NetworkPolicy and the platform's network policy/egress controls.

## Controller Manager

The reconcilers are initialized in a shared controller manager (`internal/controller/manager.go`). The manager provides:

- A shared Kubernetes client and informer cache.
- Leader election support for high availability.
- Health and readiness probes.
- The metrics endpoint.
