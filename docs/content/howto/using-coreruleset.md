---
title: "Using the OWASP CoreRuleSet"
linkTitle: "Using the OWASP CoreRuleSet"
weight: 30
description: "Generate and deploy OWASP CoreRuleSet rules using the kubectl-coraza plugin."
---

The [OWASP CoreRuleSet (CRS)](https://coreruleset.org/) is a widely used set of attack detection rules for ModSecurity-compatible WAFs. The `kubectl-coraza` plugin can generate Kubernetes ConfigMaps and RuleSet resources from CRS rule files.

{{% alert title="Important" color="warning" %}}
This project does not provide, maintain, or support CoreRuleSet rules. Users must supply their own rules. The tools described here are provided for convenience.
{{% /alert %}}

## Install the kubectl-coraza Plugin

Build the plugin from the operator repository:

```bash
git clone https://github.com/networking-incubator/coraza-kubernetes-operator.git
cd coraza-kubernetes-operator
make build
```

This produces `bin/kubectl-coraza`. Copy it to a directory on your `PATH`:

```bash
cp bin/kubectl-coraza /usr/local/bin/
```

Verify the installation:

```bash
kubectl coraza --help
```

## Download CoreRuleSet

Download the CoreRuleSet release archive and extract the rules:

```bash
export CRS_VERSION=4.24.1
curl -fsSL "https://github.com/coreruleset/coreruleset/archive/refs/tags/v${CRS_VERSION}.tar.gz" \
  | tar xz
```

The rule files are in `coreruleset-${CRS_VERSION}/rules/`.

## Generate ConfigMaps

Use `kubectl-coraza` to generate Kubernetes manifests from the rule files:

```bash
kubectl coraza generate coreruleset \
  --rules-dir "coreruleset-${CRS_VERSION}/rules" \
  --version "${CRS_VERSION}" \
  --namespace my-namespace \
  > coreruleset-manifests.yaml
```

This produces:

- One ConfigMap per `.conf` rule file.
- A Secret (type `coraza/data`) for any `.data` files.
- A RuleSet resource referencing all generated ConfigMaps.

## Apply the Generated Rules

```bash
kubectl apply -f coreruleset-manifests.yaml
```

## Excluding Specific Rules

To exclude specific rule IDs from the generated output:

```bash
kubectl coraza generate coreruleset \
  --rules-dir "coreruleset-${CRS_VERSION}/rules" \
  --version "${CRS_VERSION}" \
  --ignore-rules 949110,980130 \
  > coreruleset-manifests.yaml
```

## Excluding WASM-Unsupported Rules

By default, `kubectl-coraza` excludes rules that are not supported in the WASM execution environment. To include all rules regardless of WASM support:

```bash
kubectl coraza generate coreruleset \
  --rules-dir "coreruleset-${CRS_VERSION}/rules" \
  --version "${CRS_VERSION}" \
  --ignore-unsupported-rules none \
  > coreruleset-manifests.yaml
```

See [Known Limitations]({{< relref "../explanation/known-limitations" >}}) for details on which rules are unsupported and why.

## Excluding @pmFromFile Rules

If you do not want to use data files, you can strip rules that use the `@pmFromFile` directive:

```bash
kubectl coraza generate coreruleset \
  --rules-dir "coreruleset-${CRS_VERSION}/rules" \
  --version "${CRS_VERSION}" \
  --ignore-pmFromFile \
  > coreruleset-manifests.yaml
```

## Customizing Names

You can set a prefix, suffix, namespace, or custom RuleSet name:

```bash
kubectl coraza generate coreruleset \
  --rules-dir "coreruleset-${CRS_VERSION}/rules" \
  --version "${CRS_VERSION}" \
  --namespace production \
  --ruleset-name crs-ruleset \
  --name-prefix crs- \
  > coreruleset-manifests.yaml
```
