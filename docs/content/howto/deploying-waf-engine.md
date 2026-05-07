---
title: "Deploying a WAF Engine"
linkTitle: "Deploying a WAF Engine"
weight: 25
description: "Create an Engine resource to attach a WAF to a Kubernetes Gateway."
---

An Engine resource references a RuleSet and attaches the Coraza WAF to a Gateway via an Istio WasmPlugin.

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

## Configuring the Poll Interval

The `ruleSetCacheServer.pollIntervalSeconds` field controls how often the WASM plugin checks the cache for updated rules. The default is 15 seconds. Valid range: 1 to 3600.

```yaml
spec:
  ruleSetCacheServer:
    pollIntervalSeconds: 30
```

Lower values mean faster rule updates but slightly more network traffic between the WASM plugin and the cache server.

## Using a Custom WASM Image

By default, the operator uses its built-in WASM plugin image. To use a custom image, specify it in the Engine:

```yaml
spec:
  driver:
    type: wasm
    wasm:
      image: "oci://ghcr.io/my-org/coraza-proxy-wasm:v1.0.0"
```

The image must use the `oci://` URI scheme.

If the image is in a private registry, provide an image pull secret:

```yaml
spec:
  driver:
    type: wasm
    wasm:
      image: "oci://my-registry.example.com/coraza-proxy-wasm:v1.0.0"
      imagePullSecret: my-registry-credentials
```

The Secret must exist in the same namespace as the Engine.

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

To verify that the WAF is attached, check for the WasmPlugin and NetworkPolicy created by the operator:

```bash
kubectl get wasmplugin,networkpolicy -n my-namespace
```
