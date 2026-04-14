---
title: "Getting Started on OpenShift"
linkTitle: "Getting Started (OpenShift)"
weight: 20
description: "Install the operator and deploy your first WAF rule on OpenShift."
---

This tutorial walks you through installing the Coraza Kubernetes Operator on OpenShift Container Platform, creating firewall rules, and verifying that the WAF is filtering traffic.

By the end, you will have a working WAF protecting a sample application behind an OpenShift Gateway.

## Prerequisites

Before you begin, ensure you have:

- An OpenShift Container Platform cluster running **v4.20 or later**
- [OpenShift Service Mesh](https://docs.openshift.com/container-platform/latest/service_mesh/v2x/installing-ossm.html) or Istio installed with Gateway API support
- The `oc` CLI configured to access your cluster
- Cluster administrator privileges

## Step 1: Install the Operator

You can install the Coraza Kubernetes Operator using either the OpenShift web console or the CLI.

### Option A: Install from OperatorHub (Web Console)

1. Open the OpenShift web console.
2. Navigate to **Operators > OperatorHub**.
3. Search for "Coraza Kubernetes Operator".
4. Select the operator and click **Install**.
5. Choose the installation namespace and approval strategy, then click **Install**.

<!-- TODO: Update with the published CatalogSource URL when available. -->

### Option B: Install with Helm

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

Setting `openshift.enabled` to `true` ensures the operator pod security context is compatible with OpenShift Security Context Constraints (SCCs).

Verify the operator is running:

```bash
oc get pods -n coraza-system
```

## Step 2: Deploy a Sample Application

Create a project for the tutorial and deploy a simple echo service:

```bash
oc new-project waf-tutorial
```

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: echo
  template:
    metadata:
      labels:
        app: echo
    spec:
      containers:
        - name: echo
          image: gcr.io/k8s-staging-gateway-api/echo-basic:v20231214-v1.0.0-140-gf544a46e
          ports:
            - containerPort: 3000
---
apiVersion: v1
kind: Service
metadata:
  name: echo
spec:
  selector:
    app: echo
  ports:
    - port: 80
      targetPort: 3000
EOF
```

## Step 3: Create a Gateway and HTTPRoute

Create a Gateway using the OpenShift gateway class:

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: waf-gateway
  labels:
    istio.io/rev: openshift-gateway
spec:
  gatewayClassName: openshift-default
  listeners:
    - name: http
      port: 80
      protocol: HTTP
      allowedRoutes:
        namespaces:
          from: Same
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: echo-route
spec:
  parentRefs:
    - name: waf-gateway
  rules:
    - backendRefs:
        - name: echo
          port: 80
EOF
```

Wait for the Gateway to be ready:

```bash
oc wait -n waf-tutorial gateway/waf-gateway \
  --for=condition=Programmed --timeout=60s
```

## Step 4: Define Firewall Rules

Create ConfigMaps containing SecLang firewall rules:

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: base-rules
data:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
    SecResponseBodyAccess Off
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: block-attack
data:
  rules: |
    SecRule ARGS "@contains attack" \
      "id:1001,\
      phase:2,\
      deny,\
      status:403,\
      msg:'Blocked: attack keyword detected'"
EOF
```

## Step 5: Create a RuleSet

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: tutorial-ruleset
spec:
  rules:
    - name: base-rules
    - name: block-attack
EOF
```

Check that the RuleSet is ready:

```bash
oc get ruleset -n waf-tutorial tutorial-ruleset
```

## Step 6: Deploy an Engine

Create an Engine that attaches the RuleSet to the Gateway. The `workloadSelector` must match the labels assigned to the Gateway pods by your Istio or Service Mesh installation:

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: Engine
metadata:
  name: tutorial-engine
spec:
  ruleSet:
    name: tutorial-ruleset
  failurePolicy: fail
  driver:
    istio:
      wasm:
        mode: gateway
        workloadSelector:
          matchLabels:
            gateway.networking.k8s.io/gateway-name: waf-gateway
        ruleSetCacheServer:
          pollIntervalSeconds: 5
EOF
```

Wait for the Engine to become ready:

```bash
oc wait -n waf-tutorial engine/tutorial-engine \
  --for=condition=Ready --timeout=120s
```

## Step 7: Verify the WAF

Port-forward to the Gateway:

```bash
oc port-forward -n waf-tutorial svc/waf-gateway-istio 8080:80 &
```

Test a normal request:

```bash
curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/
```

Expected output: `200`

Test a blocked request:

```bash
curl -s -o /dev/null -w "%{http_code}" "http://localhost:8080/?q=attack"
```

Expected output: `403`

## Step 8: Clean Up

```bash
oc delete project waf-tutorial
```

## Next Steps

- Explore [Helm chart values]({{< relref "../reference/helm-values" >}}) for advanced OpenShift configuration.
- Learn how to [monitor the operator with Prometheus]({{< relref "../howto/monitoring-prometheus" >}}).
- Read about the [security model]({{< relref "../explanation/security-model" >}}).
