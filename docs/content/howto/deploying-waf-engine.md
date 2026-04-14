---
title: "Deploying a WAF Engine"
linkTitle: "Deploying a WAF Engine"
weight: 25
description: "Create an Engine resource to attach a WAF to a Kubernetes Gateway."
---

An Engine resource references a RuleSet and attaches the Coraza WAF to one or more Gateways via an Istio WasmPlugin.

## Creating an Engine

The minimum Engine configuration requires a RuleSet reference and a workload selector that matches your Gateway:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: my-engine
spec:
  ruleSet:
    name: my-ruleset
  driver:
    istio:
      wasm:
        mode: gateway
        workloadSelector:
          matchLabels:
            gateway.networking.k8s.io/gateway-name: my-gateway
        ruleSetCacheServer:
          pollIntervalSeconds: 15
```

## Selecting a Gateway

The `workloadSelector` determines which Gateway pods the WAF attaches to. Kubernetes Gateway API implementations typically label Gateway pods with `gateway.networking.k8s.io/gateway-name`. Use the label that matches your Gateway:

```bash
kubectl get pods -n my-namespace --show-labels
```

Look for labels on the Gateway pods and use them in `matchLabels`.

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

The `pollIntervalSeconds` field controls how often the WASM plugin checks the cache for updated rules. The default is 15 seconds. Valid range: 1 to 3600.

```yaml
spec:
  driver:
    istio:
      wasm:
        ruleSetCacheServer:
          pollIntervalSeconds: 30
```

Lower values mean faster rule updates but slightly more network traffic between the WASM plugin and the cache server.

## Using a Custom WASM Image

By default, the operator uses its built-in WASM plugin image. To use a custom image, specify it in the Engine:

```yaml
spec:
  driver:
    istio:
      wasm:
        image: "oci://ghcr.io/my-org/coraza-proxy-wasm:v1.0.0"
```

The image must use the `oci://` URI scheme.

If the image is in a private registry, provide an image pull secret:

```yaml
spec:
  driver:
    istio:
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

The output shows the referenced RuleSet, failure policy, and readiness:

```
NAME        RULESET      FAILURE POLICY   READY   AGE
my-engine   my-ruleset   fail             True    5m
```

For detailed status including matched Gateways:

```bash
kubectl describe engine my-engine -n my-namespace
```
