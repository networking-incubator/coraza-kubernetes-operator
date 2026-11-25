---
title: "Upgrading the Operator"
linkTitle: "Upgrading"
weight: 50
description: "Upgrade the Coraza Kubernetes Operator to a new version."
---

## Upgrading with Helm

To upgrade to the latest version:

```bash
helm repo update
helm upgrade coraza-kubernetes-operator \
  coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system
```

To upgrade to a specific version:

```bash
helm upgrade coraza-kubernetes-operator \
  coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system \
  --version 0.3.0
```

Before applying the new CRDs, inventory Engines that still use the removed OCI fields. The new
CRDs prune `spec.driver.wasm.image`, so this is the last point where the old reference is
readable:

```bash
kubectl get engines -A -o jsonpath='{range .items[?(@.spec.driver.wasm.image)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

Record each module reference and digest; [Migrating custom WASM modules](#migrating-custom-wasm-modules)
explains how to move them to `spec.driver.wasm.url` and `sha256` after the upgrade.

Helm upgrades workloads but does not upgrade existing CRDs. Apply the target release's CRDs before upgrading the operator:

```bash
kubectl apply -f https://github.com/networking-incubator/coraza-kubernetes-operator/releases/download/<version>/crds.yaml
```

Without the CRD update, the old RuleSet schema cannot store the new accepted-content fields and Engines cannot re-accept their configuration.

### Gateway filters are rebuilt once

The operator now sends each configuration under a tag in its version and its WASM VM ID, and
no longer honours the versions of earlier releases. After the upgrade, the first reconnect of
every Gateway pod delivers the tagged configuration, even if the rules did not change, and
the pod rebuilds its WAF filter once it can build it. If the upgrade also changes the WASM
module, the first tagged response may be rejected while the Gateway fetches the module, and
the operator then sends it again. This also recovers a Gateway that an earlier release left
on a configuration it had rejected, once the Gateway can build it: the upgrade does not make
a configuration that a Gateway keeps rejecting acceptable. Once a Gateway holds a tagged
version of a configuration, later restarts of the operator and moves between replicas do not
rebuild its filter while the configuration is unchanged, however many Gateways there are.
New rules or settings build the new configuration once on each Gateway, as before.

### Upgrading from a pre-ECDS release

The v0.5.0 release used Istio `WasmPlugin` resources for the WAF filter. ECDS
releases deliver WAF configuration over xDS and no longer create `WasmPlugin`
resources. An in-place upgrade from a release that used `WasmPlugin` is not
supported: the operator does not look those resources up or delete them, so a
legacy `coraza-engine-<engine-name>` `WasmPlugin` in the Engine's namespace stays
attached to the Gateway next to the new ECDS filter. Delete them as part of the
upgrade:

```bash
kubectl get wasmplugins.extensions.istio.io -A
kubectl -n <engine-namespace> delete wasmplugin coraza-engine-<engine-name>
```

Recreating an Engine does not remove them either.

## Upgrading on OpenShift (OLM)

If you installed the operator through OperatorHub with automatic approval, OLM handles upgrades automatically when new versions are published to the catalog.

If you chose manual approval, pending upgrades appear in the OpenShift web console under **Operators > Installed Operators**. Approve the upgrade to proceed.

## Checking Release Notes

Before upgrading, review the release notes for any breaking changes or migration steps:

- [GitHub Releases](https://github.com/networking-incubator/coraza-kubernetes-operator/releases)

### Migrating custom WASM modules

Use the inventory from the step above: the new API accepts only a paired
`spec.driver.wasm.url` and `sha256`; an Engine with neither selects the bundled
module. For a custom module, publish ECDS-compatible raw bytes at an HTTPS URL,
apply the upgraded CRDs (they prune the removed fields), then set both `url` and
`sha256` on each Engine. The URL host must present a certificate that chains to
the operator xDS CA over TLS 1.3, and the certificate must carry the URL host
as a Subject Alternative Name (a DNS SAN for hostnames, an IP SAN for
IP-literal hosts). The SAN is matched literally: a wildcard SAN such as
`*.svc.cluster.local` does not satisfy a specific host, and a CA-signed
certificate for a different name is rejected.
Public-CA hosts are not trusted yet. An Engine
that is left with no `url`/`sha256` will not fail visibly: it silently selects
the bundled module instead of the old image.

## Rendering Schema Upgrades

The operator renders each Engine's ECDS configuration into a versioned payload. When a release bumps the rendering schema, accepted configurations recorded by an older version are not published by the new build.

Affected Engines re-accept their configuration on the next reconcile and the new payload replaces the old one. While they wait, their older accepted records are ignored for operator readiness, so an upgrade does not block the operator from becoming Ready. An Engine that cannot re-accept (for example because its RuleSet is missing or no verified CA is configured) stays `Degraded` with `Ready=False`. An already-configured proxy keeps its previous WAF; a fail-open attachment may allow traffic without WAF; a new or reconnecting proxy has only its bootstrap policy. Do not assume every upgrade failure fails closed.

The readiness deadline applies to any accepted record this build never reconstructs, not only records left behind by a rendering schema bump; after the deadline the record stops gating operator readiness and is reported once.

## Rule Validation Guard

This release rejects rule text that opens or creates host files while the operator compiles it: `SecDebugLog` (including `/dev/stdout`), `SecUploadDir`, and `SecUploadKeepFiles` with any value other than `Off`. A RuleSet that an older version accepted with one of these directives now fails validation and becomes `Degraded` with `RejectedContent`, and the Engine drops that content from its accepted configuration instead of publishing it again; an already-running proxy keeps the WAF it has, but a new or reconnecting proxy has only its bootstrap policy (which allows traffic for a fail-open Engine) until the directive is removed. RuleSets whose rules `Include` a file carrying such a directive are affected the same way.

`SecAuditLog` is rejected entirely: `envoy-coraza` has no audit sink and refuses any policy that names one. Only `SecAuditLogType Serial` is supported; replace `Concurrent`, `Syslog`, or `Https` with `Serial`. `SecAuditLogFormat` and `SecAuditLogParts` are still accepted. Rules that enable `SecRequestBodyAccess` must also set `SecRequestBodyLimit` at or below 16 MiB; the engine default is 128 MiB and is rejected.

The same guard rejects `Include` targets the plugin cannot resolve. A CRS-enabled
`envoy-coraza` build (the CRS artifact `make demo` downloads, or `make wasm.crs`)
embeds the pinned OWASP CRS under `crs/`, so `Include crs/crs-setup.conf`, `Include crs/rules/*.conf`, and
a literal `crs/rules/<file>.conf` are accepted. Validation checks the operator image's
bundled module manifest; setting a custom `spec.driver.wasm.url` to a CRS-enabled module
does not enable `crs/...` includes when the bundled release module reports
`crs_version: none`. Use the generator to package rules as RuleSource/RuleData, or use an
operator image bundled with CRS. CKO rejects every other glob and all `@`
include aliases. For `@recommended-conf`, use the
[recommended `base-rules` manifest]({{< relref "/howto/creating-firewall-rules#start-with-the-recommended-configuration" >}})
instead. A literal `crs/<file>` must end in `.conf`: the `.conf.example` templates hold no
active rules, and the `.data` files are not SecLang, so including one would make
the plugin NACK the push while the Engine reported `Ready`. An `Include` argument
may be unquoted or wrapped in double quotes: Coraza strips every leading and
trailing `"` from a wholly quoted argument of at least three bytes, and nothing
else. A single-quoted or half-quoted target named a different file, building
either an empty WAF or a NACK, so a target that still carries a quote after that
is rejected. A non-glob target is trimmed, so extra spaces or a tab before the
name resolve to that file. A glob is not trimmed: Coraza expands it with the
argument as written, so a padded glob matches nothing. A RuleSet whose rules
`Include` a RuleData file name containing `/` is rejected as well.

## RuleData Size Limits

This release lowers the default decoded RuleData limit per Engine from 12 MiB to 6 MiB (`--xds-max-decoded-data-bytes`) and the data-file cap from 4096 to 1024 (`--xds-max-data-files`). RuleData sets above the new limits that were accepted before now fail acceptance: the Engine reports `Degraded` with `InvalidRuleSet` and its content is not delivered. Reduce the RuleData files, or raise the flags within the plugin's caps: 8 MiB of rule data, 1024 files, 1 MiB per file, and 8 MiB for the whole plugin configuration. The operator also refuses at startup a decoded limit whose keyed form would not fit `--xds-max-message-bytes`.

## Rule Content Size Limit

This release refuses, before it is compiled, RuleSet content that no Engine can deliver: rule text and merged RuleData that encode to more than the plugin's 8 MiB configuration limit, or compress past it, and RuleData past the Engine limits (`--xds-max-decoded-data-bytes`, `--xds-max-data-files`, 1 MiB per file). The size is checked for the longest possible Engine name, so content that an Engine with a short name delivered within 317 bytes of 8 MiB, or within about 2 KiB of the limit once compressed, is now refused. It also limits a RuleSet to `--rule-max-rules` rules, 20000 by default (Helm `rulePolicy.maxRules`); the OWASP CRS holds about 700. Such a RuleSet reports `Degraded` with `ContentTooLarge` and keeps serving the content it accepted before, and an Engine that meets content past its limits keeps its accepted configuration instead of withdrawing it. A RuleSet's content is compiled once; it is not compiled again on later reconciles until it changes.

The operator now refuses to start when `--xds-max-message-bytes` is set below 8393728, the smallest message that carries the largest plugin configuration. The chart does not set this flag, so only installations that pass it as a custom argument are affected.

The CRDs also bound the API: a RuleSet lists at most 256 `spec.sources` (was 2048) and 64 `spec.data` (was 256), and a RuleData holds at most 1024 files of at most 1048576 characters each (a RuleSet still refuses a file above 1048576 bytes). The RuleSet generated from the OWASP CRS uses 28 sources and one RuleData of 21 files. Objects already stored above the new bounds keep working; an edit that changes an over-limit field must bring it within the bound.

## Operator Memory Defaults

This release raises the chart's default operator memory to a `512Mi` request and a `1Gi` limit (from `128Mi` and `256Mi`): a maximal CRS-shaped RuleSet compile peaked at 492 to 556 MiB of container memory on a kind cluster. The operator now sets its Go memory limit to 90% of the container limit when it starts, and the chart restarts the container on an in-place memory resize so the limit follows. If you override `resources`, check the new sizing guidance in [Helm Chart Values]({{< relref "../reference/helm-values#sizing-the-operator" >}}).

## Rolling Back

To roll back a Helm upgrade to the previous version:

```bash
helm rollback coraza-kubernetes-operator -n coraza-system
```

To list available revision history:

```bash
helm history coraza-kubernetes-operator -n coraza-system
```
