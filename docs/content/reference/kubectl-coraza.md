---
title: "kubectl-coraza CLI"
linkTitle: "kubectl-coraza CLI"
weight: 25
description: "Command reference for the kubectl-coraza plugin."
---

`kubectl-coraza` is a kubectl plugin for generating Kubernetes manifests (RuleSource, RuleData, RuleSet) from OWASP CoreRuleSet files.

> The operator validates and compiles rules after you apply manifests; this tool does not compile Coraza rules.

## Installation

Build from source:

```bash
git clone https://github.com/networking-incubator/coraza-kubernetes-operator.git
cd coraza-kubernetes-operator
make build
```

Copy `bin/kubectl-coraza` to a directory on your `PATH`:

```bash
cp bin/kubectl-coraza /usr/local/bin/
```

Once installed, the plugin is available as `kubectl coraza`.

## Commands

### `kubectl coraza generate coreruleset`

Generate **RuleSource** resources (one per `*.conf` file), an optional **RuleData** resource for `*.data` files, and a **RuleSet** that references them.

#### Required Flags

| Flag | Description |
|------|-------------|
| `--rules-dir` | Directory containing CoreRuleSet `*.conf` and optional `*.data` files. The directory is not traversed recursively. |
| `--version` | CoreRuleSet version (e.g., `4.24.1` or `v4.24.1`). The leading `v` is normalized automatically. |

#### Optional Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-n`, `--namespace` | (none) | Set `metadata.namespace` on all generated objects. |
| `--ruleset-name` | `default-ruleset` | Name of the generated RuleSet resource. |
| `--data-source-name` | `coreruleset-data` | Name of the generated RuleData object for `*.data` files. |
| `--ignore-rules` | (none) | Comma-separated rule IDs to exclude from generated output. |
| `--ignore-unsupported-rules` | `wasm` | Unsupported-rule profile to exclude. Set to `none` to include all rules (see [Known Limitations]({{< relref "../explanation/known-limitations" >}})). |
| `--ignore-pmFromFile` | `false` | Strip `SecRule` lines that use the `@pmFromFile` directive. |
| `--include-test-rule` | `false` | Append the X-CRS-Test rule block to the `base-rules` RuleSource. Used by conformance tests. |
| `--name-prefix` | (none) | Prefix for RuleSource names derived from `*.conf` filenames (not `base-rules`). |
| `--name-suffix` | (none) | Suffix for RuleSource names derived from `*.conf` filenames. |
| `--dry-run` | (none) | Set to `client` for preview output without cluster access. |
| `--skip-size-check` | `false` | Allow oversized payloads. Not recommended -- etcd may still reject large objects. |

#### Output

The command writes a multi-document YAML stream to **stdout**; progress and warnings go to **stderr**. Each object is separated by `---`.

- One **RuleSource** per `*.conf` file, with `spec.rules` set to the file content.
- At most one **RuleData** with `spec.files` mapping each data filename to its content, if any `*.data` files are present.
- One **RuleSet** with `spec.sources` listing the generated RuleSource names in order, and `spec.data` referencing the RuleData when data files exist.

#### Examples

Generate rules with default settings:

```bash
kubectl coraza generate coreruleset \
  --rules-dir /path/to/coreruleset/rules \
  --version 4.24.1
```

Generate rules for a specific namespace, excluding certain rule IDs:

```bash
kubectl coraza generate coreruleset \
  --rules-dir /path/to/coreruleset/rules \
  --version 4.24.1 \
  --namespace production \
  --ignore-rules 949110,980130
```

Generate rules without `@pmFromFile` directives:

```bash
kubectl coraza generate coreruleset \
  --rules-dir /path/to/coreruleset/rules \
  --version 4.24.1 \
  --ignore-pmFromFile
```

Preview output without applying:

```bash
kubectl coraza generate coreruleset \
  --rules-dir /path/to/coreruleset/rules \
  --version 4.24.1 \
  --dry-run=client
```
