---
title: "Using Data Files with Rules"
linkTitle: "Using Data Files"
weight: 35
description: "Supply external data files for rules that use the @pmFromFile directive."
---

Some SecLang rules use the `@pmFromFile` directive to match against patterns stored in external data files. The Coraza Kubernetes Operator provides these files from **RuleData** resources, referenced by the **RuleSet** `spec.data` list.

## When to use data files

Use data files when your rules reference `@pmFromFile`. For example:

```
SecRule ARGS "@pmFromFile bad-patterns.data" \
  "id:3001,phase:2,deny,status:403,msg:'Blocked pattern detected'"
```

This rule reads patterns from a file named `bad-patterns.data`. Store that file in a **RuleData** `spec.files` map (filename → content).

## Creating a RuleData object

A **RuleData** holds one or more files in `spec.files`. Each key is a filename referenced in `@pmFromFile`; the value is the file body:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleData
metadata:
  name: rule-data
spec:
  files:
    bad-patterns.data: |
      malicious-pattern-one
      malicious-pattern-two
      malicious-pattern-three
```

Each line in the data file is treated as a separate pattern by the `@pm` operator (see Coraza / SecLang semantics for your rule).

## Referencing RuleData in a RuleSet

List RuleData object names in `spec.data` (same namespace as the RuleSet):

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: my-ruleset
spec:
  sources:
    - name: base-rules
    - name: pattern-rules
  data:
    - name: rule-data
```

If you reference **several** RuleData objects, their `spec.files` entries are **merged in list order; when the same filename appears in more than one object, a later list entry overwrites the earlier one** (last listed wins for duplicate keys).

## Complete example

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: base-rules
spec:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
---
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: pattern-rules
spec:
  rules: |
    SecRule ARGS "@pmFromFile bad-patterns.data" \
      "id:3001,\
      phase:2,\
      deny,\
      status:403,\
      msg:'Blocked pattern detected'"
---
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleData
metadata:
  name: rule-data
spec:
  files:
    bad-patterns.data: |
      evildata
      anotherevildata
---
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: my-ruleset
spec:
  sources:
    - name: base-rules
    - name: pattern-rules
  data:
    - name: rule-data
```

## Updating data files

When you change a **RuleData** (or a **RuleSet** that references it), the RuleSet controller re-compiles with the new data and updates the cache. Engines pick up changes at their next poll interval.

{{% alert title="Note" color="info" %}}
You can list **up to 256** `spec.data` entries. Put multiple named files in one RuleData, or split them across several RuleData objects; remember that duplicate filenames are resolved with **last-listed** RuleData winning.
{{% /alert %}}
