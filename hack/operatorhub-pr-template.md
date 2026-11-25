### New Submissions

* [x] Are you familiar with our [contribution guidelines](https://k8s-operatorhub.github.io/community-operators/contributing-via-pr/)?
* [x] Have you [packaged and deployed](https://k8s-operatorhub.github.io/community-operators/testing-operators/) your Operator for Operator Framework?
* [x] Have you tested your Operator with all Custom Resource Definitions?
* [x] Have you tested your Operator in all supported [installation modes](https://github.com/operator-framework/operator-lifecycle-manager/blob/master/doc/design/building-your-csv.md#operator-metadata)?
* [x] Have you considered whether you want use [semantic versioning order](https://k8s-operatorhub.github.io/community-operators/operator-ci-yaml/#semver-mode)?
* [x] Is your submission [signed](https://k8s-operatorhub.github.io/community-operators/contributing-prerequisites/#sign-your-work)?
* [x] Is operator [icon](https://k8s-operatorhub.github.io/community-operators/packaging-required-fields/) set?

### Updates to existing Operators

* [x] Did you create a `ci.yaml` file according to the [update instructions](https://k8s-operatorhub.github.io/community-operators/operator-ci-yaml/)?
* [x] Is your new CSV pointing to the previous version with the `replaces` property if you chose `replaces-mode` via the `updateGraph` property in `ci.yaml`?
* [x] Is your new CSV referenced in the [appropriate channel](https://k8s-operatorhub.github.io/community-operators/packaging-operator/#channels) defined in the `package.yaml` or `annotations.yaml`?
* [x] Have you tested an update to your Operator when deployed via OLM?
* [x] Is your submission [signed](https://k8s-operatorhub.github.io/community-operators/contributing-prerequisites/#sign-your-work)?

### Your submission should not

* [x] Modify more than one operator
* [x] Modify an Operator you don't own
* [x] Rename an operator - please remove and add with a different name instead
* [x] Modify any files outside the above mentioned folders
* [x] Contain more than one commit. **Please squash your commits.**

### Operator Description must contain (in order)

1. [x] Description about the managed Application and where to find more information
2. [x] Features and capabilities of your Operator and how to use it
3. [x] Any manual steps about potential pre-requisites for using your Operator

### Operator Metadata should contain

* [x] Human readable name and 1-liner description about your Operator
* [x] Valid [category name](https://github.com/k8s-operatorhub/community-operators/blob/main/categories.json)
* [x] One of the pre-defined [capability levels](https://sdk.operatorframework.io/docs/overview/operator-capabilities/)
* [x] Links to the maintainer, source code and documentation
* [x] Example templates for all Custom Resource Definitions intended to be used
* [x] A quadratic logo

### Notes for reviewers

The operator requires [Istio](https://istio.io/) and the Gateway API CRDs to be
installed; it attaches a Coraza WAF to existing Istio ingress Gateways and does
not install either itself.

It also requires a TLS Secret, `coraza-kubernetes-operator-serving-cert`, to
exist in the install namespace **before** the operator is installed. The
operator serves its configuration and its WASM module to Envoy over TLS and
does not issue that certificate itself, so the pod stays in
`ContainerCreating` until the Secret exists. Both prerequisites, and a
copy-pasteable way to create the Secret with or without cert-manager, are
documented in the CSV description under "Prerequisites".
