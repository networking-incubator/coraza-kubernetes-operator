# coraza-kubernetes-operator Helm Chart

Deploys the [Coraza Kubernetes Operator](https://github.com/networking-incubator/coraza-kubernetes-operator) — declarative Web Application Firewall (WAF) support for Kubernetes Gateways.

> **Requires Kubernetes ≥1.32.0 or OpenShift Container Platform ≥4.20.** The `Chart.yaml` enforces this minimum via `kubeVersion`.


## Installation

### Helm repository (GitHub Pages)

After a version tag is pushed, CI publishes the packaged chart to the GitHub release and deploys the Helm repo index to [GitHub Pages](https://docs.github.com/en/pages) (source: GitHub Actions). Install from the hosted repo:

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

> **GitOps / static renders:** `helm template` cannot reuse the generated serving certificate (see [Serving certificate](#serving-certificate) below) — it mints a fresh CA on every render. Add `--set tls.generate=false` and provide `tls.existingSecret`.

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
| `metrics.serviceMonitor.additionalLabels`             | object | `{}`                                                      | Extra labels on ServiceMonitor metadata (e.g. `release: kube-prometheus-stack`)                             |
| `metrics.prometheusRule.enabled`                      | bool   | `false`                                                   | Create PrometheusRule alerting and recording rules                                                            |
| `metrics.grafanaDashboard.enabled`                    | bool   | `false`                                                   | Create ConfigMap(s) with Grafana dashboard JSON for sidecar provisioning                                     |
| `metrics.grafanaDashboard.folder`                     | string | `Coraza WAF`                                              | Grafana folder annotation on dashboard ConfigMaps                                                           |
| `logging.development`                                 | bool   | `false`                                                   | Use console encoder with debug level (dev mode); when false, production flags below apply                   |
| `logging.encoder`                                     | string | `json`                                                    | Log encoding format (`json` or `console`). Only used when `development=false`                               |
| `logging.level`                                       | string | `info`                                                    | Minimum log level (`debug`, `info`, `error`). Only used when `development=false`                            |
| `logging.stacktraceLevel`                             | string | `error`                                                   | Minimum level for stack traces (`info`, `error`, `panic`). Only used when `development=false`               |
| `logging.timeEncoding`                                | string | `rfc3339nano`                                             | Timestamp format (`epoch`, `millis`, `nano`, `iso8601`, `rfc3339`, `rfc3339nano`). Only used when `development=false` |
| `istio.revision`                                      | string | `""`                                                      | Istio control plane revision label; empty means no revision label on managed resources                      |
| `tls.existingSecret`                                  | string | `""`                                                      | Existing Secret with `ca.crt`, `tls.crt`, and `tls.key` for the ECDS/WASM endpoint. Required for GitOps and static-manifest installs |
| `tls.generate`                                        | bool   | `true`                                                    | Generate a self-signed serving certificate (365-day CA and leaf) when `tls.existingSecret` is empty. Reuse across upgrades via `lookup` works only with `helm install`/`helm upgrade`; set to `false` for `helm template` and GitOps (Argo CD, Flux) |
| `tls.sans`                                            | list   | `[]`                                                      | Extra DNS SANs added when the serving certificate is generated. Changing this list does not update a reused Secret; rotate the certificate and restart the operator before using a newly added hostname. |
| `defaultWasmImage`                                    | string | `""`                                                      | Deprecated and ignored. Use the bundled module or `spec.driver.wasm.url` with `sha256`. |
| `createNamespace`                                     | bool   | `true`                                                    | Manage the release namespace with Pod Security Standard labels. Requires `--create-namespace` on first install |
| `openshift.enabled`                                   | bool   | `false`                                                   | Omit UID/fsGroup from pod security context for OpenShift SCC compatibility                                  |
| `podSecurityStandard.version`                         | string | `latest`                                                  | Kubernetes version for Pod Security Standard labels (`latest` or `vX.YZ`)                                    |
| **Kubernetes version**                                | string | `1.32+`                                                   | Minimum cluster version required by this chart                                                              |
| `nodeSelector`                                        | object | `{}`                                                      | Node selector constraints                                                                                   |
| `tolerations`                                         | list   | `[]`                                                      | Tolerations                                                                                                 |
| `affinity`                                            | object | `{}`                                                      | Affinity rules                                                                                              |
| `topologySpreadConstraints`                           | list   | `[]`                                                      | Topology spread constraints for pod scheduling                                                              |

### Serving certificate

The ECDS and WASM asset endpoint is served over TLS. The chart has two modes:

- **`helm install` / `helm upgrade` (default, `tls.generate=true`):** Helm generates a self-signed CA and leaf certificate, both valid for **365 days**, into `<release>-serving-cert`, and reuses that Secret on later upgrades via `lookup`. Reuse — and therefore a stable CA — is a `helm install`/`helm upgrade` behavior only.
- **`tls.existingSecret` / `tls.generate=false`:** the chart creates no certificate; the named Secret (keys `ca.crt`, `tls.crt`, `tls.key`) must already exist in the release namespace.

#### GitOps (Argo CD, Flux) and static renders

`lookup` returns nothing under `helm template`, `--dry-run`, and the server-side renders performed by Argo CD and Flux. With `tls.generate=true` such a pipeline takes the generate branch and **mints a fresh CA on every sync**. The Gateway trust bundle is inlined into the EnvoyFilter and only updates when the CA hash changes, so proxies end up trusting the old CA while the operator serves the new leaf: every ECDS stream fails TLS and, with the fail-closed bootstrap, Gateways return 503s.

GitOps and static-manifest installs must instead:

1. Set `tls.generate=false` (and `tls.existingSecret` to the Secret's name).
2. Provide the Secret out-of-band — created manually, by your platform tooling, or by a certificate manager such as cert-manager (a `Certificate` + `Issuer`, not a chart-managed resource; the chart adds no cert-manager dependency).

#### Rotation

The generated certificate is reused until the Secret is deleted, so it expires after 365 days even across upgrades. Changing `tls.sans` does not update the reused certificate: to use a newly added hostname, rotate the certificate first. For a Helm-generated certificate, delete `<chart-fullname>-serving-cert` and re-run `helm upgrade`. For `tls.existingSecret`, replace the Secret with a certificate containing the new SAN. Then restart the operator before using the hostname:

```bash
helm_release=coraza-kubernetes-operator
deployment=$(kubectl -n coraza-system get deployments \
  -l "app.kubernetes.io/instance=${helm_release},control-plane=coraza-controller-manager" \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n coraza-system rollout restart "deployment/${deployment}"
```

Set `helm_release` to your release name. The selector discovers the chart-rendered Deployment fullname rather than assuming it equals the release name. The operator reads `ca.crt` only at startup and inlines it into every EnvoyFilter, so after a CA change this restart publishes the new trust bundle. A leaf-only renewal under the same CA does not change the trust bundle.

Static manifests built with `make build.installer` or `make release.manifests` disable generation (`tls.generate=false`) so no private key is published. Before applying one of those manifests, create the serving Secret yourself as described above.

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

### Grafana dashboards

When `metrics.grafanaDashboard.enabled=true` **and** `metrics.prometheusRule.enabled=true`,
the chart deploys ConfigMaps labeled `grafana_dashboard=1` containing **Coraza Operator —
Overview** and **Coraza Operator — Resources** dashboards. Health summary panels query
recording rules from the chart PrometheusRule; enabling dashboards without PrometheusRule
is not supported. Compatible with the kube-prometheus-stack Grafana sidecar.

See [Observability demo on KIND](https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/docs/content/howto/observability-demo.md) for a local walkthrough.
