# coraza-kubernetes-operator Helm Chart

Deploys the [Coraza Kubernetes Operator](https://github.com/networking-incubator/coraza-kubernetes-operator) — declarative Web Application Firewall (WAF) support for Kubernetes Gateways.

> **Requires Kubernetes ≥1.33.0 or OpenShift Container Platform ≥4.20.** The chart and operator use `resizePolicy` and the `Chart.yaml` enforces this minimum.


## Installation

### Default (Kubernetes)

```bash
helm template coraza-kubernetes-operator \
  ./charts/coraza-kubernetes-operator \
  --namespace coraza-system \
  --include-crds
```

### OpenShift

```bash
helm template coraza-kubernetes-operator \
  ./charts/coraza-kubernetes-operator \
  --namespace coraza-system \
  --include-crds \
  -f charts/coraza-kubernetes-operator/examples/openshift-values.yaml
```

When `openshift.enabled=true`, `runAsUser`, `fsGroup`, and `fsGroupChangePolicy` are omitted from the pod security context so OpenShift can inject its own UID via SCCs.

## Values

| Key                                                   | Type   | Default                                                   | Description                                                                                                 |
| ----------------------------------------------------- | ------ | --------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `replicas`                                            | int    | `1`                                                       | Number of operator replicas. A PodDisruptionBudget with `minAvailable: 1` is created automatically when > 1 |
| `image.repository`                                    | string | `ghcr.io/networking-incubator/coraza-kubernetes-operator` | Container image repository                                                                                  |
| `image.tag`                                           | string | `latest`                                                     | Container image tag                                                                                         |
| `image.pullPolicy`                                    | string | `IfNotPresent`                                            | Image pull policy                                                                                           |
| `imagePullSecrets`                                    | list   | `[]`                                                      | Image pull secrets                                                                                          |
| `resources.requests.cpu`                              | string | `10m`                                                     | CPU request                                                                                                 |
| `resources.requests.memory`                           | string | `128Mi`                                                   | Memory request                                                                                              |
| `resources.limits.cpu`                                | string | `500m`                                                    | CPU limit                                                                                                   |
| `resources.limits.memory`                             | string | `256Mi`                                                   | Memory limit                                                                                                |
| `metrics.enabled`                                     | bool   | `true`                                                    | Enable the controller-runtime metrics endpoint (port 8082)                                                  |
| `metrics.serviceMonitor.enabled`                      | bool   | `false`                                                   | Create a ServiceMonitor resource                                                                            |
| `istio.revision`                                      | string | `coraza`                                                  | Istio control plane revision label                                                                          |
| `createNamespace`                                     | bool   | `true`                                                    | Create the release namespace as a chart-managed resource with PSS labels                                    |
| `openshift.enabled`                                   | bool   | `false`                                                   | Omit UID/fsGroup from pod security context for OpenShift SCC compatibility                                  |
| `podSecurityStandard.version`                         | string | `latest`                                                  | Kubernetes version for Pod Security Standard labels (`latest` or `vX.YZ`)                                    |
| **Kubernetes version**                                | string | `1.33+`                                                   | Minimum cluster version required by this chart (due to resizePolicy API) |
| `nodeSelector`                                        | object | `{}`                                                      | Node selector constraints                                                                                   |
| `tolerations`                                         | list   | `[]`                                                      | Tolerations                                                                                                 |
| `affinity`                                            | object | `{}`                                                      | Affinity rules                                                                                              |
| `topologySpreadConstraints`                           | list   | `[]`                                                      | Topology spread constraints for pod scheduling                                                              |
