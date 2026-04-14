---
title: "Creating Firewall Rules"
linkTitle: "Creating Firewall Rules"
weight: 20
description: "Write SecLang rules in ConfigMaps and aggregate them into a RuleSet."
---

Firewall rules in the Coraza Kubernetes Operator are written using [ModSecurity SecLang](https://github.com/owasp-modsecurity/ModSecurity/wiki/Reference-Manual-(v3.x)) syntax, stored in ConfigMaps, and aggregated by a RuleSet resource.

## Writing Rules in ConfigMaps

Each ConfigMap must contain a key named `rules` with SecLang directives as its value.

A basic ConfigMap with Coraza engine configuration:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: base-rules
data:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
    SecResponseBodyAccess Off
    SecAuditLog /dev/stdout
    SecAuditLogFormat JSON
    SecAuditEngine RelevantOnly
```

A ConfigMap with a SQL injection detection rule:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sqli-rules
data:
  rules: |
    SecRule ARGS "@rx (?i:select.*from|union.*select|insert.*into)" \
      "id:1001,\
      phase:2,\
      deny,\
      status:403,\
      msg:'SQL Injection Detected'"
```

## Creating a RuleSet

A RuleSet references one or more ConfigMaps. The ConfigMaps are processed in the order they are listed:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: my-ruleset
spec:
  rules:
    - name: base-rules
    - name: sqli-rules
```

{{% alert title="Important" color="warning" %}}
All ConfigMaps must be in the same namespace as the RuleSet.
{{% /alert %}}

## Rule Ordering

The order of ConfigMaps in the `rules` list matters. Rules are loaded sequentially. Place engine configuration (such as `SecRuleEngine On`) in the first ConfigMap, followed by detection rules.

## Live Rule Updates

When you update a ConfigMap, the RuleSet controller automatically detects the change, re-compiles the rules, and updates the cache. Engines polling the cache will pick up the new rules at their configured poll interval.

```bash
kubectl edit configmap sqli-rules -n my-namespace
```

No restart of the operator or Engine is required.

## Rule Validation

The operator compiles and validates all rules when a RuleSet is reconciled. If a rule has a syntax error, the RuleSet will enter a `Degraded` state and the invalid revision will not be cached. Any previously cached valid revision continues to be served.

Check the RuleSet status for validation errors:

```bash
kubectl describe ruleset my-ruleset -n my-namespace
```

## Skipping Validation for ConfigMaps

To skip per-ConfigMap rule validation (for example, if a ConfigMap contains rules that depend on directives from another ConfigMap), add the following annotation:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dependent-rules
  annotations:
    coraza.io/validation: "false"
data:
  rules: |
    SecRule TX:BLOCKING_PARANOIA_LEVEL "@ge 1" ...
```

## Maximum Rules

A RuleSet supports up to **2048** ConfigMap references.
