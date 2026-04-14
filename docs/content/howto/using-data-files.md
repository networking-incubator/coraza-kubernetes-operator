---
title: "Using Data Files with Rules"
linkTitle: "Using Data Files"
weight: 35
description: "Supply external data files for rules that use the @pmFromFile directive."
---

Some SecLang rules use the `@pmFromFile` directive to match against patterns stored in external data files. The Coraza Kubernetes Operator supports this through Secrets of type `coraza/data`.

## When to Use Data Files

Use data files when your rules reference `@pmFromFile`. For example:

```
SecRule ARGS "@pmFromFile bad-patterns.data" \
  "id:3001,phase:2,deny,status:403,msg:'Blocked pattern detected'"
```

This rule reads patterns from a file named `bad-patterns.data`. To make this file available to the operator, store it in a Secret.

## Creating a Data Secret

Create a Secret with type `coraza/data`. Each key in the Secret corresponds to a filename referenced by `@pmFromFile`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: rule-data
type: coraza/data
stringData:
  bad-patterns.data: |
    malicious-pattern-one
    malicious-pattern-two
    malicious-pattern-three
```

Each line in the data file is treated as a separate pattern by the `@pm` operator.

## Referencing the Secret in a RuleSet

Set the `ruleData` field on the RuleSet to the name of the Secret:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: my-ruleset
spec:
  rules:
    - name: base-rules
    - name: pattern-rules
  ruleData: rule-data
```

The Secret must be in the same namespace as the RuleSet.

## Complete Example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: base-rules
data:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: pattern-rules
data:
  rules: |
    SecRule ARGS "@pmFromFile bad-patterns.data" \
      "id:3001,\
      phase:2,\
      deny,\
      status:403,\
      msg:'Blocked pattern detected'"
---
apiVersion: v1
kind: Secret
metadata:
  name: rule-data
type: coraza/data
stringData:
  bad-patterns.data: |
    evildata
    anotherevildata
---
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: my-ruleset
spec:
  rules:
    - name: base-rules
    - name: pattern-rules
  ruleData: rule-data
```

## Updating Data Files

When you update the Secret, the RuleSet controller re-compiles the rules with the new data and updates the cache. Engines pick up the changes at their next poll interval.

{{% alert title="Note" color="info" %}}
A RuleSet can reference at most one data Secret. If you need multiple data files, include all of them as separate keys in a single Secret.
{{% /alert %}}
