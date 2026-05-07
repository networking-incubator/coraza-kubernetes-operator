# Engine API Redesign: Target Reference and Driver Flattening

## Problem Statement

The Engine CRD couples three independent concerns under a deeply nested path
(`spec.driver.istio.wasm`):

1. **What to protect** (workload selection via label selectors)
2. **How to deploy the WAF** (WASM plugin image, pull secrets)
3. **How to poll for rule updates** (cache server config)

This creates several issues:

- **Users must know implementation labels.** To target a Gateway, users must
  manually set `workloadSelector.matchLabels` with the
  `gateway.networking.k8s.io/gateway-name` label. Per GEP-1762, Gateway
  implementations are required to apply this label to child resources, so the
  operator can derive it from just the Gateway name.

- **The API assumes Istio is the only integration.** The `driver.istio.wasm`
  path hardcodes the service mesh as a user-facing choice. Future integrations
  (OpenShift Router, sidecar proxies, dynamic modules) would require parallel
  nesting or awkward reshuffling.

- **Cross-cutting config is buried in driver-specific types.** The cache server
  poll interval is not specific to WASM or Istio; it applies to any mechanism
  that dynamically loads rules.

## Design

### Separation of concerns

The new API splits the Engine spec into orthogonal sections:

```
spec
 +-- ruleSet           (what rules to apply)
 +-- target         (what to protect — required)
 +-- ruleSetCacheServer (how to poll for rule updates)
 +-- driver            (how to deploy the WAF filter — optional)
 +-- failurePolicy     (behavior on WAF errors)
```

### New API shape

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
spec:
  ruleSet:
    name: my-ruleset

  failurePolicy: fail

  target:
    type: Gateway
    name: my-gateway       # must be a valid DNS label (RFC 1035, max 63 chars)
    provider: Istio        # identifies the infrastructure provider (default: Istio)

  ruleSetCacheServer:
    pollIntervalSeconds: 15

  driver:                  # optional; defaults to wasm
    type: wasm
    wasm:
      image: oci://ghcr.io/corazawaf/coraza-proxy-wasm
      imagePullSecret: my-secret
```

### Key decisions

**1. `target` replaces `workloadSelector`**

Users specify a Gateway by name. The operator derives the label selector using
GEP-1762: `gateway.networking.k8s.io/gateway-name: <name>`. This:

- Eliminates the need for users to know internal labels
- Makes the API extensible to non-Gateway targets (OpenShift Router, etc.)
- Scopes each Engine to exactly one target (1:1 relationship)

To protect multiple Gateways, users create multiple Engine resources. Each
Engine produces its own WasmPlugin, NetworkPolicy, and ServiceAccount, so a
1:1 mapping simplifies both the mental model and the controller logic.

The `target.name` is validated as a DNS label (RFC 1035) with a 63-character
maximum. This is enforced both at admission (CEL `format.dns1035Label()`) and
at runtime (defense-in-depth guard in `targetLabelSelector()`), preventing
silent WAF bypass from names that would produce unmatchable label selectors.

**2. `ruleSetCacheServer` moves to top-level spec**

Cache polling is not driver-specific. A dynamic module driver would use the
same cache server mechanism. Moving it out of the driver config avoids
duplication across driver types.

**3. `driver` becomes optional with a `type` discriminator**

Instead of `driver.istio.wasm` (three levels of nesting), the driver is now
`driver.type: wasm` + `driver.wasm: {...}` (two levels). The Istio service mesh
integration is an implementation detail inferred from `target.type: Gateway`,
not a user-facing choice.

When `driver` is omitted entirely, the operator defaults to `wasm`. This means
the simplest possible Engine only needs `ruleSet`, `target`, and
`ruleSetCacheServer`.

**4. `mode` and `status.gateways` removed**

The `mode` field was always `gateway` and is now implied by `target.type`.
The `status.gateways` list was needed when a label selector could match multiple
Gateways; with `target` naming exactly one, it is redundant. Status now only
carries conditions.

### Validation

| Field | Mechanism | Constraint |
|-------|-----------|------------|
| `target` | `+required` / `omitzero` | Structurally required |
| `target.kind` | Enum | `Gateway` |
| `target.name` | MaxLength + CEL | Max 63 chars, valid DNS label (RFC 1035) |
| `driver.type` | Enum | `wasm` |
| `driver.wasm` | CEL | Required when `type == wasm` |
| `driver.wasm.image` | CEL | Must start with `oci://`, max 1024 chars |

## Migration

### Before
```yaml
spec:
  ruleSet:
    name: my-rules
  failurePolicy: fail
  driver:
    istio:
      wasm:
        mode: gateway
        workloadSelector:
          matchLabels:
            gateway.networking.k8s.io/gateway-name: my-gw
        image: oci://ghcr.io/corazawaf/coraza-proxy-wasm
        imagePullSecret: registry-creds
        ruleSetCacheServer:
          pollIntervalSeconds: 30
```

### After
```yaml
spec:
  ruleSet:
    name: my-rules
  failurePolicy: fail
  target:
    type: Gateway
    name: my-gw
  ruleSetCacheServer:
    pollIntervalSeconds: 30
  driver:
    type: wasm
    wasm:
      image: oci://ghcr.io/corazawaf/coraza-proxy-wasm
      imagePullSecret: registry-creds
```

## Extensibility

This design creates clear extension points for future work:

- **New target kinds**: Add values to `EngineTargetKind` enum (e.g.,
  `OpenShiftRoute`) and a corresponding case in `targetLabelSelector()`.
- **New driver types**: Add values to `DriverType` enum (e.g.,
  `dynamicModule`) with a corresponding config struct and provisioning function.
- **Cross-namespace targeting**: Add an optional `namespace` field to
  `Enginetarget`.

None of these require structural API changes; they extend existing enums and
switch cases.

## Follow-up work

- **Target conflict detection**: When two Engines select the same target, the
  operator should detect the conflict and reject the newer Engine with an
  `Accepted=False` condition (reason: `TargetConflict`). The oldest Engine wins;
  when it is deleted, the next-oldest is automatically accepted.

- **Driver auto-detection from GatewayClass**: The operator should inspect the
  GatewayClass controller of the targeted Gateway to determine the appropriate
  default driver, rather than always defaulting to `wasm`.
