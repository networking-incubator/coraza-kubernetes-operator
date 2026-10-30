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
the operator xDS CA over TLS 1.3; public-CA hosts are not trusted yet. An Engine
that is left with no `url`/`sha256` will not fail visibly: it silently selects
the bundled module instead of the old image.

## Rendering Schema Upgrades

The operator renders each Engine's ECDS configuration into a versioned payload. When a release bumps the rendering schema, accepted configurations recorded by an older version are not published by the new build.

Affected Engines re-accept their configuration on the next reconcile and the new payload replaces the old one. While they wait, their older accepted records are ignored for operator readiness, so an upgrade does not block the operator from becoming Ready. An Engine that cannot re-accept (for example because its RuleSet is missing or no verified CA is configured) stays `Degraded` with `Ready=False`. An already-configured proxy keeps its previous WAF; a fail-open attachment may allow traffic without WAF; a new or reconnecting proxy has only its bootstrap policy. Do not assume every upgrade failure fails closed.

The readiness deadline applies to any accepted record this build never reconstructs, not only records left behind by a rendering schema bump; after the deadline the record stops gating operator readiness and is reported once.

## Rule Validation Guard

This release rejects rule text that opens or creates host files while the operator compiles it: `SecDebugLog` (including `/dev/stdout`), `SecUploadDir`, and `SecUploadKeepFiles` with any value other than `Off`. A RuleSet that an older version accepted with one of these directives now fails validation and becomes `Degraded` with `InvalidRuleSet`, and the Engine drops that content from its accepted configuration instead of publishing it again; an already-running proxy keeps the WAF it has, but a new or reconnecting proxy has only its bootstrap policy (which allows traffic for a fail-open Engine) until the directive is removed. RuleSets whose rules `Include` a file carrying such a directive are affected the same way.

`SecAuditLog` is rejected entirely: `envoy-coraza` has no audit sink and refuses any policy that names one. Remove the directive; `SecAuditLogType`, `SecAuditLogFormat`, and `SecAuditLogParts` are still accepted. Rules that enable `SecRequestBodyAccess` must also set `SecRequestBodyLimit` at or below 16 MiB; the engine default is 128 MiB and is rejected.

The same guard rejects `Include` targets the plugin cannot resolve. A CRS-enabled
`envoy-coraza` build (the CRS artifact `make demo` downloads, or `make wasm.crs`)
embeds the pinned OWASP CRS under `crs/`, so `Include crs/crs-setup.conf`, `Include crs/rules/*.conf`, and
a literal `crs/rules/<file>.conf` are accepted. The release bundle does not embed
CRS; use the generator or a CRS-enabled module for those includes. Every other glob is rejected, and the Go
plugin's `@` aliases (`@owasp_crs/...`, `@recommended-conf`, `@demo-conf`,
`@ftw-conf`, `@crs-setup-conf`) are **not** supported by `envoy-coraza`. A
literal `crs/<file>` must end in `.conf`: the `.conf.example` templates hold no
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

## Rolling Back

To roll back a Helm upgrade to the previous version:

```bash
helm rollback coraza-kubernetes-operator -n coraza-system
```

To list available revision history:

```bash
helm history coraza-kubernetes-operator -n coraza-system
```
