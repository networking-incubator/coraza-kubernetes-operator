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
| `--operator-name` | (none) | Helm release name. When set, the operator creates Istio ServiceEntry and DestinationRule prerequisites at startup. |

### TLS Certificates

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-cert-path` | (none) | Directory containing the metrics server TLS certificate. |
| `--metrics-cert-name` | `tls.crt` | Filename of the metrics certificate. |
| `--metrics-cert-key` | `tls.key` | Filename of the metrics private key. |

### RuleSet Cache

| Flag | Default | Description |
|------|---------|-------------|
| `--cache-gc-interval` | `5m` | How often to check for and remove stale cache entries. |
| `--cache-max-age` | `24h` | Maximum age before a cache entry is considered stale. |
| `--cache-max-size` | `104857600` (100 MB) | Maximum total size of all cached rules in bytes. |
| `--cache-server-port` | `18080` | Port for the RuleSet cache HTTP server. |
| `--envoy-cluster-name` | (required) | Envoy cluster name pointing to the cache server. |

### Istio Integration

| Flag | Default | Description |
|------|---------|-------------|
| `--istio-revision` | (none) | Istio revision label value for managed Istio resources. |
| `--default-wasm-image` | Built-in default | OCI reference for the Coraza WASM plugin used when an Engine omits the `image` field. Can also be set via the `CORAZA_DEFAULT_WASM_IMAGE` environment variable. |

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `POD_NAMESPACE` | Yes | The namespace in which the operator is running. Typically set via the Kubernetes downward API. |
| `CORAZA_DEFAULT_WASM_IMAGE` | No | Override the default WASM plugin OCI image. Equivalent to `--default-wasm-image`. |

## Logging

The operator uses [Zap](https://github.com/uber-go/zap) via controller-runtime. Logging behavior is controlled through Helm values rather than direct CLI flags:

| Helm Value | Effect |
|------------|--------|
| `logging.development` | Enables console encoder with debug level. |
| `logging.encoder` | Sets the log encoding format (`json` or `console`). |
| `logging.level` | Sets the minimum log level (`debug`, `info`, `error`). |
| `logging.stacktraceLevel` | Sets the minimum level for stack traces. |
| `logging.timeEncoding` | Sets the timestamp format. |
