---
title: "Status Conditions and Troubleshooting"
linkTitle: "Status Conditions"
weight: 35
description: "Understanding resource status conditions and common troubleshooting steps."
---

Both Engine and RuleSet resources report their state through standard Kubernetes conditions. This page describes each condition type and provides troubleshooting guidance.

## Condition Types

| Type | Meaning |
|------|---------|
| `Ready` | The resource has been successfully processed and is operational. |
| `Progressing` | The resource is being created or updated. |
| `Degraded` | The resource failed to reach or maintain its desired state. |

Each condition includes:

- **status**: `True`, `False`, or `Unknown`
- **reason**: A programmatic identifier (CamelCase) explaining the condition.
- **message**: A human-readable description.
- **lastTransitionTime**: When the condition last changed.
- **observedGeneration**: The resource generation that was observed.

## Engine Conditions

### Ready

The Engine is deployed and attached to one or more Gateways.

```bash
kubectl get engine my-engine -n my-namespace
```

### Progressing

The Engine is being reconciled. This is normal during creation or after updates.

### Degraded

The Engine could not reach its desired state. Common reasons:

| Reason | Description | Resolution |
|--------|-------------|------------|
| `RuleSetNotFound` | The referenced RuleSet does not exist. | Verify the RuleSet name and namespace in the Engine spec. |
| `RuleSetDegraded` | The referenced RuleSet is in a Degraded state. | Check the RuleSet status: `kubectl describe ruleset <name>`. |
| `InvalidConfiguration` | The Engine spec contains an invalid configuration. | Check the condition message for details and fix the Engine spec. |
| `ProvisioningFailed` | Failed to create or update the WasmPlugin resource. | Check operator logs and RBAC permissions. |
| `NetworkPolicyFailed` | Failed to apply the NetworkPolicy for the cache server. | Check operator logs and RBAC permissions. |
| `ServiceAccountFailed` | Failed to ensure the cache client ServiceAccount. | Check operator logs and RBAC permissions. |
| `TokenFailed` | Failed to ensure the cache client token. | Check operator logs and RBAC permissions. |

## RuleSet Conditions

### Ready

The rules have been compiled, validated, and cached.

### Progressing

The RuleSet is being processed. This happens when ConfigMaps or Secrets are updated.

### Degraded

The RuleSet could not be compiled or cached. Common reasons:

| Reason | Description | Resolution |
|--------|-------------|------------|
| `UnsupportedRules` | The RuleSet contains rules not supported in the current execution environment. | Remove the unsupported rules, or add the annotation `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"` to the RuleSet. |
| `InvalidRuleSet` | Rule compilation failed due to syntax errors. | Check the condition message for the specific error. Fix the rule syntax in the ConfigMap. |
| `ConfigMapNotFound` | A referenced ConfigMap does not exist. | Verify the ConfigMap name and namespace. |
| `ConfigMapAccessError` | The operator could not read a referenced ConfigMap. | Check RBAC permissions for the operator ServiceAccount. |
| `InvalidConfigMap` | A referenced ConfigMap is missing the `rules` key. | Ensure each ConfigMap has a `rules` key containing SecLang directives. |
| `SecretNotFound` | A referenced Secret does not exist. | Verify the Secret name and namespace. |
| `SecretAccessError` | The operator could not read a referenced Secret. | Check RBAC permissions for the operator ServiceAccount. |
| `RuleDataSecretTypeMismatch` | A referenced Secret has an unexpected type. | Ensure the Secret type matches what the RuleSet expects. |

## Troubleshooting

### Checking Resource Status

View the summary:

```bash
kubectl get engine,ruleset -n my-namespace
```

View detailed conditions:

```bash
kubectl describe engine my-engine -n my-namespace
kubectl describe ruleset my-ruleset -n my-namespace
```

### Checking Operator Logs

```bash
kubectl logs -n coraza-system deploy/coraza-kubernetes-operator
```

For more verbose output, set the logging level to `debug` in the Helm values:

```yaml
logging:
  level: debug
```

### Common Issues

**RuleSet stays in Progressing state**

The operator may be waiting for ConfigMaps to become available. Verify all referenced ConfigMaps exist:

```bash
kubectl get configmap -n my-namespace
```

**Engine is Ready but traffic is not filtered**

1. Verify the Engine's workload selector matches the Gateway pods:
   ```bash
   kubectl get pods -n my-namespace --show-labels
   ```

2. Check that the WasmPlugin was created:
   ```bash
   kubectl get wasmplugin -n my-namespace
   ```

3. Check the Gateway proxy logs for Coraza output:
   ```bash
   kubectl logs -n my-namespace deploy/<gateway-name>-istio
   ```

**RuleSet Degraded with UnsupportedRules**

Some rules are not compatible with the WASM execution environment. See [Known Limitations]({{< relref "../explanation/known-limitations" >}}) for details. To proceed despite unsupported rules:

```bash
kubectl annotate ruleset my-ruleset -n my-namespace \
  waf.k8s.coraza.io/skip-unsupported-rules-check=true
```
