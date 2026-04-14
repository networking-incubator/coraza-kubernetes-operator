---
title: "Getting Started on Kubernetes"
linkTitle: "Getting Started (Kubernetes)"
weight: 10
description: "Install the operator and deploy your first WAF rule on a Kubernetes cluster."
---

This tutorial walks you through installing the Coraza Kubernetes Operator on a Kubernetes cluster, creating firewall rules, and verifying that the WAF is filtering traffic.

By the end, you will have a working WAF protecting a sample application behind a Kubernetes Gateway.

## Prerequisites

Before you begin, ensure you have:

- A Kubernetes cluster running **v1.32 or later**
- [Istio](https://istio.io/latest/docs/setup/) installed with [Gateway API CRDs](https://gateway-api.sigs.k8s.io/)
- [Helm 3](https://helm.sh/docs/intro/install/) installed
- [kubectl](https://kubernetes.io/docs/tasks/tools/) configured to access your cluster

## Step 1: Install the Operator

Add the Helm repository and install the operator:

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

Verify that the operator is running:

```bash
kubectl get pods -n coraza-system
```

You should see the operator pod in a `Running` state.

## Step 2: Deploy a Sample Application

Create a namespace for the tutorial and deploy a simple echo service:

```bash
kubectl create namespace waf-tutorial
```

```bash
kubectl apply -n waf-tutorial -f - <<EOF
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

Create a Gateway to receive traffic and an HTTPRoute to send it to the echo service:

```bash
kubectl apply -n waf-tutorial -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: waf-gateway
spec:
  gatewayClassName: istio
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
kubectl wait -n waf-tutorial gateway/waf-gateway \
  --for=condition=Programmed --timeout=60s
```

## Step 4: Define Firewall Rules

Create ConfigMaps containing SecLang firewall rules. The first ConfigMap sets up the base Coraza configuration. The second defines a rule that blocks requests containing the word "attack":

```bash
kubectl apply -n waf-tutorial -f - <<EOF
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

Create a RuleSet resource that aggregates the ConfigMaps in order:

```bash
kubectl apply -n waf-tutorial -f - <<EOF
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
kubectl get ruleset -n waf-tutorial tutorial-ruleset
```

The `READY` column should show `True`.

## Step 6: Deploy an Engine

Create an Engine resource that attaches the RuleSet to the Gateway:

```bash
kubectl apply -n waf-tutorial -f - <<EOF
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
kubectl wait -n waf-tutorial engine/tutorial-engine \
  --for=condition=Ready --timeout=120s
```

## Step 7: Verify the WAF

Port-forward to the Gateway:

```bash
kubectl port-forward -n waf-tutorial svc/waf-gateway-istio 8080:80 &
```

Send a normal request (should succeed):

```bash
curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/
```

Expected output: `200`

Send a request containing the blocked keyword (should be denied):

```bash
curl -s -o /dev/null -w "%{http_code}" "http://localhost:8080/?q=attack"
```

Expected output: `403`

The WAF is working. Requests containing the word "attack" in any query parameter are blocked with a 403 status.

## Step 8: Clean Up

Remove all tutorial resources:

```bash
kubectl delete namespace waf-tutorial
```

## Next Steps

- Learn how to [create more complex firewall rules]({{< relref "../howto/creating-firewall-rules" >}}).
- Deploy the [OWASP CoreRuleSet]({{< relref "../howto/using-coreruleset" >}}) for comprehensive protection.
- Read the [Architecture]({{< relref "../explanation/architecture" >}}) overview to understand how the operator works.
