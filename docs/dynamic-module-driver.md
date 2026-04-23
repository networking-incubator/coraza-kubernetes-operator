# Dynamic Module Driver for Engine Controller

This document describes the PoC implementation of the Envoy Dynamic Module
driver as an alternative to the existing WASM driver in the coraza-kubernetes-operator.

## Motivation

The operator currently deploys Coraza via Istio's `WasmPlugin` CRD, which loads
`coraza-proxy-wasm` into Envoy as a WebAssembly module. The WASM plugin polls a
cache server for rules at runtime.

Envoy natively supports [dynamic modules](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/dynamic_modules)
— shared libraries (`.so`) loaded at runtime via `dlopen`. Compared to WASM,
native dynamic modules avoid the overhead of the WASM runtime and can use the
full Go standard library.

[Built On Envoy (BOE)](https://github.com/tetratelabs/built-on-envoy) is a
community-driven marketplace that makes it easy to discover, run, and build
Envoy extensions. Among the extensions it ships is the
[coraza-waf](https://github.com/nicholasgasior/coraza-waf) dynamic module — a
WAF filter written in Go and compiled to a shared library that leverages Envoy's
dynamic module support.

This driver adds support for deploying the coraza-waf dynamic module via Istio's
`EnvoyFilter` CRD, with WAF rules embedded inline in the filter configuration.

## Architecture

### WASM driver (existing)

```
ConfigMaps ─> RuleSetReconciler ─> RuleSetCache (HTTP server)
                                         │
                                         ▼
Engine ─> EngineReconciler ─> WasmPlugin CRD ─> Envoy loads WASM
                                                      │
                                              polls cache server for rules
```

The WASM plugin runs inside Envoy and periodically polls the operator's cache
server over HTTP for the latest aggregated rules. This requires:
- A ServiceAccount and JWT token per Engine for cache authentication
- A ServiceEntry + DestinationRule to make the cache server reachable in the mesh
- A NetworkPolicy to restrict cache server access to gateway pods

### Dynamic module driver (new)

```
ConfigMaps ─> RuleSetReconciler ─> RuleSetCache (in-memory)
                                         │
                                         ▼
Engine ─> EngineReconciler ─> reads rules from cache
                                    │
                                    ▼
                            EnvoyFilter CRD (rules inline) ─> Envoy loads .so
```

The operator reads aggregated rules from the `RuleSetCache` at reconcile time
and embeds them directly in the EnvoyFilter's `filter_config` field as a JSON
string. When rules change, the RuleSet watch triggers re-reconciliation, the
operator rebuilds the EnvoyFilter with updated rules, and Envoy picks up the new
configuration.

When `moduleImage` is set, the operator also creates a ConfigMap with a
Deployment overlay that injects an init container to copy the `.so` into the
gateway pod (see [`.so` distribution](#so-distribution) below).

### What the dynamic module path does NOT need

Because rules are delivered inline rather than polled from a cache server:

- **No token management** — no ServiceAccount creation, no JWT token issuance or
  renewal, no `tokenStore`
- **No cache server access** — no ServiceEntry, no DestinationRule, no
  NetworkPolicy for cache ingress
- **No requeue for token renewal** — reconciliation returns `ctrl.Result{}`
  (no `RequeueAfter`)

The only requeue in the dynamic module path is the standard finalizer-add requeue
(100ms), which is shared with the WASM driver.

## API

### New CRD fields

`spec.driver.istio.dynamicModule` — an alternative to `spec.driver.istio.wasm`:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | `IstioIntegrationMode` | `gateway` | Integration mode. Only `gateway` is supported. |
| `workloadSelector` | `LabelSelector` | (required) | Selects which gateway pods get the filter. |
| `moduleName` | `string` | `composer` | Envoy dynamic module name (maps to `lib<name>.so`). |
| `filterName` | `string` | `coraza-waf` | HTTP filter name registered by the module. |
| `filterMode` | `DynamicModuleFilterMode` | `FULL` | Inspection mode: `REQUEST_ONLY`, `RESPONSE_ONLY`, or `FULL`. |
| `moduleImage` | `string` | (optional) | OCI image containing `lib<moduleName>.so`. When set, creates a ConfigMap with an init container overlay. |
| `proxyImage` | `string` | (optional) | Overrides the Envoy proxy image. The standard Istio proxy lacks the dynamic modules extension; use an Envoy build that includes it (e.g., `gcr.io/istio-testing/proxyv2:1.31-dev`). Only effective when `moduleImage` is also set. |

Exactly one of `wasm` or `dynamicModule` must be specified on the `istio`
driver. This is enforced by a CEL XValidation rule on `IstioDriverConfig`.

### Example

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: coraza-dynamic-module
spec:
  ruleSet:
    name: default-ruleset
  failurePolicy: fail
  driver:
    istio:
      dynamicModule:
        mode: gateway
        workloadSelector:
          matchLabels:
            gateway.networking.k8s.io/gateway-name: coraza-gateway
        moduleName: composer
        filterName: coraza-waf
        filterMode: FULL
        moduleImage: ghcr.io/tetratelabs/boe-composer:v0.6.0
        proxyImage: gcr.io/istio-testing/proxyv2:1.31-dev
```

When `moduleImage` is set, the operator creates a ConfigMap named
`coraza-dm-coraza-dynamic-module`. Reference it from the Gateway:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: coraza-gateway
spec:
  gatewayClassName: istio
  infrastructure:
    parametersRef:
      group: ""
      kind: ConfigMap
      name: coraza-dm-coraza-dynamic-module
  listeners:
  - name: http
    port: 80
    protocol: HTTP
```

## EnvoyFilter structure

The operator produces an EnvoyFilter that patches the HTTP connection manager to
insert the dynamic module filter before `envoy.filters.http.router`:

```yaml
apiVersion: networking.istio.io/v1alpha3
kind: EnvoyFilter
metadata:
  name: coraza-engine-<engine-name>
  namespace: <engine-namespace>
spec:
  workloadSelector:
    labels:
      gateway.networking.k8s.io/gateway-name: coraza-gateway
  configPatches:
  - applyTo: HTTP_FILTER
    match:
      context: GATEWAY
      listener:
        filterChain:
          filter:
            name: envoy.filters.network.http_connection_manager
            subFilter:
              name: envoy.filters.http.router
    patch:
      operation: INSERT_BEFORE
      value:
        name: coraza-waf
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
          dynamic_module_config:
            name: composer
            do_not_close: true
          filter_name: coraza-waf
          filter_config:
            "@type": type.googleapis.com/google.protobuf.StringValue
            value: '{"directives":["SecRuleEngine On","SecRule ..."],"mode":"FULL"}'
```

The `filter_config` value is a JSON string with two fields:
- `directives` — array of SecLang directive strings (one per line from the
  aggregated rules)
- `mode` — the WAF inspection mode (`REQUEST_ONLY`, `RESPONSE_ONLY`, or `FULL`)

## Implementation details

### Files changed

| File | Change |
|------|--------|
| `api/v1alpha1/engine_driver_istio_types.go` | Added `IstioDynamicModuleConfig`, `DynamicModuleFilterMode`, updated `IstioDriverConfig` XValidation |
| `internal/controller/engine_controller.go` | Added `ruleSetCache` field, EnvoyFilter `Owns()` watch, dynamic module case in `selectDriver()` |
| `internal/controller/engine_controller_driver_istio_dynamic_module.go` | **New** — provisioning, EnvoyFilter builder |
| `internal/controller/engine_controller_driver_istio.go` | Added EnvoyFilter RBAC marker, refactored `matchedGateways()` |
| `internal/controller/engine_controller_istio_utils.go` | Added `hasIstioDynamicModuleDriver()` |
| `internal/controller/engine_controller_utils.go` | Updated `engineMatchesLabels()` to be driver-agnostic |
| `internal/controller/engine_controller_map_funcs.go` | Updated `findEnginesForGateway()` to match both drivers |
| `internal/controller/engine_controller_network_policy.go` | Updated `workloadSelector()` to handle both drivers |
| `internal/controller/manager.go` | Wired `ruleSetCache` to `EngineReconciler` |
| `test/utils/resource_builders.go` | Added `NewTestDynamicModuleEngine()` |
| `internal/controller/engine_controller_driver_istio_dynamic_module_test.go` | **New** — unit and integration tests |
| `config/samples-dynamic-module/` | **New** — complete sample manifests (Engine, Gateway, RuleSet, echo, HTTPRoute) |

### Refactoring for driver-agnostic code

Several functions previously hardcoded the WASM driver path. These were
refactored to use the `workloadSelector()` helper, which returns the
`LabelSelector` from whichever driver is configured:

- `matchedGateways()` — finds Gateway pods matching the engine's workload selector
- `engineMatchesLabels()` — checks if an engine's selector matches a set of labels
- `findEnginesForGateway()` — maps Gateway events to Engine reconcile requests
- `workloadSelector()` — extracts the selector from either WASM or DynamicModule config

### How rules reach the EnvoyFilter

1. `RuleSetReconciler` aggregates rules from ConfigMaps/Secrets and stores them
   in `RuleSetCache` as a single concatenated string (one SecLang directive per
   line).
2. When `EngineReconciler` reconciles a dynamic module Engine, it calls
   `ruleSetCache.Get(namespace/rulesetName)` to retrieve the entry.
3. `buildDynamicModuleFilterConfig()` splits the rules string by newline, filters
   empty lines, and produces a JSON string:
   `{"directives":["SecRuleEngine On","SecRule ..."],"mode":"FULL"}`
4. This JSON is embedded in the EnvoyFilter's `filter_config.value` field.
5. The BOE coraza-waf module parses this JSON at filter initialization.

### NetworkPolicy behavior

The dynamic module driver still creates a NetworkPolicy for the engine. This is
because the NetworkPolicy logic is tied to the engine lifecycle (via the
finalizer), not the driver type. In a future iteration, the NetworkPolicy could
be skipped entirely for dynamic module engines since they don't access the cache
server. The current behavior is harmless — it creates a policy that allows
ingress from gateway pods, which is a reasonable default.

### Owner references and garbage collection

The EnvoyFilter uses `controllerutil.SetControllerReference()` and lives in the
same namespace as the Engine, so standard Kubernetes garbage collection handles
cleanup. No finalizer is needed for the EnvoyFilter itself (unlike the
cross-namespace NetworkPolicy, which requires a finalizer).

## Known limitations

### 1. EnvoyFilter object size

Large rule sets produce large EnvoyFilter objects because all rules are embedded
inline. Kubernetes enforces a ~1.5 MB limit on etcd object size. A typical
CoreRuleSet (CRS) deployment with all rules is well under this limit, but
custom deployments with many additional rules could approach it.

### 2. Envoy restart on rule change

When the RuleSet changes, the operator updates the EnvoyFilter, which triggers
an Envoy configuration reload. Depending on the Istio/Envoy version and
configuration, this may cause brief connection disruption. The WASM driver
avoids this because the plugin polls for rules in-place without requiring a
config reload.

### 3. No DataFiles support

The BOE coraza-waf module receives rules as a JSON string. SecLang directives
that reference external files (e.g., `@pmFromFile`) cannot work because there is
no filesystem path to resolve. Users must inline the data or avoid file-referencing
directives.

### 4. `.so` distribution via init container (workaround)

Istio does not yet have a native CRD for dynamic modules (like `WasmPlugin` for
WASM). The Istio team is working on this, but until it ships, the operator uses
a workaround: when `moduleImage` is set, it creates a ConfigMap containing a
Kubernetes Deployment overlay. Istio's gateway controller applies this overlay
via Strategic Merge Patch when the Gateway references it through
`spec.infrastructure.parametersRef`.

The overlay adds:
- A **tools init container** (`dm-tools-init`) using `busybox:stable` that
  copies a statically-linked `cp` binary into a shared tools volume. This is
  needed because the module image is typically built `FROM scratch` and
  contains no utilities.
- A **module init container** (`dynamic-module-init`) that uses the tools
  volume's `cp` to copy `lib<moduleName>.so` from the module image to the
  dynamic-modules volume at `/etc/envoy/dynamic-modules/`
- The **`ENVOY_DYNAMIC_MODULES_SEARCH_PATH`** env var on the `istio-proxy`
  container, pointing to the mount path
- The **`GODEBUG=cgocheck=0`** env var, required because Go dynamic modules use
  cgo and Envoy may hold pointers to Go-managed memory
- Two **emptyDir volumes**: `dm-tools` (for the `cp` binary) and
  `dynamic-modules` (for the `.so` file)
- When **`proxyImage`** is set, an **`image:` override** on the `istio-proxy`
  container. The standard Istio proxy image does not include the
  `envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter`
  extension. This field allows specifying an Envoy build that has it compiled
  in (e.g., `gcr.io/istio-testing/proxyv2:1.31-dev`).

This requires a manual step: the user must set `parametersRef` on their Gateway
to point to the ConfigMap (`coraza-dm-<engine-name>`). The operator cannot set
this automatically because it doesn't own the Gateway resource, and
`parametersRef` accepts only one ConfigMap — overwriting an existing value would
break user customizations.

This is a temporary workaround. Once Istio provides a native CRD for dynamic
modules, the operator should migrate to that mechanism and this ConfigMap-based
approach can be removed.

### 5. EnvoyFilter fragility across Istio upgrades

`EnvoyFilter` is a low-level Istio patching mechanism. The match criteria
(`envoy.filters.network.http_connection_manager`, `envoy.filters.http.router`)
correspond to specific Envoy filter names that could theoretically change across
Envoy versions. In practice these names have been stable for years, but
EnvoyFilter patches are inherently more fragile than the `WasmPlugin` CRD.

### 6. No failure policy enforcement at the Envoy level

The `failurePolicy` field in the Engine spec is respected by the WASM driver
(passed in `pluginConfig`). The dynamic module driver does not currently pass
the failure policy to the module — the BOE coraza-waf module's failure behavior
is determined by its own configuration. This should be addressed once the module
supports a failure policy configuration field.

## TODO

- [ ] **Migrate to Istio native CRD.** Once Istio ships a CRD for dynamic
  modules (similar to `WasmPlugin`), replace the EnvoyFilter + ConfigMap
  workaround with direct use of that CRD. This will eliminate the
  `parametersRef` manual step and the EnvoyFilter fragility.

- [ ] **Skip NetworkPolicy for dynamic module engines.** The cache server is not
  used, so the NetworkPolicy is unnecessary. Consider gating the finalizer and
  NetworkPolicy logic on the driver type.

- [ ] **Pass `failurePolicy` to the dynamic module.** The BOE coraza-waf module
  needs to support a failure policy field in its config JSON, then the operator
  can forward the Engine's `failurePolicy` value.

- [ ] **Integration and e2e tests.** The current tests use envtest (fake API
  server). Real cluster tests with an Istio mesh, a gateway pod with the `.so`
  installed, and actual traffic are needed to validate end-to-end behavior.

- [ ] **DataFiles handling.** Investigate whether the BOE module could accept
  data file contents inline (e.g., as a separate JSON field) to support
  `@pmFromFile` and similar directives.

- [ ] **Rule size monitoring.** Add a status condition or event warning when the
  aggregated rules approach the etcd object size limit.

- [ ] **Sidecar mode.** The current implementation hardcodes `context: GATEWAY`
  in the EnvoyFilter match. Supporting sidecar mode would require matching on
  `SIDECAR_INBOUND` or `SIDECAR_OUTBOUND` contexts.

- [ ] **Helm chart values.** Expose dynamic module defaults (module name, filter
  name, filter mode) as Helm values for cluster-wide configuration.

- [ ] **Documentation.** Add user-facing documentation for the dynamic module
  driver, including prerequisites, migration from WASM, and comparison of the
  two approaches.
