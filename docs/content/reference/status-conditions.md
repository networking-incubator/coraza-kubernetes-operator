---
title: "Status Conditions and Troubleshooting"
linkTitle: "Status Conditions"
weight: 35
description: "Understanding resource status conditions and common troubleshooting steps."
---

Engine, RuleSet, RuleSource, and RuleData resources report their state through standard Kubernetes conditions. This page describes each condition type and provides troubleshooting guidance.

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

### Accepted

The Engine's target Gateway has been validated. Only one Engine may target a given Gateway at a time. When multiple Engines reference the same Gateway, the oldest one (by creation timestamp) wins; if timestamps are equal, the lexicographically first name wins. The losing Engines receive `Accepted=False`.

| Reason | Description | Resolution |
|--------|-------------|------------|
| `Accepted` | The target Gateway is available and not contested by another Engine. | No action needed. |
| `TargetNotFound` | The referenced Gateway does not exist in the Engine's namespace. | Verify the Gateway name and namespace in the Engine spec. |
| `TargetConflict` | Another Engine already targets the same Gateway. | Only one Engine may target a given Gateway. Remove the conflicting Engine or change the target. |

### Ready

The Engine's configuration has been accepted and the EnvoyFilter is attached to the Gateway. When the `Ready` condition is `True`, the **reason** is `ConfigurationAccepted`: the configuration rendered from the accepted RuleSet content was provisioned.

```bash
kubectl get engine my-engine -n my-namespace
```

### Progressing

The Engine is being reconciled. This is normal during creation or after updates. While the referenced RuleSet has not accepted content for its current generation, the **reason** is `AwaitingRuleSetAcceptance` (the message explains what the RuleSet is waiting for).

### Degraded

The Engine could not reach its desired state. Common reasons:

| Reason | Description | Resolution |
|--------|-------------|------------|
| `RuleSetNotFound` | The referenced RuleSet does not exist. | Verify the RuleSet name and namespace in the Engine spec. |
| `RuleSetDegraded` | The referenced RuleSet is in a Degraded state. | Check the RuleSet status: `kubectl describe ruleset <name>`. |
| `RuleSetInputsUnavailable` | A RuleSource or RuleData the accepted RuleSet references could not be loaded (for example it was deleted). | Recreate the missing object or fix the reference; the RuleSet must re-accept its content. |
| `InvalidRuleSet` | The accepted RuleSet content cannot be assembled, or it exceeds the operator's delivery limits (`xds-max-decoded-data-bytes`, `xds-max-data-files`, rule text size). | Reduce the RuleData size or file count, or raise the limit if it stays below the plugin caps. |
| `RenderFailed` | The rendered ECDS payload cannot be delivered (it exceeds `xds-max-message-bytes` or the plugin's 8 MiB configuration cap) or rendering failed. | Reduce RuleData or rule text, or raise `xds-max-message-bytes`. |
| `InvalidConfiguration` | The Engine spec contains an invalid configuration. | Check the condition message for details and fix the Engine spec. |
| `ProvisioningFailed` | Failed to create or update the EnvoyFilter resource. | Check operator logs and RBAC permissions. |
| `NetworkPolicyFailed` | Failed to apply the NetworkPolicy that restricts access to the operator's xDS listener. | Check operator logs and RBAC permissions. |
| `TLSNotConfigured` | The operator has no verified CA chain for its xDS serving certificate. | Set `--xds-ca-path` to the CA bundle Gateways use to verify the operator certificate. |

The operator clears `status.acceptedConfiguration` when the Engine reports
`InvalidRuleSet` for content that exceeds the delivery limits, `RenderFailed` for an
oversized payload, `RuleSetInputsUnavailable` for an input that no longer exists,
`RuleSetNotFound`, or a `RuleSetDegraded` state the record cannot be reconstructed
from (a recreated RuleSet or rules this build rejects). That removes the accepted
configuration from the ECDS serving plane, so an already-running proxy keeps the
snapshot it already received but no longer gets updates, and a new or restarted proxy
gets no WAF coverage until the Engine accepts a configuration again. An Engine-side
assembly failure reports `InvalidRuleSet` but keeps the record. Wait states (a degraded RuleSet that is only
missing RuleData, or a RuleSet that has not accepted the current generation) keep the
last good accepted configuration and snapshot instead, so an already-running proxy stays
protected by the previous rules. A cleared record is not a fail-closed signal on its own:
a fresh Gateway pod receives no WAF configuration from the operator for this Engine.

An accepted configuration recorded before the operator restarted must be reconstructed
by every replica. If one cannot be reconstructed, the replica stays unready for the
ECDS plane for up to two minutes and then reports the record and starts serving anyway,
keeping the last snapshot it already had. Readiness never turns a stale snapshot into a
fail-closed Gateway; the record is reported in operator logs and the
`coraza_xds_cold_reconstruction_failures_total` metric instead.

## RuleSource Conditions

RuleSource status is updated by the **RuleSource** reconciler whenever the object’s spec generation changes (or the per-source validation annotation changes). Per-source validation does not load RuleSet-scoped **RuleData**; aggregate validation on the RuleSet still runs with the merged virtual filesystem when a RuleSet references this source.

### Ready

The operator has validated `spec.rules` with Coraza, or validation was skipped via annotation.

| Reason | Description |
|--------|-------------|
| `Validated` | Per-source Coraza rule validation passed (without RuleSet-scoped RuleData). |
| `ValidationSkipped` | Validation was skipped via the `waf.k8s.coraza.io/rule-validation: "false"` annotation. |

### Degraded

Per-source rule validation failed.

| Reason | Description | Resolution |
|--------|-------------|------------|
| `InvalidRules` | The SecLang rules failed Coraza validation. | Check the condition message for the validation error and fix the rule text. |

## RuleData Conditions

RuleData status is updated by the RuleSet reconciler when a RuleSet references the data. If a RuleData is not referenced by any RuleSet, its status reflects the last reconciliation that touched it.

### Ready

The RuleData has been loaded by a RuleSet reconciliation.

| Reason | Description |
|--------|-------------|
| `Loaded` | The data was read successfully. |

> **Note:** Coraza does not validate individual data files before aggregation, so RuleData has no `Degraded` condition. Data-related errors (missing files, access errors) are reported on the **RuleSet** status.

## RuleSet Conditions

### Ready

The rules have been compiled, validated, and accepted for delivery over ECDS. When the `Ready` condition is `True`, the **reason** is typically `RulesAccepted`.

`Ready` only means the RuleSet content is accepted. Delivery limits are enforced when an **Engine** accepts and publishes the configuration, so a RuleSet whose content exceeds those limits is still accepted while every Engine referencing it reports `Degraded`.

### Progressing

The RuleSet is being processed. This happens when the **RuleSet** or a referenced **RuleSource** or **RuleData** changes. While waiting for **RuleSource** validation status to match the current source generation, the **reason** may be `AwaitingRuleSourceValidation` (the message lists pending source names).

### Degraded

The RuleSet could not be compiled or accepted. Common reasons:

| Reason | Description | Resolution |
|--------|-------------|------------|
| `UnsupportedRules` | The RuleSet contains rules not supported in the current execution environment. | Remove the unsupported rules, or add the annotation `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"` to the RuleSet. |
| `InvalidRuleSet` | Aggregate validation failed (including `@pmFromFile` references that are not backed by a key in merged **RuleData**). | Add or fix `spec.data` so every `@pmFromFile` basename exists in the referenced RuleData objects; fix other SecLang issues in the message. |
| `ReferencedRuleSourceInvalid` | A referenced **RuleSource** is in `Degraded` with `InvalidRules` for its current spec generation. | Fix the **RuleSource** `spec.rules` (see that object’s status message). |
| `RuleSourceNotFound` | A RuleSource named in `spec.sources` does not exist. | Create the RuleSource or correct the name; it must be in the same namespace as the RuleSet. |
| `RuleSourceAccessError` | The operator could not read a referenced RuleSource. | Check RBAC and API errors in operator logs. |
| `RuleDataNotFound` | A RuleData named in `spec.data` does not exist. | Create the RuleData or correct the name. |
| `RuleDataAccessError` | The operator could not read a referenced RuleData. | Check RBAC and API errors in operator logs. |
| `DuplicateReference` | A RuleSource or RuleData name appears more than once in `spec.sources` or `spec.data`. | Remove the duplicate reference. |

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

The operator may be waiting for referenced **RuleSource** or **RuleData** objects, or for **RuleSource** validation to complete (`AwaitingRuleSourceValidation`). Verify sources and data exist in the same namespace as the RuleSet and inspect RuleSource status:

```bash
kubectl get rulesource,ruledata -n my-namespace
kubectl describe rulesource <name> -n my-namespace
```

**Engine is Ready but traffic is not filtered**

1. Verify the Engine's target name matches the Gateway resource:
   ```bash
   kubectl get gateways -n my-namespace
   ```

2. Check that the EnvoyFilter was created:
   ```bash
   kubectl get envoyfilter -n my-namespace
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
