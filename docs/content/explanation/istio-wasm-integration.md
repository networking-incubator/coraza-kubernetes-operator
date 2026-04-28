---
title: "Istio WASM Integration"
linkTitle: "Istio WASM Integration"
weight: 30
description: "How the operator integrates with Istio using WebAssembly plugins."
---

The Coraza Kubernetes Operator integrates with Istio by deploying a WebAssembly (WASM) plugin into Envoy proxies attached to Kubernetes Gateways. This page explains how that integration works.

## How Istio WasmPlugin Works

Istio provides a [WasmPlugin](https://istio.io/latest/docs/reference/config/proxy_extensions/wasm-plugin/) resource that instructs Envoy to load and execute a WASM module. The operator creates WasmPlugin resources to inject the Coraza WAF into the request processing pipeline.

When a WasmPlugin is applied, Istio:

1. Downloads the WASM binary from the specified OCI registry.
2. Loads it into the Envoy proxy as a filter.
3. Routes HTTP requests through the WASM filter before forwarding them to the backend.

## The coraza-proxy-wasm Plugin

The WASM module used by the operator is [coraza-proxy-wasm](https://github.com/networking-incubator/coraza-proxy-wasm). It is a purpose-built Envoy WASM filter that:

- Runs the Coraza WAF engine inside Envoy.
- Polls the operator's RuleSet cache server for rule updates.
- Evaluates incoming HTTP requests against the loaded rules.
- Blocks or allows requests based on rule outcomes.

The plugin is distributed as an OCI image. The operator uses a built-in default image reference, which can be overridden per Engine or globally via the `--default-wasm-image` flag.

## WasmPlugin Resource Generation

When an Engine is reconciled, the operator creates a WasmPlugin resource using server-side apply. The WasmPlugin:

- References the WASM OCI image.
- Targets Gateway pods using the label selector derived from the Engine's `target.name` (via the GEP-1762 `gateway.networking.k8s.io/gateway-name` label).
- Passes configuration to the WASM plugin, including the cache server URL and the RuleSet key.
- Sets the failure policy (fail-open or fail-closed).

The operator watches WasmPlugin resources it creates and filters out update events to prevent reconcile loops. Only create and delete events trigger re-reconciliation.

## Target Selection and Gateway Matching

The Engine's `target` field identifies the Gateway by name. The operator derives the workload label selector using the GEP-1762 convention — Gateway API implementations label Gateway pods with:

```
gateway.networking.k8s.io/gateway-name: <gateway-name>
```

The `target.provider` field (default: `Istio`) identifies the infrastructure provider, which determines which driver types are valid for the Engine. This field is immutable after creation — to switch providers, create a new Engine resource. This ensures the controller does not need to clean up and recreate child resources (WasmPlugin, NetworkPolicy, ServiceAccount tokens) from the previous driver.

## Poll-Based Rule Updates

The WASM plugin running inside Envoy polls the operator's cache server at the interval specified by `pollIntervalSeconds` (default: 15 seconds). The polling flow is:

1. The plugin sends `GET /rules/{namespace/name}/latest` to check the current UUID.
2. If the UUID has changed since the last fetch, the plugin sends `GET /rules/{namespace/name}` to download the new rules.
3. The plugin loads the new rules and begins enforcing them on subsequent requests.

This design allows rule updates without restarting Envoy or the Gateway pods.

## Cache Server Connectivity

For the WASM plugin to reach the cache server, the operator:

1. Creates a **ServiceEntry** and **DestinationRule** to make the cache server discoverable within the Istio mesh (when `--operator-name` is set).
2. Creates a **NetworkPolicy** to allow Gateway pods to connect to the cache server port.
3. Issues a **ServiceAccount token** for the WASM plugin to authenticate with the cache server.

## Custom WASM Images

Users can build and deploy custom WASM plugins by:

1. Building the `coraza-proxy-wasm` module from source using TinyGo.
2. Packaging it as an OCI image.
3. Specifying the image in the Engine's `spec.driver.wasm.image` field.

If the image is in a private registry, an `imagePullSecret` can be specified to provide authentication credentials.
