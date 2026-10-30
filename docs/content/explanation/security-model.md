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
| `waf.k8s.coraza.io` **rulesources**, **ruledata** | get, list, watch | Read SecLang and data file content for RuleSet reconciliation. |
| Pods | list, watch | Discover Gateway pods matching Engine target names. |
| ServiceAccounts | get | Resolve the Gateway identity that dials ECDS. |
| Events | create, patch | Record events on managed resources. |
| Deployments | get | Read operator deployment metadata. |
| TokenReviews, SubjectAccessReviews | create | Authenticate and authorize metrics endpoint access. |
| Leases | create, delete, get, list, patch, update, watch | Leader election. |
| EnvoyFilters (Istio) | create, delete, get, list, patch, update, watch | Attach ECDS filters to Engine Gateway workloads. |
| Gateways (Gateway API) | get, list, watch | Discover and validate Gateways for Engine target resolution. |

### Namespace-Scoped Permissions (Role)

| Resource | Verbs | Purpose |
|----------|-------|---------|
| NetworkPolicies | create, delete, get, list, patch, update, watch | Restrict Gateway access to the operator's ECDS listener. |

The operator follows the principle of least privilege. It does not request permissions beyond what is needed for its controllers.

## Namespace Scoping

All **RuleSet**, **RuleSource**, **RuleData**, and **Engine** resources involved in a deployment must reside in the **same namespace** as each other. Cross-namespace references are not supported. This ensures that tenants in a multi-tenant cluster cannot reference each other's firewall rules.

## TLS Configuration

### Metrics Endpoint

The metrics endpoint is served over HTTPS with TLS 1.3 on port 8443. HTTP/2 is explicitly disabled to mitigate CVE-2023-44487 (HTTP/2 Rapid Reset attack). The TLS configuration enforces `NextProtos: []string{"http/1.1"}`.

The endpoint requires authentication and authorization via Kubernetes RBAC. Clients (such as Prometheus) must present a valid ServiceAccount token, and the ServiceAccount must be granted the `get` verb on the `/metrics` nonResourceURL. See [Monitoring with Prometheus]({{< relref "../howto/monitoring-prometheus#configuring-prometheus-rbac" >}}) for the required ClusterRole and ClusterRoleBinding.

By default, the operator generates a self-signed certificate. Users can provide their own certificate via the `metrics.certSecret` Helm value.

## ECDS NetworkPolicy

The operator creates a NetworkPolicy in its own namespace to control access to the ECDS listener. The policy:

- Allows ingress from Gateway pods that match an Engine's target.
- Restricts access to the operator's ECDS listener port.
- Is labeled with the Engine name and namespace for management tracking.
- Is cleaned up via a finalizer when the Engine is deleted.

## Pod Security Standards

The Helm chart configures the operator namespace with [Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/) labels at the `restricted` level. The operator pod:

- Runs as a non-root user.
- Uses a read-only root filesystem.
- Drops all capabilities.
- Uses a distroless base image (`gcr.io/distroless/static:nonroot`).

On OpenShift, setting `openshift.enabled: true` omits UID and filesystem group settings to allow OpenShift SCCs to manage them.
