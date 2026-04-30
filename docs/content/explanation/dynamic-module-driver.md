---
title: "Dynamic Module Driver"
linkTitle: "Dynamic Module Driver"
weight: 35
description: "How the operator integrates with Envoy using the dynamic module filter as an alternative to WASM."
---

The Coraza Kubernetes Operator can deploy WAF rules using an Envoy **dynamic
module** instead of a WASM plugin. This page explains how this alternative
driver works and when to use it.

## How Envoy Dynamic Modules Work

Envoy supports loading native shared libraries (`.so` files) at runtime via
the [dynamic modules](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/dynamic_modules)
HTTP filter extension. Unlike WASM, dynamic modules run as native code inside
the Envoy process — no sandbox overhead, no ABI translation.

The operator uses the [Built On Envoy (BOE)](https://github.com/tetratelabs/built-on-envoy)
**composer** module with the **coraza-waf** plugin. This module:

- Loads the Coraza WAF engine as a native Go shared library.
- Receives SecLang directives inline via the EnvoyFilter configuration.
- Evaluates HTTP requests and responses against the loaded rules.
- Blocks or allows traffic based on rule outcomes.

## What the Operator Creates

When an Engine uses `spec.driver.type: dynamicModule`, the operator creates:

### EnvoyFilter

An Istio [EnvoyFilter](https://istio.io/latest/docs/reference/config/networking/envoy-filter/)
resource that patches the gateway's Envoy configuration to insert the dynamic
module HTTP filter before the router. WAF rules are embedded inline in the
filter configuration as a JSON array of SecLang directives.

### ConfigMap (optional)

When `spec.driver.dynamicModule.moduleImage` is set, the operator creates a
ConfigMap containing a Deployment overlay. This overlay injects init containers
into the gateway pod to copy the `.so` from the module image into a shared
volume accessible by the Envoy proxy.

The Gateway must reference this ConfigMap via
`spec.infrastructure.parametersRef` so that Istio's gateway controller applies
the overlay when creating the gateway pod.

## Driver Configuration

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: coraza
spec:
  ruleSet:
    name: default-ruleset
  target:
    type: Gateway
    name: coraza-gateway
  driver:
    type: dynamicModule
    dynamicModule:
      moduleImage: ghcr.io/tetratelabs/boe-composer:v0.6.0
      proxyImage: gcr.io/istio-testing/proxyv2:1.31-dev
```

### Fields

| Field | Default | Description |
|-------|---------|-------------|
| `moduleName` | `composer` | Envoy dynamic module name (maps to `lib<name>.so`) |
| `filterName` | `coraza-waf` | HTTP filter name registered by the module |
| `filterMode` | `FULL` | Inspection mode: `REQUEST_ONLY`, `RESPONSE_ONLY`, or `FULL` |
| `moduleImage` | *(none)* | OCI image containing the `.so`; triggers ConfigMap creation |
| `proxyImage` | *(none)* | Overrides the Envoy proxy image in the gateway pod |

### Proxy Image

The standard Istio proxy image does not include the dynamic modules HTTP filter
extension. You must use an Envoy build that has it compiled in. The
`proxyImage` field adds an image override to the ConfigMap Deployment overlay,
which replaces the default `istio-proxy` container image.

## WASM vs Dynamic Module

| Aspect | WASM | Dynamic Module |
|--------|------|----------------|
| Isolation | Sandboxed (V8/Wasmtime) | Native (in-process) |
| Performance | ABI overhead | Native speed |
| Rule delivery | Polled from cache server | Embedded in EnvoyFilter |
| Istio resource | WasmPlugin | EnvoyFilter + ConfigMap |
| Envoy requirement | Standard Istio proxy | Custom Envoy with dynamic modules |

The WASM driver is the default and works with standard Istio installations. The
dynamic module driver is suited for environments where native performance is
critical and a custom Envoy build is acceptable.
