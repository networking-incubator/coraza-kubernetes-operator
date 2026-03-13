![CI](https://github.com/networking-incubator/coraza-kubernetes-operator/actions/workflows/ci.yml/badge.svg)
![RELEASE](https://img.shields.io/github/v/release/networking-incubator/coraza-kubernetes-operator?include_prereleases)

# Coraza Kubernetes Operator

[Web Application Firewall (WAF)] support for [Kubernetes] [Gateways].

[Web Application Firewall (WAF)]:https://www.cloudflare.com/learning/ddos/glossary/web-application-firewall-waf/
[Kubernetes]:https://github.com/kubernetes
[Gateways]:https://gateway-api.sigs.k8s.io/api-types/gateway/

## About

The Coraza Kubernetes Operator (CKO) enables declarative management of [Web
Application Firewalls (WAF)] on Kubernetes clusters. Users can deploy
firewall engines which are attached to gateways, and rules which those
engines enforce.

[Coraza] is used as the firewall engine.

[Web Application Firewalls (WAF)]:https://wikipedia.org/wiki/Web_application_firewall
[Coraza]:https://github.com/corazawaf/coraza

### Key Features

- `Engine` API - declaratively manage WAF instances
- `RuleSet` API - declaratively manage firewall rules
- [ModSecurity Seclang] compatibility

[ModSecurity Seclang]:https://github.com/owasp-modsecurity/ModSecurity/wiki/Reference-Manual-(v3.x)

### Supported Platforms

The operator is designed to run on:

- **Kubernetes**: v1.33+
- **OpenShift Container Platform (OCP)**: v4.20+

### Supported Integrations

The operator integrates with other tools to attach WAF instances to
their gateways/proxies:

- `istio` - Istio integration ✅ **Currently Supported (ingress Gateway only)**
- `wasm` - WebAssembly deployment ✅ **Currently Supported**

> **Note**: Only Istio+WASM is supported currently.

### Architecture

The CKO's ruleset controller responds to `RuleSet` resources by validating and
compiling the rules (e.g. list of `ConfigMap` resources containing the
[Seclang] rules), which gets emitted to the `RuleSet` cache.

> **Note**: Currently, only [Seclang] rules are supported.

> **Warning**: Hosting or providing any packaged rules is an explicit non-goal
> of this project. Users must supply their own rules.

The keys for the cache are the `namespace/name` of the `RuleSet`, allowing the
compiled set of rules to be polled from a cache server hosting the cache.

> **Note**: All `RuleSets` and rules are restricted to same-namespace
> currently.

The engine controller responds to `Engine` resources by deploying the Coraza
engine according to the type and mode provided, and attaching it to a `Gateway`.

> **Note**: For example: if the type is `istio` and the mode is `wasm`, it
> will attach Coraza to an Istio `Gateway`, loading it via a [WASM] module.

`Engine` resources target a `RuleSet` to indicate the firewall rules that will
be applied to all `Gateway` traffic. Poll intervals for `RuleSets` can be set
to enable automatic and live rule updates on running `Engines`.

<img width="825" height="460" alt="cko-architecture-diagram" src="https://github.com/user-attachments/assets/e7b257e3-096f-4321-a40d-fe4e473480ac" />

[Seclang]:https://github.com/owasp-modsecurity/ModSecurity/wiki/Reference-Manual-(v3.x)
[WASM]:https://webassembly.org/

## Documentation

Documentation is available in the [wiki].

[wiki]:https://github.com/networking-incubator/coraza-kubernetes-operator/wiki

## Development: Source of Truth & Generation Pipeline

The project has several layers of generated artifacts. Understanding what is
the source of truth for each layer avoids editing the wrong file.

```
  Go types (api/)
       │
       │  controller-gen
       ▼
  config/crd/bases/*.yaml   CRDs
  config/rbac/role.yaml     ClusterRole
       │
       │  helm.sync (make helm.sync)
       ▼
  charts/.../crds/          copied from config/crd/bases/
  charts/.../clusterrole    rules injected from config/rbac/role.yaml
       │
       │  generate_bundle.py (make bundle)
       ▼
  bundle/manifests/         CSV, CRDs, extra manifests
  bundle/metadata/          OLM annotations
  bundle/bundle.Dockerfile
       │
       │  docker build (make bundle.build)
       ▼
  Bundle image              pushed to registry
       │
       │  opm render (inside catalog docker build)
       ▼
  Catalog image             serves rendered bundles to OLM
```

| What you want to change | Edit this (source of truth) | Then run |
|---|---|---|
| CRD fields, validation, status | `api/**/*_types.go` | `make manifests` then `make helm.sync` |
| RBAC permissions | `//+kubebuilder:rbac` markers in controllers | `make manifests` then `make helm.sync` |
| Helm chart templates (Deployment, Service, etc.) | `charts/.../templates/*.yaml` | nothing — these are the source |
| Helm chart values | `charts/.../values.yaml` | nothing — this is the source |
| OLM CSV metadata (description, icon, links) | `bundle/base/csv-template.yaml` | `make bundle` |
| OLM catalog channel/versions | `catalog/.../catalog.yaml` | `make catalog.update` to add a version |

**Do not edit directly** (these are regenerated):
- `config/crd/bases/*.yaml` — regenerated by `controller-gen`
- `config/rbac/role.yaml` — regenerated by `controller-gen`
- `charts/.../crds/*.yaml` — copied from `config/crd/bases/`
- `bundle/manifests/` — regenerated by `generate_bundle.py`

## Contributing

Contributions are welcome!

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0 - see [LICENSE](LICENSE).
