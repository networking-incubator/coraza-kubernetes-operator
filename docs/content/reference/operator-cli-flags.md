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

### RuleSet Cache

These flags are accepted for backward compatibility but ignored: the legacy HTTP rule cache server is not started and rules are delivered over ECDS. They have no effect on rule delivery.

| Flag | Default | Description |
|------|---------|-------------|
| `--cache-gc-interval` | `5m` | Ignored. |
| `--cache-max-age` | `24h` | Ignored. |
| `--cache-max-size` | `104857600` (100 MB) | Ignored. |

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
| `--xds-max-streams` | `512` | Maximum concurrent ECDS streams per replica. |
| `--xds-max-streams-per-identity` | `512` | Maximum concurrent ECDS streams per authenticated ServiceAccount on one replica. |
| `--xds-stream-max-lifetime` | `10m` | Maximum ECDS stream lifetime before a forced reconnect (with jitter). |
| `--xds-token-review-revalidate` | `2m` | Interval between open-stream TokenReview and authorization revalidation. |
| `--xds-authorization-max-staleness` | `5m` | Maximum age of an authorization mapping before affected streams are closed. |
| `--xds-auth-failure-rate` | `10` | Failed ECDS authentication attempts allowed per second per peer IP; successful authentication does not consume the bucket. |
| `--xds-auth-failure-burst` | `1024` | Burst of failed ECDS authentication attempts per peer IP before the rate applies. |

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
