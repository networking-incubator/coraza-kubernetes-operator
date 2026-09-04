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

## Prerequisites

- OpenShift 4.20+ with Gateway API.
- Cluster Ingress Operator support for the GatewayClass-to-Istio WAF collector
  integration. It creates the `waf-log-collector` extension provider.
- Red Hat OpenTelemetry Operator installed.
- Grafana Operator installed from OperatorHub in `openshift-operators`.
- Cluster-admin access, `oc`, Helm 3, and an operator image that the cluster
  can pull.

The commands use this tested development image:

```bash
export REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
export EX="$REPO_ROOT/charts/coraza-kubernetes-operator/examples/central-istio-waf-telemetry"
export OS="$EX/openshift"
export COLLECTOR_NS=coraza-central-waf-telemetry
export OPERATOR_IMAGE=quay.io/waf/coraza-kubernetes-operator:feat-otc-controller-d3c58786ab99
```

## Enable User Workload Monitoring

Enable it once per cluster and wait for the managed monitoring components to
become ready:

```bash
"$OS/01-enable-user-workload-monitoring.sh"
oc get configmap cluster-monitoring-config -n openshift-monitoring \
  -o jsonpath='{.data.config\.yaml}{"\n"}'
oc get pods -n openshift-user-workload-monitoring -w
```

The configuration must contain `enableUserWorkload: true`.

## Deploy the collector and ALS provider

```bash
oc apply -f "$OS/05-gatewayclass-openshift.yaml"
oc apply -f "$EX/00-namespace.yaml"
oc apply -f "$OS/10-otel-collector-openshift.yaml"
oc rollout status -n "$COLLECTOR_NS" deployment/central-waf-als-collector --timeout=300s
oc apply -f "$OS/11-collector-destinationrule.yaml"

"$OS/annotate-gatewayclass.sh"
"$OS/verify-mesh-waf-collector.sh"
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

## Install CKO and deploy the WAF example

```bash
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

oc apply -f "$OS/31-waf-workload.yaml"
oc wait -n openshift-ingress gateway/waf-gateway --for=condition=Programmed --timeout=180s
oc apply -f "$OS/40-waf-rules.yaml"
oc apply -f "$OS/41-waf-engine.yaml"
oc wait -n openshift-ingress engine/waf-engine --for=condition=Ready --timeout=180s

oc get telemetry -n openshift-ingress coraza-engine-waf-engine-telemetry \
  -o jsonpath='{.spec.accessLogging[0].providers[0].name}{"\n"}'
# waf-log-collector
```

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

The Grafana NetworkPolicy allows only its required dependencies: ingress from
the OpenShift router and Grafana Operator, DNS to `openshift-dns` (Service port
53 and pod port 5353), and Thanos Querier on TCP 9091. The DNS allowance is
required even though the services are in the same cluster: the policy restricts
egress by default.

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

Generate two blocked requests without restarting the collector between them:

```bash
BLOCK_REQUESTS=1 ALLOW_REQUESTS=0 SHOW_COLLECTOR_DEBUG=1 USE_LB=0 \
  "$OS/55-generate-traffic-and-logs.sh"
BLOCK_REQUESTS=1 ALLOW_REQUESTS=0 SHOW_COLLECTOR_DEBUG=1 USE_LB=0 \
  "$OS/55-generate-traffic-and-logs.sh"
```

Each request must return HTTP 403. The second run should take
`coraza_waf_blocked_requests_total` and `coraza_waf_requests_total` from 1 to
2. An allow request may still return 503; that is independent of the block ALS
path.

For continuous blocked traffic during dashboard validation:

```bash
while true; do
  BLOCK_REQUESTS=1 ALLOW_REQUESTS=0 USE_LB=0 \
    "$OS/55-generate-traffic-and-logs.sh" >/dev/null
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
