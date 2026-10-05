# Coraza WAF — Performance Testing Harness

Load-testing rig for the WAF deployed by the Coraza Kubernetes Operator.

## What this is about

The Coraza engine does **not** run as a standalone pod — it runs as the
`coraza-proxy-wasm` module **inside the Envoy proxy of the Gateway pod**, so it
evaluates every HTTP request inline. This harness measures exactly that: the
**latency and throughput cost the WAF adds** to real traffic.

The method is always the same — measure a **WAF-off baseline (B0) first**, then
the same scenario **WAF-on**, and report the cost as a **delta vs. B0** per
latency percentile (p50/p95/p99), never an absolute (absolutes aren't portable
across clusters). Load is generated with [fortio](https://github.com/fortio/fortio)
(open-model + HDR histograms) to keep tail latency honest.

Two **infrastructure profiles** run the same WASM filter:

- **Path A — full cluster** (`scripts/`, `manifests/`): the real operator-deployed
  topology (Istio + Gateway + operator cache + Prometheus). Production-representative,
  end-to-end; the only profile that can exercise the cache/reconcile/propagation paths.
- **Path B — standalone Envoy** (`standalone/`): Envoy + the same WASM binary in
  Docker, rules loaded statically, no operator/cache/Istio. Fast, cluster-free —
  for smoke tests, developing the harness itself, and the WASM-overhead floor.

> **Read [`ARCHITECTURE.md`](./ARCHITECTURE.md) first** — it defines the components
> under test, the tiers, the workload model, the scenario matrix, and the methodology.

## Directory guide

| Path | What it's for |
|---|---|
| `ARCHITECTURE.md` | The design doc: what/why to measure, tiers, scenarios, methodology, KPIs. **Start here.** |
| `README.md` | This file. |
| `lib.sh` | Shared config + helpers sourced by every Path A script (namespace, gateway, timeouts, CLI, `RESULTS_DIR`). |
| `manifests/` | Kubernetes YAML for the Path A target: `backend.yaml` (over-provisioned echo — never the bottleneck), `loadgen.yaml` (in-cluster fortio), `gateway.yaml` (Gateway + HTTPRoute), `engine.yaml` (attaches a RuleSet to the Gateway). |
| `manifests/rulesets/` | Ruleset fixtures — **what rules run**: `minimal.yaml` (1 rule → WASM overhead floor), `medium.yaml` (30 rules → typical hand-written policy), `crs.yaml` (563 rules → OWASP CoreRuleSet 4.7.0, committed; see below). |
| `manifests/load-profiles/` | Load-profile fixtures — **what each request looks like** (method, path, args, headers, body). Has its own [`README.md`](./manifests/load-profiles/README.md) and [`COMMANDS.md`](./manifests/load-profiles/COMMANDS.md). |
| `scenarios/` | Sourced env fragments — **how hard to push**, nothing else: `fixed-500qps`, `low-rate-200qps`, `max-throughput`, `soak`. Each sets only QPS, duration, and connections. |
| `scripts/` | The **Path A** harness (see below). |
| `standalone/` | The **Path B** rig — self-contained Docker Compose micro-benchmark. Has its own [`README.md`](./standalone/README.md). |
| `results/` | Run outputs (git-ignored): raw fortio JSON, parsed summaries, collected metrics, and the generated `dashboard.html`. |

### `scripts/` (Path A)

| Script | Purpose |
|---|---|
| `setup.sh` | Provision the target: namespace + backend + load generator + Gateway/HTTPRoute + payload ConfigMap (WAF still off). |
| `waf.sh` | Toggle the WAF: `on <minimal\|medium\|crs\|/path/to/ruleset.yaml>` \| `off` (baseline) \| `status`. Any `manifests/rulesets/<name>.yaml` resolves by bare name. |
| `run.sh` | Run one scenario: validity probes (`EXPECT_BLOCK`, plus a shape probe when `LOAD_PROFILE` is set) → warm-up → measured fortio run → JSON + summary. |
| `suite.sh` | The canonical baseline-vs-WAF comparison: WAF-off, then WAF-on per ruleset; settles between toggles; writes a self-contained HTML report at the end. |
| `collect.sh` | Pull control-plane + data-plane metrics from Prometheus for a run window into `results/`. |
| `report.py` | Aggregate `results/` into a **self-contained** `dashboard.html` (client latency + delta vs B0, plus server-side summary from `collect.sh`). No live backend needed to view. |
| `report.tpl.html` | Template used by `report.py` (charts + tables + light/dark). |
| `teardown.sh` | Delete the test namespace (results are kept). |

## Prerequisites

- The **operator already installed** (this harness does not install it).
- Kubernetes v1.32+ / OpenShift v4.20+ with Istio + Gateway API CRDs.
- `kubectl` (or `oc`), `envsubst`, `python3`, `curl` locally.
- For `collect.sh`: Prometheus reachable, with the operator's ServiceMonitor
  (`:8443`) and PodMonitor (`:15090`) enabled.

## Quick start (Path A)

```bash
cd test/performance

# 1. Provision the target (backend, load generator, gateway) — WAF still off.
./scripts/setup.sh

# 2. Baseline-vs-WAF comparison for a scenario (WAF off, then each ruleset).
./scripts/suite.sh scenarios/fixed-500qps.env minimal medium crs

#    ...or with a realistic request shape (see "Load profiles" below):
LOAD_PROFILE=args-heavy ./scripts/suite.sh scenarios/fixed-500qps.env minimal medium crs

# 3. Open the generated self-contained report.
open results/fixed-500qps.html      # or: grep -H . results/fixed-500qps__*.summary.txt

# 4. Tear down (results are kept).
./scripts/teardown.sh
```

## Load profiles — what each request looks like

A run is **three independent choices**, and they compose:

```
scenario            ×   load profile        ×   ruleset
(how hard to push)      (request shape)         (which rules)
fixed-500qps.env        args-heavy              crs
```

Scenarios set QPS/duration/connections. **Load profiles set the request shape** —
method, path, query args, headers, body, content type.

### Why this matters

CRS evaluates most detection rules **once per argument**. Of the 563 rules in
`crs.yaml`, **165 reference `ARGS`** and 16 reference `REQUEST_BODY`. A bare
`GET /` hands them an empty collection, so they cost almost nothing.

Measuring only `GET /` therefore reports the WAF's **best case**. Adding 25 query
args to the same request roughly **doubled p95** against CRS. Pick the profile
that resembles your real traffic, or your numbers will be optimistic.

### Your options

| Profile | Method | Shape | Exercises |
|---|---|---|---|
| `baseline-get` | GET | `/`, no args, no body | Reference point — the cheapest path. |
| `args-light` | GET | 5 query args | A typical search/listing request. |
| `args-heavy` | GET | 25 query args | The 165 `ARGS` rules. Highest-leverage profile. |
| `headers-heavy` | GET | 15 extra headers | Phase-1 `REQUEST_HEADERS` rules. |
| `form-urlencoded` | POST | urlencoded, 25 fields | Coraza parses the body **into `ARGS`** — ARGS rules via the body path. |
| `json-1kb` | POST | `application/json`, ~1 KB | JSON parser dispatch + the 16 `REQUEST_BODY` rules. |
| `json-100kb` | POST | `application/json`, ~100 KB | Body-size scaling. Pair with a lower-QPS scenario. |
| `multipart` | POST | `multipart/form-data` | The CRS 922 multipart family. |
| `body-1kb` | POST | 1 KB opaque bytes | Body transfer/buffering without a parser. |
| `body-100kb` | POST | 100 KB opaque bytes | Body-size cost without parsing. Pair with `low-rate-200qps`. |
| `attack-sqli` | GET | SQLi in args | Blocking-path cost on **CRS**. Expects **403**. |
| `attack-keyword` | GET | `?q=attack` | Blocking-path cost on **minimal/medium**. Expects **403**. |

Blocking profiles are ruleset-specific: `attack-sqli` is only blocked by CRS,
`attack-keyword` only by `minimal`/`medium`. Pairing one with the wrong ruleset
fails the shape probe, which is the intended signal.

### Scenarios — how hard to push

| Scenario | Rate | Duration | For |
|---|---|---|---|
| `fixed-500qps` | 500 qps | 120 s | The primary latency KPI. |
| `low-rate-200qps` | 200 qps | 120 s | Expensive shapes (large bodies, multipart). |
| `max-throughput` | unthrottled | 60 s | Finding the saturation knee. |
| `soak` | 300 qps | 1 h | Leaks, GC, drift. |

### Using one

Set `LOAD_PROFILE` to a profile name (no `.env`) or a file path:

```bash
# One run
LOAD_PROFILE=args-heavy ./scripts/run.sh scenarios/fixed-500qps.env waf-crs

# Applied to every run in a suite
LOAD_PROFILE=args-heavy ./scripts/suite.sh scenarios/fixed-500qps.env minimal medium crs
```

With no `LOAD_PROFILE`, the scenario's own `REQ_PATH`/`PAYLOAD_SIZE` apply and
behaviour is unchanged from before profiles existed.

Results are filed under `<scenario>+<profile>`, so different request shapes are
never pooled into one median:

```
results/fixed-500qps+args-heavy__waf-crs__20261005T120000Z.json
```

Before the measured run, `run.sh` sends **one request of the profile's exact
shape** and checks it against `EXPECT_STATUS` (default `200`). A profile that
the WAF rejects aborts in about a second instead of producing two minutes of
latency data for rejected requests.

### Composing your own

Create a `.env` anywhere and pass its path. Every field is optional:

```bash
METHOD="PUT"
REQ_PATH="/api/items/42?dryRun=true"
CONTENT_TYPE="application/json"
PAYLOAD_FILE="/payloads/json-1kb.json"   # path inside the loadgen pod
HEADERS=("Accept: application/json" "X-Tenant: acme")
EXPECT_STATUS="200"
```

Request bodies live in a `perf-payloads` ConfigMap mounted at `/payloads` in the
load generator. To add one, drop a file in `manifests/load-profiles/payloads/`
(or extend `gen-payloads.sh` for anything needing CRLF or more than a few KB),
then re-run `./scripts/setup.sh`.

Full variable reference and command/flag list:
[`manifests/load-profiles/README.md`](./manifests/load-profiles/README.md) and
[`COMMANDS.md`](./manifests/load-profiles/COMMANDS.md).

## Rulesets — which rules run

`minimal` (1 rule) is the WASM-overhead floor, `medium` (30) a typical
hand-written policy, and `crs` (563) the OWASP CoreRuleSet.

### CRS

The dominant latency driver and the most important data point. `crs.yaml` is
already committed, so it works by name:

```bash
./scripts/waf.sh on crs
```

To regenerate it (CRS 4.7.0), from the repo root:

```bash
curl -sL https://github.com/coreruleset/coreruleset/archive/refs/tags/v4.7.0.tar.gz | tar -xz -C /tmp
kubectl coraza generate coreruleset \
  --rules-dir /tmp/coreruleset-4.7.0/rules --version 4.7.0 \
  --ruleset-name perf-ruleset \
  --ignore-unsupported-rules wasm \
  --ignore-rules 920420 \
  > test/performance/manifests/rulesets/crs.yaml
```

Two things are hand-added to the generated file and must be re-added if you
regenerate — both are explained in its header comment: the
`skip-unsupported-rules-check` annotation on the RuleSet (for CRS 980170), and
the reason for `--ignore-rules 920420` (under coraza-proxy-wasm that rule
rejects **every** request carrying a `Content-Type`, so all POST traffic 403s;
it fires even when the content type is in the allowlist).

## Standalone (Path B)

No cluster needed — see [`standalone/README.md`](./standalone/README.md):

```bash
cd standalone && ./scripts/suite.sh && open dashboard.html
```

## Configuration

All knobs are env vars with defaults in `lib.sh`:

| Var | Default | Meaning |
|---|---|---|
| `LOAD_PROFILE` | *(unset)* | Request shape: a profile name or a file path. Unset = the scenario's own shape |
| `EXPECT_STATUS` | `200` | Status the profile's shape probe must see, else the run aborts (set inside the profile) |
| `PAYLOADS_CONFIGMAP` | `perf-payloads` | ConfigMap holding request bodies, mounted at `/payloads` in the load generator |
| `PLATFORM` | `kubernetes` | `kubernetes` or `openshift` (CLI + GatewayClass) |
| `NAMESPACE` | `waf-perf` | Test namespace |
| `GATEWAY_NAME` | `perf-gateway` | Gateway name |
| `RULESET_NAME` | `perf-ruleset` | RuleSet name the Engine attaches (match for CRS) |
| `FAILURE_POLICY` | `fail` | Engine failure policy (`fail`/`allow`) |
| `POLL_CACHE` | `false` | `true` enables cache polling (`ruleSetCacheServer`); default embeds rules statically |
| `POLL_INTERVAL_SECONDS` | `15` | Cache poll cadence when `POLL_CACHE=true` |
| `WARMUP` | `10s` | Discarded warm-up per run |
| `SETTLE` | `25s` | Post-toggle wait for the WASM poll (suite.sh) |
| `COLLECT` | `false` | suite.sh also pulls Prometheus metrics |
| `PROM_NAMESPACE`/`PROM_SVC`/`PROM_PORT` | `monitoring`/`prometheus-k8s`/`9090` | Prometheus location for collect.sh |

## Output

Each run writes to `results/` (git-ignored):

- `<scenario>__<tag>__<ts>.json` — raw fortio JSON (HDR histogram, return codes).
- `<scenario>__<tag>__<ts>.summary.txt` — parsed line: actual QPS, p50/p90/p95/p99/p99.9 (ms), error %.
- When a load profile is used, `<scenario>` becomes `<scenario>+<profile>`, so
  runs with different request shapes stay in separate result groups. Pass the
  combined name to `report.py --scenario`.
- `metrics__<tag>__<ts>.jsonl` — Prometheus range-query results (when `collect.sh` is used).
- `<scenario>.html` — self-contained report (from `report.py`); the durable per-run
  artifact. Compare runs **report-to-report**, not on a live dashboard.
