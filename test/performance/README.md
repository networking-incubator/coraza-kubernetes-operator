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
| `manifests/rulesets/` | Ruleset fixtures: `minimal.yaml` (~2 rules → WASM overhead floor), `medium.yaml` (~30 rules → typical hand-written policy). CRS is generated on demand (see below). |
| `scenarios/` | Sourced env fragments — one per test scenario — setting traffic/payload/load knobs (`benign-fixed`, `benign-max`, `body-1kb`, `body-100kb`, `attack-block`, `soak`). |
| `scripts/` | The **Path A** harness (see below). |
| `standalone/` | The **Path B** rig — self-contained Docker Compose micro-benchmark. Has its own [`README.md`](./standalone/README.md). |
| `results/` | Run outputs (git-ignored): raw fortio JSON, parsed summaries, collected metrics, and the generated `dashboard.html`. |

### `scripts/` (Path A)

| Script | Purpose |
|---|---|
| `setup.sh` | Provision the target: namespace + backend + load generator + Gateway/HTTPRoute (WAF still off). |
| `waf.sh` | Toggle the WAF: `on <minimal\|medium\|/path/to/ruleset.yaml>` \| `off` (baseline) \| `status`. |
| `run.sh` | Run one scenario: validity probe (`EXPECT_BLOCK`) → warm-up → measured fortio run → JSON + summary. |
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

# 2. Baseline-vs-WAF comparison for a scenario (WAF off, then minimal, then medium).
./scripts/suite.sh scenarios/benign-fixed.env minimal medium

# 3. Open the generated self-contained report.
open results/benign-fixed.html      # or: grep -H . results/benign-fixed__*.summary.txt

# 4. Tear down (results are kept).
./scripts/teardown.sh
```

### Larger rulesets (CRS)

The dominant latency driver and most important data point. Generate an OWASP
CoreRuleSet RuleSet with the operator's CLI plugin and point `waf.sh` at it:

```bash
kubectl coraza generate coreruleset > /tmp/crs.yaml   # name the RuleSet 'perf-ruleset' (or set RULESET_NAME)
./scripts/waf.sh on /tmp/crs.yaml
```

### Standalone (Path B)

No cluster needed — see [`standalone/README.md`](./standalone/README.md):

```bash
cd standalone && ./scripts/suite.sh && open dashboard.html
```

## Configuration

All knobs are env vars with defaults in `lib.sh`:

| Var | Default | Meaning |
|---|---|---|
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
- `metrics__<tag>__<ts>.jsonl` — Prometheus range-query results (when `collect.sh` is used).
- `<scenario>.html` — self-contained report (from `report.py`); the durable per-run
  artifact. Compare runs **report-to-report**, not on a live dashboard.
