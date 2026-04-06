# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

A Kubernetes operator that manages Web Application Firewall (WAF) deployments using Coraza, integrated with Istio via WASM plugins. Two CRDs — **RuleSet** (aggregates SecLang rules from ConfigMaps) and **Engine** (attaches a RuleSet to a Gateway via Istio WasmPlugin).

Data flow: `ConfigMaps → RuleSetReconciler → RuleSetCache (HTTP server) → WASM plugin in Envoy → traffic filtering`

## Build, test, and lint commands

```bash
make build                  # Build manager + kubectl-coraza (runs manifests, generate, fmt, vet, lint)
make test                   # Unit tests (sets ISTIO_VERSION automatically)
make lint                   # golangci-lint with -build-tags integration
make lint.fix               # Auto-fix lint issues
make manifests generate helm.sync  # Regenerate CRDs, RBAC, and sync to Helm chart
```

Run a single unit test:
```bash
ISTIO_VERSION=1.28.2 go test -v -run TestMyFunction ./internal/controller/...
```

Verify test files compile (go build silently ignores `_test.go`):
```bash
ISTIO_VERSION=1.28.2 go test -run ^$ ./...
```

### Test tiers (require build tags and a cluster)

```bash
make test.integration       # -tags=integration, needs KIND cluster
make test.e2e               # -tags=e2e, needs KIND cluster
make test.conformance       # -tags=conformance, runs CoreRuleSet FTW tests
```

Without the build tag, `go test` silently finds zero tests.

### Cluster setup

```bash
make cluster.kind           # Create KIND cluster with Istio + MetalLB + operator
make clean.cluster.kind     # Destroy it
```

## Architecture

### Two controllers, shared cache

- **RuleSetReconciler** — watches RuleSet + referenced ConfigMaps/Secrets. Aggregates rules, validates via Coraza, checks for WASM-unsupported rules, stores in RuleSetCache.
- **EngineReconciler** — watches Engine + referenced RuleSet + Gateways + Pods. When RuleSet is ready, applies a WasmPlugin resource (server-side apply) and discovers matched Gateway pods.

Both are initialized in `internal/controller/manager.go` with a shared `RuleSetCache`.

### RuleSet cache server

In-memory versioned cache (`internal/rulesets/cache/`) with an HTTP server (port 18080). WASM plugins poll `/rules/{instance}/latest` and `/rules/{instance}/data`. Garbage-collected by TTL and size limits.

### Istio prerequisites

When `--operator-name` is set, the manager creates a ServiceEntry + DestinationRule at startup (`engine_controller_istio_prerequisites.go`) so the cache server is mesh-reachable.

### Key directories

- `api/v1alpha1/` — CRD types (Engine, RuleSet, DriverConfig)
- `internal/controller/` — reconcilers and watch setup
- `internal/rulesets/` — cache, memfs (virtual FS for rule validation), unsupported rule detection
- `cmd/manager/` — operator entry point
- `cmd/kubectl-coraza/` — CLI plugin for rule generation
- `test/framework/` — integration test helpers (Scenario pattern, port-forwarding, assertions)

## Source of truth and generation pipeline

| Change | Edit (source of truth) | Then run |
|---|---|---|
| CRD fields/validation/status | `api/**/*_types.go` | `make manifests helm.sync` |
| RBAC permissions | `+kubebuilder:rbac` markers in controllers | `make manifests helm.sync` |
| Helm templates | `charts/.../templates/*.yaml` | nothing |
| Helm values | `charts/.../values.yaml` | nothing |

**Never edit directly**: `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `charts/.../crds/*.yaml` — these are all regenerated.

## Controller patterns and conventions

### Environment: ISTIO_VERSION is always required

Unit tests download Istio CRDs at startup. Without `ISTIO_VERSION`, tests fail immediately. Always read the default from `Makefile` — never hardcode it:
```bash
grep '^ISTIO_VERSION' Makefile
```

### GenerationChangedPredicate

The Engine controller uses `predicate.GenerationChangedPredicate{}` on its primary watch. Metadata-only changes (labels, annotations, finalizers) do NOT bump `.metadata.generation`, so the update event is filtered out.

If you introduce a finalizer to a controller that uses `GenerationChangedPredicate`, the finalizer-add write won't trigger an update event. You must use `RequeueAfter` (never `Requeue`, which is deprecated) to re-enter reconciliation:
```go
return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
```
This also means unit tests will need two `Reconcile()` calls — the first to add the finalizer, the second to do the actual work. The current EngineReconciler does **not** use a finalizer, so a single `Reconcile()` call is sufficient in its tests.

### EngineReconciler in tests must set operatorNamespace

The NetworkPolicy logic uses `operatorNamespace` to determine the target namespace. Missing it silently creates resources in the wrong namespace.

### Kubernetes resource naming limits

Kubernetes naming limits depend on the resource type. Many object names use the DNS subdomain constraint and may be up to 253 characters, while some fields and name segments are limited to 63. When constructing names from user input (for example, namespace + name), validate against the specific target resource's constraint and, where needed, truncate and append a stable hash suffix.

### Watch predicates for SSA-managed resources

When watching resources the controller creates via server-side apply, filter out update events to prevent reconcile loops:
```go
predicate.And(
    predicate.NewPredicateFuncs(labelFilter),
    predicate.Funcs{
        CreateFunc:  func(event.CreateEvent) bool { return true },
        DeleteFunc:  func(event.DeleteEvent) bool { return true },
        UpdateFunc:  func(event.UpdateEvent) bool { return false },
        GenericFunc: func(event.GenericEvent) bool { return false },
    },
)
```

### TLS: HTTP/2 is disabled

The operator disables HTTP/2 via `NextProtos: []string{"http/1.1"}` to mitigate CVE-2023-44487 (HTTP/2 Rapid Reset). Preserve this when modifying TLS config in `cmd/manager/main.go`.

### RBAC changes require manifest regeneration

When adding or modifying `+kubebuilder:rbac` markers, you must regenerate and sync:
```bash
make manifests generate helm.sync
```
For cluster tests, also redeploy: `make deploy`.

## Integration test framework

### Scenario pattern (mandatory)

All integration tests must use the framework `Scenario` pattern — never reimplement port-forwarding or cleanup logic directly:
```go
s := fw.NewScenario(t)
ns := s.GenerateNamespace("my-test")
s.Step("apply manifests")
s.ApplyManifest(ns, "path/to/manifest.yaml")
s.Step("verify behavior")
```

### Key helpers
- `s.OnCleanup(fn)` — LIFO cleanup, automatic via `t.Cleanup`
- `s.ProxyToGateway(ns, name)` — HTTP testing against a gateway
- `s.ProxyToPod(ns, selector, port)` — port-forward to arbitrary pods
- `framework.DefaultTimeout` / `framework.DefaultInterval` — never hardcode durations

### Skip validation annotations
- ConfigMaps: `coraza.io/validation: "false"` — skips per-ConfigMap rule validation
- RuleSets: `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"` — prevents degrading on unsupported rules
