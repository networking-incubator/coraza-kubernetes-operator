---
title: "Operator CLI Flags"
linkTitle: "Operator CLI Flags"
weight: 30
description: "Command-line flags and environment variables for the operator manager."
---

The operator manager binary accepts the following command-line flags. When deployed via Helm, these are configured through the chart values and passed as container arguments.

## Flags

### Core

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `0` | Address for the metrics endpoint. Use `:8443` for HTTPS or `0` to disable. |
| `--health-probe-bind-address` | `:8081` | Address for the health and readiness probe endpoint. |
| `--leader-elect` | `false` | Enable leader election for controller manager. Required for running multiple replicas. |
| `--operator-name` | (none) | Helm release name. Used as a fallback for the default xDS advertise address when `--operator-service-name` is unset. |

### TLS Certificates

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-cert-path` | (none) | Directory containing the metrics server TLS certificate. |
| `--metrics-cert-name` | `tls.crt` | Filename of the metrics certificate. |
| `--metrics-cert-key` | `tls.key` | Filename of the metrics private key. |

### xDS Serving

The operator serves Envoy Configuration Discovery Service (ECDS) streams and the WASM asset over one TLS listener.

| Flag | Default | Description |
|------|---------|-------------|
| `--xds-bind-address` | `:18080` | Address the ECDS and WASM asset listener binds to. |
| `--xds-advertise-address` | (derived) | The operator address Gateways dial for ECDS. Defaults to `<operator-service-name>.<POD_NAMESPACE>.svc.cluster.local`. |
| `--xds-port` | `18080` | Port appended to the advertise address when it has none. |
| `--operator-service-name` | (none) | The operator Service name used to derive the default xDS advertise address. |
| `--asset-base-url` | (derived) | Base URL Envoy fetches the WASM asset from. Defaults to `https://<advertise-address>`. |
| `--xds-ca-path` | `/etc/coraza-tls/ca.crt` | CA bundle Gateways use to verify the operator certificate. |
| `--xds-cert-path` | `/etc/coraza-tls` | Directory containing the xDS serving certificate. |
| `--xds-cert-name` | `tls.crt` | Filename of the xDS serving certificate. |
| `--xds-key-name` | `tls.key` | Filename of the xDS serving key. |
| `--xds-token-audience` | (none) | Expected TokenReview audience; empty uses the API server default audience. |

### xDS Limits

| Flag | Default | Description |
|------|---------|-------------|
| `--xds-max-message-bytes` | `16777216` (16 MiB) | Maximum gRPC message size for the ECDS server, including the ECDS envelope. The rendered plugin configuration is separately capped at 8 MiB by `envoy-coraza`, so the operator refuses to start when this is below `8393728` (8 MiB plus 5 KiB of framing). The chart does not set this flag. |
| `--xds-max-request-bytes` | `262144` (256 KiB) | Maximum size of one ECDS request from a Gateway. Envoy's node metadata dominates the first request on a stream (about 47 KB for an Istio Gateway) and grows with the Gateway pod's labels and annotations; a Gateway whose requests exceed the limit stays on its bootstrap policy. A request message must also arrive within 10s of its first byte. |
| `--xds-write-timeout` | `30s` | Each 256 KiB of a response to an ECDS or WASM asset client must be written within this time, and a whole ECDS response within it plus its size at 64 KiB/s; otherwise the stream is reset. A connection whose writes make no progress for this long is closed with all its streams. Idle ECDS streams between updates are not affected; a connection idle for a minute is pinged and closed if it does not answer within 15s. |
| `--xds-max-decoded-data-bytes` | `6291456` (6 MiB) | Maximum decoded RuleData bytes carried by one Engine. `rule_data` is carried as JSON strings, and `envoy-coraza` caps the whole plugin configuration at 8 MiB, leaving room for rule text. The plugin accepts at most 8 MiB of rule data. |
| `--xds-max-data-files` | `1024` | Maximum number of RuleData files carried by one Engine. `envoy-coraza` accepts at most 1024. |
| `--xds-max-streams` | `512` | Maximum concurrent ECDS streams per replica. A quarter of this many requests may wait for their token review at once, and at most a quarter of this many TokenReviews for new streams run at once, including reviews that continue after their request ended; a request waits up to the 5s TokenReview timeout in all, and open-stream revalidation is not counted. Authentication slots and review permits are shared by all sources, so one source sending many invalid tokens can delay other Gateways' token reviews until its failure budget is spent. Each Gateway pod may hold at most 4 streams, and a stream whose ServiceAccount no Engine authorizes is refused. At most this many connections may carry no admitted ECDS stream. Over that, the newest displaces one of the source address holding the most such connections, and over `--xds-max-connections-per-source` one of its own source: first one without a request, then one with a request in progress, and a module download being written or a request whose token is under review only when no other is left. Connections that carry an admitted ECDS stream are never closed for room. How fair the rest is depends on how many source addresses hold such connections, Gateways included, not only flooding ones: when sources tie, a newcomer from another source closes the oldest connection without a request, which can be a Gateway's connection still in its handshake or before its first stream; a connection with a request in progress, under token review or downloading the module is closed only after idle ones. A connection must finish its TLS handshake within 10s, and an HTTP/2 connection that carries no request for 15s is closed. |
| `--xds-max-streams-per-identity` | `0` (`--xds-max-streams`) | Upper bound on concurrent ECDS streams per authenticated ServiceAccount on one replica. Within it a ServiceAccount may hold 4 streams per authorized Gateway pod running as it, plus 16 for pods not yet counted after a scale-up. Separately, and not bounded by this flag, all ServiceAccounts of a namespace together may hold 4 streams per Gateway pod counted in that namespace, plus 16 once. Gateway pods are counted once per Gateway: pods that carry the Gateway's label, are Running and not being deleted, are controlled by a ReplicaSet or Deployment, and name an existing ServiceAccount. The namespace allowance grows with such pods, so many namespaces each with Gateways and Engines, or many running pods, can still use most of a replica's streams; cluster administrators bound that with ResourceQuota and by who may create Engines. A Gateway scaled to more than about three times its counted pods before the pod count refreshes can see brief refusals, which Envoy retries. |
| `--xds-max-connections-per-source` | `0` | Maximum connections from one source address that carry no admitted ECDS stream, such as module downloads; over it one of them is closed for the newest, one without a request first. `0` means a 16th of `--xds-max-streams`; see that flag for which connection is closed when the whole pool is full. Host-network Gateway pods on one node share the node's address. Under connection pressure a module download in progress can be closed this way; Envoy retries it. |
| `--xds-stream-max-lifetime` | `10m` | Maximum ECDS stream lifetime before a forced reconnect (with jitter). |
| `--xds-token-review-revalidate` | `2m` | Interval between open-stream TokenReview and authorization revalidation. |
| `--xds-authorization-max-staleness` | `5m` | Maximum age of an authorization mapping before affected streams are closed. |
| `--xds-auth-failure-rate` | `10` | Failed ECDS authentication attempts allowed per second per peer IP; successful authentication does not consume the bucket. |
| `--xds-auth-failure-burst` | `1024` | Burst of failed ECDS authentication attempts per peer IP before the rate applies. It also bounds how many new streams from one peer IP can be in token review at once. The budget is per source address, so Gateways that share an address (host-network pods on one node, or pods behind NAT) share it, and a client on that address that sends many invalid tokens can delay those Gateways' reconnects until the budget refills; Gateways whose tokens are already cached are not affected. |

### Cluster Rule Policy

These flags narrow what tenants may use in rule text. They apply to new or changed RuleSet content only; see [Rule Processing]({{< relref "../explanation/rule-processing#cluster-rule-policy" >}}). An invalid value stops the manager at startup.

| Flag | Default | Description |
|------|---------|-------------|
| `--rule-denied-operators` | (none) | Comma-separated operator names (without `@`) tenants may not use, matched case-insensitively. An alias is denied with its operator (`pmf` with `pmFromFile`). A name `envoy-coraza` does not support is a startup error. |
| `--rule-denied-directives` | (none) | Comma-separated directive names tenants may not use, and the `ctl` actions with the same effect, matched case-insensitively. A directive `envoy-coraza` does not support is a startup error. |
| `--rule-max-request-body-limit` | `16777216` (16 MiB) | Highest `SecRequestBodyLimit` accepted, in bytes. Must be in (0, 16777216], the `envoy-coraza` ceiling. |
| `--rule-response-body-access` | `Allowed` | `Allowed` or `Denied`. `Denied` rejects `SecResponseBodyAccess On`. |
| `--rule-max-rules` | `20000` | Most rules one RuleSet may hold: `SecRule` (each rule of a chain), `SecAction` and `SecMarker` directives, in the rule text and in the RuleData files it `Include`s, counted once for every `Include` of a file because the WAF reads the file again each time. Must be positive. Unlike the flags above, it applies to every RuleSet, also content accepted before: a RuleSet above it reports `ContentTooLarge` and keeps serving what it accepted. The OWASP CRS generated by `kubectl coraza generate coreruleset` holds about 700. |

### Istio Integration

| Flag | Default | Description |
|------|---------|-------------|
| `--istio-revision` | (none) | Istio revision label value for managed Istio resources. |

> **Deprecated:** `--default-wasm-image` and `--envoy-cluster-name` are accepted but ignored.

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `POD_NAMESPACE` | Yes | The namespace in which the operator is running. Typically set via the Kubernetes downward API. |

## Logging

The operator uses [Zap](https://github.com/uber-go/zap) via controller-runtime. Logging behavior is controlled through Helm values rather than direct CLI flags:

| Helm Value | Effect |
|------------|--------|
| `logging.development` | Enables console encoder with debug level. |
| `logging.encoder` | Sets the log encoding format (`json` or `console`). |
| `logging.level` | Sets the minimum log level (`debug`, `info`, `error`). |
| `logging.stacktraceLevel` | Sets the minimum level for stack traces. |
| `logging.timeEncoding` | Sets the timestamp format. |
