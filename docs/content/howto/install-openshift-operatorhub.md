---
title: "Install on OpenShift via OperatorHub"
linkTitle: "Install (OpenShift/OperatorHub)"
weight: 15
description: "Install the Coraza Kubernetes Operator on OpenShift using OperatorHub."
---

This guide covers installing the Coraza Kubernetes Operator on OpenShift Container Platform using the OperatorHub.

## Prerequisites

- OpenShift Container Platform **v4.20 or later**
- Cluster administrator privileges
- OpenShift Service Mesh or Istio installed with Gateway API support

## Install from OperatorHub (Web Console)

1. Log in to the OpenShift web console as a cluster administrator.
2. Navigate to **Operators > OperatorHub**.
3. Search for **Coraza Kubernetes Operator**.
4. Select the operator tile and click **Install**.
5. Choose the update channel, installation mode, and approval strategy.
6. Click **Install** and wait for the operator to reach the **Succeeded** phase.

<!-- TODO: Replace with the published CatalogSource details when available. -->

## Install from OperatorHub (CLI)

If the operator is available in your cluster's default catalog, create a Subscription resource:

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: coraza-kubernetes-operator
  namespace: openshift-operators
spec:
  channel: stable
  name: coraza-kubernetes-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
```

<!-- TODO: Update the source and channel fields when the operator is published. -->

```bash
oc apply -f subscription.yaml
```

## Install with Helm on OpenShift

If the operator is not yet available in OperatorHub, you can install it with Helm using the OpenShift values overlay:

```bash
helm repo add coraza-kubernetes-operator \
  https://networking-incubator.github.io/coraza-kubernetes-operator/
helm repo update
```

```bash
helm upgrade --install coraza-kubernetes-operator \
  coraza-kubernetes-operator/coraza-kubernetes-operator \
  --namespace coraza-system \
  --create-namespace \
  -f - <<EOF
openshift:
  enabled: true
istio:
  revision: openshift-gateway
metrics:
  serviceMonitor:
    enabled: true
EOF
```

Setting `openshift.enabled` to `true` omits `runAsUser`, `fsGroup`, and `fsGroupChangePolicy` from the pod security context so that OpenShift can inject its own UID via Security Context Constraints (SCCs).

## Verify the Installation

```bash
oc get pods -n coraza-system
```

The operator pod should be in a `Running` state.

## Uninstall

### OperatorHub

1. Navigate to **Operators > Installed Operators**.
2. Select the Coraza Kubernetes Operator.
3. Click **Uninstall Operator**.

### Helm

```bash
helm uninstall coraza-kubernetes-operator -n coraza-system
```
