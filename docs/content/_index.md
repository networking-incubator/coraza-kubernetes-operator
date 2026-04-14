---
title: "Coraza Kubernetes Operator"
linkTitle: "Home"
---

{{< blocks/cover title="Coraza Kubernetes Operator" image_anchor="top" height="full" color="dark" >}}
<p class="lead mt-4">Declarative Web Application Firewall management for Kubernetes Gateways</p>
<div class="mx-auto mt-5">
	<a class="btn btn-lg btn-primary me-3 mb-4" href="{{< relref "/tutorials/getting-started-kubernetes" >}}">
		Get Started <i class="fas fa-arrow-alt-circle-right ms-2"></i>
	</a>
	<a class="btn btn-lg btn-secondary me-3 mb-4" href="https://github.com/networking-incubator/coraza-kubernetes-operator">
		GitHub <i class="fab fa-github ms-2 "></i>
	</a>
</div>
{{< blocks/link-down color="info" >}}
{{< /blocks/cover >}}

{{% blocks/lead color="primary" %}}
Deploy firewall engines that attach to your Kubernetes Gateways, and manage rules
through native Kubernetes resources.

Built on [Coraza](https://github.com/corazawaf/coraza) with full
[ModSecurity SecLang](https://github.com/owasp-modsecurity/ModSecurity/wiki/Reference-Manual-(v3.x)) compatibility.
{{% /blocks/lead %}}

{{< blocks/section color="white" type="row" >}}

{{% blocks/feature icon="fa-shield-alt" title="Engine API" %}}
Declaratively manage WAF instances attached to Kubernetes Gateways.
Deploy and configure firewall engines through simple custom resources.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-file-code" title="RuleSet API" %}}
Aggregate firewall rules from ConfigMaps. Rules are compiled,
validated, and cached automatically before being served to engines.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-sync-alt" title="Live Rule Updates" %}}
Rules are polled by engines at configurable intervals,
enabling updates without restarts or redeployments.
{{% /blocks/feature %}}

{{< /blocks/section >}}

{{< blocks/section color="info" type="row" >}}

{{% blocks/feature icon="fa-check-circle" title="Automatic Validation" %}}
Rules are compiled and validated before being served.
Invalid rules are caught early, with clear status conditions.
{{% /blocks/feature %}}

{{% blocks/feature icon="fab fa-github" title="Open Source" url="https://github.com/networking-incubator/coraza-kubernetes-operator" %}}
Fully open source. Contributions, issues, and feedback are welcome.
{{% /blocks/feature %}}

{{% blocks/feature icon="fa-cubes" title="Multi-Platform" %}}
Runs on Kubernetes v1.32+ and OpenShift v4.20+.
Integrates with Istio via WebAssembly (WASM) plugins.
{{% /blocks/feature %}}

{{< /blocks/section >}}

{{% blocks/section color="white" %}}

## Where to Start

| If you are... | Start here |
|---|---|
| **New to the operator?** | [Getting Started on Kubernetes]({{< relref "/tutorials/getting-started-kubernetes" >}}) |
| **Running OpenShift?** | [Getting Started on OpenShift]({{< relref "/tutorials/getting-started-openshift" >}}) |
| **Looking for a specific task?** | [How-to Guides]({{< relref "/howto" >}}) |
| **Need API details?** | [Reference]({{< relref "/reference" >}}) |
| **Want to understand the design?** | [Explanation]({{< relref "/explanation" >}}) |

{{% /blocks/section %}}
