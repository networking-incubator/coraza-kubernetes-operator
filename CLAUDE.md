# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

A Kubernetes operator that manages Web Application Firewall (WAF) deployments using Coraza, integrated with Istio via WASM plugins. CRDs include **RuleSource** (stores SecLang rule text), **RuleData** (stores data files for `@pmFromFile`), **RuleSet** (ordered `spec.sources` listing RuleSource names + optional `spec.data` listing RuleData names), and **Engine** (attaches a RuleSet to a Gateway by rendering an EnvoyFilter and serving the artifact over xDS/ECDS).

Data flow: `RuleSource (validated by RuleSourceReconciler) + RuleData + RuleSet -> RuleSetReconciler -> rendered artifact -> PublicationReconciler -> xDS ECDS snapshot -> WASM plugin in Envoy -> traffic filtering`

## Build, test, and lint commands

```bash
make build                  # Build manager + kubectl-coraza (runs manifests, generate, fmt, vet, lint)
make test                   # Unit tests (sets ISTIO_VERSION automatically)
make lint                   # golangci-lint with -build-tags integration
make lint.fix               # Auto-fix lint issues
make manifests generate helm.sync  # Regenerate CRDs, RBAC, and sync to Helm chart
```

Run a single unit test (read the default `ISTIO_VERSION` from `Makefile`, never hardcode it):
```bash
ISTIO_VERSION=$(grep '^ISTIO_VERSION' Makefile | cut -d' ' -f3) go test -v -run TestMyFunction ./internal/controller/...
```

Verify test files compile (go build silently ignores `_test.go`):
```bash
ISTIO_VERSION=$(grep '^ISTIO_VERSION' Makefile | cut -d' ' -f3) go test -run ^$ ./...
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
make cluster.kind.otel      # Same, plus OpenTelemetry Operator (central collector CRDs)
make clean.cluster.kind     # Destroy it
```

## Architecture

### Controllers and shared cache

- **RuleSourceReconciler** - watches `RuleSource` (spec generation and validation annotation). Validates `spec.rules` with Coraza per fragment (skips patch-only fragments that contain only `SecRuleUpdate*` / `SecRuleRemove*`; without RuleSet-scoped RuleData; aggregate validation on the RuleSet remains authoritative for `@pmFromFile`) and patches **RuleSource** status (`Validated`, `InvalidRules`, or `ValidationSkipped`).
- **RuleSetReconciler** - watches RuleSet + referenced RuleSources + referenced RuleData. Gates on **RuleSource** status before aggregating; merges rules and RuleData, runs aggregate Coraza validation, checks for WASM-unsupported rules, stores in RuleSetCache.
- **EngineReconciler** - watches Engine + referenced RuleSet + Gateways + Pods. When the RuleSet is ready, renders the artifact, records the accepted configuration in Engine status, and applies an EnvoyFilter (server-side apply) that configures the WASM filter and its ECDS config source. Dataplane metrics use platform-owned central Istio ALS collectors (not per-Engine OTC sidecars).
- **PublicationReconciler** - watches Engine, RuleSet, RuleSource, RuleData, Gateway and Pods. On every replica it reconstructs the accepted Engine configuration, publishes the artifact to the local xDS snapshot store, and authorizes Gateway pod ServiceAccounts to subscribe. It never writes status.

The RuleSet reconciler is initialized in `internal/controller/manager.go` with a shared `RuleSetCache` that holds validation artifacts; the RuleSource and Engine reconcilers share the same manager client.

### xDS / ECDS delivery

The operator serves ECDS over its own listener (`internal/xds/`): EnvoyFilter-generated WASM filters subscribe to `coraza.waf.<namespace>.<engine>.<uid>` and receive the rendered runtime configuration, while the raw WASM binary is fetched from `https://<operator-service>:<xds-port>/wasm/<sha256>.wasm` (port defaults to 18080). The listener terminates TLS 1.3 and intentionally negotiates both HTTP/2 (gRPC/ECDS and asset fetches) and HTTP/1.1 (asset fetches). Subscribers are authorized by a ServiceAccount bearer token validated through TokenReview and by matching Gateway pod ServiceAccounts.

### Legacy RuleSet cache server

`internal/rulesets/cache/` still contains the versioned in-memory cache and its HTTP server, and the `--cache-server-port` flag still exists, but the server is **not started** and WASM plugins no longer poll `/rules/...`. It is retained pending removal; do not build new features on it.

### Key directories

- `api/v1alpha1/` - CRD types (Engine, RuleSet, RuleSource, RuleData, DriverConfig)
- `internal/controller/` - reconcilers and watch setup
- `internal/rulesets/` - cache, memfs (virtual FS for rule validation), unsupported rule detection, `validation/` (shared Coraza helpers for rule text)
- `cmd/manager/` - operator entry point
- `cmd/kubectl-coraza/` - CLI plugin for rule generation
- `test/framework/` - integration test helpers (Scenario pattern, port-forwarding, assertions)

## Source of truth and generation pipeline

| Change | Edit (source of truth) | Then run |
|---|---|---|
| CRD fields/validation/status | `api/**/*_types.go` | `make manifests helm.sync` |
| RBAC permissions | `+kubebuilder:rbac` markers in controllers | `make manifests helm.sync` |
| Helm templates | `charts/.../templates/*.yaml` | nothing |
| Helm values | `charts/.../values.yaml` | nothing |

**Never edit directly**: `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `charts/.../crds/*.yaml` - these are all regenerated.

## Controller patterns and conventions

### Environment: ISTIO_VERSION is always required

Unit tests download Istio CRDs at startup. Without `ISTIO_VERSION`, tests fail immediately. Always read the default from `Makefile` - never hardcode it:
```bash
grep '^ISTIO_VERSION' Makefile
```

### GenerationChangedPredicate

The Engine controller uses `predicate.GenerationChangedPredicate{}` on its primary watch and on the Gateway watch. Metadata-only changes (labels, annotations, finalizers) do NOT bump `.metadata.generation`, so the update event is filtered out.

If you introduce a finalizer to a controller that uses `GenerationChangedPredicate`, the finalizer-add write won't trigger an update event. You must use `RequeueAfter` (never `Requeue`, which is deprecated) to re-enter reconciliation:
```go
return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
```
This also means unit tests will need two `Reconcile()` calls - the first to add the finalizer, the second to do the actual work. The current EngineReconciler does **not** use a finalizer, so a single `Reconcile()` call is sufficient in its tests.

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

### TLS: two listeners with different rules

The manager's metrics server disables HTTP/2 via `NextProtos: []string{"http/1.1"}` to mitigate CVE-2023-44487 (HTTP/2 Rapid Reset). Preserve that when modifying TLS in `cmd/manager/main.go`; the health-probe server is separate and plaintext.

The xDS listener (`internal/xds/listener.go`) is the opposite: ECDS requires HTTP/2, so it negotiates `h2` and `http/1.1` and caps concurrent streams (`h2MaxConcurrentStreams`) to bound Rapid Reset exposure. Do not apply the metrics-server ALPN override there.

### RBAC changes require manifest regeneration

When adding or modifying `+kubebuilder:rbac` markers, you must regenerate and sync:
```bash
make manifests generate helm.sync
```
For cluster tests, also redeploy: `make deploy`.

## Integration test framework

### Scenario pattern (mandatory)

All integration tests must use the framework `Scenario` pattern - never reimplement port-forwarding or cleanup logic directly:
```go
s := fw.NewScenario(t)
ns := s.GenerateNamespace("my-test")
s.Step("apply manifests")
s.ApplyManifest(ns, "path/to/manifest.yaml")
s.Step("verify behavior")
```

### Key helpers
- `s.OnCleanup(fn)` - LIFO cleanup, automatic via `t.Cleanup`
- `s.ProxyToGateway(ns, name)` - HTTP testing against a gateway
- `s.ProxyToPod(ns, selector, port)` - port-forward to arbitrary pods
- `framework.DefaultTimeout` / `framework.DefaultInterval` - never hardcode durations

### Skip validation annotations
- RuleSources: `waf.k8s.coraza.io/rule-validation: "false"` - skips per-source Coraza rule validation (also emitted by `kubectl coraza generate coreruleset --skip-validation-rulesource`); patch-only fragments (`SecRuleUpdate*` / `SecRuleRemove*` only) are accepted without this annotation via `IsPatchOnlyFragment` in validation
- RuleSets: `waf.k8s.coraza.io/skip-unsupported-rules-check: "true"` - prevents degrading on unsupported rules
