# Development

Guide for developing the Coraza Kubernetes Operator (CKO).

> **Note**: See also: [CONTRIBUTING.md](CONTRIBUTING.md).

# Development Environment

A Kubernetes In Docker (KIND) cluster setup is provided. This will deploy
Istio (to provide a `Gateway`) and deploy the Coraza Kubernetes Operator.

> **Note**: Development and testing can be done on any Kubernetes cluster.

## Setup

Build your current changes:

```bash
make all
```

Create the cluster:

```bash
make cluster.kind
```

This will have built the operator with your current changes and loaded the
operator image into the cluster, and started the operator in the
`coraza-system` namespace.

When you make changes to the controllers and want to test them, you can just
run it again and it will rebuild, load and deploy:

```bash
make cluster.kind
```

When you're done, you can destroy the cluster with:

```bash
make clean.cluster.kind
```

# Testing

## Test Suites

### Unit Tests

Run all unit tests:

```bash
make test
```

Run unit tests with coverage:

```bash
make test.coverage
```

This generates a `coverage.out` file and displays per-package and total coverage statistics.

### Integration Tests

Run integration tests (requires a KIND cluster - see [Setup](#setup)):

```bash
make test.integration
```

These tests validate the operator's behavior on a live cluster, including:
- Engine and RuleSet reconciliation
- Gateway attachment and configuration
- Multiple gateways and engines scenarios
- Live ConfigMap mutations

Environment variables:
- `KIND_CLUSTER_NAME` - Name of the KIND cluster to use (default: `coraza-kubernetes-operator-integration`)
- `ISTIO_VERSION` - Istio version for testing (default: from Makefile)

### Conformance Tests

Run CoreRuleSet conformance tests using the [go-ftw](https://github.com/coreruleset/go-ftw) framework (often run a KIND cluster - see [Setup](#setup)):

```bash
make test.conformance
```

This will:
1. Download CoreRuleSet (version specified by `CORERULESET_VERSION`)
2. Generate ConfigMaps with test rules included
3. Deploy a test Gateway with the full CoreRuleSet
4. Run FTW (Framework for Testing WAFs) tests against it

**Environment Variables:**

- `CORERULESET_VERSION` - CoreRuleSet version to test (default: `v4.24.1`)
- `CONFORMANCE_EXTRA_FLAGS` - Additional flags passed to the test (e.g., `OUTPUT_FORMAT=github`)
- `INCLUDE_TESTS` - Regex pattern to filter which tests to run (e.g., `"920100.*"` to run only rule 920100 tests)
- `IGNORE_TEST_MANIFEST_ERRORS` - Boolean to ignore FTW test parsing errors (default: `false`)
- `OUTPUT_FORMAT` - Output format for test results: `quiet` (default), `github`, `json`
- `OUTPUT_FILE` - File path to write test output (default: stdout)

**Examples:**

```bash
# Run only tests matching a specific rule ID
CONFORMANCE_EXTRA_FLAGS='INCLUDE_TESTS="920100.*"' make test.conformance

# Run with GitHub Actions-formatted output
CONFORMANCE_EXTRA_FLAGS='OUTPUT_FORMAT=github' make test.conformance

# Run specific tests and save output to file
CONFORMANCE_EXTRA_FLAGS='INCLUDE_TESTS="920.*" OUTPUT_FILE=results.txt' make test.conformance

# Ignore test manifest parsing errors
CONFORMANCE_EXTRA_FLAGS='IGNORE_TEST_MANIFEST_ERRORS=true' make test.conformance
```

### E2E Tests

Run end-to-end tests (requires a KIND cluster - see [Setup](#setup)):

```bash
make test.e2e
```

These tests validate complete user workflows on a live cluster.

### Tools Tests

Run tests for auxiliary tools (e.g., github_project_manager):

```bash
make test.tools
```

## Integration Test Framework

The `test/framework/` package provides structured integration test utilities.
See [`test/framework/README.md`](test/framework/README.md) for the full API
reference.

A test scenario manages its own namespaces, resources, and port-forwards with
automatic cleanup:

```go
func TestExample(t *testing.T) {
    s := fw.NewScenario(t)

    s.CreateNamespace("my-test")
    s.CreateConfigMap("my-test", "rules", `SecRuleEngine On
SecRule ARGS "@contains attack" "id:1,phase:2,deny,status:403"`)
    s.CreateRuleSet("my-test", "ruleset", []string{"rules"})
    s.CreateGateway("my-test", "gateway")
    s.ExpectGatewayProgrammed("my-test", "gateway")

    s.CreateEngine("my-test", "engine", framework.EngineOpts{
        RuleSetName: "ruleset",
        GatewayName: "gateway",
    })
    s.ExpectEngineReady("my-test", "engine")

    gw := s.ProxyToGateway("my-test", "gateway")
    gw.ExpectBlocked("/?q=attack")
    gw.ExpectAllowed("/?q=safe")
}
```

Example scenarios for the v0.2.0 validation issues live in `test/integration/`:

- `coreruleset_test.go` - CoreRuleSet compatibility (#12)
- `multiple_gateways_test.go` - Multiple Gateways (#13)
- `multi_engine_gateway_test.go` - Multiple Engines + Gateways (#52)
- `reconcile_test.go` - Reconciliation of live RuleSet/ConfigMap mutations

# Working with CoreRuleSet

The project includes tools for testing and development with the OWASP CoreRuleSet.

> **Important**: This project does **not** provide, maintain or support CoreRuleSet rules.
> We support deploying and enforcing CoreRuleSet (or any SecLang-compatible rules),
> but users must supply their own rulesets. The tools below are for **testing and
> development purposes only**.

## Downloading CoreRuleSet

Download a specific version of CoreRuleSet for testing:

```bash
make coraza.coreruleset.download
```

This downloads and extracts the CoreRuleSet to `tmp/coreruleset/`.

Environment variables:
- `CORERULESET_VERSION` - Version to download (default: `v4.24.1`)

## Generating ConfigMaps

Generate Kubernetes ConfigMaps from CoreRuleSet rules for testing:

```bash
make coraza.generaterules
```

This runs `kubectl-coraza` (`go run ./cmd/kubectl-coraza generate coreruleset` via the Makefile) to:
1. Download CoreRuleSet (if not already present)
2. Process each `.conf` file in the rules directory (non-recursive, same as before)
3. Generate ConfigMaps for each rule file
4. Generate a Secret for `.data` files
5. Create a RuleSet resource referencing all ConfigMaps
6. Output everything to `tmp/rules/rules.yaml`

Conformance CI runs `make coreruleset.verify-parity`, which regenerates that manifest with `--include-test-rule` and checks `sha256sum` against `tools/corerulesetgen/testdata/coreruleset_parity.sha256`. After bumping `CORERULESET_VERSION` or changing generator output for that path, refresh the checksum with `make coreruleset.verify-parity` (it will fail until you run `sha256sum tmp/rules/rules.yaml` and replace the hash line in `tools/corerulesetgen/testdata/coreruleset_parity.sha256`).

Install the plugin for ad hoc use: build `bin/kubectl-coraza` (`make build`) and ensure it is on your `PATH` as `kubectl-coraza` so `kubectl coraza …` resolves it ([plugin discovery](https://kubernetes.io/docs/tasks/extend-kubectl/kubectl-plugins/)).

**Environment Variables:**

- `CORERULESET_EXTRA_FLAGS` - Additional flags passed through to the generator

**Generator flags** (also available as `kubectl coraza generate coreruleset …`):

- `--rules-dir` - Rules directory (required)
- `--version` - CoreRuleSet version (required); accepts `4.24.1` and `v4.24.1`
- `--ignore-rules` - Comma-separated rule IDs to exclude (e.g. `949110,949111,980130`)
- `--ignore-pmFromFile` - Strip rules containing `@pmFromFile` (not supported by Coraza WASM paths)
- `--include-test-rule` - Append the X-CRS-Test block to the bundled `base-rules` ConfigMap
- `--ruleset-name`, `--namespace` / `-n`, `--data-secret-name`, `--name-prefix`, `--name-suffix`
- `--dry-run=client` - Same stdout output; stderr notes dry-run (no cluster writes are performed in either case)
- `--skip-size-check` - Override the plugin’s approximate per-ConfigMap size guard (avoid unless necessary)

The version is normalized (leading `v` stripped) and validated before use.

**Examples:**

```bash
# Generate rules excluding specific rule IDs
make CORERULESET_EXTRA_FLAGS="--ignore-rules 949110,980130" coraza.generaterules

# Generate rules ignoring @pmFromFile directives
make CORERULESET_EXTRA_FLAGS="--ignore-pmFromFile" coraza.generaterules

# Generate rules with test rule included (used by conformance tests)
make CORERULESET_EXTRA_FLAGS="--include-test-rule" coraza.generaterules

# Combine multiple flags
make CORERULESET_EXTRA_FLAGS="--include-test-rule --ignore-pmFromFile" coraza.generaterules

# Direct invocation (stdout only)
go run ./cmd/kubectl-coraza generate coreruleset --rules-dir /path/to/coreruleset/rules --version 4.24.1
```

## Deploying CoreRuleSet for Testing

Deploy the generated CoreRuleSet to your test cluster:

```bash
make coraza.coreruleset
```

This will:
1. Generate the rules (via `make coraza.generaterules`)
2. Delete existing rules in the target namespace
3. Apply the new rules

Environment variables:
- `NAMESPACE` - Target namespace for deployment (default: `default`)

**Example:**

```bash
# Deploy to a specific namespace
make NAMESPACE=my-namespace coraza.coreruleset
```

# Building Custom WASM Plugins

The operator uses the [coraza-proxy-wasm](https://github.com/networking-incubator/coraza-proxy-wasm)
plugin, which runs inside Envoy/Istio.

This section describes how to build **the WASM plugin itself** in the separate
[`coraza-proxy-wasm`](https://github.com/networking-incubator/coraza-proxy-wasm) repository.
It does **not** change the Go version required to build this operator. For the operator,
always use the Go version declared in this repository's `go.mod`.

## Prerequisites

> **Critical**: The toolchain versions in this section apply to the
> **coraza-proxy-wasm** repository. Using versions other than those required by that
> repository may result in build failures or runtime incompatibilities for the plugin.

- **TinyGo**: `0.34.0` (plugin build requirement; see the coraza-proxy-wasm docs for updates)
- **Go toolchain**: as required by the `coraza-proxy-wasm` repo (currently Go `1.23.8`)

## Building from Source

To build a custom WASM plugin:

1. Clone the coraza-proxy-wasm repository:

   ```bash
   git clone https://github.com/networking-incubator/coraza-proxy-wasm.git
   cd coraza-proxy-wasm
   ```

2. Install TinyGo 0.34.0 (exact version required):

   ```bash
   # Follow installation instructions at https://tinygo.org/getting-started/install/
   # Ensure you have TinyGo 0.34.0 - no other version will work
   tinygo version  # Must show: tinygo version 0.34.0
   ```

3. Build the WASM module using the Go version required by the `coraza-proxy-wasm`
   repository (see its `go.mod`; at the time of writing this is Go `1.23.8`):

   ```bash
   GOTOOLCHAIN=go1.23.8 go run mage.go build
   ```

   This generates the WASM binary in the build directory.

4. Build the Docker image with your custom tag:

   ```bash
   docker build -t ghcr.io/YOUR_ORG/coraza-proxy-wasm:custom-tag .
   ```

5. Authenticate with GitHub Container Registry (GHCR):

   ```bash
   # Create a GitHub Personal Access Token (PAT) with 'write:packages' scope at:
   # https://github.com/settings/tokens

   # Login to GHCR
   echo $GITHUB_TOKEN | docker login ghcr.io -u YOUR_GITHUB_USERNAME --password-stdin
   ```

6. Push the image to GHCR:

   ```bash
   docker push ghcr.io/YOUR_ORG/coraza-proxy-wasm:custom-tag
   ```

7. Update your Engine resource to use the custom image:

   Edit `config/samples/engine.yaml` (or your Engine manifest):

   ```yaml
   apiVersion: waf.k8s.coraza.io/v1alpha1
   kind: Engine
   metadata:
     name: coraza
   spec:
     ruleSet:
       name: default-ruleset
     failurePolicy: fail
     driver:
       istio:
         wasm:
           image: "oci://ghcr.io/YOUR_ORG/coraza-proxy-wasm:custom-tag"
           mode: gateway
           workloadSelector:
             matchLabels:
               gateway.networking.k8s.io/gateway-name: coraza-gateway
           ruleSetCacheServer:
             pollIntervalSeconds: 5
   ```

8. Apply the updated Engine to your cluster:

   ```bash
   kubectl apply -f config/samples/engine.yaml
   ```

## Testing Custom WASM Plugins

You can override the WASM image used by tests:

```bash
# Set the image for integration/conformance tests
export CORAZA_WASM_IMAGE="oci://ghcr.io/YOUR_ORG/coraza-proxy-wasm:custom-tag"

# Run tests with your custom image
make test.integration
# or
make test.conformance
```

# Additional Development Workflows

## Linting

Run the Go linter:

```bash
make lint
```

Auto-fix linting issues:

```bash
make lint.fix
```

Verify linter configuration:

```bash
make lint.config
```

## Source of Truth & Generation Pipeline

The project has several layers of generated artifacts. Understanding what is
the source of truth for each layer avoids editing the wrong file.

```
  Go types (api/)
       │
       │  controller-gen
       ▼
  config/crd/bases/*.yaml   CRDs
  config/rbac/role.yaml     ClusterRole
       │
       │  helm.sync (make helm.sync)
       ▼
  charts/.../crds/          copied from config/crd/bases/
  charts/.../clusterrole    rules injected from config/rbac/role.yaml
       │
       │  generate_bundle.py (make bundle)
       ▼
  bundle/manifests/         CSV, CRDs, extra manifests
  bundle/metadata/          OLM annotations
  bundle/bundle.Dockerfile
       │
       │  docker build (make bundle.build)
       ▼
  Bundle image              pushed to registry
       │
       │  opm render (inside catalog docker build)
       ▼
  Catalog image             serves rendered bundles to OLM
```

| What you want to change | Edit this (source of truth) | Then run |
|---|---|---|
| CRD fields, validation, status | `api/**/*_types.go` | `make manifests` then `make helm.sync` |
| RBAC permissions | `//+kubebuilder:rbac` markers in controllers | `make manifests` then `make helm.sync` |
| Helm chart templates (Deployment, Service, etc.) | `charts/.../templates/*.yaml` | nothing — these are the source |
| Helm chart values | `charts/.../values.yaml` | nothing — this is the source |
| OLM CSV metadata (description, icon, links) | `bundle/base/csv-template.yaml` | `make bundle` |
| OperatorHub `ci.yaml` (reviewers, update graph mode) | `bundle/base/ci.yaml` | `make bundle` |
| OLM catalog channel/versions | `catalog/.../catalog.yaml` | `make catalog.update` to add a version |

**Do not edit directly** (these are regenerated):
- `config/crd/bases/*.yaml` — regenerated by `controller-gen`
- `config/rbac/role.yaml` — regenerated by `controller-gen`
- `charts/.../crds/*.yaml` — copied from `config/crd/bases/`
- `bundle/manifests/` — regenerated by `generate_bundle.py`
- `bundle/ci.yaml` — copy of `bundle/base/ci.yaml` produced by `make bundle`

## Code Generation

Generate code from CRDs and controllers:

```bash
make generate
```

Generate Kubernetes manifests (CRDs, RBAC, etc.):

```bash
make manifests
```

## Helm Chart

Lint the Helm chart:

```bash
make helm.lint
```

Render Helm templates locally:

```bash
make helm.template
```

Sync generated CRDs to the Helm chart:

```bash
make helm.sync-crds
```

Sync all generated resources (CRDs + RBAC):

```bash
make helm.sync
```

## Environment Variables Reference

| Variable | Description | Default |
|----------|-------------|---------|
| `VERSION` | Version tag for builds | `dev` |
| `IMAGE_REGISTRY` | Registry and org prefix for all images | `ghcr.io/networking-incubator` |
| `CONTROLLER_MANAGER_CONTAINER_IMAGE` | Full operator image reference | `$(IMAGE_REGISTRY)/coraza-kubernetes-operator:v0.0.0-dev` |
| `KIND_CLUSTER_NAME` | KIND cluster name for tests | `coraza-kubernetes-operator-integration` |
| `ISTIO_VERSION` | Istio version for deployment | `1.28.2` |
| `METALLB_VERSION` | MetalLB version for KIND | `0.15.3` |
| `CORERULESET_VERSION` | CoreRuleSet version | `v4.24.1` |
| `CORAZA_WASM_IMAGE` | WASM plugin image for tests | (See `test/framework/resources.go`) |
| `NAMESPACE` | Target namespace for deployments | `default` |

# Releasing

See [RELEASE.md](RELEASE.md).
