# Integration Test Framework

Test framework for writing structured integration tests against the Coraza
Kubernetes Operator. Handles cluster connection, resource lifecycle, status
polling, and HTTP traffic verification through Gateway port-forwarding.

## Design

The framework has three layers:

- **Framework** - connects to the cluster (kind or generic kubeconfig).
  One instance per test suite, created in `TestMain`.
- **Scenario** - manages a single test's lifecycle: namespace creation,
  resource creation, and cleanup (all in reverse order via `t.Cleanup`).
- **GatewayProxy** - port-forwards to a Gateway pod and provides HTTP
  assertion helpers for verifying WAF behavior.

## Environment

| Variable | Purpose |
|---|---|
| `KIND_CLUSTER_NAME` | Connects to a kind cluster via `kind get kubeconfig` |
| `KUBECONFIG` | Fallback: connects via standard kubeconfig |
| `CORAZA_WASM_IMAGE` | Override the default WASM plugin OCI image |
| `ECHO_IMAGE` | Override the default echo backend image |
| `ARTIFACTS_DIR` | If set, write diagnostic dumps (YAML, logs, events) to this directory on test failure |

## Quick Start

```go
// In suite_test.go (one per package):
var fw *framework.Framework

func TestMain(m *testing.M) {
    var err error
    fw, err = framework.New()
    if err != nil {
        panic(fmt.Sprintf("failed to initialize test framework: %v", err))
    }
    os.Exit(m.Run())
}

// In a test file:
func TestMyScenario(t *testing.T) {
    t.Parallel()
    s := fw.NewScenario(t)

    ns := s.GenerateNamespace("my-test")

    s.Step("create WAF resources")
    s.CreateConfigMap(ns, "rules", `SecRuleEngine On
SecRule ARGS "@contains attack" "id:1,phase:2,deny,status:403"`)
    s.CreateRuleSet(ns, "ruleset", []string{"rules"})
    s.CreateGateway(ns, "my-gateway")
    s.ExpectGatewayProgrammed(ns, "my-gateway")

    s.CreateEngine(ns, "my-engine", framework.EngineOpts{
        RuleSetName: "ruleset",
        GatewayName: "my-gateway",
    })
    s.ExpectEngineReady(ns, "my-engine")

    s.Step("deploy backend and verify WAF enforcement")
    s.CreateEchoBackend(ns, "echo")
    s.CreateHTTPRoute(ns, "echo-route", "my-gateway", "echo")

    gw := s.ProxyToGateway(ns, "my-gateway")
    gw.ExpectBlocked("/?q=attack")
    gw.ExpectAllowed("/?q=safe")
}
```

## API Reference

### Scenario - Resource Creation

| Method | Purpose |
|---|---|
| `GenerateNamespace(prefix)` | Create namespace with random suffix, returns generated name |
| `CreateNamespace(name)` | Create namespace with exact name and cleanup |
| `CreateConfigMap(ns, name, rules)` | Create ConfigMap with WAF rules |
| `CreateGateway(ns, name)` | Create Istio Gateway with cleanup |
| `CreateRuleSet(ns, name, configMapNames)` | Create RuleSet with cleanup |
| `CreateEngine(ns, name, opts)` | Create Engine with cleanup |
| `TryCreateRuleSet(ns, name, configMapNames)` | Create RuleSet, return error (for validation tests) |
| `TryCreateEngine(ns, name, opts)` | Create Engine, return error (for validation tests) |
| `CreateHTTPRoute(ns, name, gw, backend)` | Create HTTPRoute with cleanup |
| `CreateEchoBackend(ns, name)` | Deploy echo server (Deployment + Service), wait for Ready |
| `ApplyManifest(ns, path)` | Apply YAML file via kubectl with cleanup |

### Scenario - Resource Updates

| Method | Purpose |
|---|---|
| `UpdateRuleSet(ns, name, configMapNames)` | Replace RuleSet's ConfigMap references |
| `UpdateConfigMap(ns, name, rules)` | Replace ConfigMap rules data in-place |

### Scenario - Assertions

| Method | Purpose |
|---|---|
| `ExpectEngineReady(ns, name)` | Poll until Engine condition Ready=True |
| `ExpectEngineDegraded(ns, name)` | Poll until Engine condition Degraded=True |
| `ExpectGatewayProgrammed(ns, name)` | Poll until Gateway condition Programmed=True |
| `ExpectGatewayAccepted(ns, name)` | Poll until Gateway condition Accepted=True |
| `ExpectWasmPluginExists(ns, name)` | Poll until WasmPlugin exists |
| `ExpectResourceGone(ns, name, gvr)` | Poll until resource is deleted |
| `ExpectCondition(ns, name, gvr, type, status)` | Generic condition poll |
| `ExpectCreateFails(msg, fn)` | Assert fn returns error containing msg |
| `GetEvents(ns)` | List all events.k8s.io/v1 events in namespace |
| `ExpectEvent(ns, match)` | Poll until a matching event exists |
| `ExpectNoEvent(ns, match)` | Assert no matching event currently exists (point-in-time) |

### GatewayProxy - Traffic Assertions

| Method | Purpose |
|---|---|
| `ExpectBlocked(path)` | Poll until path returns 403 |
| `ExpectAllowed(path)` | Poll until path returns 200 (requires echo backend + HTTPRoute) |
| `ExpectStatus(path, code)` | Poll until path returns specific status |
| `Get(path)` | Single GET request, returns HTTPResult |
| `URL(path)` | Returns full URL for manual requests |

### Resource Builders

Exported builder functions for use outside scenarios:

| Function | Purpose |
|---|---|
| `BuildGateway(ns, name)` | Build unstructured Gateway |
| `BuildRuleSet(ns, name, rules)` | Build unstructured RuleSet |
| `BuildEngine(ns, name, opts)` | Build unstructured Engine |
| `BuildHTTPRoute(ns, name, gw, backend)` | Build unstructured HTTPRoute |
| `SimpleBlockRule(id, target)` | Generate a SecLang deny rule |

## Example Scenarios

See the test files in `test/integration/` for complete examples matching
the v0.2.0 milestone validation issues:

| File | Issue | What it tests |
|---|---|---|
| `coreruleset_test.go` | [#12](https://github.com/networking-incubator/coraza-kubernetes-operator/issues/12) | CoreRuleSet-compatible rules (SQLi, XSS) |
| `multiple_gateways_test.go` | [#13](https://github.com/networking-incubator/coraza-kubernetes-operator/issues/13) | RuleSets deployed to 3+ Gateways |
| `multi_engine_gateway_test.go` | [#52](https://github.com/networking-incubator/coraza-kubernetes-operator/issues/52) | Multiple Engines + Multiple Gateways combos |
| `reconcile_test.go` | — | Reconciliation loop: live RuleSet/ConfigMap mutations |
