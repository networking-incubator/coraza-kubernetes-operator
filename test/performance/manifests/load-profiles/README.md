# Load profiles

A **load profile** defines the *shape* of each request: method, path, query
args, headers, body, content type.

It is deliberately separate from a **scenario** (`test/performance/scenarios/`),
which defines load *intensity* — QPS, duration, connection count. The two
compose, so a test is three independent choices:

```
scenario (how hard)  ×  load profile (what each request looks like)  ×  ruleset (which rules)
fixed-500qps.env        args-heavy                                      crs
```

Both are plain sourced bash fragments. The profile is sourced **after** the
scenario, so on any overlap the profile wins.

## Why profiles matter

CRS evaluates most of its detection rules **once per argument**. Of the 563
rules in the generated CRS ruleset, **165 reference `ARGS`** and **16 reference
`REQUEST_BODY`**. A bare `GET /` gives them an empty collection to iterate, so
they cost almost nothing.

Measuring only `GET /` therefore reports the WAF's **best case**, not a
representative one. Measured against CRS at 50 qps, simply adding 25 query args
to the same request roughly **doubled** p95. Pick a profile that resembles your
real traffic.

## Available profiles

| Profile | Method | Shape | What it exercises |
|---|---|---|---|
| `baseline-get` | GET | `/`, no args, no body | Reference point. Header/URI-phase rules only — the WAF's cheapest path. |
| `args-light` | GET | 5 query args | A typical search/listing request. |
| `args-heavy` | GET | 25 query args | The 165 `ARGS` rules, ~25× the per-arg work. Highest-leverage profile. |
| `headers-heavy` | GET | 15 extra headers | Phase-1 `REQUEST_HEADERS` rules against a realistic browser header set. |
| `form-urlencoded` | POST | `application/x-www-form-urlencoded`, 25 fields | Coraza parses urlencoded bodies **into `ARGS`**, so this hits the ARGS rules via the body path. |
| `json-1kb` | POST | `application/json`, ~1 KB | JSON body-parser dispatch plus the 16 `REQUEST_BODY` rules. |
| `json-100kb` | POST | `application/json`, ~100 KB | Body-size scaling of parse + body-rule cost. Pair with a lower-QPS scenario. |
| `multipart` | POST | `multipart/form-data` | The only profile reaching the CRS 922 multipart family and `MULTIPART_STRICT_ERROR`. |
| `body-1kb` | POST | 1 KB opaque random bytes | Body transfer + buffering with no parser and no per-arg rules. Was `scenarios/body-1kb.env`. |
| `body-100kb` | POST | 100 KB opaque random bytes | Body-size cost without parsing. Pair with `low-rate-200qps`. Was `scenarios/body-100kb.env`. |
| `attack-sqli` | GET | SQLi patterns in args | Blocking-path cost. Expects **403** from CRS 942. **CRS only.** |
| `attack-keyword` | GET | `?q=attack` | Blocking-path cost on the hand-written rulesets. Expects **403**. **`minimal`/`medium` only** — CRS has no such rule. Was `scenarios/attack-block.env`. |

Blocking profiles declare `EXPECT_STATUS=403`, so pairing one with a ruleset
that does not block it fails the shape probe. That is deliberate. On the WAF-off
baseline run, `suite.sh` overrides the expectation to `200`.

## CRS and POST traffic — resolved, with one rule excluded

POST profiles initially got 403 from CRS for **every** request carrying a
`Content-Type`. Two separate defects were behind it; both are now fixed.

**1. CRS 920420 misfires under coraza-proxy-wasm.** "Request content type is
not allowed by policy" (CRITICAL = anomaly score 5, which alone meets the
default threshold) rejects the request in phase 1, before the body is parsed.
It fires *even when the content type is in the allowlist* — verified both via
`%{tx.allowed_request_content_type}` and with a hardcoded literal list, so it
is not a missing `crs-setup.conf`. The generated ruleset therefore excludes it
with `--ignore-rules 920420`; see the header of `../rulesets/crs.yaml`. GET-path
rules and attack detection are unaffected — SQLi and XSS probes still return
403.

**2. The generator left orphaned chain children** (fixed in
`tools/corerulesetgen`). `splitIntoRules` only recognised `SecRule` at column 0,
but CRS indents chained rules; and `chainActionRe` could not match `,\` +
newline + `chain"`, which is CRS's standard layout. So dropping a chain starter
left its children behind as standalone rules. For 920420 the orphan scored every
request on its own, meaning excluding the rule did not actually help. Regression
tests cover both in `rules_test.go`.

All nine profiles now pass against `crs`, `medium`, and `minimal`.

## Choosing a profile

Set `LOAD_PROFILE` to a profile name (no `.env`) or to a file path:

```bash
LOAD_PROFILE=args-heavy scripts/run.sh scenarios/fixed-500qps.env waf-crs
LOAD_PROFILE=./my-profile.env scripts/run.sh scenarios/fixed-500qps.env waf-crs
```

With no `LOAD_PROFILE`, the scenario's own `REQ_PATH` / `PAYLOAD_SIZE` apply and
behaviour is exactly as before profiles existed.

Results are filed under `<scenario>+<profile>` so runs with different request
shapes are never pooled into one median:

```
results/fixed-500qps+args-heavy__waf-crs__20261004T170000Z.json
```

## Composing a new profile

Create a `.env` file here (or anywhere, and pass the path). Every field is
optional; defaults are in the table below.

```bash
# my-profile.env
METHOD="PUT"
REQ_PATH="/api/items/42?dryRun=true"
CONTENT_TYPE="application/json"
PAYLOAD_FILE="/payloads/json-1kb.json"
HEADERS=(
  "Accept: application/json"
  "X-Tenant: acme"
)
EXPECT_STATUS="200"
```

| Variable | Default | Meaning |
|---|---|---|
| `METHOD` | `""` | HTTP method. Empty = fortio's default: GET, or POST when a body is set. |
| `REQ_PATH` | `/` | Path including any query string. |
| `CONTENT_TYPE` | `""` | Sets the `Content-Type` header. |
| `PAYLOAD_FILE` | `""` | Body read from this path **inside the loadgen pod** (see Payloads). |
| `PAYLOAD` | `""` | Inline literal body. Use for short bodies instead of a file. |
| `PAYLOAD_SIZE` | `0` | `>0` sends this many random bytes as `application/octet-stream`. |
| `HEADERS` | `()` | Bash array of extra headers, each `"Key: Value"`. |
| `EXPECT_STATUS` | `200` | Status the pre-run shape probe must observe, or the run aborts. |

### Payloads

`PAYLOAD_FILE` paths resolve inside the loadgen pod, where the `perf-payloads`
ConfigMap is mounted at `/payloads`. To add a body:

1. Drop the file in `payloads/` (hand-written) **or** extend `gen-payloads.sh`
   (generated — use this for anything needing strict CRLF, or larger than a few KB).
2. Rebuild and remount:
   ```bash
   ./gen-payloads.sh
   scripts/setup.sh      # recreates the ConfigMap and rolls the loadgen pod
   ```
3. Reference it as `/payloads/<filename>`.

`multipart.bin` and `json-100kb.json` are generated, not committed — multipart
needs CRLF line endings that editors and git mangle, and a 100 KB blob does not
belong in version control.

`run.sh` checks the ConfigMap for the named key before every run, so a missing
payload fails immediately instead of silently sending an empty body.

See [COMMANDS.md](COMMANDS.md) for the full command and flag reference.
