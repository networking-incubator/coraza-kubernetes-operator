---
title: "Install on Kubernetes with Helm"
linkTitle: "Install (Kubernetes/Helm)"
weight: 10
description: "Install the Coraza Kubernetes Operator on a Kubernetes cluster using Helm."
---

This guide covers installing the Coraza Kubernetes Operator using Helm on a standard Kubernetes cluster.

## Prerequisites

- Kubernetes cluster running **v1.32 or later**
- [Istio](https://istio.io/latest/docs/setup/) installed with [Gateway API CRDs](https://gateway-api.sigs.k8s.io/)
- [Helm 3](https://helm.sh/docs/intro/install/) installed

## Install from the Helm Repository

Add the Helm repository hosted on GitHub Pages and install:

```bash
helm repo add coraza-kubernetes-operator \
  https://networking-incubator.github.io/coraza-kubernetes-operator/
helm repo update
```

```bash
helm upgrade --install coraza-kubernetes-operator \
  coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace
```

## Install from the OCI Registry

Install directly from GitHub Container Registry without adding a Helm repository:

```bash
helm upgrade --install coraza-kubernetes-operator \
  oci://ghcr.io/networking-incubator/charts/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace
```

### Pin a Specific Version

```bash
helm upgrade --install coraza-kubernetes-operator \
  coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace \
  --version <chart-version>
```

Replace `<chart-version>` with the desired version (e.g. `0.1.0`). Available versions are listed on the [releases page](https://github.com/networking-incubator/coraza-kubernetes-operator/releases).

{{% alert title="Namespace conflict on versions 0.4.0 and earlier" color="warning" %}}
Versions 0.4.0 and earlier have a bug where the first install fails with `namespaces "coraza-system" already exists`. If you hit this error, run the same command again. The first run creates the namespace and a failed release record; the second run succeeds because Helm treats it as an upgrade, which patches the existing namespace instead of trying to create it.
{{% /alert %}}

## Customize the Installation

Override default values by passing individual settings:

```bash
helm upgrade --install coraza-kubernetes-operator \
  coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace \
  --set logging.level=debug \
  --set metrics.serviceMonitor.enabled=true
```

Alternatively, create a custom values file:

```yaml
# custom-values.yaml
logging:
  level: debug
  encoder: console

metrics:
  serviceMonitor:
    enabled: true

resources:
  requests:
    cpu: 50m
    memory: 256Mi
  limits:
    cpu: "1"
    memory: 512Mi
```

```bash
helm upgrade --install coraza-kubernetes-operator \
  coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace \
  -f custom-values.yaml
```

For the complete list of configurable values, see the [Helm Chart Values reference]({{< relref "../reference/helm-values" >}}).

## Verify the Installation

Check that the operator pod is running:

```bash
kubectl get pods -n coraza-system
```

Check the operator logs:

```bash
kubectl logs -n coraza-system deploy/coraza-kubernetes-operator
```

## Uninstall

```bash
helm uninstall coraza-kubernetes-operator -n coraza-system
```

To also remove the CRDs:

```bash
kubectl delete crd engines.waf.k8s.coraza.io rulesets.waf.k8s.coraza.io
```

{{% alert title="Note" color="warning" %}}
Removing CRDs will delete all Engine and RuleSet resources in the cluster.
{{% /alert %}}
