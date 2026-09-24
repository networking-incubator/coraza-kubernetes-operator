# End-to-End (E2E) Tests

E2E tests validate the **data plane** by deploying Gateways, Coraza
Engines, RuleSets, and backend applications, then sending real HTTP
traffic via port-forwards to verify the WAF blocks malicious requests
and allows benign ones.

This differs from `/test/integration`, which focuses on control-plane
behavior (reconciliation, status updates).

## Prerequisites

- Active cluster with Gateway API CRDs and a Gateway controller
  (e.g. Istio)
- Coraza Operator deployed
- `KUBECONFIG` pointing to the target cluster

## Running

```bash
# Kind cluster
make test.e2e \
  KIND_CLUSTER_NAME=coraza-kubernetes-operator-integration \
  ISTIO_VERSION=1.30.3

# OpenShift (override default GatewayClass)
GATEWAY_CLASS=openshift-default make test.e2e
```

### Central Istio ALS metrics

`TestCentralALSMetricsPipeline` validates the complete data-plane path:
Coraza Engine, WasmPlugin, Istio ALS, central OpenTelemetry Collector, and
the collector's Prometheus metrics endpoint. It requires a dedicated KIND
cluster because it temporarily updates the Istio `MeshConfig` provider.

```bash
make cluster.kind.otel

make test.e2e \
  TEST_ARGS='-run TestCentralALSMetricsPipeline -count=1 -v'
```

The test owns its minimal Kubernetes fixtures under
`test/e2e/testdata/central-als/`; it does not depend on chart examples.
