# Commands and flags reference

Every command is run from `test/performance/`. The scripts `cd` to that
directory themselves, so they also work when invoked by absolute path.

## One-time setup

```bash
# Provision namespace, backend, loadgen, gateway, and the payload ConfigMap.
# Also runs gen-payloads.sh. Safe to re-run; it is idempotent.
scripts/setup.sh

# Rebuild only the payloads after editing a body or gen-payloads.sh:
manifests/load-profiles/gen-payloads.sh
kubectl create configmap perf-payloads -n waf-perf \
  --from-file=manifests/load-profiles/payloads \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart -n waf-perf deploy/loadgen
```

## Running with a load profile

```bash
# One measured run: scenario (intensity) + profile (shape) + tag (which ruleset)
LOAD_PROFILE=args-heavy scripts/run.sh scenarios/fixed-500qps.env waf-crs

# Full baseline-vs-WAF suite with a profile applied to every run in it
LOAD_PROFILE=args-heavy scripts/suite.sh scenarios/fixed-500qps.env minimal medium crs

# A profile from an arbitrary path
LOAD_PROFILE=/tmp/my-profile.env scripts/run.sh scenarios/fixed-500qps.env waf-crs

# No profile - scenario's own REQ_PATH/PAYLOAD_SIZE apply (pre-profile behaviour)
scripts/run.sh scenarios/fixed-500qps.env waf-crs
```

Repeat each data point **at least 3 times** and report the median plus spread;
the scripts do not loop.

## Toggling the WAF

```bash
scripts/waf.sh off                  # remove the Engine (B0 baseline)
scripts/waf.sh on minimal           # rulesets/minimal.yaml
scripts/waf.sh on medium
scripts/waf.sh on crs
scripts/waf.sh on /tmp/custom.yaml  # any path also works
scripts/waf.sh status
```

Wait ~25 s after toggling before measuring, so the WasmPlugin reaches Envoy.
`suite.sh` does this for you (`SETTLE`).

## Reporting

```bash
# Single scenario+profile group
scripts/report.py --scenario fixed-500qps+args-heavy \
                  --out results/args-heavy.html

# List what result groups exist
ls results/*.json | sed 's/.*\///;s/__.*//' | sort -u

# Quick textual delta without HTML
grep -H . results/fixed-500qps+args-heavy__*.summary.txt
```

## Environment variables

### Selecting what runs

| Var | Default | Meaning |
|---|---|---|
| `LOAD_PROFILE` | *(unset)* | Profile name (no `.env`) or a file path. Unset = scenario's own shape. |
| `WARMUP` | `10s` | Discarded warm-up before the measured run. |
| `SETTLE` | `25s` | Post-toggle wait for the WasmPlugin (`suite.sh`). |
| `COLLECT` | `false` | Also pull Prometheus metrics around each run (`suite.sh`). |
| `RESULTS_DIR` | `results` | Where raw JSON and summaries are written. |

### Validity gates

| Var | Default | Meaning |
|---|---|---|
| `EXPECT_STATUS` | `200` | Status the profile's shape probe must see, else abort. Set in the profile. |
| `EXPECT_BLOCK` | *(unset)* | `true`/`false` — assert the fixed `/?q=attack` probe is/isn't blocked. `suite.sh` sets it automatically for `attack-*` scenarios. |
| `EXPECT_BENIGN_STATUS` | *(unset)* | Assert the fixed `GET /` probe returns this status. |

### Cluster and topology

| Var | Default | Meaning |
|---|---|---|
| `PLATFORM` | `kubernetes` | `kubernetes` or `openshift` (selects CLI + GatewayClass). |
| `NAMESPACE` | `waf-perf` | Test namespace. |
| `GATEWAY_NAME` | `perf-gateway` | Gateway resource name. |
| `GATEWAY_SVC` | `<gateway>-<class>` | Service the loadgen targets. Override when the derived name is wrong. |
| `RULESET_NAME` | `perf-ruleset` | RuleSet the Engine attaches. |
| `ENGINE_NAME` | `perf-engine` | Engine resource name. |
| `FAILURE_POLICY` | `fail` | Engine failure policy: `fail` or `allow`. |
| `PAYLOADS_CONFIGMAP` | `perf-payloads` | ConfigMap holding the request bodies. |
| `POLL_CACHE` | `false` | `true` injects `ruleSetCacheServer` (cache-poll model) instead of static embedding. |
| `POLL_INTERVAL_SECONDS` | `15` | Poll cadence when `POLL_CACHE=true`. |
| `WAIT_TIMEOUT` | `180s` | Timeout for rollout/condition waits. |

### Profile variables

Set these inside a profile `.env`; see [README.md](README.md) for the table —
`METHOD`, `REQ_PATH`, `CONTENT_TYPE`, `PAYLOAD_FILE`, `PAYLOAD`, `PAYLOAD_SIZE`,
`HEADERS`, `EXPECT_STATUS`.

### Scenario variables

| Var | Meaning |
|---|---|
| `QPS` | Target rate; `0` = unthrottled (find the knee). |
| `DURATION` | Measured-run length, e.g. `120s`. |
| `CONNS` | Concurrent connections. |

## fortio flags the harness passes

`run.sh` builds the fortio command from the scenario and profile. For reference:

| Flag | Source | Notes |
|---|---|---|
| `-qps` | `QPS` | Open-model rate. |
| `-c` | `CONNS` | Connections. |
| `-t` | `DURATION` / `WARMUP` | Run length. |
| `-p` | fixed | `50,75,90,95,99,99.9` percentiles. |
| `-allow-initial-errors` | fixed | Without it fortio aborts the whole run on the first non-2xx, which breaks any blocking profile. The shape probe is the real gate. |
| `-X` | `METHOD` | Omitted when empty. |
| `-H` | `HEADERS[@]` | Repeated once per header. |
| `-content-type` | `CONTENT_TYPE` | |
| `-payload-file` | `PAYLOAD_FILE` | Path inside the loadgen pod. |
| `-payload` | `PAYLOAD` | Inline body. |
| `-payload-size` | `PAYLOAD_SIZE` | Random bytes; implies POST + `application/octet-stream`. |
| `-json -` | fixed | Raw results to stdout, captured to `results/`. |

Useful flags the harness does **not** currently pass — add them to `run.sh` if
you need them: `-abort-on`, `-uniform`, `-nocatchup`, `-jitter`, `-keepalive`,
`-timeout`, `-user`.

## Debugging

```bash
# Which rules fired / why something was blocked (CRS logs to the gateway pod)
kubectl logs -n waf-perf -l gateway.networking.k8s.io/gateway-name=perf-gateway --tail=200 \
  | grep -oE '\[id "[0-9]+"\]' | sort | uniq -c | sort -rn

# Status code for an arbitrary shape. Note: fortio writes headers to STDERR,
# so reading stdout returns the body and no status line.
kubectl exec -n waf-perf deploy/loadgen -- fortio curl \
  -payload-file /payloads/json-1kb.json -content-type application/json \
  http://perf-gateway-istio.waf-perf.svc.cluster.local:80/api/orders 2>&1 >/dev/null \
  | grep -oE 'HTTP/[0-9.]+ [0-9]{3}'

# Confirm a payload reached the pod (the fortio image is distroless - no ls/cat)
kubectl get configmap perf-payloads -n waf-perf -o jsonpath='{.data}' | head -c 200

# What the backend actually received
kubectl exec -n waf-perf deploy/loadgen -- fortio curl -quiet \
  -payload-file /payloads/json-1kb.json -content-type application/json \
  http://echo.waf-perf.svc.cluster.local:80/
```

## Teardown

```bash
scripts/teardown.sh     # deletes the namespace; results/ is kept
```
