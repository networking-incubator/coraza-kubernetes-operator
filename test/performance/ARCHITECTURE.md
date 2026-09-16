# Performance Testing Architecture — Coraza Kubernetes Operator

**Status:** Draft
**Scope:** Performance testing of the Coraza WAF engine and all supporting
components deployed by the Coraza Kubernetes Operator (CKO).
**Audience:** Performance engineers, SRE, operator maintainers.

---

## Table of Contents

- [1. Purpose & Scope](#1-purpose--scope)
- [2. Product Architecture Recap](#2-product-architecture-recap)
- [3. Complete Component Inventory](#3-complete-component-inventory)
  - [3.1 Control-plane components (installed by Helm chart)](#31-control-plane-components-installed-by-helm-chart)
  - [3.2 Runtime resources created by the controllers (per Engine / RuleSet)](#32-runtime-resources-created-by-the-controllers-per-engine--ruleset)
  - [3.3 Data-plane components (the WAF runtime — NOT operator pods)](#33-data-plane-components-the-waf-runtime--not-operator-pods)
  - [3.4 Custom Resources (API surface — config inputs, not runtime processes)](#34-custom-resources-api-surface--config-inputs-not-runtime-processes)
- [4. Components That Require Performance Testing](#4-components-that-require-performance-testing)
  - [Tier 1 — Request hot path (latency & throughput critical)](#tier-1--request-hot-path-latency--throughput-critical)
  - [Tier 2 — Rule delivery path (freshness & scale under change)](#tier-2--rule-delivery-path-freshness--scale-under-change)
  - [Tier 3 — Control-plane scalability (reconcile & compile at scale)](#tier-3--control-plane-scalability-reconcile--compile-at-scale)
  - [Tier 4 — Rule-update propagation (end-to-end freshness)](#tier-4--rule-update-propagation-end-to-end-freshness)
- [5. Test Environment Architecture](#5-test-environment-architecture)
  - [5.1 Full cluster (Path A — OpenShift / K8s)](#51-full-cluster-path-a--openshift--k8s)
  - [5.2 Standalone Envoy (Path B — local process / Docker)](#52-standalone-envoy-path-b--local-process--docker)
- [6. Workload Model](#6-workload-model)
  - [6.1 Traffic profiles](#61-traffic-profiles)
  - [6.2 Payload dimensions (vary independently)](#62-payload-dimensions-vary-independently)
  - [6.3 Ruleset dimensions (the biggest latency lever)](#63-ruleset-dimensions-the-biggest-latency-lever)
  - [6.4 Load patterns](#64-load-patterns)
  - [6.5 Infrastructure profiles (where the WAF runs)](#65-infrastructure-profiles-where-the-waf-runs)
- [7. Metrics & Observability](#7-metrics--observability)
  - [7.1 Data-plane (WAF) metrics — Envoy stats port `:15090`, `coraza_waf_*`](#71-data-plane-waf-metrics--envoy-stats-port-15090-coraza_waf_)
  - [7.2 Control-plane metrics — operator `:8443`, `coraza_*` + controller-runtime](#72-control-plane-metrics--operator-8443-coraza_--controller-runtime)
  - [7.3 Load-generator metrics (client-side ground truth)](#73-load-generator-metrics-client-side-ground-truth)
  - [7.4 Resource metrics](#74-resource-metrics)
- [8. Test Scenarios (matrix)](#8-test-scenarios-matrix)
- [9. Methodology](#9-methodology)
- [10. Tuning Knobs / Independent Variables](#10-tuning-knobs--independent-variables)
- [11. Key Performance Indicators & Acceptance Criteria](#11-key-performance-indicators--acceptance-criteria)
- [12. Confounders & Risks](#12-confounders--risks)
- [13. Deliverables](#13-deliverables)
- [Appendix A — Source references](#appendix-a--source-references)

---

## 1. Purpose & Scope

This document defines the architecture for performance testing the Coraza WAF as
deployed by the Coraza Kubernetes Operator. It:

1. Inventories **every component the operator deploys or creates**, split into
   control plane and data plane.
2. Identifies **which components sit on the request hot path** and therefore
   require latency/throughput testing, versus components that require
   scalability/reconcile testing.
3. Defines the **test environment topology**, the **workload model**, the
   **metrics to collect and their sources**, and the **methodology** for
   producing repeatable, comparable results.

> **Key architectural fact that drives everything below:** the Coraza engine does
> **not** run as a standalone pod. It runs as a **WebAssembly (WASM) module
> (`coraza-proxy-wasm`) inside the Envoy proxy** of each Gateway data-plane pod.
> Every HTTP request to a protected Gateway is evaluated inline by this WASM
> filter. This is the single most performance-critical component in the system.

---

## 2. Product Architecture Recap

CKO is a **two-plane** system (see `docs/driver-metrics-contract.md`):

```
+----------------------------------+         +------------------------------------+
|         CONTROL PLANE            |         |          DATA PLANE                |
|  (Operator manager pod)          |         |  (Envoy + WASM in Gateway pods)    |
|                                  |         |                                    |
|  - RuleSource/RuleSet/Engine     |         |  - Intercepts every HTTP request   |
|    reconcilers                   |         |  - Runs Coraza WAF evaluation      |
|  - Compiles + validates SecLang  |         |  - Blocks / allows / detects       |
|  - RuleSetCache HTTP server      |←─poll───|  - Polls cache for rule updates    |
|    (:18080, in-memory, versioned)|──rules─→|  - Emits coraza_waf_* metrics      |
|  - Applies WasmPlugin via SSA    |         |                                    |
|  Metrics: controller-runtime +   |         |  Metrics: coraza_waf_* on Envoy    |
|  coraza_* on :8443               |         |  stats port :15090                 |
+----------------------------------+         +------------------------------------+
```

**Control flow (rules — how config becomes enforced rules):**

This is control-plane activity: it distributes *configuration* (compiled rules), not
request traffic. The only data-plane step is the final hand-off (inline evaluation).
```
kubectl apply: RuleSource / RuleData / RuleSet / Engine (CRs)
      │
      ▼
RuleSourceReconciler ──► validate SecLang per fragment ──► status: Validated / InvalidRules / ValidationSkipped
      │  (RuleSet gates on RuleSource status before aggregating)
      ▼
RuleSetReconciler ──► aggregate sources + data ──► compile ──► validate ──► check WASM-unsupported ──► version → RuleSetCache (:18080)
      │
      ▼
EngineReconciler ──► RuleSet ready? ──► SSA WasmPlugin + NetworkPolicy + SA token ──► discover matched Gateway pods
      │
      ▼
[hand-off to data plane] coraza-proxy-wasm polls /rules/{ns/name}/latest every
pollIntervalSeconds (default 15s) ──► loads new version ──► inline enforcement
```

**Data flow (request hot path — a single request):**
```
Client ─► Gateway Service ─► Envoy (Gateway pod) ─► [coraza-proxy-wasm WASM filter]
                                                          │
                                    phase 1: request headers
                                    phase 2: request body   ──► allow │ deny(403) │ detect
                                          │ (if allowed)
                                          ▼
                                     Backend (echo/app)
                                          │
                                    phase 3/4: response (response body access is
                                    off by default in the quick-start config)
```

---

## 3. Complete Component Inventory

Everything below is either **installed by the Helm chart** (control plane +
observability) or **created at runtime by the controllers** (per Engine/RuleSet).
Verified against `charts/coraza-kubernetes-operator/templates/` and
`internal/controller/`.

### 3.1 Control-plane components (installed by Helm chart)

| Component | K8s kind | Source | Role |
|---|---|---|---|
| Custom Resource Definitions | `CustomResourceDefinition` (×4: Engine, RuleSet, RuleSource, RuleData) | `crds/*.yaml` | Register the operator's API types; installed via Helm's `crds/` dir (install-only — not upgraded/deleted by Helm) |
| Operator manager | `Deployment` | `deployment.yaml` | Hosts all reconcilers + cache server + metrics |
| RuleSet cache server | HTTP server **inside** the manager process (`:18080`) | `ruleset_controller_cache.go` | Serves compiled rules to WASM plugins |
| Metrics endpoint | HTTPS `:8443` inside manager | `metrics.go` | `coraza_*` + controller-runtime metrics |
| Health/readiness probe | HTTP `:8081` inside manager | manager | Liveness/readiness |
| Operator Service | `Service` | `service.yaml` | Exposes cache + metrics |
| RBAC | `ClusterRole(Binding)`, `Role(Binding)`, `ServiceAccount` | `*role*.yaml`, `serviceaccount.yaml` | Operator permissions |
| Availability | `PodDisruptionBudget` | `poddisruptionbudget.yaml` | HA guarantees |
| Operator NetworkPolicy | `NetworkPolicy` | `networkpolicy.yaml` | Ingress/egress for operator |
| Namespace | `Namespace` | `namespace.yaml` | `coraza-system` |
| Observability | `ServiceMonitor`, `PodMonitor`, `PrometheusRule`, Grafana dashboard `ConfigMap` | `servicemonitor.yaml`, `podmonitor.yaml`, `prometheusrule.yaml`, `grafana-dashboard-configmap.yaml` | Scrape + alert + visualize |

> **CRD lifecycle note:** files under the chart's `crds/` directory are applied by
> Helm **only on first install** — `helm upgrade` does not update them and
> `helm uninstall` does not remove them. When redeploying between test runs,
> apply CRD schema changes out-of-band (e.g. `kubectl apply -f crds/`) or the
> harness will run against a stale API surface.

### 3.2 Runtime resources created by the controllers (per Engine / RuleSet)

| Component | K8s kind | Created by | Role |
|---|---|---|---|
| WASM plugin injection | Istio `WasmPlugin` | `engine_controller_wasm_driver.go` | Loads `coraza-proxy-wasm` into Envoy of matched Gateway pods |
| Gateway→cache access | `NetworkPolicy` | `engine_controller_network_policy.go` | Allows Gateway pods to reach cache `:18080` |
| Cache auth | `ServiceAccount` token | `engine_controller_token.go` | Authenticates WASM plugin to cache server |
| Cache mesh discovery | Istio `ServiceEntry` + `DestinationRule` | `engine_controller_istio_prerequisites.go` (at startup when `--operator-name` set) | Makes cache reachable inside the mesh |

### 3.3 Data-plane components (the WAF runtime — NOT operator pods)

| Component | Where it runs | Role |
|---|---|---|
| **Envoy proxy** | Gateway data-plane pod (`<gateway>-<class>`) | Terminates traffic, hosts filter chain |
| **`coraza-proxy-wasm` module** | WASM VM inside that Envoy | **Runs the Coraza engine — evaluates every request** |
| Cache poller | Inside the WASM module | Polls RuleSetCache for new rule versions |

### 3.4 Custom Resources (API surface — config inputs, not runtime processes)

`Engine`, `RuleSet`, `RuleSource`, `RuleData`, `DriverConfig` (`api/v1alpha1/`).
These are declarative inputs; their performance relevance is **reconcile
throughput and rule-compilation time at scale**, not request-time latency.

---

## 4. Components That Require Performance Testing

Grouped by the *kind* of performance question each answers.

### Tier 1 — Request hot path (latency & throughput critical)

These sit inline on every request. **This is the primary focus.**

Measured in two phases: **(1) establish the baseline (B0)** — run each scenario
with the WAF off first, discard warm-up, and record p50/p95/p99 latency, CPU per
request, and max RPS before saturation; **(2) measure with the WAF on** and report
every metric as the delta vs. B0 (see §9 for the gap calculation).

| # | Component | Why it matters | Primary metrics |
|---|---|---|---|
| 1 | **`coraza-proxy-wasm` (Coraza engine in Envoy)** | Evaluates every request against SecLang/CRS; WASM VM + rule count dominate added latency & CPU | Added p50/p95/p99 latency, CPU per request, max RPS before saturation |
| 2 | **Envoy (Gateway pod) with WAF filter** | The WASM filter adds a filter-chain stage; request body buffering (`SecRequestBodyAccess On`) adds cost | Envoy CPU/mem, connection limits, filter overhead |
| 3 | **Request-body inspection path** | Phase 2 body access buffers and scans payloads; scales with body size | Latency vs. payload size |

### Tier 2 — Rule delivery path (freshness & scale under change)

| # | Component | Why it matters | Primary metrics |
|---|---|---|---|
| 4 | **RuleSetCache HTTP server (`:18080`)** | Every Gateway pod × every Engine polls it (`/latest` + `/rules`); poll load scales with pods × engines / interval | Cache req rate, `coraza_cache_server_request_duration_seconds`, in-flight requests |
| 5 | **WASM cache-poll loop** | Determines rule-update propagation latency and generates steady background load | Time from RuleSet update → enforcement |

### Tier 3 — Control-plane scalability (reconcile & compile at scale)

| # | Component | Why it matters | Primary metrics |
|---|---|---|---|
| 6 | **RuleSetReconciler (compile + validate SecLang)** | CRS-sized rulesets are expensive to compile; large `spec.data`/`@pmFromFile` files add cost | `coraza_ruleset_validation_duration_seconds`, `coraza_cache_set_duration_seconds` |
| 7 | **RuleSourceReconciler** | Per-fragment validation load with many RuleSources | `coraza_rulesource_validation_duration_seconds` |
| 8 | **EngineReconciler** | Time to apply WasmPlugin + discover Gateway pods; scales with #Engines/#Gateways | `controller_runtime_reconcile_*`, `workqueue_depth` |
| 9 | **Operator process (memory)** | Cache is **in-memory** (`--cache-max-size`, default 100 MB); many/large RuleSets pressure it | Operator RSS, `coraza_cache_size_bytes`, GC prune counters |

### Tier 4 — Rule-update propagation (end-to-end freshness)

| # | Component | Why it matters | Primary metrics |
|---|---|---|---|
| 10 | Full path: RuleSet edit → compile → cache → WASM poll → enforcement | SLA for how fast a rule change takes effect fleet-wide | End-to-end update latency (bounded below by `pollIntervalSeconds`) |

> **Out of scope for request-latency testing but tracked:** RBAC objects,
> PDB, NetworkPolicies, ServiceEntry/DestinationRule, ServiceAccount tokens —
> these are set-up-time resources, not hot-path. They are validated for
> *correctness* under scale (do they get created/reconciled in time), not
> per-request latency.

---

## 5. Test Environment Architecture

The two infrastructure profiles (§6.5) each have their own topology: **Path A**
(full cluster — production-representative) and **Path B** (standalone Envoy —
cluster-free smoke / dev).

### 5.1 Full cluster (Path A — OpenShift / K8s)

```
                         ┌──────────────────────────────────────────────┐
                         │           Kubernetes cluster (SUT)           │
                         │                                              │
  ┌────────────┐  load   │  ┌────────────┐   WAF-filtered   ┌────────┐  │
  │  Load gen  │────────►│  │ Gateway pod│─────────────────►│ Backend│  │
  │ (k6/fortio/│  HTTP   │  │  Envoy +   │                  │ (echo/ │  │
  │ wrk/vegeta)│         │  │ coraza-wasm│◄──poll rules──┐  │  app)  │  │
  └────────────┘         │  └────────────┘               │  └────────┘  │
        │                │                          ┌────┴───────────┐  │
        │                │                          │ Operator pod   │  │
        │                │  reconcile Engine/RuleSet│ (control plane)│  │
        │                │  + RuleSetCache :18080   │  cache server  │  │
        │                │                          └────────────────┘  │
        │                │                                              │
        │                │  ┌───────────────────────────────────────┐   │
        │                │  │ Prometheus + Grafana                  │   │
        │                │  │  - ServiceMonitor → operator :8443    │   │
        │                │  │  - PodMonitor → Envoy stats :15090    │   │
        │                │  └───────────────────────────────────────┘   │
        │                └──────────────────────────────────────────────┘
        │
   results/CSV ──► analysis (latency percentiles, RPS, CPU, regressions)
```

**Environment requirements (align with operator support matrix):**

- Kubernetes **v1.32+** (or OpenShift **v4.20+**), Istio + Gateway API CRDs.
- **Dedicated, pinned node pools:** isolate the load generator, the Gateway pods,
  and the backend onto separate nodes so they do not steal CPU from each other.
- **Fixed resource requests/limits** on Gateway and backend pods (CPU-throttling
  invalidates latency numbers — pin CPU or run guaranteed QoS).
- **Load generator lives inside the cluster** (same-AZ node) to keep network
  latency out of the WAF measurement; or a dedicated external node with a
  measured, stable baseline RTT.
- Prometheus retention long enough to cover the whole run at ≤15s scrape.

### 5.2 Standalone Envoy (Path B — local process / Docker)

No Kubernetes, no operator, no Istio. Envoy runs the **exact WASM image the
operator ships**, with rules loaded **statically** in the Envoy config (no cache
server, no auth token, no xDS). Everything runs on one host over a private Docker
network. Lives in `test/performance/standalone/`.

```
        ┌────────────────────────────────────────────────────────┐
        │   Docker Compose network (single host — no cluster)    │
        │                                                        │
   ┌─────────┐  HTTP  ┌─────────────────────┐  proxied ┌───────┐ │
   │ fortio  │───────►│ Envoy                │─────────►│ echo │ │
   │(loadgen)│        │  + coraza-proxy-wasm │          │ back-│ │
   └─────────┘        │  static rules        │          │ end  │ │
        │             │  (no cache/operator) │          └──────┘ │
        │             └─────────────────────┘                    │
        │                (same WASM image the operator ships)    │
        └───────┼────────────────────────────────────────────────┘
                ▼
   results/*.json ──► report.py ──► dashboard.html (latency deltas)
```

**Notes & limits:**

- **Covers Tier 1 only.** No control plane means no RuleSetCache, no cache-poll
  loop, no rule-update propagation, no Istio/xDS injection — so **Tier 2/3/4
  cannot be measured here** (that's Path A).
- **Metrics are client-side** (fortio JSON) plus the Envoy admin `/stats`
  endpoint; there is **no ServiceMonitor / PodMonitor / Prometheus**.
- Envoy configs `envoy/envoy-{baseline,waf,crs}.yaml` select B0 / minimal / CRS.
- **Single host, usually no CPU pinning** (e.g. Docker Desktop) → treat results
  as a **relative / smoke signal**, not a production number (see §6.5 caveat).

---

## 6. Workload Model

### 6.1 Traffic profiles

| Profile | Description | Purpose |
|---|---|---|
| **Benign / passing** | Requests that match no rule (200) | Measure baseline WAF overhead on legitimate traffic (the common case) |
| **Malicious / blocking** | Requests that trigger a deny (403) | Measure cost of the blocking path (short-circuits backend) |
| **Detect-only** | Requests scored but not blocked | Measure full-evaluation cost without backend short-circuit |
| **Mixed** | e.g. 95% benign / 5% malicious | Realistic production mix |

### 6.2 Payload dimensions (vary independently)

- **Body size:** 0 B, 1 KB, 10 KB, 100 KB, 1 MB (exercises `SecRequestBodyAccess`).
- **Header count / query args:** low vs. high (`ARGS`, headers scanned in phases).
- **Method mix:** GET (no body) vs. POST/PUT (body inspection).

### 6.3 Ruleset dimensions (the biggest latency lever)

| Ruleset | Rough size | Represents |
|---|---|---|
| Minimal (quick-start) | ~2 rules | Floor / overhead of the WASM filter itself |
| Medium custom | ~50–100 rules | Typical hand-written policy |
| **OWASP CoreRuleSet (CRS)** | ~700+ rules | Realistic production WAF; dominant cost driver |
| CRS + `@pmFromFile` data | CRS + large data files | Exercises RuleData + memfs path |

### 6.4 Load patterns

Each pattern answers a different question and **produces a different output**:

- **Open-model, fixed-rate steps** (fortio/vegeta): set a fixed arrival rate
  regardless of reply speed; step it up until latency spikes (the **knee**).
  *Produces:* trustworthy latency percentiles p50–p99 (reported as a delta vs. B0;
  the open model avoids coordinated omission) **+ the saturation RPS**. This is the
  source of truth for the primary latency KPI.
- **Closed-model concurrency ramp** (k6 VUs): hold a fixed number of clients that
  each wait for a reply, adding clients until throughput stops rising (the **RPS
  plateau** — detected where more VUs raise p99 but RPS gains fall below ~2–3% and
  Gateway CPU ≈ 100%). *Produces:* max sustainable RPS per pod → **replica & CPU
  capacity sizing**. Cross-check: the plateau RPS should agree with the open-model
  knee.
- **Soak:** hold ~70% of max RPS for 1–4 h. *Produces:* a **memory-leak / GC
  verdict over time** for the Envoy WASM VM and operator cache (pair with the
  ramp + large-body scenarios for the peak-memory high-water mark).
- **Rule-churn:** update RuleSets on an interval during load. *Produces:*
  **rule-update propagation latency + the extra cache-poll load** under traffic.

### 6.5 Infrastructure profiles (where the WAF runs)

The same `coraza-proxy-wasm` filter can be exercised in two very different
environments. Pick the profile by goal; the scenario matrix (§8) names one per row.

| Profile | What it is | Goal | Covers |
|---|---|---|---|
| **Standalone Envoy (Path B)** | Envoy + `coraza-proxy-wasm` as a local process or Docker container, rules loaded **statically** — no operator, no RuleSetCache, no Istio. Same WASM binary the operator ships (`test/performance/standalone/`). | Fast, cheap, cluster-free: **smoke tests, developing/validating the harness itself, and the WASM-overhead floor**; CI-able latency deltas. | **Tier 1 only** (request hot path) |
| **Full cluster (Path A — OpenShift / K8s)** | The real operator-deployed topology from §5: Istio + Gateway + WasmPlugin + RuleSetCache + Prometheus, multi-pod. | **Production-representative, end-to-end** measurement. | **Tiers 1–4** (hot path + cache fan-out + reconcile scale + propagation) |

Rule of thumb: **prove it on Standalone (Path B), confirm it on the cluster (Path A).**
Only the full cluster has a control plane, so the rule-delivery, control-plane-scale,
and propagation scenarios (Tier 2/3/4) **cannot** run on Standalone.

> **Never compare absolutes across profiles.** Deltas are only meaningful **within**
> one profile (WAF-on − WAF-off on the *same* infra). Standalone on a laptop /
> Docker Desktop has no CPU pinning, so its tails are noisy — read trends, not
> absolutes, and treat it as a relative/smoke signal, not a production number.

---

## 7. Metrics & Observability

Two independent metric planes — collect **both**, correlate by timestamp.

### 7.1 Data-plane (WAF) metrics — Envoy stats port `:15090`, `coraza_waf_*`

Scraped via the chart's opt-in **PodMonitor** (`metrics.podMonitor.enabled=true`,
`gatewaySelector`). Per `docs/driver-metrics-contract.md`:

| Metric | Use in perf testing |
|---|---|
| `coraza_waf_requests_total{outcome}` | RPS by outcome (pass/block/detect/error); throughput ground truth |
| `coraza_waf_request_anomaly_score` (histogram) | Confirms rules actually evaluated; correlate score with latency |
| `coraza_waf_rule_hits_total` | Which rules fire (top-N bounded); hot rules driving cost |
| `coraza_waf_plugin_rule_count` (gauge) | Active rule count — the primary latency lever |
| `coraza_waf_plugin_loads_total{status}` | Detect reloads during a run (invalidates a window) |
| `coraza_waf_blocked_requests_total{category}` | Blocking-path validation |

> **Cardinality guard:** the PodMonitor keeps only `coraza_waf_.*` and bounds
> `rule_hits_total` to top-N (N≤200) with `rule_id="other"` overflow. Do **not**
> disable this during load tests — Envoy emits thousands of internal stats.

### 7.2 Control-plane metrics — operator `:8443`, `coraza_*` + controller-runtime

Scraped via **ServiceMonitor** (`metrics.serviceMonitor.enabled=true`):

| Metric | Use in perf testing |
|---|---|
| `coraza_cache_server_request_duration_seconds` | Cache poll latency under pod×engine fan-out |
| `coraza_cache_server_requests_total{handler,code}` | Poll request rate (`rules` vs `latest`) |
| `coraza_cache_server_in_flight_requests` | Cache concurrency saturation |
| `coraza_cache_size_bytes`, `coraza_cache_total_entries` | Cache memory pressure vs. `--cache-max-size` |
| `coraza_cache_gc_pruned_entries_total{reason}` | Eviction under churn/scale |
| `coraza_ruleset_validation_duration_seconds` | Compile cost at scale (Tier 3) |
| `coraza_rulesource_validation_duration_seconds` | Per-source validation cost |
| `coraza_cache_set_duration_seconds` | Cache write cost per RuleSet transition |
| `controller_runtime_reconcile_total`, `workqueue_depth` | Reconcile throughput/backlog under scale |

`honorLabels: true` is set on the ServiceMonitor so `namespace` reflects the CR
namespace — keep this for correct multi-tenant attribution.

### 7.3 Load-generator metrics (client-side ground truth)

- Client-observed latency **percentiles** (p50/p90/p95/p99/p99.9) — the primary
  latency KPI; do not rely on averages.
- Achieved RPS, error rate, timeout rate, HTTP status distribution.
- Use **HDR histograms** (fortio/wrk2/k6) to avoid coordinated omission.

### 7.4 Resource metrics

- Envoy (Gateway pod) CPU/memory (cAdvisor / kube-state-metrics).
- Operator pod CPU/memory (esp. cache RSS).
- Node CPU saturation & throttling counters (rule out noisy-neighbor).

---

## 8. Test Scenarios (matrix)

Each scenario = {infrastructure} × {ruleset} × {traffic profile} × {payload} ×
{load pattern} (dimensions defined in §6.5 / §6.3 / §6.1 / §6.2 / §6.4).
Prioritized subset:

| ID | Scenario | Infra (§6.5) | Ruleset (§6.3) | Traffic (§6.1) | Payload (§6.2) | Load pattern (§6.4) | What it answers |
|---|---|---|---|---|---|---|---|
| B0 | **Baseline, WAF disabled** | Both | none (Engine removed) | benign | GET, 0 B | open, fixed-rate | Reference latency/throughput of the path |
| B1 | **WASM overhead floor** | Both | minimal (~2) | benign | GET, 0 B | open, fixed-rate | Cost of the WASM filter itself |
| L1 | Latency vs ruleset size | Both | minimal → medium → CRS | benign | GET, 0 B | open, fixed-rate | How rule count drives added latency |
| L2 | Latency vs body size | Both | CRS | benign | POST, 1 KB → 1 MB | open, fixed-rate | Cost of `SecRequestBodyAccess` |
| T1 | Max throughput | Both | CRS | benign | GET, 0 B | open max-rate + closed ramp | Knee / saturation RPS with WAF on |
| BLK | Blocking path | Both | CRS | 100% malicious | GET (attack in args) | open, fixed-rate | Cost / behavior of deny path |
| MIX | Realistic mix | Cluster | CRS | 95/5 benign/malicious | mixed GET/POST | open, fixed-rate | Production-representative overhead |
| C1 | Cache fan-out | **Cluster** | CRS | benign | GET, 0 B | open, fixed-rate (steady) | Cache poll load at N pods × M engines |
| C2 | Rule churn under load | **Cluster** | CRS | mixed | GET, 0 B | rule-churn (edits under load) | Propagation latency + poll load |
| S1 | Soak | Both | CRS | benign | GET, 0 B | soak (~70% max, 1–4 h) | Leaks / GC in WASM VM + operator cache |
| SC1 | Control-plane scale | **Cluster** | many RuleSources/Sets/Engines | none | n/a | none (reconcile only) | Reconcile + compile time at scale |
| FP | Failure policy | **Cluster** | CRS, `failurePolicy: fail` vs `allow` | benign | GET, 0 B | open, fixed-rate + inject cache/plugin failure | Latency / availability under WAF failure |

**Infra legend:** *Both* = runnable on **Standalone (Path B)** for a fast smoke/dev
signal **and** on the **Full cluster (Path A)** for the real number. *Cluster* =
requires the operator control plane (cache / reconcile / Istio) and **cannot** run
on Standalone — these are the Tier 2/3/4 scenarios (C1, C2, SC1, FP) and the
multi-pod / cross-plane cases (MIX).

---

## 9. Methodology

1. **Isolate the variable.** Change one dimension per run (ruleset size, body
   size, RPS). Everything else pinned.
2. **Always measure B0 (WAF off) first**, in the same environment, same session.
   Report WAF cost as a **delta vs. B0**, not an absolute — absolute numbers are
   environment-specific and not portable. Compute the gap **per percentile,
   like-for-like** on the median of the repeats: `added pN = pN(WAF on) − pN(WAF
   off)`, reported in **ms** (felt cost) and **% = (on − off) / off** (regression
   budget). Trust a gap only when it exceeds run-to-run variance.
3. **Warm-up then steady-state.** Discard the first N seconds (JIT/WASM VM warm,
   connection pools, cache poll settled). Measure only steady state.
4. **Confirm the WAF actually ran.** Every measured window must show
   non-zero `coraza_waf_requests_total` and expected block counts — otherwise the
   filter was bypassed and the run is invalid.
5. **Pin rule version.** No RuleSet edits during a latency run (except C2/rule-churn
   scenarios). A `coraza_waf_plugin_loads_total` increment mid-window invalidates it.
6. **Repeat ≥3×** per data point; report median of runs + variance. Reject runs
   with node CPU throttling or high client error rate.
7. **Avoid coordinated omission** — use open-model / constant-throughput load
   with HDR histograms (fortio, wrk2, vegeta, k6 with `constant-arrival-rate`).
8. **Correlate planes.** Overlay client latency, Envoy CPU, and cache metrics on
   the same time axis to attribute regressions to a component.

---

## 10. Tuning Knobs / Independent Variables

From `docs/content/reference/operator-cli-flags.md` and the CRDs:

| Knob | Where | Perf effect |
|---|---|---|
| Ruleset size / CRS paranoia level | RuleSource/RuleSet content | **Dominant** latency & CPU driver |
| `SecRequestBodyAccess`, `SecResponseBodyAccess` | RuleSource | Body buffering/scan cost |
| `pollIntervalSeconds` (default 15s) | Engine driver config | Cache poll load ↔ rule-update freshness trade-off |
| `failurePolicy` (`fail`/`allow`) | Engine | Behavior + latency when WAF unavailable |
| Gateway pod replicas | Gateway | Horizontal throughput; multiplies cache poll load |
| Envoy/Gateway CPU & memory limits | Gateway pod | Saturation point; throttling risk |
| `--cache-max-size` / `--cache-max-age` / `--cache-gc-interval` | Operator | Cache memory pressure & eviction under scale |
| WASM image (`--default-wasm-image` / per-Engine) | Engine | Compare driver builds/versions |
| Operator replicas + `--leader-elect` | Operator | Control-plane HA (only one active reconciler) |

---

## 11. Key Performance Indicators & Acceptance Criteria

Define targets per environment; suggested KPI set:

- **Added latency (WAF on − WAF off)** at fixed RPS, p95 and p99. *(Primary.)*
- **Throughput retention:** max RPS with CRS ÷ max RPS at B0.
- **Latency vs. body size** slope (ms per additional KB scanned).
- **Cache poll latency** p99 under target pod×engine fan-out (< a few ms).
- **Rule-update propagation** p95 (should approach `pollIntervalSeconds`).
- **Control-plane:** RuleSet compile p95 for CRS; reconcile queue drains
  (`workqueue_depth`→0) within target at scale.
- **Stability:** no Envoy WASM VM or operator cache memory growth over a 4h soak;
  zero unexpected `plugin_loads` or `outcome="error"`.

Each KPI needs a **baseline capture** and a **regression threshold** (e.g. "p99
added latency must not increase >10% vs. the previous release baseline") so this
harness can gate CI/releases.

---

## 12. Confounders & Risks

- **CPU throttling** on Gateway/backend/load-gen → the #1 source of bogus
  latency spikes. Pin CPU, use guaranteed QoS, watch throttle counters.
- **Coordinated omission** in the load tool → hides tail latency. Use open-model
  generators.
- **Backend as the bottleneck** → make the backend trivially fast (echo) and
  over-provisioned so it never gates; otherwise you measure the app, not the WAF.
- **Load generator saturation** → verify the client isn't the limit (its own CPU).
- **Rule reloads / cache eviction mid-run** → invalidate the window; assert none.
- **WASM VM warm-up** → discard warm-up; keep it consistent across runs.
- **Noisy neighbors / shared nodes** → dedicate and pin node pools.
- **Network path variance** → keep the load generator in-cluster or on a fixed,
  measured link.

---

## 13. Deliverables

1. Reusable test harness (load profiles + scenario configs + result collection)
   under `test/performance/` — Path A (`scripts/`) and Path B (`standalone/`).
2. Per-run reports: a **self-contained HTML report** (`report.py` →
   `dashboard.html`, no live backend) that bakes in the run's own data — client
   latency percentiles + delta vs. B0, and (Path A) the server-side series pulled
   by `collect.sh` (`coraza_waf_*`, cache, Envoy/operator resources) — plus the
   environment fingerprint (versions, ruleset, image digest). This is the
   **durable artifact**; cross-run comparison (§9) is done report-to-report, not
   on a live dashboard.
3. A committed **baseline** per ruleset (minimal, CRS) for regression gating.
4. **Live Grafana views** (Path A, optional) correlating `coraza_waf_*` ↔
   cache ↔ Envoy/operator resources for **real-time, per-run attribution** while
   Prometheus is up. Grafana stores nothing — it queries Prometheus — so it is a
   *live* view only; the frozen, portable record is the self-contained HTML in #2
   (or a Grafana **snapshot** if a Grafana-native frozen view is wanted).

---

## Appendix A — Source references

| Topic | Doc / code |
|---|---|
| Two-plane split, data-plane metric contract | `docs/driver-metrics-contract.md` |
| Controllers, cache server, Istio prerequisites | `docs/content/explanation/architecture.md` |
| WASM integration, poll loop, target selection | `docs/content/explanation/istio-wasm-integration.md` |
| Rule aggregation/compile/validate lifecycle | `docs/content/explanation/rule-processing.md` |
| Operator metrics & cardinality | `docs/content/reference/metrics-cardinality.md` |
| CLI flags / cache tuning | `docs/content/reference/operator-cli-flags.md` |
| Deployed components (chart) | `charts/coraza-kubernetes-operator/templates/` |
| Runtime resources (controllers) | `internal/controller/engine_controller_*.go` |
```
