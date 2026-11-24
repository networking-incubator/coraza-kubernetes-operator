---
title: "Install on Kubernetes from OperatorHub.io"
linkTitle: "Install (OperatorHub.io)"
weight: 12
description: "Install the Coraza Kubernetes Operator on a Kubernetes cluster with OLM, from the OperatorHub.io catalog."
---

This guide covers installing the Coraza Kubernetes Operator through the
[Operator Lifecycle Manager](https://olm.operatorframework.io/) (OLM) using the
[OperatorHub.io](https://operatorhub.io/) catalog. Use it if you already manage
operators with OLM; otherwise the [Helm installation](../install-kubernetes-helm/)
is simpler.

## Prerequisites

- Kubernetes cluster running **v1.32 or later**
- [Istio](https://istio.io/latest/docs/setup/) installed with [Gateway API CRDs](https://gateway-api.sigs.k8s.io/)
- Cluster administrator privileges
- OLM installed on the cluster

Install OLM if it is not present:

```bash
curl -sL https://github.com/operator-framework/operator-lifecycle-manager/releases/latest/download/install.sh | bash -s v0.28.0
```

{{% alert title="Note" color="info" %}}
The operator is not yet listed on OperatorHub.io. Track progress in [issue #201](https://github.com/networking-incubator/coraza-kubernetes-operator/issues/201). Until the listing is live, use the [Helm installation](../install-kubernetes-helm/) or point a `CatalogSource` at the project's own catalog image (see [Install from the project catalog](#install-from-the-project-catalog) below).
{{% /alert %}}

## Create the serving certificate first

The operator serves its configuration and the WASM module to Envoy over TLS,
and it does not issue that certificate itself. Installing with Helm, the chart
creates the Secret for you. There is no Helm run behind an OLM install, so you
create it yourself, **before** subscribing: the operator pod does not start
until the Secret exists, and OLM reports the install as failed if it is
missing.

The Secret must be named `coraza-kubernetes-operator-serving-cert`, live in the
namespace the operator is installed into, and hold three keys:

| Key | Contents |
|---|---|
| `tls.crt` | Serving certificate, valid for `coraza-kubernetes-operator.<namespace>.svc` |
| `tls.key` | Its private key |
| `ca.crt` | The CA that signed `tls.crt` |

`ca.crt` is not optional padding. The operator publishes it to every Gateway it
configures, and Envoy rejects a chain it cannot verify against it.

### With cert-manager

A self-signed issuer and a Certificate produce a Secret in exactly this shape:

```bash
kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: coraza-selfsigned
  namespace: operators
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: coraza-serving-cert
  namespace: operators
spec:
  isCA: true
  secretName: coraza-kubernetes-operator-serving-cert
  duration: 8760h
  renewBefore: 720h
  commonName: coraza-kubernetes-operator.operators.svc
  dnsNames:
    - coraza-kubernetes-operator
    - coraza-kubernetes-operator.operators
    - coraza-kubernetes-operator.operators.svc
    - coraza-kubernetes-operator.operators.svc.cluster.local
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: coraza-selfsigned
    kind: Issuer
EOF
```

`isCA: true` makes cert-manager write the signing certificate to `ca.crt`,
which is what the operator publishes to the Gateways.

### Without cert-manager

Generate a CA, sign a leaf for the Service name with it, and load all three
files:

```bash
NS=operators
SVC=coraza-kubernetes-operator

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes -keyout ca.key -out ca.crt -days 365 \
  -subj "/CN=${SVC}-ca"

openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes -keyout tls.key -out tls.csr -subj "/CN=${SVC}"

openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out tls.crt -days 365 \
  -extfile <(printf "subjectAltName=DNS:%s,DNS:%s.%s,DNS:%s.%s.svc,DNS:%s.%s.svc.cluster.local\nextendedKeyUsage=serverAuth" \
    "$SVC" "$SVC" "$NS" "$SVC" "$NS" "$SVC" "$NS")

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic coraza-kubernetes-operator-serving-cert \
  --namespace "$NS" \
  --from-file=ca.crt=ca.crt \
  --from-file=tls.crt=tls.crt \
  --from-file=tls.key=tls.key
```

Keep `ca.key` somewhere safe if you intend to re-sign later, and delete the
local copies of `tls.key` once the Secret exists.

### Rotation

Replace the Secret and restart the operator Deployment so it republishes the
CA. A leaf-only renewal signed by the same CA needs no restart: the listener
watches the certificate files and reloads them. Changing the CA does need one,
because the previous CA is already inlined into every rendered EnvoyFilter.

## Install from OperatorHub.io

Once the operator is listed, the OperatorHub.io catalog is already available on
any cluster running OLM as the `operatorhubio-catalog` `CatalogSource` in the
`olm` namespace. Create a `Subscription` against it:

```bash
kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: coraza-kubernetes-operator
  namespace: operators
spec:
  channel: alpha
  name: coraza-kubernetes-operator
  source: operatorhubio-catalog
  sourceNamespace: olm
  installPlanApproval: Automatic
EOF
```

The operator supports the `AllNamespaces` install mode only: it watches
RuleSources, RuleSets, RuleData and Engines cluster-wide, so it is installed
into the global operator namespace (`operators`) with a cluster-scoped
`OperatorGroup`. The default OLM installation already provides one.

## Install from the project catalog

Every release also publishes a catalog image you can consume directly:

```bash
kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: coraza-kubernetes-operator
  namespace: olm
spec:
  sourceType: grpc
  image: ghcr.io/networking-incubator/coraza-kubernetes-operator-catalog:vX.Y.Z
  displayName: Coraza Kubernetes Operator
  updateStrategy:
    registryPoll:
      interval: 10m
EOF
```

Then subscribe with `source: coraza-kubernetes-operator` and
`sourceNamespace: olm` instead of the OperatorHub.io values above.

## Verify the Installation

Watch the ClusterServiceVersion until it reports `Succeeded`:

```bash
kubectl get csv -n operators -w
```

```bash
kubectl get deployment -n operators -l control-plane=coraza-controller-manager
```

### If the pod never starts

A pod stuck in `ContainerCreating`, with an event reading
`secret "coraza-kubernetes-operator-serving-cert" not found`, means the Secret
is missing from the install namespace. The kubelet will not start the container
until it exists.

```bash
kubectl get secret -n operators coraza-kubernetes-operator-serving-cert
kubectl describe pod -n operators -l control-plane=coraza-controller-manager | tail -20
```

Create it as described in [Create the serving certificate
first](#create-the-serving-certificate-first), in the namespace the CSV was
installed into. The pod starts on the next kubelet sync; no reinstall is
needed.

### If the pod runs but never becomes ready

The Secret exists but its contents are not usable. Check the operator log:

```bash
kubectl logs -n operators -l control-plane=coraza-controller-manager | grep -i 'certificate\|CA bundle'
```

The usual causes are a certificate that does not cover
`coraza-kubernetes-operator.<namespace>.svc`, or a `ca.crt` that did not sign
`tls.crt`.

## Uninstall

```bash
kubectl delete subscription coraza-kubernetes-operator -n operators
kubectl delete csv -n operators coraza-kubernetes-operator.vX.Y.Z
```

Deleting the Subscription alone leaves the CSV behind, which is why both are
needed. Use the name `kubectl get csv -n operators` reports.

Deleting the CSV removes the operator but leaves the CRDs and your Engine,
RuleSet, RuleSource and RuleData resources in place. Remove them explicitly if
you want a clean slate:

```bash
kubectl delete crd engines.waf.k8s.coraza.io rulesets.waf.k8s.coraza.io \
  rulesources.waf.k8s.coraza.io ruledata.waf.k8s.coraza.io
```

## Next Steps

- [Deploying a WAF Engine](../deploying-waf-engine/)
- [Creating Firewall Rules](../creating-firewall-rules/)
- [Using OWASP CoreRuleSet](../using-coreruleset/)
