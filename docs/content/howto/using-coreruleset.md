---
title: "Using the OWASP CoreRuleSet"
linkTitle: "Using the OWASP CoreRuleSet"
weight: 30
description: "Generate and deploy OWASP CoreRuleSet rules using the kubectl-coraza plugin."
---

The [OWASP CoreRuleSet (CRS)](https://coreruleset.org/) is a widely used set of attack detection rules for ModSecurity-compatible WAFs. The `kubectl-coraza` plugin can generate **RuleSource**, **RuleData**, and **RuleSet** manifests from CRS rule files.

{{% alert title="Important" color="warning" %}}
This project does not maintain CoreRuleSet rules. A CRS-enabled `envoy-coraza` build embeds an upstream CRS release and rule behavior follows that release. The default and release bundles do **not** embed CRS, so `crs/...` includes are rejected there.
{{% /alert %}}

## Use the embedded CoreRuleSet

`make wasm.crs` (used by `make demo`) builds `envoy-coraza` with the CRS release pinned by the operator (`v4.30.0`; recorded as `crs_version` in `plugin.manifest.yaml`). The default and release builds do **not** embed CRS, so `crs/...` includes require a CRS-enabled module: the operator rejects a RuleSet that uses them while the bundled module reports `crs_version: none`. Reference the embedded tree directly:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: crs
  namespace: my-namespace
spec:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
    SecRequestBodyLimit 13107200
    # CRS 4.30 parses a JSON body only when the request selects the JSON
    # processor; without this rule the ARGS rules never see JSON payloads.
    SecRule REQUEST_HEADERS:Content-Type "@rx application/json" \
      "id:900,phase:1,pass,nolog,ctl:requestBodyProcessor=JSON"
    Include crs/crs-setup.conf
    Include crs/rules/*.conf
---
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: crs
  namespace: my-namespace
spec:
  sources:
    - name: crs
```

Point the Engine at the RuleSet:

```yaml
spec:
  ruleSet:
    name: crs
```

`crs/crs-setup.conf`, `crs/rules/*.conf`, a literal `crs/rules/<file>.conf`, and `@pmFromFile crs/...` resolve inside the module. They are not RuleData files: `crs/` paths cannot be supplied through `spec.data`.

Verify enforcement:

```bash
kubectl -n my-namespace wait --for=condition=Ready ruleset/crs --timeout=300s

ADDR=$(kubectl -n my-namespace get gateway <gateway-name> \
  -o jsonpath='{.status.addresses[0].value}')

# normal request
curl -s -o /dev/null -w '%{http_code}\n' "http://$ADDR/"

# SQLi probe; expect 403
curl -s -o /dev/null -w '%{http_code}\n' \
  "http://$ADDR/?q=1%27%20OR%20%271%27=%271"
```

The previous Go plugin's aliases (`@owasp_crs/...`, `@recommended-conf`, `@demo-conf`, `@ftw-conf`, `@crs-setup-conf`) are not supported. Rewrite them to `crs/...`.

## Generate manifests for a different CRS release

Use the generator when you need a different CRS version or per-rule exclusions. To match the embedded release, set `--version 4.30.0`.

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
export CRS_VERSION=4.28.0
curl -fsSL "https://github.com/coreruleset/coreruleset/archive/refs/tags/v${CRS_VERSION}.tar.gz" \
  | tar xz
```

The rule files are in `coreruleset-${CRS_VERSION}/rules/`.

## Generate manifests

Use `kubectl coraza generate coreruleset` to emit a multi-document YAML stream (stdout) from the rule files:

```bash
kubectl coraza generate coreruleset \
  --rules-dir "coreruleset-${CRS_VERSION}/rules" \
  --version "${CRS_VERSION}" \
  --namespace my-namespace \
  > coreruleset-manifests.yaml
```

This typically produces:

- One **RuleSource** per `*.conf` file (`spec.rules` holds the file text).
- At most one **RuleData** (`spec.files` maps each data filename to content) if the directory contains `*.data` files. Its name is controlled by `--data-source-name` (default `coreruleset-data`).
- A **RuleSet** with `spec.sources` (and `spec.data` when RuleData is emitted) wired to those names in order.

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
