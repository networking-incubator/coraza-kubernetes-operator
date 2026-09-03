# Central Istio WAF telemetry on OpenShift (RH OTEL + CIO)

OpenShift-shaped fixtures for the WAF observability path when:

- **Red Hat build of OpenTelemetry** operator is installed
- **Cluster Ingress Operator** registers mesh `extensionProviders` from the
  `openshift-default` GatewayClass annotation (CIO WAF OTEL work)

The Coraza operator does **not** reconcile MeshConfig or the central collector.
It creates Gateway-scoped `Telemetry` for an observability-enabled Engine and
sets `enable_filter_state_logs: true` on WasmPlugin
`pluginConfig` so Envoy filter state (`wasm.io.coraza.waf.*`) is populated on
blocked requests.

This is not an OpenShift GA howto. Ownership and SCC/mTLS gaps remain
demonstrator concerns. The optional Grafana resources reuse the chart-maintained
Coraza WAF dashboard; they are not product docs.

`openshift-ingress` is the fixed namespace for this example's Gateway, backend
workload, RuleSources, RuleSet, Engine, generated Telemetry, and gateway
`PodMonitor`. Keeping those resources with the CIO-managed ingress components
ensures the Gateway receives the expected DNS and load-balancing configuration.
The central OpenTelemetry collector intentionally remains separate in
`coraza-central-waf-telemetry`.

## Manifest index

| Step | File | Purpose |
|------|------|---------|
| 0 | `01-enable-user-workload-monitoring.sh` | Explicit, idempotent cluster-admin patch for User Workload Monitoring |
| 1 | `05-gatewayclass-openshift.yaml` | `GatewayClass/openshift-default` |
| 2 | `../00-namespace.yaml` | Collector namespace + NetworkPolicy |
| 3 | `10-otel-collector-openshift.yaml` | Central collector CR, gRPC/HTTP2 and cumulative Prometheus counters |
| 3a | `11-collector-destinationrule.yaml` | Plaintext OTLP route to the sidecarless collector |
| 3b | `12-otel-collector-debug-logging.yaml` | Optional: dump ALS OTLP logs to collector stdout |
| 4 | `annotate-gatewayclass.sh` | CIO annotation → `waf-log-collector` |
| 4b | `verify-mesh-waf-collector.sh` | Check `openshift-ingress/values-openshift-gateway` |
| 5 | `15-operator-openshift-values.yaml` | Helm values (OpenShift) |
| 5 | `install-operator-openshift.sh` | Build/push operator image + Helm install |
| 6 | `31-waf-workload.yaml` | Echo + Gateway + HTTPRoute in `openshift-ingress` |
| 7 | `40-waf-rules.yaml` | RuleSource + RuleSet in `openshift-ingress` |
| 8 | `41-waf-engine.yaml` | Engine + `coraza-proxy-wasm` filter-state image in `openshift-ingress` |
| 10 | Engine reconciliation | Gateway-scoped ALS (`waf-log-collector`) Telemetry |
| 11 | `60-prometheus-monitors.yaml` | Collector `ServiceMonitor` + gateway `PodMonitor` for User Workload Monitoring |
| 12 | `70-grafana-openshift.yaml` | Grafana instance and Thanos read-only RBAC binding |
| 13 | `71-grafana-datasource.yaml` | Authenticated Thanos Querier Prometheus datasource |
| 14 | `72-grafana-dashboard.yaml` | Imports the recovered chart-maintained `coraza-waf.json` dashboard |
| 15 | `73-apply-grafana-dashboard.sh` | Creates the dashboard ConfigMap and applies steps 12–14 |
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
| OTEL operator | Upstream Helm (optional) | Red Hat build (OLM); collector CR pins upstream contrib `0.152.1` |
| WASM image | `coraza-proxy-wasm` ≥ PR #20 (`enable_filter_state_logs`) | `coraza-proxy-wasm` ≥ PR #20 (`enable_filter_state_logs`) |
| Coraza operator image | KIND-loaded image | Repository and tag supplied by the test runner |

## Prerequisites

- OpenShift 4.20+ with Gateway API
- Cluster Ingress Operator with the GatewayClass-to-Istio integration from CIO PR 1555; no manual OSSM installation is required for this example
- Red Hat build of OpenTelemetry operator (`OpenTelemetryCollector` CRD)
- OpenShift User Workload Monitoring enabled by a cluster administrator. This is a cluster-wide prerequisite; the example never modifies `cluster-monitoring-config`.
- `helm`, `oc` with cluster-admin for CIO annotation and resources in `openshift-ingress`
- A Coraza operator image in a registry that this cluster can pull. Its repository
  and tag are intentionally chosen by the person running the test.
- Coraza operator branch with WasmPlugin `enable_filter_state_logs`
- WASM OCI image with filter-state logging:

  `oci://ghcr.io/networking-incubator/coraza-proxy-wasm:e20b40ca25e3c50f212999e3decfde5503e630c3`

## Reproduce through Prometheus scrape targets

The Grafana phase below uses the installed Grafana Operator and the existing
OpenShift Thanos Querier. Do not install a second Prometheus or
`kube-prometheus-stack`.

### 0. Validate the cluster prerequisites

The Cluster Ingress Operator build containing [CIO PR 1555](https://github.com/openshift/cluster-ingress-operator/pull/1555)
must already be deployed. It creates Istio for `GatewayClass/openshift-default`;
this example does **not** install OSSM.

```bash
oc whoami
oc get clusterversion
oc get co ingress
oc api-resources --api-group=gateway.networking.k8s.io
oc api-resources --api-group=opentelemetry.io
oc api-resources --api-group=monitoring.coreos.com
```

Install the Red Hat OpenTelemetry Operator before continuing. The Grafana
Operator can also be installed now, but is not used until the dashboard step.

### 1. Enable User Workload Monitoring (cluster administrator, once per cluster)

This is an OpenShift cluster setting, not an application setting. Do not add it
to the Coraza Helm chart and `apply-openshift.sh` never invokes it implicitly.
Run the versioned patch explicitly as cluster-admin; it preserves all existing
monitoring configuration and is idempotent:

```bash
./01-enable-user-workload-monitoring.sh
```

Confirm the effective configuration:

```bash
oc get configmap cluster-monitoring-config -n openshift-monitoring \
  -o jsonpath='{.data.config\.yaml}{"\n"}'
```

This enables monitor discovery for user projects; it does not scrape every pod
automatically. This example's `ServiceMonitor` selects only the central
collector, and its `PodMonitor` selects only `waf-gateway` pods in
`openshift-ingress`.

Wait for the managed components to become ready:

```bash
oc get pods -n openshift-user-workload-monitoring -w
# Stop when prometheus-user-workload, prometheus-operator, and
# thanos-ruler-user-workload are Running and Ready.
```

### 2. Apply the example

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

# 3–5 GatewayClass, collector, and CIO-generated ALS provider
oc apply -f $OS/05-gatewayclass-openshift.yaml
oc apply -f $EX/00-namespace.yaml
oc apply -f $OS/10-otel-collector-openshift.yaml
oc rollout status -n coraza-central-waf-telemetry deploy/central-waf-als-collector --timeout=300s

# Confirm the collector image selected by this fixture.
oc get deployment -n coraza-central-waf-telemetry central-waf-als-collector \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Confirm the generated Service explicitly declares the OTLP gRPC protocol.
oc get svc -n coraza-central-waf-telemetry central-waf-als-collector \
  -o jsonpath='{.spec.ports[?(@.port==4317)].appProtocol}{"\n"}'

# The central collector has no Istio sidecar, so ALS must use plaintext gRPC.
oc apply -f $OS/11-collector-destinationrule.yaml

# CIO → mesh. The annotation value is collector host:port; CIO creates the
# Istio extension provider named waf-log-collector.
oc annotate gatewayclass openshift-default \
  'internal.do-not-use.openshift.io/waf-otel-collector=central-waf-als-collector.coraza-central-waf-telemetry.svc.cluster.local:4317' \
  --overwrite
oc get configmap -n openshift-ingress values-openshift-gateway \
  -o jsonpath='{.data.merged-values}' \
  | grep -A20 -B5 'waf-log-collector'

# 6 Operator. Build and push the image using your chosen workflow, then provide
# its coordinates explicitly. This example does not prescribe a registry or tag.
: "${IMAGE_REPOSITORY:?set the operator image repository}"
: "${IMAGE_TAG:?set the operator image tag}"
BUILD_IMAGE=0 IMAGE_REPOSITORY="$IMAGE_REPOSITORY" IMAGE_TAG="$IMAGE_TAG" \
  $OS/install-operator-openshift.sh

# 7–10 WAF workload + observability-enabled Engine. All Gateway and Engine
# resources use openshift-ingress so CIO supplies the expected DNS and
# load-balancing configuration.
oc apply -f $OS/31-waf-workload.yaml
oc wait -n openshift-ingress gateway/waf-gateway --for=condition=Programmed --timeout=180s
oc apply -f $OS/40-waf-rules.yaml
oc apply -f $OS/41-waf-engine.yaml
oc wait -n openshift-ingress engine/waf-engine --for=condition=Ready --timeout=180s

# 11 Telemetry: provider name must be waf-log-collector
oc get telemetry -n openshift-ingress coraza-engine-waf-engine-telemetry \
  -o jsonpath='{.spec.accessLogging[0].providers[0].name}{"\\n"}'

# 12 Prometheus scrape targets (requires User Workload Monitoring)
oc apply -f $OS/60-prometheus-monitors.yaml

oc get servicemonitor -n coraza-central-waf-telemetry central-waf-als-collector
oc get podmonitor -n openshift-ingress waf-gateway-dataplane
```

The collector CR pins `ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.152.1`.
The Red Hat operator remains installed and reconciles the CR; only the
collector workload image is overridden. The `count` connector emits delta
metrics, so `deltatocumulative` accumulates them before the Prometheus
exporter. Keep `replicas: 1`, because that processor keeps state in memory.

This is a demonstration-fixture choice. An upstream collector image used with
the Red Hat operator is a support-boundary decision that requires review for a
supported production deployment.

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
  | grep -iE 'waf_event|action|rule_id|outcome|LogRecord|Metric #|AggregationTemporality|coraza_waf_'

# Revert when done
oc apply -f $OS/10-otel-collector-openshift.yaml
oc rollout status -n coraza-central-waf-telemetry deploy/central-waf-als-collector
```

## Grafana dashboard (optional demonstrator)

The repository already maintains the complete
`charts/coraza-kubernetes-operator/dashboards/coraza-waf.json` dashboard. The
OpenShift resources import that exact JSON through a `ConfigMap`; they do not
duplicate or fork its panels. Grafana reads the existing OpenShift Thanos
Querier rather than deploying another Prometheus.

The datasource and dashboard use an empty instance selector intentionally:
this namespace is dedicated to this one Grafana instance, and it avoids a
known Grafana Operator v5 matching regression on OpenShift. Do not add another
Grafana instance to this namespace without replacing it with a tested label
selector.

The datasource needs a short-lived ServiceAccount token. It is deliberately
created at deployment time and stored only in the cluster Secret; never add a
token to a YAML file or commit it.

```bash
EX=charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry
OS="$EX/openshift"
GRAFANA_NS=coraza-central-waf-telemetry

# Creates Grafana, its narrowly-scoped NetworkPolicy, and grants its
# operator-created ServiceAccount read-only access to Thanos Querier.
oc apply -f "$OS/70-grafana-openshift.yaml"
oc get serviceaccount -n "$GRAFANA_NS" central-waf-grafana-sa

# TokenRequest token: renew this Secret before it expires for a long-running
# demonstrator. `cluster-monitoring-view` is read-only access to Thanos.
THANOS_TOKEN="$(oc create token -n "$GRAFANA_NS" central-waf-grafana-sa --duration=24h)"
oc create secret generic -n "$GRAFANA_NS" central-waf-grafana-thanos-token \
  --from-literal=THANOS_TOKEN="$THANOS_TOKEN" \
  --dry-run=client -o yaml | oc apply -f -
unset THANOS_TOKEN

# Copies the maintained dashboard JSON into a ConfigMap and applies the
# datasource and GrafanaDashboard CR.
bash "$OS/73-apply-grafana-dashboard.sh"

oc get grafana,grafanadatasource,grafanadashboard -n "$GRAFANA_NS"
oc get pods -n "$GRAFANA_NS" -l app.kubernetes.io/instance=central-waf-grafana
```

The example creates an explicit OpenShift `Route` named
`central-waf-grafana`. Get its URL with:

```bash
oc get route -n coraza-central-waf-telemetry central-waf-grafana \
  -o jsonpath='https://{.spec.host}{"\n"}'
```

Use the admin credentials Secret generated by the Grafana Operator. Inspect
the Secret names rather than assuming a password or committing one:

```bash
oc get secret -n coraza-central-waf-telemetry | grep central-waf-grafana
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
- **Collector TLS mode:** the central collector deliberately has no Istio
  sidecar. `11-collector-destinationrule.yaml` disables Istio mTLS only for
  its OTLP Service, so Gateway ALS uses plaintext gRPC. Validate this path in
  each OpenShift environment; track broader SCC/mTLS work on [#60](https://github.com/rafaelvzago/coraza-kubernetes-operator/issues/60).
- **Collector gRPC protocol:** port `4317` sets `appProtocol: grpc`. The
  descriptive port name `otlp-grpc` is not an Istio protocol prefix; without
  the explicit declaration, a Gateway may select HTTP/1.1 for the gRPC ALS
  exporter instead of HTTP/2.
- **Collector image and state:** the Red Hat operator remains installed and
  reconciles the CR, but the CR explicitly selects upstream contrib `0.152.1`.
  The Red Hat default image observed by this fixture did not provide
  `deltatocumulativeprocessor`, which is required to convert the count
  connector's delta sums into Prometheus-safe cumulative `*_total` counters.
  This is an image override for the demonstrator workload, not a replacement
  of the OLM-managed operator and it is a support-boundary choice for
  production. `deltatocumulative` state is in memory, so this fixture keeps one
  collector replica and its counter values reset if the pod is replaced.
- **Coraza operator image:** the test runner provides a repository and tag that
  the cluster can pull. The example does not choose, publish, or retain image
  coordinates.

## Metrics scope

Same baseline-only scope as the KIND example: `coraza_waf_requests_total` and
`coraza_waf_blocked_requests_total` via ALS → collector. See
[`docs/driver-metrics-contract.md`](../../../../docs/driver-metrics-contract.md).
