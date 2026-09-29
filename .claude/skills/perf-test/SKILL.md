---
name: perf-test
description: Run Coraza WAF performance tests — baseline-vs-WAF latency/throughput comparison — using the harness in test/performance. Use for "perf test", "benchmark the WAF", "WAF latency/overhead", "load test the gateway", "perf regression".
user_invocable: true
allowed_tools: Bash, Read, Grep, Glob
---

# Coraza WAF Performance Testing

Drive the performance-testing harness in `test/performance/` to measure the WAF
the way it actually runs: as the `coraza-proxy-wasm` module **inside the Envoy
proxy of the Gateway pod**, with load generated **in-cluster** by fortio.

**You orchestrate the existing scripts and enforce the methodology — you do NOT
reimplement port-forwarding, fortio invocation, or cleanup.** The scripts under
`test/performance/scripts/` are the source of truth; this skill adds the correct
ordering, the guardrails, and the delta analysis.

Read `test/performance/ARCHITECTURE.md` (methodology, tiers, scenarios) and
`test/performance/README.md` if you need background. All scripts `cd` to
`test/performance/` themselves, so they can be invoked by path from anywhere.

## Prerequisites (verify first)

- The **operator is already installed** — this harness does not install it.
- A reachable cluster with Istio + Gateway API CRDs (`kubectl` context set).
- Local tools: `kubectl` (or `oc`), `envsubst`, `python3`, `curl`.
- For `collect`/metrics: Prometheus reachable with the operator's ServiceMonitor
  (`:8443`) and PodMonitor (`:15090`) enabled.

If the user hasn't provisioned the target yet, run **setup** before any run.

## Modes

Pick the mode from the user's request (or the `args`). Modes map to scripts:

| Mode (`args`) | What it does | Command |
|---|---|---|
| `setup` | Provision ns + backend + loadgen + gateway (WAF still off) | `test/performance/scripts/setup.sh` |
| `suite <scenario> [rulesets…]` | **Canonical run:** WAF-off (B0) then WAF-on per ruleset | `test/performance/scripts/suite.sh <scenario-file> minimal medium` |
| `run <scenario> <tag>` | One measured run with a validity probe | `EXPECT_BLOCK=… test/performance/scripts/run.sh <scenario-file> <tag>` |
| `waf on\|off\|status` | Toggle the WAF manually | `test/performance/scripts/waf.sh on medium` |
| `collect <start> <end> <tag>` | Pull Prometheus metrics for a window | `test/performance/scripts/collect.sh <start> <end> <tag>` |
| `analyze [scenario]` | Build the self-contained HTML report + WAF-cost delta from `results/` | `test/performance/scripts/report.py [--scenario NAME]` (no cluster needed) |
| `teardown` | Delete the test namespace (results kept) | `test/performance/scripts/teardown.sh` |
| `crs` | Generate an OWASP CoreRuleSet ruleset for a run | see **Larger rulesets** below |

**Default when the user just says "run a perf test":** ensure `setup` has run,
then `suite scenarios/benign-fixed.env minimal medium`, then `analyze`.

## Methodology — non-negotiable guardrails

`suite.sh` already encodes most of this; **preserve it, and enforce the rest.**

1. **B0 (WAF off) first, same session.** Always capture the WAF-off baseline
   before any WAF-on run. `suite.sh` does this automatically; if you drive
   `run.sh` manually, run `waf.sh off` → measure `waf-off` **before** any
   `waf-*` run. Never report an absolute — report the **delta vs. B0**.
2. **Settle after toggling.** After `waf.sh on/off`, wait for the WASM plugin to
   poll the new rules (default poll 15s; `suite.sh` uses `SETTLE=25s`). Don't
   measure inside the settle window.
3. **Validity probe.** For attack/blocking scenarios, pass `EXPECT_BLOCK=true`
   (and `EXPECT_BLOCK=false` for benign against an active WAF) so a run where the
   WAF silently wasn't active is **rejected**, not silently wrong. `suite.sh`
   sets this automatically for `attack-*` scenarios.
4. **Warm-up is discarded** (`WARMUP`, default 10s) — `run.sh` handles it.
5. **Pin the rule version.** No RuleSet edits during a latency run (except the
   rule-churn scenario). A mid-run plugin reload invalidates the window.
6. **Repeat ≥3× per data point.** The scripts do **not** loop — you must run the
   suite/scenario **at least 3 times** and report the **median + spread**. One
   run is noise.
7. **Reject bad runs.** Discard any run with node CPU throttling or high client
   error rate (check `error_pct` in the summary).
8. **Report the gap correctly** (see Analyze): per percentile, like-for-like,
   `added pN = pN(WAF on) − pN(WAF off)`, in **ms** and **% = (on−off)/off**.

## Scenarios

Files live in `test/performance/scenarios/` (sourced env fragments):

| File | Purpose |
|---|---|
| `benign-fixed.env` | Latency at a fixed sub-saturation rate (primary latency KPI) |
| `benign-max.env` | Push max QPS to find the knee (saturation RPS) |
| `body-1kb.env` / `body-100kb.env` | Body-size cost (`SecRequestBodyAccess`) |
| `attack-block.env` | 100% malicious — blocking-path cost (use `EXPECT_BLOCK=true`) |
| `soak.env` | Long-duration stability (leaks/GC) |

## Rulesets

- `minimal` (~2 rules) — WASM-filter overhead floor.
- `medium` (~50–100 rules) — typical hand-written policy.
- **CRS** (~700+ rules) — the dominant latency driver and most important data
  point. Generate it (see below) and pass the file path to `waf.sh on`.

## Larger rulesets (CRS)

```bash
# from the operator repo root
kubectl coraza generate coreruleset > /tmp/crs.yaml
# ensure the RuleSet is named 'perf-ruleset' (or set RULESET_NAME to match)
test/performance/scripts/waf.sh on /tmp/crs.yaml
# then run the scenario tagged waf-crs, e.g.:
test/performance/scripts/run.sh test/performance/scenarios/benign-fixed.env waf-crs
```

## Analyze — the self-contained HTML report + WAF-cost delta

`report.py` is the primary analyzer. It reads `results/` (fortio `*.json` grouped
by tag; `waf-off` = B0), takes the **median across repeats**, computes the delta
vs B0 per percentile, folds in the server-side summary from any
`metrics__*.jsonl` (`collect.sh`), and writes a **self-contained**
`dashboard.html` — the durable per-run artifact (ARCHITECTURE.md §13). Needs
**no cluster** and **no live Grafana/Prometheus** to view.

```bash
test/performance/scripts/report.py                 # single scenario in results/
test/performance/scripts/report.py --scenario benign-fixed --out results/benign-fixed.html
```

Cross-run comparison (e.g. runs a week apart) is **report-to-report** against the
committed baseline — not on a live dashboard (§9). For a quick textual delta
without the HTML:

1. `grep -H . results/<scenario>__*.summary.txt` (has `p50_ms…p99.9_ms`, `error_pct`).
2. Per percentile: `added = pX(waf-*) − pX(waf-off)` (median of repeats), in ms and %.
3. Flag elevated `error_pct`; warn if the gap is **smaller than waf-off's run-to-run
   variance** (noise, not signal). Prefer p95/p99; never headline the average.

## Configuration knobs (env vars; defaults in `test/performance/lib.sh`)

| Var | Default | Meaning |
|---|---|---|
| `PLATFORM` | `kubernetes` | `kubernetes` or `openshift` (CLI + GatewayClass) |
| `NAMESPACE` | `waf-perf` | Test namespace |
| `GATEWAY_NAME` | `perf-gateway` | Gateway name |
| `RULESET_NAME` | `perf-ruleset` | RuleSet name the Engine attaches (match for CRS) |
| `FAILURE_POLICY` | `fail` | Engine failure policy (`fail`/`pass`) |
| `WARMUP` | `10s` | Discarded warm-up per run |
| `SETTLE` | `25s` | Post-toggle wait for WASM poll (suite.sh) |
| `COLLECT` | `false` | suite.sh also pulls Prometheus metrics |
| `PROM_NAMESPACE`/`PROM_SVC`/`PROM_PORT` | `monitoring`/`prometheus-k8s`/`9090` | Prometheus location |

To also gather metrics during a suite: `COLLECT=true test/performance/scripts/suite.sh …`.

## Gotchas

- **Don't measure inside the settle window** — the WASM plugin polls every ~15s;
  a fresh toggle isn't live yet.
- **A `coraza_waf_plugin_loads_total` bump mid-window invalidates the run.**
- **fortio sends a POST only when a payload is set**; arbitrary methods aren't
  parameterized (extend `run.sh` if PUT/PATCH bodies are needed).
- **Mixed benign/malicious ratios aren't a single fortio run** — run benign and
  attack scenarios separately.
- **Absolute latency is not portable** across clusters — only the delta vs. B0
  is. Never headline an absolute number.
- **Backend must never be the bottleneck** — it's an over-provisioned echo; if
  it saturates you're measuring the app, not the WAF.
