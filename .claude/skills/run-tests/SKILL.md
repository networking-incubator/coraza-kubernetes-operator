---
name: run-tests
description: Run unit, integration, conformance, or e2e tests for coraza-kubernetes-operator
user_invocable: true
allowed_tools: Bash, Read, Grep, Glob
---

# Run Tests

Run the appropriate tests for the coraza-kubernetes-operator project.

## Instructions

You are about to run tests for this project. Follow these rules strictly.

### 1. Determine which tests to run

Ask yourself: what changed? Pick the narrowest test tier that covers the change.

| Tier | Build tag | What it tests | When to use |
|------|-----------|---------------|-------------|
| Unit | _(none)_ | Controller logic via envtest, utils, API types | Code-only changes (no cluster needed) |
| Integration | `integration` | Full operator in a Kubernetes/OpenShift cluster + Istio | Operator behavior, gateway proxying, metrics |
| Conformance | `conformance` | CoreRuleSet parity via go-ftw against a live gateway | Rule processing changes |
| E2E | `e2e` | End-to-end scenarios | Full-stack validation |

If the user says "run tests" without specifying, run **unit tests** first. Only escalate to integration/conformance/e2e if the change clearly requires it or the user asks.

### 2. Environment variables

**ISTIO_VERSION is always required.** The unit test suite downloads Istio CRDs at startup and will fail immediately without it.

Read `ISTIO_VERSION` from the Makefile default. Never hardcode a version—always check the Makefile first:

```bash
grep '^ISTIO_VERSION' Makefile
```

Other variables (needed only for integration/conformance/e2e):

| Variable | Default | Purpose |
|----------|---------|---------|
| `ISTIO_GATEWAY_REVISION` | `coraza` | `istio.io/rev` label on test Gateways |
| `K8S_VERSION` | _(latest GA)_ | envtest binary version for unit tests |

### 3. Running unit tests

```bash
ISTIO_VERSION=<version> go test ./internal/controller/... -count=1 -timeout 180s
```

To run a specific test:
```bash
ISTIO_VERSION=<version> go test ./internal/controller/... -run 'TestName' -count=1 -timeout 180s
```

To run all unit tests (including API types, cmd, etc.):
```bash
ISTIO_VERSION=<version> go test ./... -count=1 -timeout 180s
```

**Do NOT use `go build` or `go vet` to verify test files.** They ignore `_test.go` files. Use `go test -c` or `go test -run` instead.

### 4. Running integration, conformance, or e2e tests

These tests run against a real Kubernetes/OpenShift cluster. **Before running them, you MUST ensure the cluster has the latest code:**

#### Prerequisites

- Access to a Kubernetes or OpenShift cluster
- Istio installed (use `istioctl install --set profile=demo --set revision=coraza -y` if not installed)
- Coraza operator deployed (`make deploy`)

#### Step 1: Regenerate manifests if RBAC, CRDs, or Helm templates changed

If changes touched RBAC markers (`+kubebuilder:rbac`), CRD types (`api/`), or Helm templates:

```bash
make manifests generate    # Regenerate CRDs and RBAC from markers
make helm.sync             # Sync generated CRDs + RBAC into the Helm chart
```

#### Step 2: Redeploy the operator with updated manifests

If manifests changed (Step 1) or Go code changed:

```bash
make deploy                # Helm upgrade --install with current image + chart
```

If only minor Go code changes, a rollout restart suffices:

```bash
kubectl rollout restart deployment/coraza-kubernetes-operator -n coraza-system
kubectl rollout status deployment/coraza-kubernetes-operator -n coraza-system --timeout=60s
```

#### Step 3: Run the tests

```bash
make test.integration    # Integration tests
make test.conformance    # Conformance tests (also runs coreruleset parity check)
make test.e2e            # End-to-end tests
```

**Common failure mode:** "engine never gets ready" or resources stuck in Progressing almost always means the operator image in the cluster is stale, or RBAC/CRDs are out of sync with the code. Redeploy with `make deploy`.

### 5. Linting

```bash
make lint          # golangci-lint with -tags integration
make lint.fix      # auto-fix lint issues
make lint.api      # kube-api-linter for API types
```

### 6. Important gotchas

- **`go build ./...` does NOT compile test files.** It silently ignores `_test.go`. Always use `go test -c` or `go test -run` to verify test compilation.
- **Unit tests download Istio CRDs from GitHub** on first run. If offline or rate-limited, tests will fail at startup.
- **Integration tests use the `integration` build tag.** Running `go test ./test/integration/...` without `-tags=integration` finds zero tests.
- **Conformance tests use the `conformance` build tag.** Same pattern.
- **The test framework serializes namespace creation** to avoid Istio CA contention. Tests may appear slow to start—this is expected.
- **RBAC changes require `make manifests helm.sync deploy`**, not just an image reload. The ClusterRole/Role in the cluster must match the controller's expectations.
