Unable to open session log file "/home/rzago/.cache/starship/session_1333825492836220.log": Os { code: 30, kind: ReadOnlyFilesystem, message: "Read-only file system" }!
# Central Istio WAF telemetry on OpenShift (OSSM + RH OTEL + CIO)

OpenShift-shaped fixtures for the WAF observability path when:

- **OpenShift Service Mesh (OSSM)** is installed
- **Red Hat build of OpenTelemetry** operator is installed
- **Cluster Ingress Operator** registers mesh `extensionProviders` from the
  `openshift-default` GatewayClass annotation (CIO WAF OTEL work)

The Coraza operator does **not** reconcile MeshConfig or the central collector.
It creates Gateway-scoped `Telemetry` for an observability-enabled Engine and
sets `enable_filter_state_logs: true` on WasmPlugin
`pluginConfig` so Envoy filter state (`wasm.io.coraza.waf.*`) is populated on
blocked requests.

This is not an OpenShift GA howto. Ownership, SCC/mTLS gaps, and a Grafana
dashboard suggestion: [gist](https://gist.github.com/rafaelvzago/35440655260569dfbafa6bae31a72781)
(working notes, not product docs).

## Manifest index

| Step | File | Purpose |
|------|------|---------|
| 1 | `05-gatewayclass-openshift.yaml` | `GatewayClass/openshift-default` |
| 2 | `../00-namespace.yaml` | Collector namespace + NetworkPolicy |
| 3 | `10-otel-collector-openshift.yaml` | Central RH OTEL collector CR |
| 3b | `12-otel-collector-debug-logging.yaml` | Optional: dump ALS OTLP logs to collector stdout |
| 4 | `annotate-gatewayclass.sh` | CIO annotation → `waf-log-collector` |
| 4b | `verify-mesh-waf-collector.sh` | Check `openshift-ingress/values-openshift-gateway` |
| 5 | `15-operator-openshift-values.yaml` | Helm values (OpenShift) |
| 5 | `install-operator-openshift.sh` | Build/push operator image + Helm install |
| 6 | `30-waf-telemetry-namespace.yaml` | WAF test namespace |
| 7 | `31-waf-workload.yaml` | Echo + Gateway + HTTPRoute |
| 8 | `40-waf-rules.yaml` | RuleSource + RuleSet |
| 9 | `41-waf-engine.yaml` | Engine + `coraza-proxy-wasm` filter-state image |
| 10 | Engine reconciliation | Gateway-scoped ALS (`waf-log-collector`) Telemetry |
| — | `50-validate-traffic.sh` | Smoke test allow/block |
| — | `55-generate-traffic-and-logs.sh` | Generate traffic; show gateway logs + collector metrics |
| — | `apply-openshift.sh` | Run all steps above |

## KIND vs OpenShift

| | KIND (`../`) | OpenShift (this dir) |
|---|--------------|----------------------|
| MeshConfig | Manual `../apply-meshconfig.sh` on Sail `Istio` CR | CIO via GatewayClass annotation |
| Verify mesh | `kubectl get istio …` | `verify-mesh-waf-collector.sh` (`openshift-ingress/values-openshift-gateway`) |
| ALS provider name | `coraza-central-otel-als` | `waf-log-collector` |
| WAF attribute source | `%FILTER_STATE(wasm.io.coraza.waf.*:PLAIN)%` | `%FILTER_STATE(wasm.io.coraza.waf.*:PLAIN)%` |
| OTEL operator | Upstream Helm (optional) | Red Hat build (OLM) — **no `spec.image` on collector** |
| WASM image | `coraza-proxy-wasm` ≥ PR #20 (`enable_filter_state_logs`) | `coraza-proxy-wasm` ≥ PR #20 (`enable_filter_state_logs`) |
| Coraza operator image | `ghcr.io/…` (KIND load) | Build + internal registry (`install-operator-openshift.sh`) |

## Prerequisites

- OpenShift 4.20+ with Gateway API
- OSSM / Istio with Gateway API
- Red Hat build of OpenTelemetry operator (`OpenTelemetryCollector` CRD)
- `helm`, `podman` (or `docker`), `oc` with cluster-admin for CIO annotation
- Coraza operator branch with WasmPlugin `enable_filter_state_logs`
- WASM OCI image with filter-state logging:

  `oci://ghcr.io/networking-incubator/coraza-proxy-wasm:e20b40ca25e3c50f212999e3decfde5503e630c3`

## Apply (all steps)

```bash
cd charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry/openshift
chmod +x *.sh
./apply-openshift.sh
```

Skip operator if already installed:

```bash
SKIP_OPERATOR=1 ./apply-openshift.sh
```

## Apply (step by step)

```bash
EX=charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry
OS=$EX/openshift

# 1–3 Collector
oc apply -f $OS/05-gatewayclass-openshift.yaml
oc apply -f $EX/00-namespace.yaml
oc apply -f $OS/10-otel-collector-openshift.yaml
oc rollout status -n coraza-central-waf-telemetry deploy/central-waf-als-collector --timeout=300s

# 4 CIO → mesh
$OS/annotate-gatewayclass.sh
$OS/verify-mesh-waf-collector.sh

# 5 Operator (use BUILD_METHOD=buildconfig on CI clusters if registry login fails)
$OS/install-operator-openshift.sh
# BUILD_METHOD=buildconfig $OS/install-operator-openshift.sh

# 6–9 WAF workload + observability-enabled Engine
oc apply -f $OS/30-waf-telemetry-namespace.yaml
oc apply -f $OS/31-waf-workload.yaml
oc wait -n waf-telemetry gateway/waf-gateway --for=condition=Programmed --timeout=180s
oc apply -f $OS/40-waf-rules.yaml
oc apply -f $OS/41-waf-engine.yaml
oc wait -n waf-telemetry engine/waf-engine --for=condition=Ready --timeout=180s

# 10 Telemetry
```

Validate:

```bash
$OS/50-validate-traffic.sh
$OS/55-generate-traffic-and-logs.sh
oc -n coraza-central-waf-telemetry port-forward svc/central-waf-als-collector 9090:9090
curl -s localhost:9090/metrics | grep coraza_waf_
hack/observability/validate-openshift-waf-telemetry.sh   # from repo root
```

### Generate traffic and observe logs

`55-generate-traffic-and-logs.sh` sends allow/block requests, then shows:

- **Gateway logs** — Coraza deny lines and 403 access log entries
- **Collector `:9090`** — `coraza_waf_*` counters (before/after delta)
- **Collector `:8888`** — `otelcol_receiver_accepted_log_records_total` (OTLP ALS intake)

```bash
cd openshift
./55-generate-traffic-and-logs.sh
SHOW_COLLECTOR_DEBUG=1 ./55-generate-traffic-and-logs.sh   # also dump collector pod stdout
USE_LB=0 ./55-generate-traffic-and-logs.sh                # port-forward instead of external LB
```

ALS log bodies are shipped as OTLP to the collector (not Envoy stdout). Use the metrics
deltas above to confirm intake; raw OTLP log fields match CIO `%FILTER_STATE%` labels.

### Inspect raw OTLP ALS payloads (optional debug)

```bash
oc apply -f $OS/12-otel-collector-debug-logging.yaml
oc rollout status -n coraza-central-waf-telemetry deploy/central-waf-als-collector

$OS/55-generate-traffic-and-logs.sh

oc logs -n coraza-central-waf-telemetry deploy/central-waf-als-collector -f \
  | grep -iE 'waf_event|action|rule_id|outcome|LogRecord'

# Revert when done
oc apply -f $OS/10-otel-collector-openshift.yaml
oc rollout status -n coraza-central-waf-telemetry deploy/central-waf-als-collector
```

## First-test success criteria

1. Block traffic through a Coraza-protected Gateway returns 403 as expected.
2. `coraza_waf_blocked_requests_total` appears on collector `:9090` after blocks.
3. WAF allow/block still works when the collector Deployment is scaled to 0.

## Known gaps (first OpenShift test)

- **Allow path:** `coraza-proxy-wasm` filter state is emitted on **interruptions
  only**; `coraza_waf_requests_total{outcome="pass"}` is not expected yet.
- **Tenancy labels:** `engine` / `namespace` / `driver_type` may show `unknown`
  until WASM exports them via filter state (CIO ALS uses gateway pod metadata,
  not Engine CR tenancy).
- **SCC / Envoy→collector mTLS:** unproven in this repo; track on [#60](https://github.com/rafaelvzago/coraza-kubernetes-operator/issues/60).
- **RH OTEL collector image:** pinned by the operator; does not include the
  `deltatocumulative` processor (the KIND example uses upstream contrib `0.120.0`).
  The OpenShift CR omits it; validate `coraza_waf_*` on `:9090` after blocks.
- **Coraza operator image:** `ghcr.io` release tags may be missing; use
  `install-operator-openshift.sh` to build from source and push to the cluster registry.
  If external registry login fails (common on `*.ci.openshift.org`), use
  `BUILD_METHOD=buildconfig ./install-operator-openshift.sh`.

## Metrics scope

Same baseline-only scope as the KIND example: `coraza_waf_requests_total` and
`coraza_waf_blocked_requests_total` via ALS → collector. See
[`docs/driver-metrics-contract.md`](../../../../docs/driver-metrics-contract.md).
