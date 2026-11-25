---
title: "Istio WASM Integration"
linkTitle: "Istio WASM Integration"
weight: 30
description: "How the operator injects the Coraza WAF into Istio gateways with EnvoyFilter and ECDS."
---

The Coraza Kubernetes Operator integrates with Istio by patching the Gateway's Envoy
configuration directly (an `EnvoyFilter`) and streaming the WAF configuration to the proxy
over `ExtensionConfigDiscoveryService` (ECDS). No `WasmPlugin` resource is created.

## How the integration works

For each accepted Engine the operator:

1. Creates or updates an **EnvoyFilter** whose `spec.targetRefs` identifies the
   same-namespace `gateway.networking.k8s.io/Gateway`, with two patches:
   - a `CLUSTER` patch adding `coraza_wasm`, the HTTPS cluster that serves the module, and
   - an `HTTP_FILTER` patch inserting the WASM filter before the router, subscribed to ECDS
     for the Engine's resource name.
2. Serves the filter configuration over **ECDS** on its own listener (port 18080, HTTP/2),
   authenticated with the Gateway's ServiceAccount token and a CA-pinned TLS channel.
3. Serves the **bundled** module over HTTPS at `/wasm/<sha256>.wasm`; Envoy fetches it once,
   verifies the digest, and caches it, so rule updates never carry the binary again. With
   `spec.driver.wasm.url` set, Envoy fetches the module from that host instead and the
   operator neither serves nor proxies the custom bytes.

Until the ECDS resource arrives, Envoy applies the attachment's bootstrap config. For a
fail-closed Engine that is a **deny-all RBAC** default. This is not a blanket guarantee for
unaccepted Engines: a newly rejected Engine may have no EnvoyFilter, target rejection removes
an existing filter, and a fail-open Engine intentionally allows traffic. A Gateway has WAF
coverage only while an accepted ECDS resource is attached.

## The plugin

The WASM module is the `envoy-coraza` filter built from
[`coraza-rs`](https://github.com/networking-incubator/coraza-rs). It runs the
Coraza WAF engine inside Envoy and blocks or allows requests based on the rules it is given.

The operator's ECDS resource is the plugin's schema-versioned configuration: the assembled
SecLang text in `directives_map`, the RuleData files in `rule_data`, the failure policy,
and the engine identity. The module itself is referenced by URL and digest.

The artifact is built for `wasm32-wasip1` and linked with the WASI stubs from
`hack/wasi-stub.wat`, because Istio 1.31 ships Envoy 1.39, which does not provide those
imports; Envoy 1.40 and newer ignore the stubs. The release bundle does not embed CRS.
`make demo` downloads the published CRS-enabled module (`make wasm.crs` builds it
from source), whose pinned tree is addressed through coraza-rs's native `crs/` namespace: `Include crs/crs-setup.conf` and
`Include crs/rules/*.conf`. The previous Go plugin's `@owasp_crs` aliases are not
supported. See the [ECDS plugin
contract]({{< relref "/reference/ecds-plugin-contract" >}}) for the exact payload.

## Rule updates

When a RuleSet changes and the Engine accepts the new configuration, the operator publishes
a new ECDS snapshot. Envoy reconfigures the filter in place without restarting Gateway pods.

If the plugin rejects a payload, Envoy NACKs the update and keeps the previous configuration.

Each applied change is visible on both sides without raising log levels:

```
# operator
ECDS configuration published  version=sha256:... ruleSources=4 ruleBytes=... dataFiles=0
# proxy acknowledgement seen by the operator
ECDS configuration accepted by proxy  resource=coraza.waf.<ns>.<engine>.<uid>  version=sha256:...
```

Envoy itself only logs failures by default. Set `wasm=debug` on the proxy admin port
(`curl -X POST localhost:15000/logging?wasm=debug`) to see the plugin instance being
recreated. The module is not downloaded again: Envoy keeps it in its digest-keyed code cache, so a rule
change is a kilobyte-sized message. The compiled module is not re-instantiated either — the
VM is keyed by `vm_id` plus the module bytes — and only the WAF is rebuilt from the new
configuration.

## Target selection

The Engine's `target` field identifies a Gateway by name in the same namespace.
The EnvoyFilter uses this target as a `spec.targetRefs` entry:

```yaml
targetRefs:
- group: gateway.networking.k8s.io
  kind: Gateway
  name: <gateway-name>
```

The GEP-1762 Gateway pod label remains in use for pod selection by the ECDS
NetworkPolicy and controller watches; it is not the EnvoyFilter attachment.

The installed EnvoyFilter CRD schema and the Istiod revision that processes the
resource must both recognize `targetRefs`. The Istio networking API has this
field starting in 1.22
([1.22 API proto](https://github.com/istio/api/blob/release-1.22/networking/v1alpha3/envoy_filter.proto));
a CRD schema without it causes the operator's server-side apply to fail. The
complete CKO/WASM integration was live-tested with Istio 1.31.1. The API
field's introduction in 1.22 is not a guarantee that every Istio 1.22
deployment is supported, nor a complete product compatibility matrix.

In a mixed-revision mesh, ensure only a revision that understands `targetRefs`
processes the EnvoyFilter. Istio recommends pinning the resource to a 1.22-or-
newer revision with `istio.io/rev`; older control planes may interpret an
unknown `targetRefs` field as a namespace-wide EnvoyFilter. CKO adds this label
only when `--istio-revision` or the Helm `istio.revision` value is configured,
so set it to the compatible revision when older revisions coexist. See the
[Istio EnvoyFilter reference](https://istio.io/latest/docs/reference/config/networking/envoy-filter/)
for the current attachment and revision guidance.

The `target.provider` field (default: `Istio`) identifies the infrastructure provider, which
determines which driver types are valid for the Engine. This field is immutable after
creation — to switch providers, create a new Engine resource.

## Custom modules

By default the operator serves the module bundled in its image. To use another build, set
both fields on the Engine (see [Deploying a WAF
Engine]({{< relref "/howto/deploying-waf-engine" >}})):

```yaml
spec:
  driver:
    type: wasm
    wasm:
      url: "https://files.example.com/coraza-rs/envoy-coraza-<version>.wasm"
      sha256: "<64 lowercase hex characters>"
```

The URL must be HTTPS, and Envoy refuses bytes that do not match the digest. The URL host
must present a certificate chain that verifies against the operator xDS CA (`--xds-ca-path`)
and negotiate TLS 1.3; hosts with public certificates are not trusted yet. Until they are,
the fetch fails, the proxy NACKs the push, and a fail-closed Engine keeps its deny-all
bootstrap default while still reporting `Ready`.
