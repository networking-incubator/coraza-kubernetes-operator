---
title: "kubectl-coraza CLI"
linkTitle: "kubectl-coraza CLI"
weight: 25
description: "Command reference for the kubectl-coraza plugin."
---

`kubectl-coraza` is a kubectl plugin for generating Kubernetes manifests from OWASP CoreRuleSet files.

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

Generate Kubernetes ConfigMaps, a Secret, and a RuleSet resource from CoreRuleSet rule files.

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
| `--data-secret-name` | `coreruleset-data` | Name of the generated Secret for `*.data` files. |
| `--ignore-rules` | (none) | Comma-separated rule IDs to exclude from generated output. |
| `--ignore-unsupported-rules` | `wasm` | Unsupported-rule profile to exclude. Set to `none` to include all rules. |
| `--ignore-pmFromFile` | `false` | Strip SecRule lines that use the `@pmFromFile` directive. |
| `--include-test-rule` | `false` | Append the X-CRS-Test rule block to the base-rules ConfigMap. Used by conformance tests. |
| `--name-prefix` | (none) | Prefix for generated ConfigMap names. |
| `--name-suffix` | (none) | Suffix for generated ConfigMap names. |
| `--dry-run` | (none) | Set to `client` for preview output without cluster access. |
| `--skip-size-check` | `false` | Allow oversized payloads. Not recommended -- etcd may still reject large objects. |

#### Output

The command writes YAML to stdout. Each generated object is separated by `---`.

- One ConfigMap per `.conf` file, with a `rules` key containing the file content.
- One Secret of type `coraza/data` for any `.data` files found in the rules directory.
- One RuleSet resource referencing all generated ConfigMaps.

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
