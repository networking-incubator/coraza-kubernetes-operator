# Standalone Envoy + coraza-proxy-wasm rig (Path B)

A **no-Kubernetes, no-operator** micro-benchmark of the Coraza WAF running as the
`coraza-proxy-wasm` module inside a **standalone Envoy**, driven by fortio over a
private Docker network. It isolates the **WASM-filter + Envoy overhead** from the
operator/cache machinery measured by the in-cluster harness one level up
(`../ARCHITECTURE.md`, "Path A").

## Why this is faithful to production

It runs the **exact image the operator deploys by default**
(`internal/defaults/defaults.go` → `ghcr.io/networking-incubator/coraza-proxy-wasm`).
That fork build accepts **static inline rules** (`directives_map` /
`default_directives`) *in addition to* its cache-polling mode, so we can run the
production binary with no cache server, no auth token, and no rule plumbing — the
rules live directly in the Envoy config.

What this rig does **not** cover (by design — that's Path A): the RuleSetCache
HTTP server, the WASM cache-poll loop, rule-update propagation, and Istio/xDS
injection. See Tier 2/4 in `../ARCHITECTURE.md`.

## Layout

```
standalone/
├── docker-compose.yaml       # envoy + echo backend + fortio (load profile)
├── envoy/
│   ├── envoy-baseline.yaml   # Envoy WITHOUT the filter (B0)
│   ├── envoy-waf.yaml        # Envoy WITH the filter, minimal ~2 rules
│   └── envoy-crs.yaml        # Envoy WITH the filter, full OWASP CRS (~900 rules)
├── scripts/
│   ├── extract-wasm.sh       # copy plugin.wasm out of the OCI image -> ./coraza.wasm
│   ├── run.sh                # probe + warm-up + one measured fortio run -> results/
│   ├── suite.sh              # baseline + rulesets, REPEATS runs each (median)
│   ├── report.py             # aggregate results/*.json -> dashboard.html
│   └── dashboard.tpl.html    # self-contained dashboard template (charts/table)
├── coraza.wasm               # extracted binary (gitignored, ~13 MB)
├── dashboard.html            # generated report (gitignored)
└── results/                  # run JSON + summaries (gitignored)
```

## Prerequisites

- Docker (Compose v2). Apple Silicon works — the `.wasm` is architecture-neutral
  and runs under Envoy's V8 runtime even though the source image is `linux/amd64`.
- `curl` and `python3` on the host (validity probe + summary parsing).

## Quick start

```bash
cd test/performance/standalone

# 1. Extract the WASM module from the operator's default OCI image.
./scripts/extract-wasm.sh

# 2. Run the whole comparison: baseline -> minimal -> CRS, 3 runs each (median).
REPEATS=3 QPS=800 DURATION=20s CONNECTIONS=8 ./scripts/suite.sh

# 3. Build the dashboard and open it.
python3 scripts/report.py && open dashboard.html

# 4. Tear down (results + dashboard are kept, gitignored).
docker compose down
```

### One ruleset at a time

```bash
QPS=800 ./scripts/run.sh baseline   # B0, no filter
QPS=800 ./scripts/run.sh minimal    # ~2 rules
QPS=800 ./scripts/run.sh crs        # full OWASP CRS
cat results/*.summary.txt           # WAF cost = <mode> minus baseline
```

## Dashboard

`scripts/report.py` scans `results/*.json`, takes the **median across repeats**
per percentile, computes WAF cost as a delta vs baseline, and writes a single
self-contained `dashboard.html` (no network, no build step): KPI tiles, a
latency-tail line chart, WAF-cost delta bars, and a full medians table, with a
dark-mode toggle. Re-run it any time after more runs land in `results/`.

## Knobs (env vars for `run.sh`)

| Var | Default | Meaning |
|---|---|---|
| `QPS` | `500` | Open-model constant target QPS |
| `DURATION` | `20s` | Measured window (after warm-up) |
| `CONNECTIONS` | `8` | fortio concurrent connections |
| `WARMUP` | `5s` | Discarded warm-up window (WASM VM warm) |
| `PATH_` | `/` | Request path (`PATH_`, not `PATH`, to avoid clobbering shell `$PATH`) |
| `PAYLOAD_KB` | `0` | POST body size in KB (fortio POSTs when >0; exercises body inspection) |

## Methodology (mirrors `../ARCHITECTURE.md` §9)

- **Baseline (B0) first**, same environment; report WAF cost as a **delta**, not
  an absolute — absolutes aren't portable, especially off a laptop.
- **Validity probe** before every measured run: benign `GET /` must be `200` and
  `GET /admin` must be `403` in WAF mode, else the run aborts as invalid.
- **Warm-up discarded**, then steady-state only.
- **Open-model / constant-QPS** load with fortio's HDR histogram (no coordinated
  omission). Repeat ≥3× and take the median for anything you report.

> **Laptop caveat:** Docker Desktop on macOS runs Envoy inside a VM with no CPU
> pinning — absolute numbers and tails are noisy. This rig is for **relative**
> comparisons (ruleset A vs B, release-over-release) and for validating the
> harness itself. For portable numbers use pinned nodes (Path A) per the parent
> ARCHITECTURE.md.

## The rulesets

- `envoy-waf.yaml` — **minimal** (`SecRuleEngine On` + one deny rule): the WASM
  filter overhead floor.
- `envoy-crs.yaml` — **full OWASP CRS** via the embedded aliases
  (`Include @demo-conf` / `@crs-setup-conf` / `@owasp_crs/*.conf`), PL1 anomaly
  blocking. No RuleData or cache wiring — CRS ships inside the extension.

Edit the `directives_map` in either file to change the policy. Validity probes:
`minimal` blocks `GET /admin`; `crs` blocks an XSS query arg — both asserted
before each measured run.
