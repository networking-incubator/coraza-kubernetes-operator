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
| `resources.requests.memory` | string | `512Mi` | Memory request. See [Sizing the operator](#sizing-the-operator). |
| `resources.limits.cpu` | string | `500m` | CPU limit. |
| `resources.limits.memory` | string | `1Gi` | Memory limit. The operator sets its Go memory limit to 90% of it. See [Sizing the operator](#sizing-the-operator). |
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
| `xds.authFailureRate` | number | `10` | Failed ECDS authentication attempts allowed per second per peer IP. Successful authentication does not consume the bucket. |
| `xds.authFailureBurst` | int | `1024` | Burst of failed ECDS authentication attempts per peer IP before the rate applies. |
| `rulePolicy.deniedOperators` | list | `[]` | Operator names (without `@`) tenants may not use in new or changed rules. Matched case-insensitively; an alias is denied with its operator. An operator `envoy-coraza` does not support fails startup. |
| `rulePolicy.deniedDirectives` | list | `[]` | Directive names tenants may not use in new or changed rules, and the `ctl` actions with the same effect. Matched case-insensitively; a directive `envoy-coraza` does not support fails startup. |
| `rulePolicy.maxRequestBodyLimit` | int | `16777216` | Highest `SecRequestBodyLimit` accepted in new or changed rules, in bytes; at most 16777216, the `envoy-coraza` ceiling. |
| `rulePolicy.responseBodyAccess` | string | `Allowed` | `Allowed` or `Denied`. `Denied` rejects `SecResponseBodyAccess On` in new or changed rules. |
| `rulePolicy.maxRules` | int | `20000` | Most rules one RuleSet may hold; see `--rule-max-rules` in [Operator CLI Flags]({{< relref "operator-cli-flags" >}}). Applies to every RuleSet. |
| `tls.existingSecret` | string | `""` | Existing Secret with `ca.crt`, `tls.crt`, and `tls.key` for the ECDS/WASM endpoint. When set, the chart creates no serving Secret. Required for GitOps and static-manifest installs. |
| `tls.generate` | bool | `true` | Generate a self-signed serving certificate (365-day CA and leaf) when `tls.existingSecret` is empty. Reuse across upgrades via `lookup` works only with `helm install`/`helm upgrade`; set to `false` for `helm template` and GitOps pipelines (Argo CD, Flux), which cannot reuse the Secret and would regenerate the CA on every sync. |
| `tls.sans` | list | `[]` | Extra DNS SANs added when the serving certificate is generated. Changing this list does not update a reused Secret; rotate the certificate and restart the operator before using a newly added hostname. |
| `networkPolicy.egress.enabled` | bool | `false` | Restrict the operator's egress to the Kubernetes API server. Adds `Egress` to the operator NetworkPolicy with one rule for `apiServer.cidrs` on `apiServer.ports`. No DNS rule is added. |
| `networkPolicy.egress.apiServer.cidrs` | list | `[]` | API server endpoint addresses in CIDR form (usually `/32` or `/128`). When empty, `helm install`/`helm upgrade` read them from the `kubernetes` EndpointSlice in the `default` namespace; if none are found, rendering fails. Set explicitly for `helm template` and GitOps renderers. |
| `networkPolicy.egress.apiServer.ports` | list | `[6443, 443]` | API server endpoint ports allowed by the egress rule; at least one is required. When the addresses are read from the `kubernetes` EndpointSlice, its ports are allowed too. |
| `defaultWasmImage` | string | `""` | Deprecated and ignored. Use the bundled module or `spec.driver.wasm.url` with `sha256`. |
| `createNamespace` | bool | `true` | Manage the release namespace with Pod Security Standard labels. Requires `--create-namespace` on first install. |
| `openshift.enabled` | bool | `false` | Omit `runAsUser`, `fsGroup`, and `fsGroupChangePolicy` from the pod security context for OpenShift SCC compatibility. |
| `podSecurityStandard.version` | string | `latest` | Kubernetes version for Pod Security Standard labels (`latest` or `vX.YZ`). |
| `nodeSelector` | object | `{}` | Node selector constraints. |
| `tolerations` | list | `[]` | Tolerations. |
| `affinity` | object | `{}` | Affinity rules. |
| `topologySpreadConstraints` | list | `[]` | Topology spread constraints for pod scheduling. |

## Cluster rule policy

The `rulePolicy` values narrow what tenants may use in rule text. They apply to new or changed RuleSet content only; see [Rule Processing]({{< relref "../explanation/rule-processing#cluster-rule-policy" >}}).

```yaml
rulePolicy:
  deniedOperators: [pmFromFile, ipMatchFromFile]
  deniedDirectives: [SecRuleEngine]
  maxRequestBodyLimit: 1048576
  responseBodyAccess: Denied
```

## Sizing the operator

Every replica caches all RuleSource and RuleData objects in the cluster and compiles one RuleSet at a time. Plan the memory limit as:

- **Steady state:** the cached rule text and RuleData (about 1 byte of memory per byte stored; the kubectl last-applied annotation and managed fields are not cached), plus one published configuration per Engine, plus a runtime baseline. The operator with the OWASP CRS and three Engines uses about 63 MiB.
- **One RuleSet compile:** its cost depends on the shape of the rules, not only on their size, and the content limits bound what can be delivered rather than what a compile costs. Measured on a kind cluster: a maximal CRS-shaped RuleSet peaked at 492 to 556 MiB of container memory, with up to about 350 MiB of live heap. The rule limit refuses RuleSets of many small rules; the costliest shape within it, rules with long inline `@pm` lists, reached about 400 MiB of live heap and took about 18 s at 500m of CPU. Regular expressions that expand when compiled are not bounded yet. Without a memory limit the heap grows further.

The defaults (`512Mi` request, `1Gi` limit) cover one maximal CRS-shaped compile on top of a modest rule cache. The operator sets the Go runtime memory limit to 90% of `resources.limits.memory`, read from the `MEMORY_LIMIT_BYTES` environment variable when the container starts (an explicit `GOMEMLIMIT` takes precedence), so the garbage collector works harder before the container reaches its limit; it does not cap the live heap. Because the value is read at start, the chart's `resizePolicy` restarts the container on an in-place memory resize. Without a memory limit, Kubernetes passes the node's allocatable memory instead. Raise the limit when tenants store many large RuleSources.

Because every replica caches every rule object, and every RuleSet change can start a compile, cluster administrators should bound how many of each a tenant can create with a `ResourceQuota` in each tenant namespace, for example:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: coraza-rule-objects
  namespace: tenant-a
spec:
  hard:
    count/rulesets.waf.k8s.coraza.io: "10"
    count/rulesources.waf.k8s.coraza.io: "100"
    count/ruledata.waf.k8s.coraza.io: "20"
```

## Serving certificate and GitOps

The chart's default `tls.generate=true` mints a self-signed CA and leaf (365-day lifetime) and reuses the generated Secret across upgrades via `lookup`. That reuse is a **`helm install`/`helm upgrade` behavior only**: `helm template`, `--dry-run`, and GitOps renderers (Argo CD, Flux) cannot read the cluster, so they regenerate the CA on every render, which breaks the Gateway trust bundle inlined into EnvoyFilters and causes ECDS TLS failures.

For GitOps and static-manifest installs, set `tls.generate=false` and provide the serving Secret out-of-band — via `tls.existingSecret`, platform tooling, or a certificate manager such as cert-manager (the chart itself adds no cert-manager dependency). Rotate a helm-managed certificate by deleting the generated Secret and re-running `helm upgrade`. Changing `tls.sans` does not update a reused Secret; rotate the certificate and restart the chart-rendered Deployment before using a newly added hostname. A new CA requires this restart so EnvoyFilters receive the new trust bundle; a leaf-only renewal does not.

Rotate before expiry: Gateways reject an expired certificate. The operator reports upcoming expiry through metrics, logs and alerts (see [xDS serving certificate]({{< relref "../howto/monitoring-prometheus#xds-serving-certificate" >}})); expiry does not affect operator readiness.

## Restricting operator egress

The operator only opens outbound connections to the Kubernetes API server (including TokenReview); the ECDS listener, metrics and probes are inbound. `networkPolicy.egress.enabled=true` adds `Egress` to the operator's NetworkPolicy with a single rule: the API server endpoint addresses in `apiServer.cidrs`, on `apiServer.ports`. Everything else, including DNS, is denied. It is off by default because the right addresses depend on where the cluster's API server runs.

### When to enable it

Enable it when the API server endpoint addresses are known and stable, for example a self-managed cluster whose control-plane nodes keep their addresses.

Leave it off, or manage egress with your own policy, when:

- the API server runs outside the cluster behind addresses that change, such as a hosted or managed control plane whose load-balancer addresses are replaced during upgrades or scaling. A stale address cuts the operator off from the API server;
- the cluster's CNI does not match API server traffic against `ipBlock` rules for those addresses. CNIs differ here, so test it once per cluster type.

The rule lists the API server endpoint addresses, not the `kubernetes` Service ClusterIP, because most CNIs evaluate NetworkPolicy after the Service address is translated. The operator uses `KUBERNETES_SERVICE_HOST` (an IP), so no DNS rule is needed.

### Enable it with `helm install` or `helm upgrade`

When `apiServer.cidrs` is empty, the chart reads the addresses from the `kubernetes` EndpointSlice in the `default` namespace, and adds that EndpointSlice's ports to `apiServer.ports`:

```bash
helm upgrade --install coraza-kubernetes-operator charts/coraza-kubernetes-operator \
  --namespace coraza-system --reset-then-reuse-values \
  --set networkPolicy.egress.enabled=true
```

`--reset-then-reuse-values` keeps the values you set before and takes the new chart's defaults for the rest.

The addresses are read when the chart is rendered. If the API server endpoints change later, run `helm upgrade` again so the policy picks up the new addresses.

### Enable it with `helm template` or a GitOps tool

`lookup` returns nothing under `helm template`, `--dry-run` and GitOps renderers, so set the addresses explicitly; rendering fails if they are missing. Find them with:

```bash
kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{range .items[*].endpoints[*]}{.addresses[*]}{"\n"}{end}'
kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[*].ports[*].port}'
```

Then set them as `/32` (IPv4) or `/128` (IPv6) entries:

```yaml
networkPolicy:
  egress:
    enabled: true
    apiServer:
      cidrs: ["172.18.0.2/32"]
      ports: [6443, 443]
```

On OpenShift and managed clusters (EKS, GKE, AKS) the endpoints are control-plane node or load-balancer addresses. Include every address the cluster can return, and keep the list up to date when the control plane is scaled or replaced.

### Check that it works

After enabling it:

1. The operator pod stays `Ready` and does not restart: `kubectl -n coraza-system get pods`.
2. Leader election keeps renewing: `kubectl -n coraza-system get lease waf.k8s.coraza.io -o jsonpath='{.spec.renewTime}'` changes over time.
3. The operator logs show no API connection errors, and Engines keep reconciling.

If the operator loses the API server, set `networkPolicy.egress.enabled=false` (or fix `apiServer.cidrs`) and run `helm upgrade` again.

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
