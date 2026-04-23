# Sample: Coraza WAF with Envoy Dynamic Module

Deploys a Coraza WAF Engine using the Envoy dynamic module driver instead of
WASM. Rules are embedded inline in an EnvoyFilter, and the `.so` is injected
into the gateway pod via an init container.

This is a workaround until Istio provides a native CRD for dynamic modules
(similar to WasmPlugin). See `docs/dynamic-module-driver.md` for details.

## What's included

| File | Description |
|------|-------------|
| `ruleset.yaml` | ConfigMaps with SecRule directives (base config, SQLi, XSS, custom) and a `RuleSet` CR |
| `engine.yaml` | `Engine` CR that references the RuleSet and configures the dynamic module driver with `moduleImage` |
| `gateway.yaml` | Kubernetes Gateway API `Gateway` with `parametersRef` pointing to the operator-managed ConfigMap |
| `httproute.yaml` | `HTTPRoute` that sends all traffic through the gateway to the echo service |
| `echo.yaml` | A simple echo Deployment and Service to act as the backend |

## Differences from the WASM sample (`config/samples/`)

- **Engine** uses `dynamicModule` driver instead of `wasm`
- **Gateway** includes `spec.infrastructure.parametersRef` so Istio injects the
  init container that copies `libcomposer.so` into the gateway pod
- **RuleSet** does not include `@pmFromFile` rules or data files, since
  file-referencing directives cannot resolve when rules are delivered inline

## Prerequisites

- Kubernetes cluster with Istio and Gateway API CRDs installed
- coraza-kubernetes-operator running in the cluster
- The `moduleImage` must be accessible from the cluster (the default in
  `engine.yaml` points to the BOE composer image on `ghcr.io`)

## Deploy

All samples must be deployed to the same namespace.

```bash
kubectl apply -f config/samples-dynamic-module/
```

After applying, verify the operator created the ConfigMap:

```bash
kubectl get configmap coraza-dm-coraza -o yaml
```

The Gateway's `parametersRef` in `gateway.yaml` already points to this
ConfigMap. Istio's gateway controller picks it up and injects the init container
into the gateway Deployment.

## Verify the init container

```bash
kubectl get pods -l gateway.networking.k8s.io/gateway-name=coraza-gateway -o jsonpath='{.items[0].spec.initContainers[*].name}'
```

You should see `dynamic-module-init` in the output.

## Test

```bash
kubectl port-forward svc/coraza-gateway-istio 8080:80
```

```bash
curl http://localhost:8080/                                  # normal request
curl -I "http://localhost:8080/?q=evilmonkey"                # blocked (rule 3001, 403)
curl "http://localhost:8080/?q=select+*+from+users"          # logged (rule 1001)
curl "http://localhost:8080/?q=<script>alert(1)</script>"    # logged (rule 2001)
```

Check gateway logs for Coraza output:

```bash
kubectl logs deploy/coraza-gateway-istio
```

## Cleanup

```bash
kubectl delete -f config/samples-dynamic-module/
```
