# kubectl-coraza

A [kubectl plugin](https://kubernetes.io/docs/tasks/extend-kubectl/kubectl-plugins/) that generates **RuleSource** (rule text), **RuleData** (data files), and **RuleSet** manifests from OWASP CoreRuleSet files on disk.

> The operator validates and compiles rules after you apply them; this tool does not compile Coraza rules.

## Install

Place the `kubectl-coraza` binary on your `PATH`, then verify:

```bash
kubectl coraza --version
```

## Usage

```bash
kubectl coraza generate coreruleset \
  --rules-dir /path/to/rules \
  --version 4.24.1 \
  [--namespace my-ns] \
  [--ruleset-name default-ruleset] \
  [flags...]
```

Reads `*.conf` and optional `*.data` from a single directory (non-recursive). Outputs multi-document YAML to stdout; progress and warnings go to stderr.

### Flags

| Flag | Description |
|------|-------------|
| `--rules-dir` | Directory with `*.conf` / `*.data` (required) |
| `--version` | CRS version, e.g. `4.24.1` (required) |
| `-n`, `--namespace` | Set `metadata.namespace` on every object |
| `--ruleset-name` | RuleSet name (default `default-ruleset`) |
| `--data-source-name` | `RuleData` name for `*.data` files (default `coreruleset-data`) |
| `--ignore-rules` | Comma-separated rule IDs to drop |
| `--ignore-unsupported-rules` | Unsupported-rule profile to exclude (default `wasm`); set to `none` to emit full CRS (see `LIMITATIONS.md`) |
| `--ignore-pmFromFile` | Strip `SecRule` lines using `@pmFromFile` |
| `--include-test-rule` | Append X-CRS-Test block to `base-rules` |
| `--name-prefix` / `--name-suffix` | Prefix/suffix for `RuleSource` names derived from `*.conf` filenames |
| `--dry-run=client` | Preview output without cluster access |
| `--skip-size-check` | Allow oversized payloads (etcd may still reject) |

## Library

Generation logic lives in [`../../tools/corerulesetgen`](../../tools/corerulesetgen) and can be used directly without the kubectl wrapper.

```go
ver, _ := corerulesetgen.ParseCRSVersion("4.24.1")
scan, _ := corerulesetgen.Scan("/path/to/rules")
bundle, _ := corerulesetgen.Build(corerulesetgen.Options{
    RulesDir:    "/path/to/rules",
    Version:     "4.24.1",
    RuleSetName: "my-ruleset",
}, scan, ver)
corerulesetgen.WriteManifests(os.Stdout, bundle)
```

Tests: `go test ./tools/corerulesetgen/...` — golden fixtures in [`../../tools/corerulesetgen/testdata`](../../tools/corerulesetgen/testdata).
