---
title: "Creating Firewall Rules"
linkTitle: "Creating Firewall Rules"
weight: 20
description: "Write SecLang rules in RuleSource objects and aggregate them in a RuleSet."
---

Firewall rules in the Coraza Kubernetes Operator are written using [ModSecurity SecLang](https://github.com/owasp-modsecurity/ModSecurity/wiki/Reference-Manual-(v3.x)) syntax. Rule text is stored in **RuleSource** resources; a **RuleSet** lists RuleSource (and optional RuleData) names in order and drives compilation and caching.

## Writing rules in RuleSources

Each **RuleSource** has `spec.rules`: a string containing SecLang directives (use a `|` block scalar in YAML for multiline text).

A basic RuleSource with Coraza engine configuration:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: base-rules
spec:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
    SecResponseBodyAccess Off
    SecAuditLog /dev/stdout
    SecAuditLogFormat JSON
    SecAuditEngine RelevantOnly
```

A RuleSource with a SQL injection detection rule:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: sqli-rules
spec:
  rules: |
    SecRule ARGS "@rx (?i:select.*from|union.*select|insert.*into)" \
      "id:1001,\
      phase:2,\
      deny,\
      status:403,\
      msg:'SQL Injection Detected'"
```

## Creating a RuleSet

A **RuleSet** lists RuleSource names in `spec.sources`. The operator fetches and concatenates them in list order:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: my-ruleset
spec:
  sources:
    - name: base-rules
    - name: sqli-rules
```

{{% alert title="Important" color="warning" %}}
All referenced RuleSource and RuleData objects must be in the **same namespace** as the RuleSet.
{{% /alert %}}

## Rule ordering

The order of entries in `spec.sources` matters. Rules are concatenated in that order. Place engine configuration (such as `SecRuleEngine On`) in the first RuleSource, followed by detection rules.

## Live rule updates

When you change a **RuleSource** the RuleSet controller reconciles, re-compiles, and updates the cache. Engines polling the cache pick up the new rules at their configured poll interval.

```bash
kubectl edit rulesource sqli-rules -n my-namespace
```

No restart of the operator or Engine is required.

## Rule validation

The operator compiles and validates rules when a RuleSet is reconciled. If a rule has a syntax error, the RuleSet can enter a `Degraded` state and the invalid revision is not cached. A previously cached valid revision continues to be served.

Check the RuleSet status for errors:

```bash
kubectl describe ruleset my-ruleset -n my-namespace
```

## Skipping validation for a RuleSource

To skip per-fragment Coraza validation on a **RuleSource** (for example, if its rules only make sense in the full aggregated context), set:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: dependent-rules
  annotations:
    coraza.io/validation: "false"
spec:
  rules: |
    SecRule TX:BLOCKING_PARANOIA_LEVEL "@ge 1" ...
```

## Maximum references

A RuleSet supports up to **2048** entries in `spec.sources` and up to **256** in `spec.data` (for RuleData objects; see [Using data files]({{< relref "/howto/using-data-files" >}})).
