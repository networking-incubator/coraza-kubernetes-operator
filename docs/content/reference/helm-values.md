---
title: "Helm Chart Values"
linkTitle: "Helm Chart Values"
weight: 20
description: "Complete reference for Helm chart configuration values."
---

The Coraza Kubernetes Operator Helm chart is located at `charts/coraza-kubernetes-operator/`.

## Values Reference

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `replicas` | int | `1` | Number of operator replicas. A PodDisruptionBudget with `minAvailable: 1` is created automatically when greater than 1. |
| `image.repository` | string | `ghcr.io/networking-incubator/coraza-kubernetes-operator` | Container image repository. |
| `image.tag` | string | `latest` | Container image tag. |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | list | `[]` | Image pull secrets for private registries. |
| `resources.requests.cpu` | string | `10m` | CPU request. |
| `resources.requests.memory` | string | `128Mi` | Memory request. |
| `resources.limits.cpu` | string | `500m` | CPU limit. |
| `resources.limits.memory` | string | `256Mi` | Memory limit. |
| `metrics.enabled` | bool | `true` | Enable the controller-runtime metrics endpoint (HTTPS on port 8443). |
| `metrics.certSecret` | string | `""` | Name of a Secret with TLS cert/key for metrics. When empty, a self-signed certificate is generated. |
| `metrics.certName` | string | `tls.crt` | Key name of the certificate file inside `certSecret`. |
| `metrics.keyName` | string | `tls.key` | Key name of the private key file inside `certSecret`. |
| `metrics.caName` | string | `""` | Key name of a CA certificate inside `certSecret` for ServiceMonitor TLS verification. |
| `metrics.serviceMonitor.enabled` | bool | `false` | Create a Prometheus ServiceMonitor resource. |
| `logging.development` | bool | `false` | Use console encoder with debug level (development mode). When false, the production settings below apply. |
| `logging.encoder` | string | `json` | Log encoding format (`json` or `console`). Only used when `development` is false. |
| `logging.level` | string | `info` | Minimum log level (`debug`, `info`, `error`). Only used when `development` is false. |
| `logging.stacktraceLevel` | string | `error` | Minimum level for stack traces (`info`, `error`, `panic`). Only used when `development` is false. |
| `logging.timeEncoding` | string | `rfc3339nano` | Timestamp format (`epoch`, `millis`, `nano`, `iso8601`, `rfc3339`, `rfc3339nano`). Only used when `development` is false. |
| `istio.revision` | string | `""` | Istio control plane revision label. When empty, no revision label is set on managed resources. |
| `tls.existingSecret` | string | `""` | Existing Secret with `ca.crt`, `tls.crt`, and `tls.key` for the ECDS/WASM endpoint. When set, the chart creates no serving Secret. Required for GitOps and static-manifest installs. |
| `tls.generate` | bool | `true` | Generate a self-signed serving certificate (365-day CA and leaf) when `tls.existingSecret` is empty. Reuse across upgrades via `lookup` works only with `helm install`/`helm upgrade`; set to `false` for `helm template` and GitOps pipelines (Argo CD, Flux), which cannot reuse the Secret and would regenerate the CA on every sync. |
| `tls.sans` | list | `[]` | Extra DNS SANs added when the serving certificate is generated. Changing this list does not update a reused Secret; rotate the certificate and restart the operator before using a newly added hostname. |
| `defaultWasmImage` | string | `""` | Deprecated and ignored. Use the bundled module or `spec.driver.wasm.url` with `sha256`. |
| `createNamespace` | bool | `true` | Manage the release namespace with Pod Security Standard labels. Requires `--create-namespace` on first install. |
| `openshift.enabled` | bool | `false` | Omit `runAsUser`, `fsGroup`, and `fsGroupChangePolicy` from the pod security context for OpenShift SCC compatibility. |
| `podSecurityStandard.version` | string | `latest` | Kubernetes version for Pod Security Standard labels (`latest` or `vX.YZ`). |
| `nodeSelector` | object | `{}` | Node selector constraints. |
| `tolerations` | list | `[]` | Tolerations. |
| `affinity` | object | `{}` | Affinity rules. |
| `topologySpreadConstraints` | list | `[]` | Topology spread constraints for pod scheduling. |

## Serving certificate and GitOps

The chart's default `tls.generate=true` mints a self-signed CA and leaf (365-day lifetime) and reuses the generated Secret across upgrades via `lookup`. That reuse is a **`helm install`/`helm upgrade` behavior only**: `helm template`, `--dry-run`, and GitOps renderers (Argo CD, Flux) cannot read the cluster, so they regenerate the CA on every render, which breaks the Gateway trust bundle inlined into EnvoyFilters and causes ECDS TLS failures.

For GitOps and static-manifest installs, set `tls.generate=false` and provide the serving Secret out-of-band — via `tls.existingSecret`, platform tooling, or a certificate manager such as cert-manager (the chart itself adds no cert-manager dependency). Rotate a helm-managed certificate by deleting the generated Secret and re-running `helm upgrade`. Changing `tls.sans` does not update a reused Secret; rotate the certificate and restart the chart-rendered Deployment before using a newly added hostname. A new CA requires this restart so EnvoyFilters receive the new trust bundle; a leaf-only renewal does not.

## Platform Requirements

| Platform | Minimum Version |
|----------|----------------|
| Kubernetes | v1.32+ |
| OpenShift Container Platform | v4.20+ |

## OpenShift Values Example

For OpenShift installations, use the following values overlay:

```yaml
openshift:
  enabled: true

istio:
  revision: openshift-gateway

metrics:
  serviceMonitor:
    enabled: true
```

This overlay is also available at `charts/coraza-kubernetes-operator/examples/openshift-values.yaml` in the repository.
