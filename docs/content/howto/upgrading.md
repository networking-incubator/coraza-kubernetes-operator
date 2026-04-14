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

Helm automatically applies any CRD changes included in the new chart version.

## Upgrading on OpenShift (OLM)

If you installed the operator through OperatorHub with automatic approval, OLM handles upgrades automatically when new versions are published to the catalog.

If you chose manual approval, pending upgrades appear in the OpenShift web console under **Operators > Installed Operators**. Approve the upgrade to proceed.

## Checking Release Notes

Before upgrading, review the release notes for any breaking changes or migration steps:

- [GitHub Releases](https://github.com/networking-incubator/coraza-kubernetes-operator/releases)

## Rolling Back

To roll back a Helm upgrade to the previous version:

```bash
helm rollback coraza-kubernetes-operator -n coraza-system
```

To list available revision history:

```bash
helm history coraza-kubernetes-operator -n coraza-system
```
