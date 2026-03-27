#!/usr/bin/env python3
"""
Set up or tear down a Coraza integration environment on OpenShift.

Automates deployment of the internal registry, OSSM/Istio via the Sail
operator, and the Coraza operator itself.
"""

import argparse
import os
import sys
import time
from pathlib import Path

from lib import (
    HELM_CHART_DIR, HELM_RELEASE_NAME,
    detect_container_runtime, die, run,
)

# ---------------------------------------------------------------------------
# Version Helpers
# ---------------------------------------------------------------------------


def get_versions(args) -> tuple[str, str]:
    """Resolve Istio and OSSM versions from args/env, ensuring 'v' prefix."""
    istio = args.istio_version or os.environ.get("ISTIO_VERSION") or "v1.27.5"
    ossm = args.ossm_version or os.environ.get("OSSM_VERSION") or "v3.2.2"
    if not istio.startswith("v"):
        istio = f"v{istio}"
    if not ossm.startswith("v"):
        ossm = f"v{ossm}"
    return istio, ossm


# ---------------------------------------------------------------------------
# Internal Registry
# ---------------------------------------------------------------------------


def setup_internal_registry(args):
    """Expose the OCP internal image registry and grant pull/push RBAC."""
    print(f"--- Setting up OCP Internal Registry in {args.coraza_ns} ---")
    run(
        "oc patch configs.imageregistry.operator.openshift.io/cluster "
        "--patch '{\"spec\":{\"defaultRoute\":true}}' --type=merge"
    )

    # Wait for the registry route to appear
    url = ""
    start = time.time()
    while time.time() - start < args.timeout:
        res = run(
            "oc get route default-route -n openshift-image-registry "
            "--template='{{ .spec.host }}'",
            check=False, capture_output=True,
        )
        if res.returncode == 0:
            url = res.stdout.strip()
            break
        time.sleep(5)
    if not url:
        die("Timed out waiting for internal registry route")

    run(
        f"oc create namespace {args.coraza_ns} "
        f"--dry-run=client -o yaml | oc apply -f -"
    )

    rbac_manifest = """\
apiVersion: v1
kind: List
items:
- apiVersion: rbac.authorization.k8s.io/v1
  kind: RoleBinding
  metadata:
    name: image-puller
  roleRef:
    apiGroup: rbac.authorization.k8s.io
    kind: ClusterRole
    name: system:image-puller
  subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: system:unauthenticated
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: system:serviceaccounts
- apiVersion: rbac.authorization.k8s.io/v1
  kind: RoleBinding
  metadata:
    name: image-pusher
  roleRef:
    apiGroup: rbac.authorization.k8s.io
    kind: ClusterRole
    name: system:image-builder
  subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: Group
    name: system:unauthenticated
"""
    print(f"Applying registry RoleBindings in {args.coraza_ns}...")
    run(f"oc apply -f - -n {args.coraza_ns}", input_str=rbac_manifest)


# ---------------------------------------------------------------------------
# GatewayClass & Istio Control Plane
# ---------------------------------------------------------------------------


def deploy_gateway_class(args, istio_version, ossm_version):
    """Create the OpenShift GatewayClass and wait for the OSSM CSV."""
    if not ossm_version.startswith("servicemeshoperator3."):
        ossm_version = f"servicemeshoperator3.{ossm_version}"

    print(
        f"--- Creating GatewayClass "
        f"(Istio: {istio_version}, OSSM: {ossm_version}) ---"
    )

    gw_class_yaml = f"""
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
  annotations:
    unsupported.do-not-use.openshift.io/ossm-channel: stable
    unsupported.do-not-use.openshift.io/ossm-version: {ossm_version}
    unsupported.do-not-use.openshift.io/ossm-catalog: redhat-operators
    unsupported.do-not-use.openshift.io/istio-version: {istio_version}
spec:
  controllerName: "openshift.io/gateway-controller/v1"
"""
    run("oc apply -f -", input_str=gw_class_yaml)

    # Wait for the servicemeshoperator3 CSV to report Succeeded
    run(
        f"timeout {args.timeout}s bash -c '"
        "until oc get csv -n openshift-operators 2>/dev/null "
        "| grep -i servicemeshoperator3 | grep -q Succeeded; "
        "do echo \"Waiting for operator CSV...\"; sleep 5; done'"
    )


# ---------------------------------------------------------------------------
# Operator Deployment
# ---------------------------------------------------------------------------


def deploy_coraza_operator(args):
    """Build, push, and deploy the Coraza operator via Helm."""
    print("--- Deploying Coraza Operator ---")

    project_root = Path(__file__).parent.parent.absolute()
    os.chdir(project_root)

    runtime = detect_container_runtime()
    run(f"make build.image CONTAINER_TOOL={runtime}")

    source_repo = os.environ.get(
        "CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE",
        "ghcr.io/networking-incubator/coraza-kubernetes-operator",
    )
    source_tag = os.environ.get(
        "CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG", "v0.0.0-dev",
    )
    source_image = f"{source_repo}:{source_tag}"

    # Resolve the external registry route for push
    res = run(
        "oc get route default-route -n openshift-image-registry "
        "--template='{{ .spec.host }}'",
        capture_output=True,
    )
    registry_host = res.stdout.strip()
    push_image = (
        f"{registry_host}/{args.coraza_ns}"
        f"/coraza-operator:{source_tag}"
    )
    pull_repo = (
        f"image-registry.openshift-image-registry.svc:5000"
        f"/{args.coraza_ns}/coraza-operator"
    )

    print(f"Logging in to OpenShift registry at {registry_host}...")
    run(f"{runtime} login -u kubeadmin -p $(oc whoami -t) {registry_host}")

    print(f"Tagging and pushing image: {push_image}...")
    run(f"{runtime} tag {source_image} {push_image}")
    run(f"{runtime} push {push_image}")

    print(f"Deploying operator via Helm (pull: {pull_repo}:{source_tag})...")
    run(
        f"helm upgrade --install {HELM_RELEASE_NAME} {HELM_CHART_DIR} "
        f"--namespace {args.coraza_ns} "
        f"--create-namespace "
        f"--set image.repository={pull_repo} "
        f"--set image.tag={source_tag} "
        f"--set openshift.enabled=true "
        f"--set istio.revision=openshift-gateway "
        f"--set createNamespace=false"
    )

    # seccompProfile: RuntimeDefault may conflict with some OCP SCC configs
    print("Patching deployment to remove seccompProfile (SCC compat)...")
    patch = (
        "'[{\"op\": \"remove\", "
        "\"path\": \"/spec/template/spec/securityContext/seccompProfile\"}]'"
    )
    run(
        f"oc patch deployment {HELM_RELEASE_NAME} "
        f"-n {args.coraza_ns} --type=json -p={patch}",
        check=False,
    )

    print(f"Waiting for operator (timeout: {args.timeout}s)...")
    run(
        f"oc wait --for=condition=Available "
        f"deployment/{HELM_RELEASE_NAME} "
        f"-n {args.coraza_ns} --timeout={args.timeout}s"
    )


# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------


def cleanup(args):
    """Remove all Coraza, Istio, and OSSM resources from the cluster."""
    print("\n--- Initiating Cleanup ---")

    print("Cleaning up Coraza WAF instances...")
    run("oc delete engines.waf.k8s.coraza.io --all -A", check=False)
    run("oc delete rulesets.waf.k8s.coraza.io --all -A", check=False)

    print("Cleaning up Istio control planes...")
    run(f"oc delete istio --all -n {args.coraza_ns}", check=False)

    print("Removing Coraza Operator...")
    run(
        f"helm uninstall {HELM_RELEASE_NAME} "
        f"--namespace {args.coraza_ns}",
        check=False,
    )

    print("Removing GatewayClasses...")
    run("oc delete gatewayclass openshift-default", check=False)

    print("Removing OpenShift Service Mesh Operator (OSSM)...")
    run(
        "oc delete subscription servicemeshoperator3 "
        "-n openshift-operators",
        check=False,
    )
    run(
        "oc get clusterserviceversion -n openshift-operators "
        "| grep servicemeshoperator3 | awk '{print $1}' "
        "| xargs -r oc delete clusterserviceversion -n openshift-operators",
        check=False,
    )

    print("Deleting namespaces...")
    namespaces = f"{args.coraza_ns} {args.test_ns} sail-operator"
    if args.deploy_metallb:
        namespaces += " metallb-system"
    run(f"oc delete ns {namespaces}", check=False)

    print("\nCleanup completed!")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    parser = argparse.ArgumentParser(
        description="Coraza OCP integration setup and teardown.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument(
        "action", choices=["setup", "cleanup"],
        help="'setup' to deploy, 'cleanup' to remove resources",
    )
    parser.add_argument(
        "--coraza-ns",
        default=os.getenv("CORAZA_NS", "coraza-system"),
        help="Namespace for the operator and Istio control plane",
    )
    parser.add_argument(
        "--test-ns",
        default=os.getenv("TEST_NS", "integration-tests"),
        help="Namespace for the test gateway and sample apps",
    )
    parser.add_argument(
        "--istio-version",
        default=os.getenv("ISTIO_VERSION", "v1.27.5"),
        help="Istio version (must be supported by the Sail catalog)",
    )
    parser.add_argument(
        "--ossm-version",
        default=os.getenv("OSSM_VERSION", "v3.2.2"),
        help="OSSM version string",
    )
    parser.add_argument(
        "--timeout", type=int,
        default=int(os.getenv("TIMEOUT", 300)),
        help="Seconds to wait for deployments/CSVs",
    )
    parser.add_argument(
        "--sail-repo-url",
        default=os.getenv(
            "SAIL_REPO_URL",
            "https://github.com/istio-ecosystem/sail-operator.git",
        ),
        help="Git URL for the Sail Operator repository",
    )
    parser.add_argument(
        "--deploy-metallb", action="store_true", default=False,
        help="Deploy MetalLB for LoadBalancer support",
    )
    parser.add_argument(
        "--working-dir",
        default=os.getenv("WORKING_DIR", Path.cwd()),
        help="Base directory for temporary files",
    )
    args = parser.parse_args()
    args.working_dir = Path(args.working_dir)

    if args.action == "setup":
        # Set GATEWAY_CLASS for OpenShift environment so integration tests
        # use the correct GatewayClass (openshift-default instead of istio)
        os.environ["GATEWAY_CLASS"] = "openshift-default"

        istio_version, ossm_version = get_versions(args)
        setup_internal_registry(args)
        deploy_gateway_class(args, istio_version, ossm_version)
        deploy_coraza_operator(args)
        print("\nCoraza Operator and Istio are ready on OCP!")
        print("\nTo run integration tests, use:")
        print("  GATEWAY_CLASS=openshift-default go test -tags=integration ./test/integration/...")

    elif args.action == "cleanup":
        cleanup(args)


if __name__ == "__main__":
    main()
