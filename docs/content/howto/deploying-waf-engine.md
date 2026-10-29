---
title: "Deploying a WAF Engine"
linkTitle: "Deploying a WAF Engine"
weight: 25
description: "Create an Engine resource to attach a WAF to a Kubernetes Gateway."
---

An Engine resource references a RuleSet and attaches the Coraza WAF to a Gateway. The operator renders an Istio `EnvoyFilter` for the Gateway and delivers the compiled rules to it over ECDS (Envoy Configuration Discovery Service).

## Creating an Engine

The minimum Engine configuration requires a RuleSet reference and a target that identifies your Gateway:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: my-engine
spec:
  ruleSet:
    name: my-ruleset
  target:
    type: Gateway
    name: my-gateway
    provider: Istio
```

## Selecting a Gateway

The `target.name` identifies the Gateway resource in the same namespace. The operator derives the workload label selector using the GEP-1762 convention (`gateway.networking.k8s.io/gateway-name` label).

Only one Engine resource may target a given Gateway. If multiple Engines reference the same Gateway, only the oldest one (by creation timestamp) is accepted; the others receive an `Accepted=False` condition with reason `TargetConflict`. See [Status Conditions]({{< relref "../reference/status-conditions" >}}) for details.

To verify your Gateway name:

```bash
kubectl get gateways -n my-namespace
```

## Configuring the Failure Policy

The `failurePolicy` field controls what happens when the WAF is not ready or encounters an error:

| Value | Behavior |
|-------|----------|
| `fail` (default) | Block all traffic when the WAF is not ready. |
| `allow` | Allow traffic through when the WAF is not ready. |

```yaml
spec:
  failurePolicy: allow
```

See [Configuring Failure Policies]({{< relref "configuring-failure-policies" >}}) for guidance on choosing.

## Rule Set Delivery

Rules are delivered to the Gateway over ECDS, and the operator pushes updates as soon as they are accepted. The Gateway does not poll. The `spec.ruleSetCacheServer` field is retained for API compatibility but is ignored.

## Selecting the WASM Module

By default, the operator serves its own bundled WASM module over its HTTPS asset route and
references it in the ECDS configuration; no module reference is needed. To use a custom
module, specify an HTTPS URL together with its SHA-256 digest, and Envoy fetches it:

```yaml
spec:
  driver:
    type: wasm
    wasm:
      url: "https://files.example.com/coraza-rs/envoy-coraza-<version>.wasm"
      sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
```

The `url` and `sha256` fields must be set together: `url` must be an HTTPS URL without query or fragment, and `sha256` must be the 64-character lowercase hexadecimal SHA-256 of the raw WASM bytes served at `url`. Omit both fields (or set `wasm: {}`) to use the bundled module.

The custom URL host must present a certificate that chains to the CA configured with `--xds-ca-path` (the operator serving CA). Public certificate hosts are not supported by this MVP, so use an internal host with a matching certificate or keep the bundled module.

## Verifying the Engine

Check the Engine status:

```bash
kubectl get engine my-engine -n my-namespace
```

The output shows the referenced RuleSet, provider, target, failure policy, and readiness:

```
NAME        RULESET      PROVIDER   TARGET TYPE   TARGET NAME   FAILURE POLICY   READY   AGE
my-engine   my-ruleset   Istio      Gateway       my-gateway    fail             True    5m
```

For detailed status conditions and events:

```bash
kubectl describe engine my-engine -n my-namespace
```

To verify that the WAF is attached, check for the EnvoyFilter and NetworkPolicy created by the operator:

```bash
kubectl get envoyfilter,networkpolicy -n my-namespace
```
