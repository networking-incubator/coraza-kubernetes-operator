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
    SecRequestBodyLimit 13107200
    SecResponseBodyAccess Off
    SecAuditLogFormat JSON
    SecAuditEngine RelevantOnly
    # Enable JSON body parsing so that ARGS-based rules inspect application/json request bodies.
    # Without this, JSON request bodies are read as raw bytes and are invisible to ARGS rules.
    SecRule REQUEST_HEADERS:Content-Type "@rx ^application/json(?:\s*;|$)" \
      "id:900,phase:1,t:none,t:lowercase,pass,nolog,ctl:requestBodyProcessor=JSON"
```

{{% alert title="JSON body inspection" color="info" %}}
`SecRequestBodyAccess On` enables body reading but does **not** automatically parse JSON.
Coraza requires an explicit `requestBodyProcessor` directive per content type before `ARGS`
rules can inspect the body (see [corazawaf/coraza#438](https://github.com/corazawaf/coraza/issues/438)).
The example above adds a JSON processor — add equivalent rules for other content types
(e.g. `application/xml`) as needed.
{{% /alert %}}

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

When you change a **RuleSource** the RuleSet controller reconciles, re-compiles, and publishes the new configuration to the Gateway over ECDS.

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
    waf.k8s.coraza.io/rule-validation: "false"
spec:
  rules: |
    SecRule TX:BLOCKING_PARANOIA_LEVEL "@ge 1" ...
```

## Maximum references

A RuleSet supports up to **256** entries in `spec.sources` and up to **64** in `spec.data` (for RuleData objects; see [Using data files]({{< relref "/howto/using-data-files" >}})).

A RuleSet's content must also fit what an Engine can deliver:

- The rule text of all referenced RuleSources and the merged RuleData files (only the last-listed file of each name counts) must fit the WAF plugin's 8 MiB configuration limit as the configuration encodes them: quotes, backslashes, newlines, tabs, `<`, `>` and `&` each take two or six bytes there.
- The merged RuleData must stay within the Engine limits: `--xds-max-decoded-data-bytes` (6 MiB by default), `--xds-max-data-files` (1024 by default) and 1 MiB per file.
- Compressed for delivery, the configuration must still fit 8 MiB: text with little repetition, such as random tokens, compresses poorly.
- The RuleSet may hold at most `--rule-max-rules` rules (20000 by default, Helm `rulePolicy.maxRules`): `SecRule` (each rule of a chain), `SecAction` and `SecMarker` directives, counted once for every `Include` of a RuleData file that holds them.

A RuleSet past any of these reports `Degraded` with reason `ContentTooLarge` and a message naming the limit, and keeps the content it accepted before; Engines keep serving that content. The RuleSet generated from the OWASP CRS v4.28 (`kubectl coraza generate coreruleset`) encodes to about 0.9 MiB and holds about 700 rules.
