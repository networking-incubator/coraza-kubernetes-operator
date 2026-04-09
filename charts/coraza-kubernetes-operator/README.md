# coraza-kubernetes-operator Helm Chart

Deploys the [Coraza Kubernetes Operator](https://github.com/networking-incubator/coraza-kubernetes-operator) — declarative Web Application Firewall (WAF) support for Kubernetes Gateways.

> **Requires Kubernetes ≥1.33.0 or OpenShift Container Platform ≥4.20.** The chart and operator use `resizePolicy` and the `Chart.yaml` enforces this minimum.


## Installation

### Helm repository (GitHub Pages)

After a version tag is pushed, CI publishes the packaged chart to the GitHub release and updates the Helm index on the `gh-pages` branch. With [GitHub Pages](https://docs.github.com/en/pages) enabled for this repository (source: branch `gh-pages`, root), install from the hosted repo:

```bash
helm repo add coraza-kubernetes-operator https://networking-incubator.github.io/coraza-kubernetes-operator/
helm repo update
helm upgrade --install coraza-kubernetes-operator coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace
```

Forks and other remotes use their own Pages URL: `https://<owner>.github.io/<repository>/`.

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
| `metrics.enabled`                                     | bool   | `true`                                                    | Enable the controller-runtime metrics endpoint (HTTPS on port 8443)                                         |
| `metrics.certSecret`                                  | string | `""`                                                      | Name of a Secret with TLS cert/key for metrics. When empty, a self-signed certificate is generated          |
| `metrics.certName`                                    | string | `tls.crt`                                                 | Key name of the certificate file inside `certSecret`                                                        |
| `metrics.keyName`                                     | string | `tls.key`                                                 | Key name of the private key file inside `certSecret`                                                        |
| `metrics.caName`                                      | string | `""`                                                      | Key name of a CA certificate inside `certSecret` for ServiceMonitor TLS verification                        |
| `metrics.serviceMonitor.enabled`                      | bool   | `false`                                                   | Create a ServiceMonitor resource                                                                            |
| `logging.development`                                 | bool   | `false`                                                   | Use console encoder with debug level (dev mode); when false, production flags below apply                   |
| `logging.encoder`                                     | string | `json`                                                    | Log encoding format (`json` or `console`). Only used when `development=false`                               |
| `logging.level`                                       | string | `info`                                                    | Minimum log level (`debug`, `info`, `error`). Only used when `development=false`                            |
| `logging.stacktraceLevel`                             | string | `error`                                                   | Minimum level for stack traces (`info`, `error`, `panic`). Only used when `development=false`               |
| `logging.timeEncoding`                                | string | `rfc3339nano`                                             | Timestamp format (`epoch`, `millis`, `nano`, `iso8601`, `rfc3339`, `rfc3339nano`). Only used when `development=false` |
| `istio.revision`                                      | string | `""`                                                      | Istio control plane revision label; empty means no revision label on managed resources                      |
| `defaultWasmImage`                                    | string | `""`                                                      | Default WASM plugin OCI URL when an Engine omits `spec.driver.istio.wasm.image`; empty uses operator built-in default |
| `createNamespace`                                     | bool   | `true`                                                    | Create the release namespace as a chart-managed resource with PSS labels                                    |
| `openshift.enabled`                                   | bool   | `false`                                                   | Omit UID/fsGroup from pod security context for OpenShift SCC compatibility                                  |
| `podSecurityStandard.version`                         | string | `latest`                                                  | Kubernetes version for Pod Security Standard labels (`latest` or `vX.YZ`)                                    |
| **Kubernetes version**                                | string | `1.33+`                                                   | Minimum cluster version required by this chart (due to resizePolicy API)                                    |
| `nodeSelector`                                        | object | `{}`                                                      | Node selector constraints                                                                                   |
| `tolerations`                                         | list   | `[]`                                                      | Tolerations                                                                                                 |
| `affinity`                                            | object | `{}`                                                      | Affinity rules                                                                                              |
| `topologySpreadConstraints`                           | list   | `[]`                                                      | Topology spread constraints for pod scheduling                                                              |

## Metrics

The metrics endpoint is always served over HTTPS on port **8443** with TLS 1.3 and requires authentication via [controller-runtime authentication/authorization filters](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/metrics/filters).

### Self-signed certificates (default)

When `metrics.certSecret` is empty, controller-runtime generates a self-signed certificate automatically. No extra configuration is needed.

### User-provided certificates

To use your own TLS certificate, create a Secret containing the certificate and key, then reference it:

```yaml
metrics:
  certSecret: my-metrics-tls
  certName: tls.crt    # key name inside the secret
  keyName: tls.key      # key name inside the secret
  caName: ca.crt        # optional: CA for ServiceMonitor TLS verification
```

### Prometheus RBAC

The metrics endpoint uses Kubernetes authentication. Prometheus must present a valid ServiceAccount token and the ServiceAccount must have permission to access the `/metrics` endpoint. Create a ClusterRole and ClusterRoleBinding:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: coraza-metrics-reader
rules:
  - nonResourceURLs: ["/metrics"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: coraza-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: coraza-metrics-reader
subjects:
  - kind: ServiceAccount
    name: prometheus  # adjust to your Prometheus SA
    namespace: monitoring
```
