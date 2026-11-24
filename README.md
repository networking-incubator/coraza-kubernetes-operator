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
firewall engines which are attached to `Gateways`, and rules which those
engines enforce.

[Coraza] is used as the firewall engine.

[Web Application Firewalls (WAF)]:https://wikipedia.org/wiki/Web_application_firewall
[Coraza]:https://github.com/corazawaf/coraza

### Key Features

- `Engine` API - declaratively manage WAF instances
- `RuleSet` API - declaratively manage firewall rules (via ordered `RuleSource` and `RuleData` references)
- `RuleSource` API - store SecLang rules consumed by a `RuleSet`
- `RuleData` API - store data files (e.g. for `@pmFromFile`) consumed by a `RuleSet`
- [ModSecurity Seclang] compatibility

[ModSecurity Seclang]:https://github.com/owasp-modsecurity/ModSecurity/wiki/Reference-Manual-(v3.x)

### Supported Platforms

The operator is designed to run on:

- **Kubernetes**: v1.32+
- **OpenShift Container Platform (OCP)**: v4.20+

### Supported Integrations

The operator integrates with other tools to attach WAF instances to
their gateways/proxies:

- `istio` - Istio integration ✅ **Currently Supported (ingress Gateway only)**
- `wasm` - WebAssembly deployment ✅ **Currently Supported**

> **Note**: Only Istio+WASM is supported currently.

### Architecture

`RuleSet` resources list named `RuleSource` objects in `spec.sources` (order
matters) for [Seclang] rule text, and optional `RuleData` objects in `spec.data`
for data files used with `@pmFromFile`. The operator validates and assembles the
inputs before accepting a configuration for an Engine.

> **Note**: Currently, only [Seclang] rules are supported.

`Engine` resources select a `RuleSet` and attach the Coraza WAF to a `Gateway`.
The operator publishes accepted configuration to Gateways over Envoy's
Extension Config Discovery Service (ECDS), and applies accepted updates without
restarting Gateway pods.

> **Warning**: Hosting or providing any packaged rules is an explicit non-goal
> of this project. Users must supply their own rules.

> **Note**: All `RuleSets` and rules are restricted to same-namespace
> currently.

For Istio, the Engine controller attaches Coraza with an `EnvoyFilter`; the
Gateway receives the WASM module over the operator's digest-addressed HTTPS
asset route.

<img width="825" height="460" alt="cko-architecture-diagram" src="https://github.com/user-attachments/assets/e7b257e3-096f-4321-a40d-fe4e473480ac" />

[Seclang]:https://github.com/owasp-modsecurity/ModSecurity/wiki/Reference-Manual-(v3.x)
[WASM]:https://webassembly.org/

## Documentation

Documentation is available at the [project documentation site].

[project documentation site]:https://networking-incubator.github.io/coraza-kubernetes-operator/latest/

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for build instructions, test suites,
and the source-of-truth / generation pipeline reference.

## Contributing

Contributions are welcome!

Please see the [CONTRIBUTING.md](CONTRIBUTING.md) guide before you get started.

## License

Apache License 2.0 - see [LICENSE](LICENSE).
