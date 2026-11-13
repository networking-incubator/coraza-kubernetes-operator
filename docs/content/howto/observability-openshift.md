---
title: "Observability on OpenShift"
linkTitle: "Observability on OpenShift"
weight: 47
description: "Deploy the central Istio ALS, Prometheus, and Grafana demonstrator on OpenShift."
---

This how-to deploys the OpenShift demonstrator for Coraza WAF telemetry:

```text
Coraza-protected Gateway → Istio ALS → central OpenTelemetry Collector →
OpenShift User Workload Monitoring → Thanos Querier → Grafana
```

It is a demonstrator, not a supported production topology. The fixture is in
`charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry/openshift/`.
Run every command below from the repository root. YAML resources are applied
with `oc apply -f`; CKO installation uses Helm directly.

The Gateway and all Engine workload resources use the OpenShift ingress
namespace named `openshift-ingress` (without a hyphen between `open` and
`shift`). Keep the central collector and Grafana resources in the separate
`coraza-central-waf-telemetry` namespace.

## Prerequisites

- OpenShift 4.20+ with Gateway API.
- Cluster Ingress Operator support for the GatewayClass-to-Istio WAF collector
  integration. It creates the `waf-log-collector` extension provider.
- Red Hat OpenTelemetry Operator installed.
- Grafana Operator installed from OperatorHub in `openshift-operators`.
- Cluster-admin access, `oc`, and Helm 3. Either provide an operator image the
  cluster can pull or have permission to create and run a binary `BuildConfig`
  in `coraza-system`.

Start by defining the common paths and collector namespace:

```bash
export REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
export EX="$REPO_ROOT/charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry"
export OS="$EX/openshift"
export COLLECTOR_NS=coraza-central-waf-telemetry
```

Choose the Coraza operator image for the environment before the installation
step. This example deliberately does not select a repository or tag.

## Enable User Workload Monitoring

Enable it once per cluster and wait for the managed monitoring components to
become ready. If `cluster-monitoring-config` does not exist, create it:

```bash
oc create configmap cluster-monitoring-config -n openshift-monitoring \
  --from-literal=config.yaml=$'enableUserWorkload: true\n'
```

If it already exists, preserve its other settings and add
`enableUserWorkload: true` under `data.config.yaml` with:

```bash
oc edit configmap cluster-monitoring-config -n openshift-monitoring
```

Then verify the configuration and wait for the managed monitoring components:

```bash
oc get configmap cluster-monitoring-config -n openshift-monitoring \
  -o jsonpath='{.data.config\.yaml}{"\n"}'
oc get pods -n openshift-user-workload-monitoring -w
```

The configuration must contain `enableUserWorkload: true`.

## Deploy the collector and ALS provider

```bash
# The annotation target must exist before it can be annotated.
oc apply -f "$OS/05-gatewayclass-openshift.yaml"
oc get gatewayclass openshift-default
oc apply -f "$EX/00-namespace.yaml"
oc apply -f "$OS/10-otel-collector-openshift.yaml"
oc rollout status -n "$COLLECTOR_NS" deployment/central-waf-als-collector --timeout=300s
oc apply -f "$OS/11-collector-destinationrule.yaml"

oc annotate gatewayclass openshift-default \
  'internal.do-not-use.openshift.io/waf-otel-collector=central-waf-als-collector.coraza-central-waf-telemetry.svc.cluster.local:4317' \
  --overwrite
oc get configmap -n openshift-ingress values-openshift-gateway \
  -o jsonpath='{.data.merged-values}' | grep -A20 -B5 waf-log-collector
```

Verify the collector image and the gRPC Service declaration:

```bash
oc get deployment -n "$COLLECTOR_NS" central-waf-als-collector \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
# ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.152.1

oc get svc -n "$COLLECTOR_NS" central-waf-als-collector \
  -o jsonpath='{.spec.ports[?(@.port==4317)].appProtocol}{"\n"}'
# grpc
```

The collector CR deliberately overrides only the collector workload image. The
Red Hat OpenTelemetry Operator still reconciles it. The upstream contrib image
includes `deltatocumulative`, which converts the count connector's delta sums
to Prometheus-safe cumulative counters. Keep one collector replica because
that processor keeps state in memory.

## Build or select the CKO image

To deploy an image you have already built and pushed to a registry reachable by
the cluster, set its complete reference. Building locally and pushing to a
registry is the recommended demonstrator workflow.

```bash
export OPERATOR_IMAGE=registry.example.com/your-project/coraza-kubernetes-operator:your-tag
```

Alternatively, build the current checkout inside OpenShift. This creates or
reuses a binary `BuildConfig` in `coraza-system`, sends the repository as the
build input, and installs the resulting internal-registry image. Choose a tag
that identifies your build:

```bash
export IMAGE_TAG=your-tag
BUILD_IMAGE=1 BUILD_METHOD=buildconfig IMAGE_TAG="$IMAGE_TAG" \
  "$OS/install-operator-openshift.sh"
```

The helper performs the Helm installation, so skip the next command block when
using the in-cluster build path.

## Install CKO from a selected image and deploy the WAF example

```bash
: "${OPERATOR_IMAGE:?Set OPERATOR_IMAGE to an image reference including its tag}"
export IMAGE_REPOSITORY="${OPERATOR_IMAGE%:*}"
export IMAGE_TAG="${OPERATOR_IMAGE##*:}"

helm upgrade --install coraza-kubernetes-operator \
  "$REPO_ROOT/charts/coraza-kubernetes-operator" \
  --namespace coraza-system \
  --create-namespace \
  -f "$OS/15-operator-openshift-values.yaml" \
  --set createNamespace=false \
  --set image.repository="$IMAGE_REPOSITORY" \
  --set image.tag="$IMAGE_TAG"
oc rollout status -n coraza-system deployment/coraza-kubernetes-operator --timeout=300s

# This must print the OPERATOR_IMAGE selected above.
oc get deployment -n coraza-system coraza-kubernetes-operator \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}{"\n"}'

oc apply -f "$OS/31-waf-workload.yaml"
oc wait -n openshift-ingress gateway/waf-gateway --for=condition=Programmed --timeout=180s
oc apply -f "$OS/40-waf-rules.yaml"
# Render the Engine with the proxy image selected for this test session. This
# does not change the committed example manifest.
export WASM_IMAGE=oci://registry.example.com/your-project/coraza-proxy-wasm:your-tag
awk -v image="$WASM_IMAGE" \
  '/^[[:space:]]*image: oci:/{sub(/oci:.*/, image)} {print}' \
  "$OS/41-waf-engine.yaml" | oc apply -f -

oc wait -n openshift-ingress engine/waf-engine --for=condition=Ready --timeout=180s

oc get telemetry -n openshift-ingress coraza-engine-waf-engine-telemetry \
  -o jsonpath='{.spec.accessLogging[0].providers[0].name}{"\n"}'
# waf-log-collector
```

Set `WASM_IMAGE` to a proxy build the cluster can pull. The command renders the
Engine with that image before applying it.

## Configure Prometheus and Grafana

```bash
oc apply -f "$OS/60-prometheus-monitors.yaml"
oc get servicemonitor -n "$COLLECTOR_NS" central-waf-als-collector
oc get podmonitor -n openshift-ingress waf-gateway-dataplane

export GRAFANA_NS="$COLLECTOR_NS"
oc apply -f "$OS/70-grafana-openshift.yaml"

THANOS_TOKEN="$(oc create token -n "$GRAFANA_NS" central-waf-grafana-sa --duration=24h)"
oc create secret generic -n "$GRAFANA_NS" central-waf-grafana-thanos-token \
  --from-literal=THANOS_TOKEN="$THANOS_TOKEN" \
  --dry-run=client -o yaml | oc apply -f -
unset THANOS_TOKEN

oc create configmap -n "$GRAFANA_NS" central-waf-grafana-dashboard \
  --from-file=coraza-waf.json="$REPO_ROOT/charts/coraza-kubernetes-operator/dashboards/coraza-waf.json" \
  --dry-run=client -o yaml | oc apply -f -
oc apply -f "$OS/71-grafana-datasource.yaml"
oc apply -f "$OS/72-grafana-dashboard.yaml"
```

The Grafana NetworkPolicy restricts egress to DNS and Thanos Querier on TCP
9091. It leaves ingress unisolated because an OpenShift router using
HostNetwork reaches the pod from a node IP. A portable namespace-based policy
cannot select that traffic. Route TLS and Grafana authentication protect
browser access.

Get the public Route and the generated credentials:

```bash
oc get route -n "$GRAFANA_NS" central-waf-grafana \
  -o jsonpath='https://{.spec.host}{"\n"}'

oc get secret -n "$GRAFANA_NS" central-waf-grafana-admin-credentials \
  -o jsonpath='{.data.GF_SECURITY_ADMIN_USER}' | base64 -d; echo
oc get secret -n "$GRAFANA_NS" central-waf-grafana-admin-credentials \
  -o jsonpath='{.data.GF_SECURITY_ADMIN_PASSWORD}' | base64 -d; echo
```

The dashboard imported by OpenShift is the same maintained
`charts/coraza-kubernetes-operator/dashboards/coraza-waf.json` used by the KIND
example; it is not a fork.

## Generate traffic and verify panels

In one terminal, forward the Gateway and collector ports:

```bash
oc port-forward -n openshift-ingress svc/waf-gateway-openshift-default 8080:80
oc port-forward -n "$COLLECTOR_NS" svc/central-waf-als-collector 19090:9090
```

In another terminal, send blocked traffic and inspect the collector counters:

```bash
for n in 1 2; do
  curl -sS -o /dev/null -w 'block %{http_code}\n' "http://localhost:8080/?q=attack&n=$n"
done
curl -fsS http://localhost:19090/metrics | grep -E '^coraza_waf_(requests|blocked_requests)_total'
```

Each request must return HTTP 403 and increase
`coraza_waf_blocked_requests_total` and `coraza_waf_requests_total`.

For continuous blocked traffic during dashboard validation:

```bash
while true; do
  curl -sS -o /dev/null "http://localhost:8080/?q=attack"
  sleep 15
done
```

Stop the loop with `Ctrl+C`. In Grafana Explore, validate the datasource with:

```promql
sum(rate(coraza_waf_blocked_requests_total[5m]))
```

## Troubleshooting

**Grafana reports `plugin.requestFailureError` or panels show no data.** Check
the Grafana logs for a Thanos DNS timeout. Reapply the NetworkPolicy bundled in
`70-grafana-openshift.yaml`; it permits both DNS port views required by the
OpenShift Service translation.

```bash
oc apply -f "$OS/70-grafana-openshift.yaml"
oc logs -n "$GRAFANA_NS" deployment/central-waf-grafana-deployment --since=10m
```

**The Route does not open.** Confirm the Route is admitted, the Service has an
endpoint, and Grafana is ready:

```bash
oc get route -n "$GRAFANA_NS" central-waf-grafana
oc get endpointslice -n "$GRAFANA_NS" -l kubernetes.io/service-name=central-waf-grafana-service
oc get grafana -n "$GRAFANA_NS" central-waf-grafana
```
