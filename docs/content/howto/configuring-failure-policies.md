---
title: "Configuring Failure Policies"
linkTitle: "Configuring Failure Policies"
weight: 40
description: "Choose how the WAF behaves when it is not ready or encounters errors."
---

The Engine `failurePolicy` field determines how traffic is handled when the WAF is not ready or encounters an error during rule evaluation.

## Available Policies

| Policy | Behavior |
|--------|----------|
| `fail` (default) | Block all traffic when the WAF is not ready or encounters an error. This prioritizes security. |
| `allow` | Allow traffic through when the WAF is not ready or encounters an error. This prioritizes availability. |

## Setting the Failure Policy

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: my-engine
spec:
  failurePolicy: fail
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

## When to Use Each Policy

### Use `fail` when:

- Security is the highest priority.
- You prefer to block traffic rather than risk allowing unfiltered requests.
- The application behind the Gateway can tolerate brief outages during WAF startup or rule updates.

### Use `allow` when:

- Availability is the highest priority.
- You prefer to serve traffic unfiltered rather than block it during WAF startup.
- The WAF provides defense-in-depth alongside other security controls.

## Changing the Policy

You can change the failure policy on an existing Engine at any time:

```bash
kubectl patch engine my-engine -n my-namespace \
  --type merge \
  -p '{"spec":{"failurePolicy":"allow"}}'
```

The change takes effect at the next reconciliation cycle.
