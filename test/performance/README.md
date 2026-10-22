# Coraza WAF Performance Testing

Harness for measuring what the WAF actually costs: the `coraza-proxy-wasm`
module running **inside the Envoy proxy of a Gateway pod**, driven by fortio
**from inside the cluster**.

The headline number this harness produces is never an absolute latency. It is
the **delta against a WAF-off baseline captured in the same session** — see
[Methodology](#methodology).

---

## Table of contents

- [Quick start](#quick-start)
- [Architecture](#architecture)
- [The three axes](#the-three-axes)
- [Components under test](#components-under-test)
- [Running tests](#running-tests)
- [Methodology](#methodology)
- [Results and reporting](#results-and-reporting)
- [Metrics and observability](#metrics-and-observability)
- [KPIs and acceptance criteria](#kpis-and-acceptance-criteria)
- [Confounders and risks](#confounders-and-risks)
- [Configuration knobs](#configuration-knobs)
- [Customization](#customization)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)
- [File structure](#file-structure)
- [References](#references)

---

## Quick start

Regenerate the matrix report from results already in `results/` — no cluster
needed:

```bash
python3 generate-report.py
open results/performance-report.html
```

Run the full 3×4 matrix against a live cluster (operator and Istio already
installed):

```bash
scripts/setup.sh              # namespace, backend, loadgen, Gateway, payloads
./run-comprehensive-tests.sh  # 12 runs, then generates the report
scripts/teardown.sh           # optional; results/ is kept
```

`run-comprehensive-tests.sh` takes roughly **45–55 minutes**: 12 runs ×
(10s warm-up + 120s measured + 25s settle + overhead), plus a 15s pause between
profiles.

---

## Architecture

### Product: two planes

```
+----------------------------------+         +------------------------------------+
|         CONTROL PLANE            |         |          DATA PLANE                |
|  (Operator manager pod)          |         |  (Envoy + WASM in Gateway pods)    |
|                                  |         |                                    |
|  - RuleSource/RuleSet/Engine     |         |  - Intercepts every HTTP request   |
|    reconcilers                   |         |  - Runs Coraza WAF evaluation      |
|  - Compiles + validates SecLang  |         |  - Blocks / allows / detects       |
|  - RuleSetCache HTTP server      |<--poll--|  - Polls cache for rule updates    |
|    (:18080, in-memory, versioned)|--rules->|  - Emits coraza_waf_* metrics      |
|  - Applies WasmPlugin via SSA    |         |                                    |
|  Metrics: controller-runtime +   |         |  Metrics: coraza_waf_* on Envoy    |
|  coraza_* on :8443               |         |  stats port :15090                 |
+----------------------------------+         +------------------------------------+
```

The Coraza engine runs as a **WebAssembly module inside Envoy**. Every HTTP
request is evaluated inline by that filter, which makes it the most
performance-critical component in the system.

### Execution model — what runs where

The harness is **split**: orchestration is local, measurement is in-cluster.
Nothing of the harness itself is installed into the cluster — there is no test
controller, operator, or CI agent. The scripts are ordinary bash running on your
workstation, and every cluster interaction is a `kubectl`/`oc` call against the
API server, so a remote cluster works exactly like a local one.

| Runs on your workstation | Runs inside the cluster |
|---|---|
| `scripts/*.sh`, `run-comprehensive-tests.sh` — orchestration | `echo` backend Deployment — the target |
| `generate-report.py`, `scripts/report.py` — reporting | `loadgen` Deployment (fortio server) — validity probes and the discarded warm-up, driven by `kubectl exec` |
| `results/` — all raw JSON, summaries, HTML | **one-off fortio Job per measured run** — the actual measurement |
| `curl` against a port-forwarded Prometheus (`collect.sh`) | Gateway pod (Envoy + `coraza-proxy-wasm`), operator pod, Prometheus |

**The measured traffic never leaves the cluster.** The Job hits the Gateway over
cluster-internal service DNS (`TARGET_URL`), so no part of the measured path
crosses your laptop's network. Your workstation only issues control calls and
collects results afterwards; its latency and link quality do not enter the
numbers.

Results come back out of the cluster with `kubectl logs job/<name>` once the Job
has finished — a single bounded fetch, not a live stream — and are parsed into
`results/` locally. `collect.sh` reaches Prometheus by `kubectl port-forward` to
`localhost` and queries it with `curl` from there.

### Test topology

```
  +-----------------------------------+
  |         your workstation          |
  |  scripts/*.sh   (orchestration)   |
  |  *.py           (reporting)       |
  |  results/       (raw JSON + HTML) |
  +-----------------+-----------------+
                    |
                    | kubectl / oc: apply CRs, wait, create Job,
                    | exec probes, read logs, port-forward Prometheus
                    v
  +-------------------------------------------------------------------+
  |                     Kubernetes cluster (SUT)                       |
  |                                                                    |
  |  +---------------+   +---------------+                             |
  |  | loadgen pod   |   | fortio Job    |   <- measured load, no mesh |
  |  | probes +      |   | (one per run) |                             |
  |  | warm-up       |   +-------+-------+                             |
  |  +-------+-------+           |                                     |
  |          |    in-cluster HTTP (TARGET_URL)                         |
  |          +-----------+-------+                                     |
  |                      v                                             |
  |             +------------------+  WAF-filtered  +---------------+  |
  |             |   Gateway pod    |--------------->| echo backend  |  |
  |             | Envoy +          |                +---------------+  |
  |             | coraza-proxy-wasm|                                   |
  |             +--------+---------+                                   |
  |                      ^                                             |
  |                      | WasmPlugin (SSA) + optional rule poll       |
  |             +--------+---------+                                   |
  |             |   Operator pod   |                                   |
  |             | control plane +  |                                   |
  |             | RuleSetCache     |                                   |
  |             +------------------+                                   |
  |                                                                    |
  |  +--------------------------------------------------------------+  |
  |  | Prometheus + Grafana (optional)                              |  |
  |  |   ServiceMonitor -> operator :8443                           |  |
  |  |   PodMonitor     -> Envoy stats :15090                       |  |
  |  +--------------------------------------------------------------+  |
  +-------------------------------------------------------------------+
```

### Why a Job, and why outside the mesh

The measured load runs as a **one-off Kubernetes Job** rather than a long-lived
`kubectl exec`. Some client↔API-server paths reset multi-minute exec streams at
around 120s with zero bytes in flight, which aborted every run at the default
duration. Polling Job status uses short independent API calls that need no
connection held open for the length of the run.

The Job is annotated `sidecar.istio.io/inject: "false"`, matching the loadgen
Deployment. An injected sidecar would add its own latency and CPU to every
measured request, and would also stop the Job from ever reporting success, since
`istio-proxy` does not exit on its own.

### Rule control flow

```
kubectl apply: RuleSource / RuleData / RuleSet / Engine
      |
      v
RuleSourceReconciler --> validate SecLang --> status: Validated / InvalidRules
      |
      v
RuleSetReconciler --> aggregate + validate --> RuleSetCache (in-memory)
      |
      v
EngineReconciler --> WasmPlugin (SSA) --> Istio injects WASM into Gateway pods
      |
      v
HTTP request --> WASM filter evaluates rules --> allow / block / detect
```

By default the Engine **embeds rules statically** — the least fragile mode and
the correct one for request-hot-path latency. Set `POLL_CACHE=true` to inject
`ruleSetCacheServer` and exercise the cache-poll model instead; that path
requires the operator's cache prerequisites.

### Environment requirements

- Kubernetes **v1.32+** or OpenShift **v4.20+**, with Istio and the Gateway API CRDs
- Operator already installed — **this harness does not install it**
- Local tools: `kubectl` (or `oc`), `envsubst`, `python3`, `curl`
- **Dedicated node pools** for load generator, Gateway, and backend
- **CPU requests = limits** (guaranteed QoS) to avoid throttling
- Optional: Prometheus with retention ≥1h at ≤15s scrape interval

Istio installation is out of scope for this harness; use `hack/install-istio.sh`
(detects the platform and installs Sail Operator on OpenShift, upstream Istio on
Kubernetes) or see [DEVELOPMENT.md](../../DEVELOPMENT.md).

---

## The three axes

A test is three independent choices. Keeping them separate is what makes runs
comparable.

```
scenario (how hard)  x  load profile (request shape)  x  ruleset (which rules)
fixed-500qps            args-heavy                       crs
```

### Scenario — load intensity

Sourced env fragments in `scenarios/`. They set **only** QPS, duration, and
connection count, and say nothing about request shape.

| File | QPS | Duration | Conns | Purpose |
|---|---|---|---|---|
| `fixed-500qps.env` | 500 | 120s | 16 | Primary latency KPI, sub-saturation |
| `low-rate-200qps.env` | 200 | 120s | 16 | Expensive shapes that would saturate the link at 500 |
| `max-throughput.env` | 0 (unthrottled) | 60s | 16 | Find the saturation knee |
| `soak.env` | 300 | 3600s | 16 | Leaks, GC behaviour, drift |
| `fixed-500qps-short.env` | 500 | 60s | 16 | Shorter variant; **not** what the matrix script uses |

Latency from `max-throughput` is not comparable to fixed-rate runs.

### Load profile — request shape

Sourced env fragments in `manifests/load-profiles/`, selected with
`LOAD_PROFILE`. Sourced **after** the scenario, so the profile wins on overlap.
They control `METHOD`, `REQ_PATH`, `CONTENT_TYPE`, `PAYLOAD_FILE`, `PAYLOAD`,
`PAYLOAD_SIZE`, `HEADERS`, and `EXPECT_STATUS`.

The three the matrix uses:

| Profile | Shape | Exercises |
|---|---|---|
| `baseline-get` | `GET /`, no args, no body | Header/URI-phase rules only — the WAF's cheapest path |
| `args-heavy` | `GET /search?…` with 25 query args | The 165 CRS `ARGS` rules, ~25× per-arg work |
| `headers-heavy` | `GET /` with 15 extra headers | Phase-1 `REQUEST_HEADERS` rules |

Nine more exist (`args-light`, `json-1kb`, `json-100kb`, `form-urlencoded`,
`multipart`, `body-1kb`, `body-100kb`, `attack-sqli`, `attack-keyword`) — see
[`manifests/load-profiles/README.md`](manifests/load-profiles/README.md). The
four that post a body from a file need `gen-payloads.sh` and `setup.sh` to have
built the `perf-payloads` ConfigMap first.

**Why shape matters:** CRS evaluates most detection rules **once per argument**.
Of the 563 rules in the generated CRS ruleset, **165 reference `ARGS`** and
**16 reference `REQUEST_BODY`**. A bare `GET /` hands them an empty collection,
so they cost almost nothing. Measuring only `GET /` reports the WAF's **best
case**, not a representative one.

### Ruleset — which rules

Committed under `manifests/rulesets/`, resolved by bare name
(`waf.sh on <name>`) or by path. Rule counts are parsed from the manifests at
report time, not hardcoded.

| Ruleset | Rules | Represents |
|---|---|---|
| `off` | 0 | No Engine attached — the B0 baseline, same Gateway/Envoy path with no WASM filter |
| `minimal` | 1 | Floor: cost of the WASM filter itself |
| `medium` | 30 | Typical hand-written policy |
| `crs` | 563 | OWASP CoreRuleSet 4.7.0 — the dominant latency driver |

`off` is not a ruleset file. `waf.sh off` **deletes the Engine**, so traffic
flows through the identical Gateway path with no filter attached.

### The matrix

`run-comprehensive-tests.sh` runs 3 profiles × 4 rulesets = **12 combinations**,
all at `fixed-500qps`:

```
                    +---------+---------+--------+-----+
                    |   off   | minimal | medium | crs |
+-------------------+---------+---------+--------+-----+
| baseline-get      | Test 1  | Test 2  | Test 3 | T 4 |
+-------------------+---------+---------+--------+-----+
| args-heavy        | Test 5  | Test 6  | Test 7 | T 8 |
+-------------------+---------+---------+--------+-----+
| headers-heavy     | Test 9  | Test 10 | Test 11| T12 |
+-------------------+---------+---------+--------+-----+
```

### Detection-only vs blocking

The matrix rulesets use `SecRuleEngine DetectionOnly` so that **every rule is
evaluated** and nothing short-circuits. This is what makes the comparison
apples-to-apples.

In blocking mode (`SecRuleEngine On`) a request denied at rule 8 never
evaluates rules 9–30, so a 30-rule policy can look *faster* than a 2-rule one.
That is an artifact of early exit, not a real result. Use blocking mode only
when you specifically want the cost of the deny path, via the `attack-*`
profiles, which declare `EXPECT_STATUS=403` and are validated automatically.

---

## Components under test

### Tier 1 — request hot path (latency critical)

| Component | Where | Measurement |
|---|---|---|
| Coraza WASM filter | Gateway pod (Envoy) | Added latency per request |
| Rule evaluation | WASM filter | p50–p99.9 by ruleset size and request shape |
| Istio overhead | Gateway pod | B0 baseline (WAF off) |

### Tier 2 — rule delivery path

| Component | Where | Measurement |
|---|---|---|
| RuleSetCache | Operator pod (:18080) | Cache hit rate, memory usage |
| Poll loop | Gateway WASM | Poll frequency vs freshness |

### Tier 3 — control-plane scalability

| Component | Where | Measurement |
|---|---|---|
| RuleSource reconciler | Operator | Validation time per rule count |
| RuleSet reconciler | Operator | Compilation time, memory usage |
| Engine reconciler | Operator | WasmPlugin apply time |

### Tier 4 — rule-update propagation

| Component | Where | Measurement |
|---|---|---|
| End-to-end freshness | CR update → Gateway enforcement | Total propagation latency |

Tiers 2 and 4 require `POLL_CACHE=true`; the default static-embedded mode
bypasses the cache entirely.

---

## Running tests

### Provision the target

```bash
scripts/setup.sh
```

Creates the `waf-perf` namespace, builds the `perf-payloads` ConfigMap from
`manifests/load-profiles/payloads/`, deploys the `echo` backend and `loadgen`
deployment, renders `manifests/gateway.yaml` through `envsubst`, waits for
everything to be ready, and smoke-tests loadgen → gateway. It does **not**
enable the WAF.

Apply the manifests through the scripts, not with raw `kubectl apply`:
`gateway.yaml` and `engine.yaml` contain `envsubst` variables and would be
applied with literal `${ENGINE_NAME}` placeholders.

### Run the canonical comparison

`suite.sh` is the canonical entry point. It measures WAF-off first, then once
per ruleset:

```bash
LOAD_PROFILE=args-heavy scripts/suite.sh scenarios/fixed-500qps.env minimal medium crs
```

For the full 12-cell matrix plus report:

```bash
./run-comprehensive-tests.sh
```

### Single run

```bash
LOAD_PROFILE=baseline-get scripts/run.sh scenarios/fixed-500qps.env waf-crs
```

`run.sh` probes before measuring: a benign `GET /`, an attack `GET /?q=attack`,
and — when `LOAD_PROFILE` is set — one request of the profile's **exact shape**
compared against `EXPECT_STATUS`. A mismatch aborts in about a second rather
than measuring rejected traffic as though it were real work.

### Toggle the WAF manually

```bash
scripts/waf.sh on medium     # apply ruleset + Engine, wait for Ready
scripts/waf.sh off           # delete the Engine (B0 baseline)
scripts/waf.sh status        # show engine, ruleset, rulesource, wasmplugin
```

### Collect server-side metrics

```bash
scripts/collect.sh <start_epoch> <end_epoch> <tag>
# or, for a whole suite:
COLLECT=true LOAD_PROFILE=args-heavy scripts/suite.sh scenarios/fixed-500qps.env crs
```

`run.sh` prints `window_utc=<start>..<end>` for exactly this purpose.

### Tear down

```bash
scripts/teardown.sh   # deletes the namespace; results/ is retained
```

---

## Methodology

`suite.sh` and `run.sh` encode most of this. Preserve it, and enforce the rest
by hand.

1. **Isolate the variable.** Change one dimension per run — ruleset size, body
   size, or RPS. Pin everything else.
2. **Measure B0 (WAF off) first, same session, same environment.** Report WAF
   cost as a delta, never an absolute: absolute latency is not portable across
   clusters. Compute it per percentile, like-for-like, on the median of the
   repeats — `added pN = pN(WAF on) − pN(WAF off)` — in **ms** (felt cost) and
   **% = (on − off) / off** (regression budget).
3. **Trust a gap only when it exceeds run-to-run variance.** A delta smaller
   than the WAF-off spread is noise, not signal. Report the spread alongside
   the median.
4. **Warm up, then measure steady state.** `run.sh` discards a `WARMUP`
   (default 10s) run before the measured one.
5. **Settle after toggling.** After `waf.sh on|off`, wait for Istio to apply or
   remove the WasmPlugin — plus one poll interval (~15s) if the cache is
   enabled. `suite.sh` uses `SETTLE=25s`; `run-comprehensive-tests.sh` uses 30s.
   Never measure inside the settle window.
6. **Confirm the WAF actually ran.** Every measured window must show non-zero
   `coraza_waf_requests_total` and the expected block counts. A bypassed filter
   makes the run invalid, not merely optimistic.
7. **Pin the rule version.** No RuleSet edits during a latency run. A
   `coraza_waf_plugin_loads_total` increment mid-window invalidates it.
8. **Repeat ≥3× per data point** and report median plus variance. The scripts do
   not loop — you must re-invoke them. One run is noise.
9. **Avoid coordinated omission.** Use open-model, constant-throughput load with
   HDR histograms. Fortio qualifies; `-qps` is an open model.
10. **Reject bad runs.** Discard any run with node CPU throttling, elevated
    `error_pct`, or an `ActualQPS` that misses the requested rate (see below).
11. **Correlate planes.** Overlay client latency, Envoy CPU, and cache metrics
    on one time axis to attribute a regression to a component.

### Validity criteria

A run is valid when:

- `error_pct` is 0 — all 2xx, except `attack-*` profiles where 403 is expected
  and counted separately as `blocked_pct`
- `ActualQPS` is within ~5% of the requested rate
- The shape probe returned `EXPECT_STATUS`
- No plugin reload occurred inside the window

**An `ActualQPS` shortfall is itself a result** — it means the system could not
sustain the offered load — but it invalidates the *latency* comparison for that
cell, because the percentiles then describe a different, lighter workload than
the baseline they are compared against. Report it as a throughput finding and
re-run the latency cell at a rate the system can hold.

---

## Results and reporting

### Raw output

Each run writes two files to `results/`:

```
results/<scenario>+<profile>__<tag>__<UTC timestamp>.json         # raw fortio
results/<scenario>+<profile>__<tag>__<UTC timestamp>.summary.txt  # parsed line
```

Folding the profile into the scenario name keeps runs with different request
shapes in separate groups so they are never pooled into one median.

The summary carries `actual_qps`, `count`, `avg/min/max`, `p50…p99.9`,
`ret_codes`, `blocked_pct`, `error_pct`, and the `window_utc` range.

Fortio JSON, abridged:

```json
{
  "DurationHistogram": {
    "Count": 60000,
    "Avg": 0.00347,
    "Percentiles": [
      {"Percentile": 50,   "Value": 0.00280},
      {"Percentile": 95,   "Value": 0.00819},
      {"Percentile": 99,   "Value": 0.01631},
      {"Percentile": 99.9, "Value": 0.03300}
    ]
  },
  "ActualQPS": 500.0,
  "RetCodes": {"200": 60000}
}
```

### Two report generators

| Generator | Scope | Output |
|---|---|---|
| `scripts/report.py` | One scenario group; medians across repeats; delta vs B0; folds in `collect.sh` metrics | `results/<scenario>.html`, run automatically by `suite.sh` |
| `generate-report.py` | The full 3×4 matrix | `results/performance-report.html` |

Both are self-contained HTML and need no cluster, Prometheus, or Grafana to
view. `report.py` is the one that computes the **delta vs B0 with repeats** —
prefer it for anything you intend to report.

`generate-report.py` pipeline:

```
Fortio JSON -> parse_result() -> results{(profile, config)} -> prepare_chart_data()
            -> DATA object -> templates/report-template.html -> performance-report.html
```

It picks the **latest** result per `(profile, config)` pair, so re-running one
cell updates just that cell. Rule counts come from
`resolve_rule_counts()` reading the manifests, and the key findings are
generated from the data rather than written by hand.

Sections: Architecture, Scenario, Profiles, WAF Rulesets, Test Matrix,
Performance Results (7 charts: latency by profile, CRS overhead, latency
distribution, complexity impact, rules efficiency, tail-latency gap, p95
heatmap), and Findings.

### Reading the numbers

Do not quote figures from this README or from any previous run's prose. Regenerate
the report and read the current data. Latency is environment-specific, and the
only portable quantity is the delta against a B0 captured in the same session.

For a quick textual delta without HTML:

```bash
grep -H . results/<scenario>__*.summary.txt
```

Then compute `added pN = pN(waf-*) − pN(waf-off)` on the median of repeats.
Prefer p95/p99; never headline the average.

---

## Metrics and observability

### Data plane (WAF)

Envoy stats port `:15090`, prefix `coraza_waf_*`, scraped by a PodMonitor.

- `coraza_waf_transactions_total` — requests processed
- `coraza_waf_interruptions_total` — blocked requests
- `coraza_waf_transaction_duration_seconds` — rule evaluation time
- `coraza_waf_plugin_loads_total` — a mid-window increment invalidates the run

### Control plane

Operator `:8443`, prefix `coraza_*` plus controller-runtime, scraped by a
ServiceMonitor.

- `controller_runtime_reconcile_total`
- `controller_runtime_reconcile_time_seconds`
- `coraza_ruleset_cache_size_bytes`
- `workqueue_depth` — should drain to 0 at scale

### Load generator

Client-side ground truth, from the fortio JSON: latency percentiles, actual QPS,
status-code distribution, request count.

### Resources

Gateway pods (Envoy + WASM), operator pod, backend pods — CPU, memory, WASM heap
size, and CPU throttle counters.

---

## KPIs and acceptance criteria

Targets are environment-specific; each KPI needs a **baseline capture** and a
**regression threshold** (for example, "p99 added latency must not rise more
than 10% against the previous release baseline") before this harness can gate CI.

- **Added latency (WAF on − WAF off)** at fixed RPS, p95 and p99 — *primary*
- **Throughput retention:** max RPS with CRS ÷ max RPS at B0
- **Latency vs body size** slope — ms per additional KB scanned
- **Cache poll latency** p99 under target pod × engine fan-out
- **Rule-update propagation** p95 — should approach `pollIntervalSeconds`
- **Control plane:** RuleSet compile p95 for CRS; reconcile queue drains
- **Stability:** no WASM VM or cache memory growth over a 4h soak; zero
  unexpected plugin loads or `outcome="error"`

---

## Confounders and risks

- **CPU throttling** on Gateway, backend, or load generator — the single largest
  source of bogus latency spikes. Pin CPU, use guaranteed QoS, watch throttle
  counters.
- **Coordinated omission** in the load tool hides tail latency. Use open-model
  generators.
- **Backend as the bottleneck.** It is a trivially fast, over-provisioned echo
  so it never gates. If it saturates you are measuring the app, not the WAF.
- **Load generator saturation** — verify the client's own CPU is not the limit.
- **Rule reloads or cache eviction mid-run** invalidate the window; assert none.
- **WASM VM warm-up** — discard it, and keep it consistent across runs.
- **Noisy neighbours** on shared nodes — dedicate and pin node pools.
- **Network path variance** — keep the generator in-cluster or on a fixed link.

---

## Configuration knobs

Environment variables; defaults in `lib.sh`.

| Var | Default | Meaning |
|---|---|---|
| `PLATFORM` | `kubernetes` | `kubernetes` or `openshift` — selects CLI and GatewayClass |
| `CLI` | `kubectl` / `oc` | Cluster CLI, derived from `PLATFORM` |
| `NAMESPACE` | `waf-perf` | Test namespace |
| `GATEWAY_NAME` | `perf-gateway` | Gateway name |
| `GATEWAY_CLASS` | `istio` / `openshift-default` | GatewayClass, derived from `PLATFORM` |
| `ENGINE_NAME` | `perf-engine` | Engine name |
| `RULESET_NAME` | `perf-ruleset` | RuleSet the Engine attaches to |
| `TARGET_URL` | in-cluster gateway svc | What the generator hits |
| `LOADGEN_IMAGE` | `fortio/fortio:latest` | Load generator image |
| `WARMUP` | `10s` | Discarded warm-up per run |
| `SETTLE` | `25s` | Post-toggle wait (`suite.sh`) |
| `COLLECT` | `false` | Also pull Prometheus metrics per run |
| `FAILURE_POLICY` | `fail` | Engine failure policy (`fail` or `allow`) |
| `POLL_CACHE` | `false` | Inject `ruleSetCacheServer` (cache-poll model) |
| `POLL_INTERVAL_SECONDS` | `15` | Poll cadence when `POLL_CACHE=true` |
| `EXPECT_STATUS` | `200` | Status the shape probe must see |
| `EXPECT_BLOCK` | unset | Assert the attack probe is (`true`) or isn't (`false`) blocked |
| `EXPECT_BENIGN_STATUS` | unset | Assert the benign probe's status |
| `WAIT_TIMEOUT` | `180s` | Readiness wait timeout |
| `RESULTS_DIR` | `./results` | Where results are written |
| `PROM_NAMESPACE` / `PROM_SVC` / `PROM_PORT` | `monitoring` / `prometheus-k8s` / `9090` | Prometheus location |

Scenario files set `QPS`, `DURATION`, and `CONNS`. Profiles set `METHOD`,
`REQ_PATH`, `CONTENT_TYPE`, `PAYLOAD_FILE`, `PAYLOAD`, `PAYLOAD_SIZE`,
`HEADERS`, and `EXPECT_STATUS`.

Operator-side knobs that move the numbers: ruleset size and CRS paranoia level
(dominant), `SecRequestBodyAccess` / `SecResponseBodyAccess`,
`pollIntervalSeconds`, `failurePolicy`, Gateway replicas and CPU limits,
`--cache-max-size` / `--cache-max-age` / `--cache-gc-interval`, and the WASM
image.

---

## Customization

### New load profile

```bash
cp manifests/load-profiles/baseline-get.env manifests/load-profiles/my-profile.env
```

Edit the shape, then use it with `LOAD_PROFILE=my-profile`. To include it in the
matrix report, add it to `profiles` in `generate-report.py:1181`.

A profile that needs a body file also needs `gen-payloads.sh` and a re-run of
`setup.sh` so the payload lands in the ConfigMap — and see
[known limitations](#known-limitations).

### New ruleset

```bash
cp manifests/rulesets/minimal.yaml manifests/rulesets/my-ruleset.yaml
```

Use it with `waf.sh on my-ruleset`. Add it to `configs` in
`generate-report.py:1182` and to `RULESET_FILES` so its rules are counted.

### New scenario

Copy any file in `scenarios/` and set `QPS`, `DURATION`, `CONNS`. If you change
the scenario *name*, update the glob in `find_latest_result()`
(`generate-report.py:53`), which is currently hardcoded to `fixed-500qps+`.

### Regenerating CRS

Only needed to bump the CRS version. From the repo root:

```bash
curl -sL https://github.com/coreruleset/coreruleset/archive/refs/tags/v4.7.0.tar.gz | tar -xz -C /tmp
kubectl coraza generate coreruleset \
  --rules-dir /tmp/coreruleset-4.7.0/rules --version 4.7.0 \
  --ruleset-name perf-ruleset \
  --ignore-unsupported-rules wasm \
  --ignore-rules 920420 \
  > test/performance/manifests/rulesets/crs.yaml
```

Two things are hand-added to the generated file and must be re-added afterwards,
both explained in its header comment:

- the `skip-unsupported-rules-check` annotation on the RuleSet (CRS 980170)
- `--ignore-rules 920420` — under coraza-proxy-wasm that rule rejects **every**
  request carrying a `Content-Type`, so all POST traffic 403s, even when the
  content type is in the allowlist

### New chart

Add a `<canvas id="myChart">` to `templates/report-template.html`, build the
config in `prepare_chart_data()`, return it under a `myChart` key, and
initialize it in the template.

---

## Known limitations

- **The matrix is a single repeat per cell.** The methodology calls for ≥3.
  `run-comprehensive-tests.sh` runs each combination once and says so in its
  banner, so its output is indicative, not reportable, until you re-run it and
  take medians.
- **`PAYLOAD_FILE` profiles need the ConfigMap in place.** The measured-run Job
  mounts `perf-payloads` at `/payloads`, matching the loadgen pod, but the mount
  is `optional` — so a profile whose payload was never built would send an empty
  body. `run.sh` guards against this by checking the key exists in the ConfigMap
  before measuring, and aborts with the `gen-payloads.sh` + `setup.sh` fix.
- **`generate-report.py` is hardcoded to the `fixed-500qps` scenario** in
  `find_latest_result()`.

---

## Troubleshooting

### Missing data in the report

```bash
ls -1 results/fixed-500qps+*.json | wc -l      # expect 12+
ls -lh results/fixed-500qps+*.json             # empty files are skipped
python3 -m json.tool results/fixed-500qps+baseline-get__waf-off__*.json | head
```

### Unexpected 403s

The matrix expects 0% errors. 403s mean the WAF is blocking rather than
detecting:

```bash
grep "SecRuleEngine" manifests/rulesets/*.yaml   # expect DetectionOnly
kubectl get rulesets -n waf-perf -o yaml
scripts/waf.sh status
```

For `attack-*` profiles 403 is the expected status and is reported as
`blocked_pct`, not `error_pct`.

### Run aborts on the shape probe

The profile's own request shape returned something other than `EXPECT_STATUS`.
Either the WAF is blocking that shape — CRS 920420 against any `Content-Type`
is the usual culprit — or the target is misconfigured. Set `EXPECT_STATUS` in
the profile if the status is intended.

### Run aborts: Job did not complete

The measured-run Job exceeded `DURATION + 90s`:

```bash
kubectl logs -n waf-perf job/<job-name>
kubectl get pods -n waf-perf
```

### Pods not ready

```bash
kubectl get pods -n waf-perf
kubectl logs -n waf-perf deployment/loadgen
kubectl logs -n waf-perf deployment/echo
```

The backend deployment is named `echo`, not `backend`.

### Payload missing from the ConfigMap

```bash
manifests/load-profiles/gen-payloads.sh && scripts/setup.sh
```

### Inconsistent results across runs

Usually CPU throttling, network variance, or noisy neighbours. Pin Gateway pods
to dedicated nodes, set CPU requests equal to limits, and run ≥3 iterations
taking the median with spread. Warm-up is already handled by `run.sh`.

### Report generation fails

```bash
python3 -u generate-report.py 2>&1 | tee report-gen.log
python3 --version                      # 3.7+
ls -l templates/report-template.html
```

---

## File structure

```
test/performance/
├── README.md                     # this file
├── lib.sh                        # shared config + helpers (sourced, not run)
├── run-comprehensive-tests.sh    # the 12-cell matrix + report
├── generate-report.py            # matrix report generator
├── analyze-results.py            # data analysis helper
├── scripts/
│   ├── setup.sh                  # provision namespace, backend, loadgen, gateway
│   ├── waf.sh                    # on <ruleset> | off | status
│   ├── run.sh                    # one measured run with validity probes
│   ├── suite.sh                  # B0 + one run per ruleset (canonical)
│   ├── collect.sh                # pull Prometheus metrics for a window
│   ├── report.py                 # per-scenario report, delta vs B0
│   ├── report.tpl.html
│   └── teardown.sh
├── scenarios/                    # QPS / duration / connections
├── manifests/
│   ├── backend.yaml              # echo backend
│   ├── loadgen.yaml              # fortio deployment
│   ├── gateway.yaml              # Gateway + HTTPRoute (envsubst)
│   ├── engine.yaml               # Engine (envsubst)
│   ├── rulesets/                 # minimal, medium, crs
│   └── load-profiles/            # request shapes + payloads + README
├── templates/
│   └── report-template.html      # matrix report template
└── results/                      # raw JSON, summaries, generated HTML
```

## References

### Dependencies

Python 3.7+ and a modern browser for reports. For runs: Kubernetes v1.32+ or
OpenShift v4.20+, Istio v1.27+, Fortio v1.76+, and
coraza-kubernetes-operator v0.5.0+. Prometheus and Grafana are optional.

### Related docs

| Topic | Doc |
|---|---|
| Load profile details | `manifests/load-profiles/README.md` |
| Driving this harness as an agent | `.claude/skills/perf-test/SKILL.md` |
| Cluster and Istio setup | `../../DEVELOPMENT.md` |
| Two-plane split, metric contract | `docs/driver-metrics-contract.md` |
| Controllers, cache server, Istio prerequisites | `docs/content/explanation/architecture.md` |
| WASM integration, poll loop, target selection | `docs/content/explanation/istio-wasm-integration.md` |
| Rule aggregation and validation lifecycle | `docs/content/explanation/rule-processing.md` |
| Operator metrics and cardinality | `docs/content/reference/metrics-cardinality.md` |
| CLI flags and cache tuning | `docs/content/reference/operator-cli-flags.md` |

### External links

- [Fortio](https://github.com/fortio/fortio)
- [Chart.js](https://www.chartjs.org/docs/)
- [Coraza WAF](https://coraza.io/)
- [OWASP CoreRuleSet](https://coreruleset.org/)
- [Istio performance and scalability](https://istio.io/latest/docs/ops/deployment/performance-and-scalability/)
- [SecRuleEngine directive](https://coraza.io/docs/seclang/directives/#secruleengine)
