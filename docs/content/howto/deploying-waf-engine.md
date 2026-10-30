---
title: "Deploying a WAF Engine"
linkTitle: "Deploying a WAF Engine"
weight: 25
description: "Create an Engine resource to attach a WAF to a Kubernetes Gateway."
---

An Engine resource references a RuleSet and attaches the Coraza WAF to a Gateway. The operator renders an Istio `EnvoyFilter` for the Gateway and delivers the compiled rules to it over ECDS (Envoy Configuration Discovery Service).

## Creating an Engine

The minimum Engine configuration requires a RuleSet reference and a target that identifies your Gateway:

```yaml
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: my-engine
spec:
  ruleSet:
    name: my-ruleset
  target:
    type: Gateway
    name: my-gateway
    provider: Istio
```

## Selecting a Gateway

The `target.name` identifies the Gateway resource in the same namespace. The operator derives the workload label selector using the GEP-1762 convention (`gateway.networking.k8s.io/gateway-name` label).

Only one Engine resource may target a given Gateway. If multiple Engines reference the same Gateway, only the oldest one (by creation timestamp) is accepted; the others receive an `Accepted=False` condition with reason `TargetConflict`. See [Status Conditions]({{< relref "../reference/status-conditions" >}}) for details.

To verify your Gateway name:

```bash
kubectl get gateways -n my-namespace
```

## Configuring the Failure Policy

The `failurePolicy` field controls what happens when the WAF is not ready or encounters an error:

| Value | Behavior |
|-------|----------|
| `fail` (default) | Block all traffic when the WAF is not ready. |
| `allow` | Allow traffic through when the WAF is not ready. |

```yaml
spec:
  failurePolicy: allow
```

See [Configuring Failure Policies]({{< relref "configuring-failure-policies" >}}) for guidance on choosing.

## Rule Set Delivery

Rules are delivered to the Gateway over ECDS, and the operator pushes updates as soon as they are accepted. The Gateway does not poll. The `spec.ruleSetCacheServer` field is retained for API compatibility but is ignored.

## Selecting the WASM Module

By default, the operator serves its own bundled WASM module over its HTTPS asset route and
references it in the ECDS configuration; no module reference is needed. To use a custom
module, specify an HTTPS URL together with its SHA-256 digest, and Envoy fetches it:

```yaml
spec:
  driver:
    type: wasm
    wasm:
      url: "https://files.example.com/coraza-rs/envoy-coraza-<version>.wasm"
      sha256: "<sha256-of-the-exact-served-wasm-bytes>"
```

Calculate the digest from the exact raw file served at `url`, for example with
`sha256sum envoy-coraza.wasm`, and put its 64 lowercase hexadecimal characters in `sha256`.
The `url` and `sha256` fields must be set together: `url` must be an HTTPS URL without
userinfo, query, or fragment. Envoy rejects downloaded bytes whose digest differs. Omit
both fields (or set `wasm: {}`) to use the bundled module.

The custom URL host must present a certificate that chains to the CA configured with `--xds-ca-path` (the operator serving CA). Public certificate hosts are not supported by this MVP, so use an internal host with a matching certificate or keep the bundled module.

## Verifying the Engine

Check the Engine status:

```bash
kubectl get engine my-engine -n my-namespace
```

The output shows the referenced RuleSet, provider, target, failure policy, and readiness:

```
NAME        RULESET      PROVIDER   TARGET TYPE   TARGET NAME   FAILURE POLICY   READY   AGE
my-engine   my-ruleset   Istio      Gateway       my-gateway    fail             True    5m
```

For detailed status conditions and events:

```bash
kubectl describe engine my-engine -n my-namespace
```

Checking that the EnvoyFilter exists does not prove the WAF is enforcing. Send one benign and
one malicious request through the Gateway:

```bash
ADDR=$(kubectl -n my-namespace get gateway <gateway-name> \
  -o jsonpath='{.status.addresses[0].value}')

curl -s -o /dev/null -w '%{http_code}\n' "http://${ADDR}/"
curl -s -o /dev/null -w '%{http_code}\n' "http://${ADDR}/?q=%3Cscript%3Ealert(1)%3C%2Fscript%3E"
```

The first request returns `200`; the second returns `403` only if the selected RuleSet has a
deny rule matching that XSS payload. Choose a probe that matches a deny rule in your RuleSet:
for example, this XSS request may return `200` when the RuleSet contains only the sample
SQL-injection rule, and that result alone does not mean enforcement failed. If a matching
probe and the normal request both return `200`, check that the Engine is `Ready`, that the
Gateway pod loaded the module (`kubectl -n <gateway-namespace> logs <gateway-pod> -c
istio-proxy | grep -i wasm`), and that the operator logged `ECDS configuration accepted by
proxy` for the Engine. A `403` by itself is also insufficient: the fail-closed bootstrap
policy can deny traffic before the WASM rules load.

The operator also creates the EnvoyFilter and NetworkPolicy:

```bash
kubectl get envoyfilter,networkpolicy -n my-namespace
```
