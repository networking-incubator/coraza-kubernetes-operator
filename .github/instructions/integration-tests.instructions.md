---
applyTo: "test/integration/**/*.go"
---

- All integration test files must have `//go:build integration` and belong to `package integration`.
- A shared `*framework.Framework` (`fw`) is created once in `suite_test.go` via `TestMain`. Never call `framework.New()` inside a test function.
- Every test must call `t.Parallel()` and create a Scenario. Cleanup is registered automatically via `t.Cleanup` — do not call `defer s.Cleanup()`:
  ```go
  t.Parallel()
  s := fw.NewScenario(t)
  ns := s.GenerateNamespace("my-test")
  ```
- Use `s.GenerateNamespace(prefix)` to create namespaces with random suffixes. Do not hardcode namespace names — tests run in parallel.
- Use `s.Step("description")` to separate logical phases. This appears in test output on failure.
- Resource ordering matters — the operator has a dependency chain:
  1. `GenerateNamespace` → `CreateGateway` → `ExpectGatewayProgrammed`
  2. `CreateRuleSource` → `CreateRuleSet`
  3. `CreateEngine` (references RuleSet + Gateway) → `ExpectEngineReady`
  4. `ProxyToGateway` → `ExpectBlocked` / `ExpectAllowed` / `ExpectStatus`
- Do not use raw `dynamic.Resource()` calls for create/delete. Use `s.Create*` methods — they register cleanup automatically.
- Do not write manual polling loops. Use the framework's `Expect*` assertion methods which use `require.Eventually` with standard timeout/interval.
- Do not use raw `http.Get` for traffic checks. Use `GatewayProxy` methods which handle port-forward lifecycle and polling.
- If the framework doesn't support what you need, extend the framework (in `test/framework/`) rather than working around it in the test.
