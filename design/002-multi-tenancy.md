# Multi-Tenancy Support for Coraza Kubernetes Operator

## Problem Statement

The Coraza Kubernetes Operator currently enforces a **1:1 relationship** between
an Engine CR and a Gateway target. Platform operators manage all WAF
configuration (rules, exclusions, anomaly thresholds) centrally via GitOps.
Application teams that need WAF rule exclusions for false positives must request
changes through the platform team, creating a bottleneck.

Specific issues:

- **Exclusion bottleneck.** Application teams cannot self-service WAF exclusions.
  Every false-positive fix requires a platform-side PR to modify a shared
  exclusion ConfigMap, regenerate overlays, and wait for sync.

- **Blast radius of exclusions.** A rule exclusion added to a shared scope (e.g.
  `coraza-rule-exclusions-default-post`) weakens WAF protection for every
  application behind that gateway, not just the one that triggered the false
  positive.

- **No per-application anomaly tuning.** All applications behind a gateway share
  the same anomaly score threshold (`tx.inbound_anomaly_score_threshold`). An
  application generating many low-confidence matches cannot raise its own
  threshold without raising it for all traffic on that gateway.

- **No per-application detection-only mode.** A team onboarding a new application
  cannot run Coraza in detection-only mode for their routes while other
  applications on the same gateway remain in blocking mode.

- **Single RuleSet per Engine.** The Engine CR references exactly one RuleSet.
  There is no mechanism to compose multiple RuleSets (e.g. a platform base + a
  team overlay) into a single Engine evaluation.

## Goals

1. Allow application teams to self-service WAF exclusions scoped to their own
   routes without modifying shared platform configuration.
2. Preserve the platform team's ability to enforce a mandatory baseline (OWASP
   CRS, anomaly scoring, blocking mode) that tenants cannot weaken.
3. Scope exclusion blast radius to the application that needs it — an exclusion
   for `team-foo` must not affect `team-bar` traffic on the same gateway.
4. Support per-application anomaly threshold overrides and detection-only mode.
5. Remain compatible with the new Engine API (`target.type: Gateway`,
   `target.name`) from [design 001][design-001].

## Non-Goals

- Per-route WAF bypass (disabling WAF entirely for specific routes).
- Unrestricted raw SecLang authoring by tenants (raw SecLang is available only
  as a gated escape hatch, not the primary interface).
- Tenant-managed CRS rule additions beyond the platform-curated library (tenants
  can enable pre-approved rules, not supply arbitrary rule content).
- Cross-namespace Engine targeting (tracked separately in [design 001 extensibility][design-001]).
- Rate limiting, geo-blocking, or bot management (separate concerns).

## Design

### Hierarchical RuleSet CRs (RuleSetOverride)

Introduce a `RuleSetOverride` CR that tenants create in their own namespace.
The operator discovers overrides via `spec.engineRef` and merges them into the
Engine's associated RuleSet.

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSetOverride
metadata:
  name: team-foo-exclusions
  namespace: team-foo
spec:
  engineRef:
    name: coraza-intranet
    namespace: openshift-ingress
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: team-foo-route # must be in the same namespace and accepted by a Gateway
  hostnames: # optional: narrow scope to specific hostnames on the HTTPRoute
    - "app.team-foo.example.com" # must be a subset of the HTTPRoute's hostnames
  additionalRules:
    # Enable RuleSources from the platform library not in the baseline
    - name: crs-request-922-multipart-attack # not globally enabled due to FP rate
    - name: crs-request-944-application-attack-java # only relevant for Java workloads
  disabledRules:
    # Disable specific baseline rules for this application's routes
    - id: 942100 # SQL injection rule causing false positives on legitimate queries
    - id: 941100 # XSS rule incompatible with team-foo's rich text editor
  customExclusions:
    # Structured exclusions (preferred — auditable, validatable, portable)
    - targetRules: [942100, 942200]
      description: "GraphQL query arguments trigger SQLi false positives"
      conditions:
        requestUri: { operator: STARTS_WITH, value: "/graphql" }
      requestFields:
        args: ["query", "variables"]
    - targetRules: [941100]
      description: "Rich text editor content triggers XSS detection"
      conditions:
        requestUri: { operator: EQUALS, value: "/api/content" }
        requestMethod: POST
      requestFields:
        args: ["body", "html_content"]
  customInclusions:
    # Structured detection/blocking rules (app-specific threat patterns)
    - description: "Block direct admin privilege escalation attempts"
      conditions:
        requestUri: { operator: STARTS_WITH, value: "/internal-api" }
        requestArgs: { name: "admin", operator: EQUALS, value: "true" }
      action: deny
      severity: CRITICAL
    - description: "Detect unusual bulk export requests"
      conditions:
        requestUri: { operator: STARTS_WITH, value: "/api/export" }
        requestMethod: GET
        requestArgs: { name: "limit", operator: GREATER_THAN, value: "10000" }
      action: detect
      severity: WARNING
  # Raw SecLang escape hatch (requires namespace label waf.k8s.coraza.io/allow-raw-seclang=true)
  rawRules:
    pre:
      - ruleSourceRef:
          name: team-foo-pre-rules # RuleSource CR in same namespace
    post:
      - ruleSourceRef:
          name: team-foo-post-rules # RuleSource CR in same namespace
  overrides:
    anomalyScoreThreshold: 10 # raise from default 5 for this app
    secRuleEngine: DetectionOnly # detection-only during onboarding
    variables: # override CRS tx.* variables for this app's hosts
      tx.allowed_methods: "GET HEAD POST OPTIONS PUT PATCH DELETE"
status:
  conditions:
    - type: Accepted
      status: "True"
      reason: Valid
      message: "Override merged into Engine coraza-intranet"
      lastTransitionTime: "2026-04-30T12:00:00Z"
    - type: RulesValid
      status: "True"
      reason: ValidationPassed
      message: "All custom exclusions and additional rules passed validation"
      lastTransitionTime: "2026-04-30T12:00:00Z"
    - type: TargetResolved
      status: "True"
      reason: HTTPRouteAccepted
      message: "HTTPRoute team-foo-route is accepted by Gateway intranet"
      lastTransitionTime: "2026-04-30T12:00:00Z"
```

**Status conditions:**

| Condition        | Description                                                                                                     |
| ---------------- | --------------------------------------------------------------------------------------------------------------- |
| `Accepted`       | Overall status — `True` when the override is merged into the Engine's RuleSet                                   |
| `RulesValid`     | Whether custom exclusions and additional rules passed validation (permitted directives, rules exist in library) |
| `TargetResolved` | Whether the HTTPRoute referenced by `targetRef` exists, is in the same namespace, and is accepted by a Gateway  |

Failure reasons for each condition:

- `Accepted`: `EngineNotFound`, `GatewayMismatch`, `RulesInvalid`, `TargetNotAccepted`, `HostnameConflict`, `HostnamesRequired`
- `RulesValid`: `ValidationPassed`, `ForbiddenDirective`, `RuleNotInLibrary`, `RuleSourceNotFound`, `RawSecLangNotAllowed`, `CompilationFailed`
- `TargetResolved`: `HTTPRouteAccepted`, `HTTPRouteNotFound`, `HTTPRouteNotAccepted`

**How it works:**

1. Tenants create `RuleSetOverride` CRs in their own namespace.
2. The operator watches all namespaces for `RuleSetOverride` CRs and uses
   `spec.engineRef` to resolve the target Engine and its associated RuleSet.
   If the referenced Engine does not exist or is not ready, the override is
   rejected with `Accepted=False` reason `EngineNotFound`.
3. The operator resolves `targetRef` to an HTTPRoute in the same namespace and
   verifies it has an `Accepted` condition. If the HTTPRoute is not accepted by
   any Gateway, the override is rejected with `Accepted=False` reason
   `TargetNotAccepted`.
4. The operator validates that the HTTPRoute's parent Gateway matches the
   Engine's target Gateway — ensuring the override applies to the correct
   traffic path. For HTTPRoutes attached to multiple Gateways, only the
   parentRef matching the Engine's Gateway is considered.
5. The operator determines the effective hostname scope:
   - If `spec.hostnames` is set, it must be a subset of the HTTPRoute's
     `spec.hostnames` AND a subset of the Engine's Gateway listener hostnames.
   - If `spec.hostnames` is omitted, the operator uses the intersection of the
     HTTPRoute's hostnames and the Engine's Gateway listener hostnames.
   - If the HTTPRoute has no `spec.hostnames` (matches all listeners), the
     override is rejected with `Accepted=False` reason `HostnamesRequired`.
     HTTPRoutes without explicit hostnames represent an unbounded scope and
     are a security risk.
     This prevents an override intended for one Gateway from leaking into
     hostnames served by a different Gateway on the same shared HTTPRoute.
6. **Hostname isolation enforcement:** All custom exclusions, disabled rules,
   additional rules, and overrides are scoped using runtime `ctl:` directives
   (see step 8) conditioned on the effective hostnames from step 5. The
   operator also validates that the effective hostnames belong to the
   override's namespace — if any hostname is claimed by an HTTPRoute in a
   different namespace, the override is rejected with `Accepted=False` reason
   `HostnameConflict`. This ensures a tenant's custom exclusions can never
   leak into another team's endpoints.

   > **Note:** Until [ListenerSets](https://gateway-api.sigs.k8s.io/guides/listener-set/)
   > become available in Gateway API, HTTPRoute hostname leakage across namespaces
   > remains possible at the Gateway level. A tenant can attach an HTTPRoute to a
   > shared Gateway listener using hostnames that belong to another team. The
   > operator's `HostnameConflict` validation mitigates this for WAF overrides,
   > but full hostname isolation requires ListenerSets to enforce per-tenant
   > listener scoping on the Gateway itself.

7. `additionalRules` references are validated against the parent RuleSet's
   `spec.library` field (see [Rule Library Contract](#rule-library-contract)).
   Tenants can only enable rules that exist in the library — they cannot supply
   arbitrary rule content. This allows teams to opt into stricter protections
   (e.g., Java-specific attack rules) for their routes without the platform
   enabling them globally.
8. `disabledRules` generates `ctl:ruleRemoveById` directives (runtime action)
   wrapped in `SecRule REQUEST_HEADERS:Host` conditions matching the effective
   hostnames. `ctl:ruleRemoveById` is used instead of `SecRuleRemoveById`
   because the latter is a config-time directive that cannot be scoped per-host
   — it would apply globally across all traffic on the Engine. Any rule may be
   disabled; compliance monitoring (see [Metrics and Compliance Monitoring](#metrics-and-compliance-monitoring))
   ensures visibility into disabled rules and alerts when overrides weaken
   posture beyond acceptable thresholds.
9. **Custom exclusion compilation** (see
   [Rule Compilation Strategy](#rule-compilation-strategy) for full detail):
   - **Structured exclusions** (`customExclusions[]`): The operator compiles
     each structured exclusion into a host-scoped
     `ctl:ruleRemoveTargetById` directive. For example, an exclusion
     targeting rule 942100 with `requestFields.args: ["query"]` and
     `conditions.requestUri: STARTS_WITH /graphql` compiles to:

     ```text
     SecRule REQUEST_HEADERS:Host "@rx ^app\.team-foo\.example\.com$" \
       "id:9000001,phase:1,pass,nolog,\
        ctl:ruleRemoveTargetById=942100;ARGS:query"
     ```

     When a `conditions.requestUri` is present, the operator adds an
     additional chained condition on `REQUEST_URI`. This approach is
     auditable (structured fields map 1:1 to directives), validatable
     (the operator can reject invalid rule IDs or non-existent fields),
     and portable (compilation can target different engines in the future).

   - **Raw SecLang escape hatch** (`rawRules.pre` / `rawRules.post`):
     For complex logic that cannot be expressed via `customExclusions` or
     `customInclusions` (e.g., multi-condition chains, transform-dependent
     logic, phase-specific actions), tenants can reference RuleSource CRs
     containing raw SecLang. Raw rules can contain both exclusion logic
     (`ctl:` actions) and detection/blocking logic (`deny`/`block` actions).
     This requires the label `waf.k8s.coraza.io/allow-raw-seclang: "true"`
     on the tenant's **namespace** — without it, `rawRules` is rejected with
     `RulesValid=False` reason `RawSecLangNotAllowed`. This is a namespace-level
     gate so that platform teams control which namespaces can use raw SecLang
     via GitOps (namespace labels are typically platform-managed). When permitted:
     - `rawRules.pre`: Injected before CRS rules. Typical use: `ctl:` actions
       for exclusions, or custom detection rules that should fire before CRS.
     - `rawRules.post`: Injected after CRS rules. Typical use:
       `ctl:ruleRemoveTargetById` for post-CRS exclusions, or custom
       detection rules that depend on CRS variables/state.
     - All raw rules are subject to the isolation guarantees defined in
       [Raw SecLang Isolation Guarantees](#raw-seclang-isolation-guarantees):
       mandatory host-wrapping, forbidden directives, forbidden `allow`/`drop`/
       `redirect` actions, and ID rewriting.

10. **Custom inclusion compilation:** `customInclusions[]` are compiled into
    host-scoped `SecRule` directives with `deny` or `pass,log` actions
    (depending on whether `action` is `deny` or `detect`). Each structured
    inclusion is compiled into a rule matching the specified conditions with
    the operator-injected host gate. Inclusions are injected at the tenant
    pre injection point (before CRS) so they can short-circuit known-bad
    requests early without CRS evaluation overhead.
11. Overrides are merged into the parent RuleSet's compiled rule chain with
    deterministic ordering (see [Override Precedence](#override-precedence)).
12. `overrides.secRuleEngine`, `overrides.anomalyScoreThreshold`, and
    `overrides.variables` are injected at the tenant pre injection point:
    - `overrides.secRuleEngine: DetectionOnly` emits a host-scoped
      `ctl:ruleEngine=DetectionOnly` action. This is a **runtime** directive
      that switches the engine to detection-only mode for the current
      transaction only. It does NOT use `SecRuleEngine` (which is config-time
      and cannot be host-scoped). The Engine's static `SecRuleEngine On`
      remains in effect for all other hosts.
    - `overrides.anomalyScoreThreshold` and `overrides.variables` are injected
      as host-scoped `SecAction` directives with `setvar` actions. Variables
      are injected before CRS initialization rules (e.g., 901160) check their
      `@eq 0` guards.

**RBAC model:**

```text
Platform team:
  - Engine, RuleSet: full control
  - RuleSetOverride: read (for visibility), no create/update in tenant NS

Application team (namespaced):
  - RuleSetOverride: create, update, delete (in own namespace only)
  - RuleSource: create, update, delete (only needed if using rawRules escape hatch)
```

> **Note:** Most tenants will only use structured `customExclusions` and never
> need to create RuleSource CRs. Platform teams can omit the RuleSource RBAC
> grant for tenants that don't require the raw SecLang escape hatch, providing
> defense-in-depth through RBAC restriction in addition to the
> `allow-raw-seclang` namespace label gate.

**Available `overrides.variables` keys:**

The following CRS `tx.*` variables can be overridden per-host. All have `@eq 0`
guards in CRS 901-initialization — setting them before CRS loads pre-empts the
default.

| Variable                                  | CRS Default                         | Description                                                                     |
| ----------------------------------------- | ----------------------------------- | ------------------------------------------------------------------------------- |
| `tx.inbound_anomaly_score_threshold`      | `5`                                 | Anomaly score threshold for blocking inbound requests                           |
| `tx.outbound_anomaly_score_threshold`     | `4`                                 | Anomaly score threshold for blocking outbound responses                         |
| `tx.blocking_paranoia_level`              | `1`                                 | Paranoia level (1-4); higher enables more rules with more false positives       |
| `tx.detection_paranoia_level`             | `= blocking_paranoia_level`         | Detection-only paranoia level (logs above blocking without blocking)            |
| `tx.allowed_methods`                      | `GET HEAD POST OPTIONS`             | Space-separated HTTP methods allowed (rule 911100 blocks others)                |
| `tx.allowed_request_content_type`         | `application/x-www-form-... (list)` | Pipe-delimited content types allowed (rule 920420 blocks others)                |
| `tx.allowed_request_content_type_charset` | `utf-8, iso-8859-1, ... (list)`     | Pipe-delimited charsets allowed in Content-Type                                 |
| `tx.allowed_http_versions`                | `HTTP/1.0 HTTP/1.1 HTTP/2 HTTP/3`   | Space-separated HTTP versions allowed (rule 920230 blocks others)               |
| `tx.restricted_extensions`                | `.asa/ .backup/ .bak/ ...`          | Slash-delimited file extensions blocked in URI (rule 920440)                    |
| `tx.restricted_headers_basic`             | `/proxy/ /lock-token/ ...`          | Slash-delimited request headers blocked (rule 920450)                           |
| `tx.restricted_headers_extended`          | `/accept-charset/`                  | Additional restricted headers (PL2+, rule 920451)                               |
| `tx.enforce_bodyproc_urlencoded`          | `0`                                 | Force URLENCODED body processor for requests without Content-Type               |
| `tx.crs_validate_utf8_encoding`           | `0`                                 | Enable UTF-8 encoding validation                                                |
| `tx.early_blocking`                       | `0`                                 | Block in phase 1 before full request body is read (reduces latency for attacks) |
| `tx.reporting_level`                      | `4`                                 | Minimum anomaly score severity to report (4=critical, 3=error, 2=warning)       |
| `tx.sampling_percentage`                  | `100`                               | Percentage of requests to inspect (100 = all traffic)                           |
| `tx.critical_anomaly_score`               | `5`                                 | Score added per critical-severity rule match                                    |
| `tx.error_anomaly_score`                  | `4`                                 | Score added per error-severity rule match                                       |
| `tx.warning_anomaly_score`                | `3`                                 | Score added per warning-severity rule match                                     |
| `tx.notice_anomaly_score`                 | `2`                                 | Score added per notice-severity rule match                                      |

> **Note:** `tx.anomalyScoreThreshold` in `spec.overrides` is syntactic sugar
> for `tx.inbound_anomaly_score_threshold` in `spec.overrides.variables`. If
> both are set, `variables` takes precedence. The per-severity scores
> (`tx.critical_anomaly_score`, etc.) should generally not be overridden by
> tenants — changing them breaks the scoring model for all rules.

**Advantages:**

- Cleanest separation — tenants manage their own CRs in their own namespace
- No modification to shared RuleSet CR needed for tenant onboarding
- Supports per-application anomaly threshold and detection-only mode natively
- Explicit `engineRef` makes the relationship clear and auditable
- Tenants can opt into additional rules from a platform-curated library, enabling
  stricter protections for their specific workloads without affecting others
- HTTPRoute-based scoping provides implicit host validation — no accepted route,
  no active override

**Disadvantages:**

- New CRD to maintain and version
- Cross-namespace watch increases operator resource consumption
- Merge ordering across multiple overrides needs deterministic resolution
- More complex status reporting (override acceptance/rejection conditions)

## Design Rationale

1. **True self-service** — tenants create their own RuleSetOverride CRs in their
   own namespace without any platform-side changes.

2. **HTTPRoute-based scoping** — deriving host scope from an accepted HTTPRoute
   provides automatic validation and avoids tenants needing to hardcode hostnames
   or the platform team needing to verify them.

3. **Additive rule library** — tenants can opt into additional protections from a
   platform-curated library, giving teams control over their security posture
   without weakening the baseline.

4. **Per-application tuning** — anomaly thresholds and detection-only mode are
   first-class fields on the override CR, not workarounds with host-scoped
   SecAction directives.

5. **Structured exclusions over raw SecLang** — the structured `customExclusions`
   interface covers ~90% of real-world exclusion patterns (field X excluded from
   rule Y on path Z) without requiring tenants to learn ModSecurity SecLang. The
   operator compiles structured fields into correct `ctl:` directives, making
   exclusions auditable, validatable, and safe by construction. Raw SecLang is
   available as a gated escape hatch for complex edge cases.

## Rule Compilation Strategy

The operator compiles each Engine's final rule chain by concatenating sources in
a fixed order. Tenant overrides are inserted at specific injection points within
this chain. The compilation must respect the constraint that config-time
directives (e.g., `SecRuleUpdateTargetById`) apply globally, while runtime `ctl:`
actions can be conditioned on request properties like `Host`.

### Compilation Order

```text
1. coraza-recommended-conf          (platform, static)
2. coraza-crs-setup                 (platform, static)
3. Platform pre-CRS exclusions      (platform, static — e.g., coraza-rule-exclusions-*-pre)
4. ── TENANT PRE INJECTION POINT ──
   For each accepted RuleSetOverride (ordered by precedence):
     - overrides.variables → host-scoped SecAction with setvar
     - overrides.secRuleEngine → host-scoped ctl:ruleEngine=DetectionOnly
     - disabledRules → host-scoped ctl:ruleRemoveById
     - additionalRules inverse-disables → ctl:ruleRemoveById for non-requesting hosts
     - customExclusions (structured) → operator-compiled ctl:ruleRemoveTargetById
     - rawRules.pre → tenant RuleSource content (host-wrapped, validated)
5. CRS request rules 901–949        (platform, static)
6. Additional library rules          (union of all additionalRules — loaded once globally)
7. CRS response rules 959,980       (platform, static)
8. coraza-base-rules                (platform, static)
9. ── TENANT POST INJECTION POINT ──
   For each accepted RuleSetOverride (ordered by precedence):
     - rawRules.post → tenant RuleSource content (host-wrapped, validated)
10. Platform post-CRS exclusions    (platform, static — e.g., coraza-rule-exclusions-*-post)
```

### Why `ctl:ruleRemoveTargetById` for Post-CRS

The current static config uses `SecRuleUpdateTargetById` for post-CRS exclusions
(e.g., removing `ARGS:password` from rule 942100's inspection targets). This
works in a single-tenant static config because:

- It is a config-time directive — processed once at rule load.
- It requires the target rule to already be loaded (hence "post-CRS" placement).
- It applies unconditionally to all traffic on the Engine.

In a multi-tenant context, `SecRuleUpdateTargetById` **cannot** be host-scoped
because it modifies the rule's compiled target list at load time, not per
transaction. Two alternatives were considered:

1. **Per-host WasmPlugin splitting**: Generate separate Coraza configs per
   hostname, each with its own `SecRuleUpdateTargetById` directives. Rejected —
   this defeats the shared-Engine model and scales configs linearly with tenants.

2. **`ctl:ruleRemoveTargetById` (chosen)**: This runtime action removes a
   specific target variable (e.g., `ARGS:password`) from a rule's inspection
   list for the current transaction only. It can be wrapped in a
   `SecRule REQUEST_HEADERS:Host` condition, making it host-scopeable. The
   trade-off is a small per-request CPU cost for evaluating the host condition,
   which is negligible compared to CRS rule evaluation.

### Structured Exclusions and Inclusions vs Raw SecLang

The `customExclusions` and `customInclusions` fields use structured, declarative
interfaces. The operator compiles them into the correct SecLang directives —
tenants never write SecLang directly for common cases.

#### Structured Exclusions (`customExclusions`)

Inspired by Google Cloud Armor's preconfigured WAF exclusions model. Compiles
to `ctl:ruleRemoveTargetById` directives.

**Structured exclusion fields:**

| Field           | Description                                                       |
| --------------- | ----------------------------------------------------------------- |
| `targetRules`   | List of rule IDs to exclude the specified fields from             |
| `description`   | Human-readable explanation (stored in compiled comment for audit) |
| `conditions`    | Optional request matching conditions (see below)                  |
| `requestFields` | Which request fields to exclude from target rules' inspection     |

**Condition operators:**

| Field           | Operators                                        |
| --------------- | ------------------------------------------------ |
| `requestUri`    | `EQUALS`, `STARTS_WITH`, `ENDS_WITH`, `CONTAINS` |
| `requestMethod` | Exact match (e.g., `POST`, `PUT`)                |

**Request field types:**

| Field            | Maps to ModSecurity variable | Example                  |
| ---------------- | ---------------------------- | ------------------------ |
| `args`           | `ARGS:<name>`                | `["query", "variables"]` |
| `argsNames`      | `ARGS_NAMES`                 | `true` (all arg names)   |
| `requestHeaders` | `REQUEST_HEADERS:<name>`     | `["X-Custom-Token"]`     |
| `requestCookies` | `REQUEST_COOKIES:<name>`     | `["session_id"]`         |
| `requestBody`    | `REQUEST_BODY`               | `true` (entire body)     |
| `xmlContent`     | `XML:<xpath>`                | `["/*", "/soap:Body"]`   |

**Why structured over raw:**

1. **Auditable.** Compliance tooling can programmatically inspect structured
   fields without parsing SecLang. A structured exclusion's intent is
   self-documenting.
2. **Validatable.** The operator can reject invalid configurations at admission
   time: non-existent rule IDs, unsupported field types, or conditions that
   would match too broadly.
3. **Portable.** If the underlying engine changes (e.g., different `ctl:`
   syntax, different WASM plugin), the operator recompiles — tenants don't
   need to update their CRs.
4. **Safe.** Structured exclusions cannot express `allow`/`deny` actions,
   regex backtracking bombs, or directives that interfere with CRS control
   flow. The blast radius is limited to "stop inspecting field X for rule Y."
5. **No rule ID management.** Tenants don't need to allocate or manage rule
   IDs — the operator handles it.

#### Structured Inclusions (`customInclusions`)

Allows tenants to add custom detection or blocking rules for app-specific
threat patterns. Compiles to host-scoped `SecRule` directives with the
specified action.

**Structured inclusion fields:**

| Field         | Description                                                   |
| ------------- | ------------------------------------------------------------- |
| `description` | Human-readable explanation (stored in compiled comment)       |
| `conditions`  | Request matching conditions (required, defines what to match) |
| `action`      | `deny` (block request) or `detect` (log only, no block)       |
| `severity`    | `CRITICAL`, `ERROR`, `WARNING`, or `NOTICE`                   |

**Inclusion condition fields:**

| Field            | Operators                                         |
| ---------------- | ------------------------------------------------- |
| `requestUri`     | `EQUALS`, `STARTS_WITH`, `ENDS_WITH`, `CONTAINS`  |
| `requestMethod`  | Exact match (e.g., `POST`, `PUT`)                 |
| `requestArgs`    | `{name, operator, value}` - match specific arg    |
| `requestHeaders` | `{name, operator, value}` - match specific header |
| `requestBody`    | `{operator, value}` - match body content          |

**Condition value operators** (for `requestArgs`, `requestHeaders`,
`requestBody`):

`EQUALS`, `CONTAINS`, `STARTS_WITH`, `ENDS_WITH`, `MATCHES` (regex),
`GREATER_THAN`, `LESS_THAN`

**Compilation example:**

```yaml
customInclusions:
  - description: "Block admin escalation attempts"
    conditions:
      requestUri: { operator: STARTS_WITH, value: "/internal-api" }
      requestArgs: { name: "admin", operator: EQUALS, value: "true" }
    action: deny
    severity: CRITICAL
```

Compiles to:

```text
SecRule REQUEST_HEADERS:Host "@rx ^app\.team-foo\.example\.com$" \
  "id:9000200,phase:2,chain,deny,status:403,\
   severity:CRITICAL,msg:'Block admin escalation attempts'"
  SecRule REQUEST_URI "@beginsWith /internal-api" "chain"
    SecRule ARGS:admin "@streq true" ""
```

**Why structured inclusions are safe:**

- Mandatory host-wrapping ensures custom detection rules only fire for the
  tenant's own traffic.
- `allow` action is not available — tenants cannot bypass WAF baseline.
- No regex authoring in structured mode — operators like `EQUALS`,
  `STARTS_WITH` compile to `@streq`, `@beginsWith` (no backtracking risk).
  Only `MATCHES` allows regex, which the operator can validate for
  catastrophic backtracking patterns at admission time.
- Same benefits as structured exclusions: auditable, validatable, portable,
  no rule ID management.

#### When to use raw rules

**When to use the raw escape hatch:**

The `rawRules` field is available for the ~10% of cases that cannot be
expressed via `customExclusions` or `customInclusions`:

- Multi-condition chains (e.g., exclude only when header A AND cookie B match)
- Transform-dependent logic (e.g., `t:urlDecode,t:lowercase` before matching)
- Phase-specific actions (e.g., `ctl:requestBodyAccess=Off` for specific paths)
- Complex variable selectors (e.g., regex on argument names `ARGS:/^utm_/`)
- Custom detection with complex matching logic beyond structured operators
- Third-party or internally-developed rule files

The escape hatch requires both:

1. The namespace label `waf.k8s.coraza.io/allow-raw-seclang: "true"` (platform
   teams control namespace labels via GitOps — tenants cannot self-grant).
2. RBAC permission to create RuleSource CRs in the tenant namespace (which
   platform teams can withhold for tenants that don't need it).

### Raw SecLang Isolation Guarantees

Because raw SecLang can express arbitrary ModSecurity directives, the operator
must enforce strict isolation to prevent a tenant's rules from affecting other
hosts or the global config. The following protections apply at compilation time:

**1. Forbidden directives (rejected at validation):**

The operator parses each RuleSource and rejects it (`RulesValid=False` reason
`ForbiddenDirective`) if it contains any config-time or globally-scoped
directive:

| Forbidden Directive       | Reason                                            |
| ------------------------- | ------------------------------------------------- |
| `SecRuleEngine`           | Config-time; changes engine mode for all traffic  |
| `SecRuleRemoveById`       | Config-time; removes rules globally               |
| `SecRuleUpdateTargetById` | Config-time; modifies rule targets globally       |
| `SecRuleUpdateActionById` | Config-time; modifies rule actions globally       |
| `SecRequestBodyAccess`    | Config-time; changes body processing globally     |
| `SecResponseBodyAccess`   | Config-time; changes response processing globally |
| `SecAuditEngine`          | Config-time; changes audit logging globally       |
| `SecMarker`               | Would interfere with CRS `skipAfter` control flow |

**2. Forbidden actions (rejected at validation):**

Individual `SecRule` directives are parsed and rejected if they use actions
that could bypass WAF or affect other tenants:

| Forbidden Action | Reason                                                   |
| ---------------- | -------------------------------------------------------- |
| `allow`          | Bypasses all remaining rules for the transaction         |
| `drop`           | Drops TCP connection; disproportionate for WAF use cases |
| `redirect`       | Redirects traffic; could be used for phishing/exfil      |

> **Note:** `deny` and `block` are **permitted** — with mandatory host-wrapping
> they only affect the tenant's own traffic. `pass` is permitted (used with
> `ctl:` actions for exclusions). The `allow` action is forbidden because it
> bypasses all subsequent rules including the platform's mandatory baseline,
> violating the isolation boundary even for the tenant's own hosts.

**3. Mandatory host-wrapping (enforced at compilation):**

The operator does **not** trust tenant-authored host conditions. Instead, it
**prepends** an operator-generated host-matching rule to each tenant rule using
`chain`:

```text
# Operator-injected host gate (cannot be omitted or overridden by tenant)
SecRule REQUEST_HEADERS:Host "@rx ^app\.team-foo\.example\.com$" \
  "id:9000100,phase:1,chain,pass,nolog"
  # Tenant-authored rule (from RuleSource)
  SecRule ARGS:session_token "@rx ^[a-f0-9]+$" \
    "t:none,ctl:ruleRemoveTargetById=943100;ARGS:session_token"
```

Because the operator controls the outer `chain` rule, the tenant's rule only
executes when the host matches. This is applied **per-rule** in the RuleSource
— each `SecRule` directive gets its own host-gate prefix.

**4. Single-rule-per-directive constraint:**

To make host-wrapping reliable, each `SecRule` in a `rawRules` RuleSource
is treated as an independent directive. Tenant-authored `chain` rules are
permitted (for multi-condition matching within a single logical rule), but the
operator wraps the **entire chain** with an additional outer host condition.
This means:

- Tenant writes: `SecRule A chain → SecRule B → action`
- Operator compiles: `SecRule HOST chain → SecRule A chain → SecRule B → action`

Multi-rule RuleSources (multiple `SecRule` directives in one file) are
supported — each is independently wrapped.

**5. No `skipAfter` / `SecMarker` in tenant rules:**

Tenant raw SecLang cannot use `skipAfter` actions or `SecMarker` directives.
These would create control flow that jumps past operator-injected host gates,
breaking isolation. If detected, the RuleSource is rejected with
`ForbiddenDirective`.

**Summary of isolation model:**

| Threat                               | Mitigation                         |
| ------------------------------------ | ---------------------------------- |
| Config-time directives (global)      | Forbidden directive allowlist      |
| Rules without host conditions        | Mandatory operator host-wrapping   |
| `allow` action (bypasses baseline)   | Forbidden action allowlist         |
| `deny`/`block` affecting other hosts | Mandatory host-wrapping (safe)     |
| `skipAfter` bypassing host gate      | Forbidden in tenant rules          |
| Multi-rule files leaking scope       | Per-rule independent host-wrapping |
| Rule ID collisions                   | Operator rewrites all IDs          |

### Host-Scoped Additional Rules

`additionalRules` from a RuleSetOverride reference platform-library RuleSources.
These rules must be effectively host-scoped — a rule enabled by tenant A should
not evaluate (and potentially block) tenant B's traffic. However, CRS rule files
present a compilation challenge:

**Problem:** CRS rule files are multi-rule with internal control flow
(`skipAfter`, `SecMarker`, chained rules). A single outer
`SecRule REQUEST_HEADERS:Host ... chain` only gates the _next_ rule — subsequent
rules in the file execute unconditionally for all hosts. Wrapping each
individual rule in the file with host conditions would break `skipAfter` targets
(which reference rule IDs, not host-gated wrappers). Additionally, enabling
the same RuleSource for multiple tenants would duplicate rule IDs (invalid) or
require ID rewriting that breaks `skipAfter`/`SecMarker` references.

**Solution — inverse disable:** Additional rules are loaded globally into the
Engine (required for `skipAfter`/`SecMarker` integrity), but the operator
auto-generates `ctl:ruleRemoveById` directives for all hosts that did **not**
request those rules. This achieves effective host-scoping:

1. The operator unions all `additionalRules` across accepted overrides and
   includes each referenced RuleSource exactly once in the compilation.
2. For each additional rule file, the operator determines which hosts requested
   it (from the overrides' effective hostname scopes).
3. For all **other** hosts on the Engine (those that did not request the rule
   file), the operator emits `ctl:ruleRemoveById` directives at the tenant pre
   injection point, disabling those rules for non-requesting hosts.

```text
1. coraza-recommended-conf
2. coraza-crs-setup
3. Platform pre-CRS exclusions
4. ── TENANT PRE INJECTION POINT ──
   - overrides.variables → host-scoped SecAction with setvar
   - disabledRules → host-scoped ctl:ruleRemoveById
   - additionalRules inverse-disables → ctl:ruleRemoveById for non-requesting hosts
   - customExclusions (structured) → operator-compiled ctl:ruleRemoveTargetById
   - rawRules.pre → tenant RuleSource content (host-scoped ctl: actions)
5. CRS baseline request rules 901–949
6. Additional library rules (union of all additionalRules across overrides)
7. CRS response rules 959, 980
8. coraza-base-rules
9. ── TENANT POST INJECTION POINT ── (rawRules.post)
10. Platform post-CRS exclusions
```

**Example:** Tenant A (hosts `a.example.com`) enables
`crs-request-944-application-attack-java`. Tenant B (hosts `b.example.com`)
does not. The operator compiles:

```text
# Position 4: inverse-disable for non-requesting hosts
SecRule REQUEST_HEADERS:Host "@rx ^b\.example\.com$" \
  "id:9000050,phase:1,pass,nolog,\
   ctl:ruleRemoveById=944100,\
   ctl:ruleRemoveById=944110,\
   ctl:ruleRemoveById=944120,\
   ..."

# Position 6: rules loaded globally (skipAfter/SecMarker intact)
Include crs-request-944-application-attack-java
```

This preserves CRS control flow integrity while ensuring tenant B's traffic
never evaluates tenant A's opted-in rules.

**Library constraint — detection rules only:** Because additional rules load
globally (even though effectively scoped via inverse-disables), only rules that
**add** detection/blocking qualify for the library. Allowlist or exception rules
(e.g., 905-common-exceptions) would weaken WAF protection if the inverse-disable
mechanism failed or had a gap. Response-body rules (950-955) are also excluded
because Envoy WASM response-body inspection is not currently supported.

**Edge case — host without any override:** Hosts on the Engine that have no
RuleSetOverride at all receive inverse-disables for all library rules that any
tenant opted into. This ensures the baseline experience is unchanged for
applications that haven't created overrides.

### Rule ID Allocation

All operator-generated and tenant-custom SecLang directives use rule IDs in the
**9,000,000+** range. The operator automatically assigns unique IDs — neither
platform teams nor tenants manage rule IDs manually.

| Range           | Owner                                   |
| --------------- | --------------------------------------- |
| 1–899,999       | CRS and platform rules (never modified) |
| 900,000–999,999 | Platform setup/exclusion rules          |
| 9,000,000+      | Operator-generated (all tenant rules)   |

The operator deterministically assigns IDs based on a hash of the
RuleSetOverride's `{namespace}/{name}` and the exclusion's list index. This
guarantees:

- No collisions between tenants (different namespace/name → different hash seed)
- Stable IDs across reconciliations (same input → same ID)
- No manual bookkeeping or namespace-to-range mappings

For `rawRules`, the operator **rewrites** rule IDs in tenant-authored
RuleSources into the 9,000,000+ range at compilation time. Tenants can use any
placeholder IDs in their RuleSource CRs — the operator replaces them with
deterministic IDs during compilation. This eliminates the need for tenants to
coordinate or even be aware of ID allocation.

**Validation rules:**

- Structured `customExclusions` and `disabledRules` are compiled by the operator
  — tenants specify which CRS rule IDs to target (e.g., `targetRules: [942100]`,
  `disabledRules: [{id: 942100}]`), but the operator assigns the **SecRule
  directive IDs** (the `id:` action in the generated `SecRule` directives).
  Tenants never manage directive IDs.
- `rawRules` RuleSources have their IDs rewritten automatically. If a
  RuleSource contains directives that reference rule IDs internally (e.g.,
  `skipAfter`), the operator preserves relative references during rewriting.
- `additionalRules` reference existing CRS RuleSources that retain their
  original IDs (no rewriting needed — they are included once globally).

## Override Precedence

When multiple RuleSetOverrides target HTTPRoutes with overlapping hostnames on
the same Engine, the operator must produce a deterministic rule chain. Undefined
ordering would mean that identical Git state could produce different WAF behavior
depending on reconciliation timing.

### Merge Rules

1. **No hostname overlap allowed (default).** If two accepted RuleSetOverrides
   resolve to overlapping effective hostnames (from step 5 of reconciliation),
   the **later-created** override is rejected with `Accepted=False` reason
   `HostnameConflict`. Creation timestamp (`metadata.creationTimestamp`) is the
   tiebreaker; if identical, lexicographic ordering of
   `{namespace}/{name}` is used.

2. **Within a single override**, structured `customExclusions` and
   `customInclusions` are compiled in list order, `rawRules.pre` sources are
   concatenated in list order, and `rawRules.post` sources are concatenated
   in list order. This gives tenants deterministic control over their own
   rule ordering.

3. **`disabledRules` and `additionalRules`** are sets (order-independent) — they
   produce `ctl:` actions and host-gated rule blocks that do not have
   order-sensitive interactions with each other.

4. **Platform static rules always win.** Platform pre/post exclusions (positions
   3 and 10 in the compilation order) are never overridden by tenant directives.
   If a tenant's `ctl:ruleRemoveTargetById` conflicts with a platform-level
   `SecRuleUpdateTargetById` on the same rule+target, the platform directive
   takes effect at load time and the tenant's runtime removal is a no-op (the
   target is already removed globally).

### Rationale for Rejecting Overlaps

Allowing ordered merge of overlapping overrides (e.g., priority fields) was
considered and rejected because:

- Priority fields create implicit dependencies between teams that don't
  coordinate.
- Combined exclusion sets from different teams could weaken WAF posture in
  non-obvious ways that neither team intended.
- Rejection forces teams to scope their HTTPRoutes to non-overlapping hostnames,
  which is already best practice for blast-radius isolation.

## Rule Library Contract

The parent RuleSet declares which RuleSources are available for tenants to
enable via `additionalRules`. This is specified in a new `spec.library` field:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: intranet-ruleset
  namespace: openshift-ingress
spec:
  ruleData: crs-rule-data
  rules:
    - name: coraza-recommended-conf
    # ... (baseline rules loaded for all traffic)
  library:
    # Rules NOT in the baseline, available for tenants to enable via
    # RuleSetOverride.additionalRules. Only DETECTION rules qualify for the
    # library — they add protection without weakening other tenants.
    # Allowlist/exception rules (e.g., 905) and response-body rules (950-955)
    # are excluded: allowlists weaken protection globally, and response-body
    # inspection is not supported in the current WASM runtime.
    - name: crs-request-922-multipart-attack
      description: "Multipart request attack detection (high FP on file upload apps)"
    - name: crs-request-933-application-attack-php
      description: "PHP injection (only relevant for PHP workloads)"
    - name: crs-request-943-application-attack-session-fixation
      description: "Session fixation via cookie injection (high FP on apps with dynamic session params)"
    - name: crs-request-944-application-attack-java
      description: "Java-specific attacks - RCE, deserialization, OGNL/SpEL (only relevant for Java workloads)"
```

### Tenant Discovery

Tenants discover available library rules by reading the parent RuleSet's
`spec.library` field:

```bash
kubectl get ruleset intranet-ruleset -n openshift-ingress \
  -o jsonpath='{.spec.library[*].name}'
```

This requires a cross-namespace read grant for RuleSet objects. Platform teams
should create a ClusterRole granting read-only access to RuleSets and bind it
to tenant groups:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: coraza-ruleset-viewer
rules:
  - apiGroups: ["waf.k8s.coraza.io"]
    resources: ["rulesets"]
    verbs: ["get", "list"]
    resourceNames: ["intranet-ruleset"] # optional: scope to specific RuleSets
```

The operator validates `additionalRules[].name` against `spec.library[].name`.
References not in the library are rejected with `RulesValid=False` reason
`RuleNotInLibrary`.

### Adding to the Library

To add a new RuleSource to the library, teams submit a PR to the platform
repository modifying the RuleSet manifest. The platform team reviews the
security impact before merging. This is the only path — there is no
`RuleRequest` CR or self-service library expansion.

## Controller RBAC

The operator's controller ServiceAccount requires the following RBAC to
reconcile RuleSetOverrides across namespaces:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: coraza-operator-controller
rules:
  # Core operator resources (own namespace)
  - apiGroups: ["waf.k8s.coraza.io"]
    resources: ["engines", "rulesets"]
    verbs: ["get", "list", "watch", "update", "patch"]
  - apiGroups: ["waf.k8s.coraza.io"]
    resources: ["engines/status", "rulesets/status"]
    verbs: ["update", "patch"]

  # Cross-namespace override discovery and status updates
  - apiGroups: ["waf.k8s.coraza.io"]
    resources: ["rulesetoverrides"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["waf.k8s.coraza.io"]
    resources: ["rulesetoverrides/status"]
    verbs: ["update", "patch"]

  # Read tenant RuleSources referenced by customExclusions and library
  - apiGroups: ["waf.k8s.coraza.io"]
    resources: ["rulesources", "ruledata"]
    verbs: ["get", "list", "watch"]

  # Resolve targetRef HTTPRoutes in tenant namespaces
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["httproutes"]
    verbs: ["get", "list", "watch"]

  # Resolve Engine's target Gateway for hostname intersection
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways"]
    verbs: ["get", "list", "watch"]

  # Read namespace labels (for allow-raw-seclang gate)
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["get", "list", "watch"]
```

The ClusterRole is bound to the operator's ServiceAccount via a
ClusterRoleBinding. This grants read access to tenant-namespace objects
(HTTPRoutes, RuleSources, RuleSetOverrides) and write access only to status
subresources of RuleSetOverrides.

**Security considerations:**

- The controller never writes to tenant-namespace objects beyond status updates.
- RuleSource content is read but only validated (SecLang parse) — never executed
  by the controller itself. Compiled output is written to the Engine's
  namespace. Most tenants use structured `customExclusions` which never involve
  RuleSource CRs — the operator generates all SecLang internally.
- A compromised RuleSource in a tenant namespace can only affect that tenant's
  WAF evaluation — the operator enforces isolation via mandatory host-wrapping,
  forbidden directive/action validation, and ID rewriting (see
  [Raw SecLang Isolation Guarantees](#raw-seclang-isolation-guarantees)). Config-time
  directives that would affect other tenants or the platform baseline are rejected
  at validation time.

## Metrics and Compliance Monitoring

The operator and WAF engine must expose Prometheus metrics sufficient to
replicate the monitoring capabilities expected of a managed WAF service:
policy compliance verification, malicious traffic telemetry, and per-tenant
visibility.

### WAF Traffic Metrics

The Coraza WASM plugin must emit per-request metrics scoped by Engine
(the bounded tenant boundary):

| Metric                                    | Type      | Labels                                                      | Description                                |
| ----------------------------------------- | --------- | ----------------------------------------------------------- | ------------------------------------------ |
| `coraza_requests_total`                   | Counter   | `engine`, `namespace`, `action` (`block`, `detect`, `pass`) | Total requests evaluated by the WAF        |
| `coraza_rule_matches_total`               | Counter   | `engine`, `namespace`, `rule_id`, `severity`, `action`      | Individual rule match events               |
| `coraza_anomaly_score`                    | Histogram | `engine`, `namespace`                                       | Distribution of anomaly scores per request |
| `coraza_rule_evaluation_duration_seconds` | Histogram | `engine`, `namespace`                                       | Time spent evaluating rules per request    |

> **Cardinality note:** Metrics use `engine` + `namespace` as the tenant
> identifier rather than raw `host` labels. The `engine` label is bounded by
> the number of Engine CRs (platform-controlled), and `namespace` is bounded by
> cluster namespace count. Per-host breakdown is available via log-based queries
> (structured access logs include the `Host` header) for ad-hoc investigation,
> but is not suitable as a Prometheus label in a shared cluster where tenant
> domain count is unbounded. The `rule_id` label on `coraza_rule_matches_total`
> is bounded by the CRS rule count (~400 rules) and does not scale with tenant
> count.

### Compliance Metrics

The operator must emit metrics that enable automated compliance monitoring —
verifying that all Engines and RuleSetOverrides adhere to the mandatory
security baseline:

| Metric                                 | Type  | Labels                               | Description                                                |
| -------------------------------------- | ----- | ------------------------------------ | ---------------------------------------------------------- |
| `coraza_engine_compliance_status`      | Gauge | `engine`, `namespace`                | 1 = compliant, 0 = non-compliant                           |
| `coraza_engine_rules_enforced_total`   | Gauge | `engine`, `namespace`                | Number of mandatory rules actively enforced                |
| `coraza_engine_rules_required_total`   | Gauge | `engine`, `namespace`                | Number of mandatory rules expected                         |
| `coraza_override_status`               | Gauge | `override`, `namespace`, `condition` | Status of each RuleSetOverride condition (1=True, 0=False) |
| `coraza_override_disabled_rules_total` | Gauge | `override`, `namespace`              | Count of baseline rules disabled by this override          |
| `coraza_override_detection_only`       | Gauge | `override`, `namespace`              | 1 = override has secRuleEngine: DetectionOnly active       |

### Compliance Requirements

The following compliance criteria must be enforceable and observable via
metrics:

1. **All mandatory rules enforced.** The baseline rules defined in the parent
   RuleSet's `spec.rules` (e.g., SQLi, XSS, RCE, LFI, RFI, scanner detection,
   session fixation, Java attacks, etc.) must be present in every Engine's
   compiled config. The operator should expose
   `coraza_engine_rules_enforced_total` vs `coraza_engine_rules_required_total`
   to detect gaps. Note: tenants can disable individual rules via
   `disabledRules`, which is tracked by `coraza_override_disabled_rules_total`.

2. **No unacknowledged detection-only overrides.** The Engine itself must have
   `secRuleEngine: On` (blocking mode). Individual tenants may run in
   detection-only mode via `overrides.secRuleEngine: DetectionOnly`, which is
   tracked per-override by `coraza_override_detection_only`. Compliance
   automation monitors this metric and alerts when detection-only mode exceeds
   the allowed onboarding window. Time-boxing is the responsibility of external
   automation, not the operator.

3. **Per-tenant override visibility.** Every `RuleSetOverride` that disables
   baseline rules must be visible in metrics so that compliance automation
   can flag overrides that weaken posture beyond acceptable thresholds.

### Telemetry Dashboard Requirements

The metrics and structured access logs should power dashboards providing:

- **Daily malicious traffic summary** — blocked/detected requests by rule
  category and severity (metric-derived via `coraza_rule_matches_total`).
  Per-tenant hostname breakdown is available via structured access log queries.
- **Top triggered rules** — identify which CRS rules fire most frequently,
  aiding false-positive triage (metric-derived via `coraza_rule_matches_total`).
- **Per-tenant WAF posture** — which overrides are active, which rules are
  disabled, and whether the tenant is in blocking or detection-only mode
  (metric-derived via `coraza_override_status` and
  `coraza_override_detection_only`).
- **Compliance overview** — all Engines and their compliance status at a
  glance, with alerting on drift (metric-derived via
  `coraza_engine_compliance_status`).
- **Per-host traffic analysis** — per-hostname request volume, block rates,
  and anomaly scores (log-derived; not available via Prometheus metrics due to
  unbounded host cardinality).
- **Anomaly score distribution** — per-Engine histogram to identify Engines
  with high anomaly scores that may need threshold tuning (metric-derived via
  `coraza_anomaly_score`).

### Alerting

The following alerts should be derivable from the metrics:

- Engine non-compliant (missing mandatory rules in compiled config)
- RuleSetOverride in detection-only mode beyond allowed onboarding window
- RuleSetOverride disabling more than N baseline rules
- Anomaly score threshold raised beyond platform-defined maximum
- RuleSetOverride in `Accepted=False` state for more than X minutes
- Spike in blocked requests for a specific Engine (potential attack or
  misconfiguration; per-host detail available in logs)

## Open Questions

1. **Audit trail.** How should the operator log which override contributed which
   custom exclusion? This is critical for debugging false-positive chains.

2. **Quota / limits.** Should there be a maximum number of RuleSetOverrides per
   parent RuleSet to prevent unbounded growth?

3. **`ctl:ruleRemoveTargetById` support in coraza-proxy-wasm.** This design
   relies on runtime `ctl:ruleRemoveTargetById` for host-scoped post-CRS
   exclusions. This action is supported by Coraza's engine but needs
   verification in the WASM plugin build. If unsupported, the fallback is
   per-host config splitting (significantly higher complexity).

## Extensibility

This design creates extension points for future work:

- **Path-scoped overrides**: Extend `targetRef` to support specific path matches
  when Istio WasmPlugin gains path-level match support.
- **Additional route types**: Support `GRPCRoute` or `TCPRoute` in `targetRef`
  for non-HTTP workloads.
- **Policy inheritance**: Allow a "parent" RuleSet to be referenced by "child"
  RuleSets, enabling organization-wide baselines with team-level customization.
- **Admission webhook for custom exclusion validation**: Complement the operator's
  `RulesValid` status condition with a validating webhook that rejects invalid
  custom exclusion RuleSources at write time (fail-fast before reconciliation).
- **Per-host metrics via log aggregation**: Provide per-hostname WAF dashboards
  using structured access logs (not Prometheus labels) for tenant-specific
  traffic analysis without cardinality concerns.
- **Disable allowlist**: Add a `spec.disableAllowlist` field to RuleSet that
  restricts which rule IDs tenants may disable. Not needed for MVP — compliance
  monitoring provides sufficient visibility — but useful if policy requires
  hard enforcement of specific rules that must never be disabled.

## Follow-Up Work

- **Verify `ctl:ruleRemoveTargetById` in coraza-proxy-wasm** — confirm the WASM
  build supports this runtime action. If not, file an upstream issue.
- **Prototype RuleSetOverride reconciler** to validate cross-namespace discovery
  performance and correctness with multiple overrides per RuleSet.
- **Implement raw SecLang validation parser** — the forbidden directive/action
  lists and host-wrapping logic defined in the Raw SecLang Isolation Guarantees
  section require a SecLang parser in the operator. Evaluate whether to use
  Coraza's parser library or build a lightweight subset parser.
- **RBAC template** — provide a Kustomize component that platform teams can
  include to grant tenant namespace RBAC for RuleSetOverride (and optionally
  RuleSource for teams requiring the raw SecLang escape hatch).
- **Metrics implementation** — instrument the operator and WASM plugin with the
  Prometheus metrics defined in the Metrics and Compliance Monitoring section.
- **Log-based per-host dashboards** — implement structured access log queries for
  per-hostname WAF visibility (complementing the bounded Prometheus metrics).
- **Documentation** — tenant-facing guide for creating RuleSetOverride CRs with
  examples for common false-positive patterns.

[design-001]: https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/design/001-engine-api-target-ref-and-driver-redesign.md
