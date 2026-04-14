---
title: "Security Model"
linkTitle: "Security Model"
weight: 50
description: "RBAC, TLS, authentication, and other security aspects of the operator."
---

This page describes the security model of the Coraza Kubernetes Operator, including RBAC permissions, network security, and authentication mechanisms.

## RBAC Permissions

The operator requires two sets of RBAC permissions:

### Cluster-Scoped Permissions (ClusterRole)

| Resource | Verbs | Purpose |
|----------|-------|---------|
| ConfigMaps, Secrets | get, list, watch | Read firewall rules and data files. |
| Pods | list, watch | Discover Gateway pods matching Engine workload selectors. |
| ServiceAccounts | create, get, list, patch, update, watch | Manage service accounts for cache authentication. |
| ServiceAccounts/token | create | Issue tokens for WASM plugin authentication. |
| Events | create, patch | Record events on managed resources. |
| Deployments | get | Read operator deployment metadata. |
| TokenReviews, SubjectAccessReviews | create | Authenticate and authorize metrics endpoint access. |
| Leases | create, delete, get, list, patch, update, watch | Leader election. |
| WasmPlugins (Istio) | create, delete, get, list, patch, update, watch | Manage Istio WASM plugin resources. |
| Gateways (Gateway API) | list, watch | Discover Gateways for Engine status reporting. |
| ServiceEntries, DestinationRules (Istio) | create, get, patch, update | Create Istio prerequisites for cache server mesh connectivity. |

### Namespace-Scoped Permissions (Role)

| Resource | Verbs | Purpose |
|----------|-------|---------|
| NetworkPolicies | create, delete, get, list, patch, update, watch | Manage network policies for cache server access. |

The operator follows the principle of least privilege. It does not request permissions beyond what is needed for its controllers.

## Namespace Scoping

All RuleSets, ConfigMaps, Secrets, and Engines must reside in the same namespace. Cross-namespace references are not supported. This ensures that tenants in a multi-tenant cluster cannot reference each other's firewall rules.

## TLS Configuration

### Metrics Endpoint

The metrics endpoint is served over HTTPS with TLS 1.3 on port 8443. HTTP/2 is explicitly disabled to mitigate CVE-2023-44487 (HTTP/2 Rapid Reset attack). The TLS configuration enforces `NextProtos: []string{"http/1.1"}`.

The endpoint requires authentication and authorization via Kubernetes RBAC. Clients (such as Prometheus) must present a valid ServiceAccount token, and the ServiceAccount must be granted the `get` verb on the `/metrics` nonResourceURL. See [Monitoring with Prometheus]({{< relref "../howto/monitoring-prometheus#configuring-prometheus-rbac" >}}) for the required ClusterRole and ClusterRoleBinding.

By default, the operator generates a self-signed certificate. Users can provide their own certificate via the `metrics.certSecret` Helm value.

### Cache Server

The RuleSet cache server listens on port 18080. Access to the cache server is controlled through:

- Kubernetes ServiceAccount token authentication.
- NetworkPolicies that restrict which pods can connect.

## Cache Server Authentication

The cache server authenticates requests using Kubernetes ServiceAccount tokens. When an Engine is created, the operator:

1. Creates a bound ServiceAccount token scoped to the Engine and RuleSet.
2. Passes the token to the WASM plugin via the WasmPlugin configuration.
3. Validates incoming tokens using the Kubernetes TokenReview API.

This ensures that only authorized WASM plugins can fetch rules from the cache server.

## NetworkPolicy

The operator creates a NetworkPolicy in its own namespace to control access to the cache server. The policy:

- Allows ingress from Gateway pods that match an Engine's workload selector.
- Restricts access to the cache server port only.
- Is labeled with the Engine name and namespace for management tracking.
- Is cleaned up via a finalizer when the Engine is deleted.

## Pod Security Standards

The Helm chart configures the operator namespace with [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/) labels at the `restricted` level. The operator pod:

- Runs as a non-root user.
- Uses a read-only root filesystem.
- Drops all capabilities.
- Uses a distroless base image (`gcr.io/distroless/static:nonroot`).

On OpenShift, setting `openshift.enabled: true` omits UID and filesystem group settings to allow OpenShift SCCs to manage them.
