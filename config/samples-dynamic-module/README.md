# Sample: Coraza WAF with Envoy Dynamic Module Driver

Deploys a Coraza WAF Engine using the Envoy dynamic module driver instead of
WASM. The dynamic module embeds WAF rules inline in an EnvoyFilter and loads a
native `.so` shared library into the gateway pod.

## What's included

| File | Description |
|------|-------------|
| `ruleset.yaml` | `RuleSource` and `RuleSet` with basic SecLang directives |
| `engine.yaml` | `Engine` CR using `spec.driver.type: dynamicModule` |
| `gateway.yaml` | Kubernetes Gateway API `Gateway` with `parametersRef` pointing to the operator-managed ConfigMap |
| `httproute.yaml` | `HTTPRoute` that sends traffic through the gateway to the echo service |
| `echo.yaml` | Echo Deployment and Service backend |

## Prerequisites

- Kubernetes cluster with Istio (built with dynamic modules support) and Gateway API CRDs
- An Envoy proxy image that includes the dynamic modules HTTP filter
  extension (e.g., `gcr.io/istio-testing/proxyv2:1.31-dev`)
- coraza-kubernetes-operator running in the cluster

## Deploy

All samples must be deployed to the same namespace.

```bash
kubectl apply -f config/samples-dynamic-module/
```

The operator creates:

1. An **EnvoyFilter** that patches the gateway's Envoy config to load the
   dynamic module and pass WAF rules inline.
2. A **ConfigMap** (`coraza-dm-coraza`) containing a Deployment overlay that
   injects init containers to copy the `.so` into the gateway pod.

The Gateway's `spec.infrastructure.parametersRef` references this ConfigMap so
Istio's gateway controller applies the overlay when creating the gateway pod.

## Test

```bash
kubectl port-forward svc/coraza-gateway-istio 8080:80
```

```bash
curl http://localhost:8080/                                  # normal request
curl -I "http://localhost:8080/?q=evilmonkey"                # blocked (rule 3001, 403)
```

## Cleanup

```bash
kubectl delete -f config/samples-dynamic-module/
```
