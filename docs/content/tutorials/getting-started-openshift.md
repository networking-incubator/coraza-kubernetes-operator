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
- Gateway API enabled on your cluster (see [Enable Gateway API](#enable-gateway-api) below)
- [OpenShift Service Mesh](https://docs.redhat.com/en/documentation/red_hat_openshift_service_mesh/3.0/html-single/gateways/index) or Istio installed with Gateway API support
- [Helm 3](https://helm.sh/docs/intro/install/) installed
- The `oc` CLI configured to access your cluster
- Cluster administrator privileges

### Enable Gateway API

On OpenShift 4.19 and later, the Gateway API CRDs are included by default. You must create the `openshift-default` GatewayClass, which is the [officially supported GatewayClass](https://docs.redhat.com/en/documentation/openshift_container_platform/4.21/html/ingress_and_load_balancing/configuring-ingress-cluster-traffic#ingress-gateway-api) provided by the OpenShift Ingress Operator:

```bash
oc apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: openshift.io/gateway-controller/v1
EOF
```

On OpenShift 4.18 and earlier, you must install [Red Hat OpenShift Service Mesh 3.0](https://docs.redhat.com/en/documentation/red_hat_openshift_service_mesh/3.0/html-single/gateways/index) to enable Gateway API support.

## Step 1: Install the Operator

The preferred installation method on OpenShift is OperatorHub, but the operator is not yet published there (see [issue #201](https://github.com/networking-incubator/coraza-kubernetes-operator/issues/201)). In the meantime, install with Helm.

### Install with Helm

Ensure [Helm 3](https://helm.sh/docs/intro/install/) is installed, then add the repository and install:

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
  --set openshift.enabled=true \
  --set istio.revision=openshift-gateway \
  --set metrics.serviceMonitor.enabled=true
```

Setting `openshift.enabled` to `true` omits `runAsUser`, `fsGroup`, and `fsGroupChangePolicy` from the pod security context so that OpenShift can inject its own UID via Security Context Constraints (SCCs).

{{% alert title="Namespace conflict on versions 0.4.0 and earlier" color="warning" %}}
Versions 0.4.0 and earlier have a bug where the first install fails with `namespaces "coraza-system" already exists`. If you hit this error, run the same command again. The first run creates the namespace and a failed release record; the second run succeeds because Helm treats it as an upgrade, which patches the existing namespace instead of trying to create it.
{{% /alert %}}

For more installation options (version pinning, advanced values), see the [Install on OpenShift]({{< relref "../howto/install-openshift-operatorhub" >}}) how-to guide.

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

Create a Gateway using the `openshift-default` GatewayClass. This is the [officially supported GatewayClass](https://docs.redhat.com/en/documentation/openshift_container_platform/4.21/html/ingress_and_load_balancing/configuring-ingress-cluster-traffic#ingress-gateway-api) provided by the OpenShift Ingress Operator:

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: waf-gateway
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

Create **RuleSource** resources with SecLang. The first sets the base Coraza configuration; the second blocks requests whose query string contains the word "attack":

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: base-rules
spec:
  rules: |
    SecRuleEngine On
    SecRequestBodyAccess On
    SecResponseBodyAccess Off
---
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSource
metadata:
  name: block-attack
spec:
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

Create a **RuleSet** that lists those RuleSource names in order:

```bash
oc apply -n waf-tutorial -f - <<EOF
apiVersion: waf.k8s.coraza.io/v1alpha1
kind: RuleSet
metadata:
  name: tutorial-ruleset
spec:
  sources:
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
oc port-forward -n waf-tutorial svc/waf-gateway-openshift-default 8080:80 &
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

Check the Gateway logs to see the blocked request:

```bash
oc logs -n waf-tutorial deploy/waf-gateway-openshift-default
```

You should see a log entry from Coraza indicating the request was denied.

## Step 8: Clean Up

```bash
oc delete project waf-tutorial
```

## Next Steps

- Explore [Helm chart values]({{< relref "../reference/helm-values" >}}) for advanced OpenShift configuration.
- Learn how to [monitor the operator with Prometheus]({{< relref "../howto/monitoring-prometheus" >}}).
- Read about the [security model]({{< relref "../explanation/security-model" >}}).
