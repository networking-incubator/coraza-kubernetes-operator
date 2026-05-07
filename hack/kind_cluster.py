#!/usr/bin/env python3
# pylint: disable=missing-function-docstring,missing-module-docstring
# flake8: noqa: E501
#
# NOTE: generally you should run this from the Makefile ("make cluster.kind")

import argparse
import ipaddress
import json
import os
import sys

from lib import (
    HELM_CHART_DIR, HELM_RELEASE_NAME,
    detect_container_runtime, die, run,
)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

DEFAULT_NAMESPACE = "integration-tests"
GATEWAY_API_URL = (
    "https://github.com/kubernetes-sigs/gateway-api"
    "/releases/download/v1.4.1/standard-install.yaml"
)
SAIL_REPO = "https://istio-ecosystem.github.io/sail-operator"


# ---------------------------------------------------------------------------
# Environment Helpers
# ---------------------------------------------------------------------------


def require_env(key: str) -> str:
    """Read a required environment variable or exit."""
    value = os.environ.get(key)
    if not value:
        die(f"{key} environment variable is required")
        return ""  # unreachable
    return value


def get_kind_context(name: str) -> str:
    return f"kind-{name}"


# ---------------------------------------------------------------------------
# KIND Cluster Lifecycle
# ---------------------------------------------------------------------------


def create_cluster(name: str) -> None:
    """Create a KIND cluster (no-op if it already exists)."""
    print(f"Creating kind cluster: {name}")
    result = run(f"kind get clusters | grep -q '^{name}$'", check=False)
    if result.returncode == 0:
        print(f"Cluster {name} already exists, skipping creation")
    else:
        run(f"kind create cluster --name {name}")


def delete_cluster(name: str) -> None:
    print(f"Deleting kind cluster: {name}")
    run(f"kind delete cluster --name {name}", check=False)


# ---------------------------------------------------------------------------
# Image Build & Load
# ---------------------------------------------------------------------------


def build_images() -> None:
    print("Building container images")
    run("make build.image")


def load_images(name: str) -> None:
    print(f"Loading images into kind cluster: {name}")
    run("make cluster.load-images")


# ---------------------------------------------------------------------------
# Gateway API
# ---------------------------------------------------------------------------


def deploy_gateway_api_crds(context: str) -> None:
    print("Deploying Gateway API CRDs")
    run(f"kubectl --context {context} apply -f {GATEWAY_API_URL}")


# ---------------------------------------------------------------------------
# MetalLB
# ---------------------------------------------------------------------------


def get_kind_network_range() -> str:
    """Derive a MetalLB IP pool from the KIND docker network subnet."""
    result = run("docker network inspect kind", check=False, capture_output=True)
    if result.returncode != 0:
        die("Could not inspect the kind docker network")

    try:
        pool_size = int(os.environ.get("METALLB_POOL_SIZE", "128"))
        if pool_size > 255 or pool_size < 1:
            print(f"WARNING: Unusual METALLB_POOL_SIZE: {pool_size}", file=sys.stderr)

        kind_network = json.loads(result.stdout)
        ipam_config = kind_network[0].get("IPAM", {}).get("Config", [])
        if not ipam_config:
            raise ValueError("No IPAM configuration found for kind network")

        ipv4_config = next((c for c in ipam_config if ":" not in c.get("Subnet", "")), None)
        if not ipv4_config:
            raise ValueError("No IPv4 configuration found")

        cidr = ipv4_config.get("IPRange") or ipv4_config.get("Subnet")
        net = ipaddress.ip_network(cidr)
        last = net.broadcast_address - 1
        first = net.broadcast_address - pool_size
        return f"{first}-{last}"
    except (ValueError, KeyError, IndexError, StopIteration) as e:
        die(f"Invalid IP address range: {e}")
        return ""  # unreachable


def deploy_metallb(context: str) -> bool:
    """Deploy MetalLB. Returns True on success, False if METALLB_VERSION is unset."""
    metallb_version = os.environ.get("METALLB_VERSION")
    if not metallb_version:
        print("WARNING: METALLB_VERSION is not set, skipping MetalLB", file=sys.stderr)
        return False

    print("Deploying MetalLB")
    try:
        metallb_manifest_url = (
            f"https://raw.githubusercontent.com/metallb/metallb"
            f"/v{metallb_version}/config/manifests/metallb-native.yaml"
        )
        run(
            f"kubectl --context {context} apply --server-side "
            f"-f {metallb_manifest_url}",
            capture_output=True,
        )
        run(
            f"kubectl --context {context} wait --for=condition=Available "
            f"deployment/controller -n metallb-system --timeout=300s",
            capture_output=True,
        )
        # Webhook may not exist in all MetalLB versions
        run(
            f"kubectl --context {context} wait --for=condition=Ready "
            f"pod -l component=webhook-server -n metallb-system --timeout=300s",
            check=False, capture_output=True,
        )
        return True
    except Exception as e:
        die(f"Failed to deploy MetalLB: {e}")
        return False  # unreachable


def create_metallb_manifests(context: str, iprange: str) -> None:
    """Create the MetalLB IPAddressPool and L2Advertisement."""
    print("Creating MetalLB pool and L2Advertisement")
    manifests = f"""
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  namespace: metallb-system
  name: kube-services
spec:
  addresses:
  - {iprange}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: kube-services
  namespace: metallb-system
spec:
  ipAddressPools:
  - kube-services
"""
    run(f"echo '{manifests}' | kubectl --context {context} apply --server-side -f -")


# ---------------------------------------------------------------------------
# Istio (Sail Operator)
# ---------------------------------------------------------------------------


def deploy_istio_sail(context: str) -> None:
    """Install the Sail operator via Helm and wait for readiness."""
    istio_version = require_env("ISTIO_VERSION")

    print("Deploying Istio Sail Operator")
    run(f"helm repo add sail-operator {SAIL_REPO}")
    run("helm repo update")
    run(f"kubectl --context {context} create namespace sail-operator", check=False)

    result = run(
        f"helm list --namespace sail-operator --kube-context {context} "
        f"-o json | grep -q sail-operator",
        check=False,
    )
    if result.returncode != 0:
        run(
            f"helm install sail-operator sail-operator/sail-operator "
            f"--version {istio_version} "
            f"--namespace sail-operator --kube-context {context}"
        )
    else:
        print("Sail operator already installed, skipping")

    run(
        f"kubectl --context {context} wait --for=condition=Available "
        f"deployment/sail-operator -n sail-operator --timeout=300s"
    )


def create_istio_control_plane(context: str) -> None:
    """Create the Istio CR for the control plane and wait for readiness."""
    istio_version = require_env("ISTIO_VERSION")

    run(f"kubectl --context {context} create namespace coraza-system", check=False)

    print("Creating Istio control-plane")
    istio_cr = f"""
apiVersion: sailoperator.io/v1
kind: Istio
metadata:
  namespace: coraza-system
  name: coraza
spec:
  namespace: coraza-system
  version: v{istio_version}
  values:
    pilot:
      env:
        PILOT_GATEWAY_API_CONTROLLER_NAME: "istio.io/gateway-controller"
        PILOT_ENABLE_GATEWAY_API: "true"
        PILOT_ENABLE_GATEWAY_API_STATUS: "true"
        PILOT_ENABLE_ALPHA_GATEWAY_API: "false"
        PILOT_ENABLE_GATEWAY_API_DEPLOYMENT_CONTROLLER: "true"
        PILOT_ENABLE_GATEWAY_API_GATEWAYCLASS_CONTROLLER: "false"
        PILOT_GATEWAY_API_DEFAULT_GATEWAYCLASS_NAME: "istio"
        PILOT_MULTI_NETWORK_DISCOVER_GATEWAY_API: "false"
        ENABLE_GATEWAY_API_MANUAL_DEPLOYMENT: "false"
        PILOT_ENABLE_GATEWAY_API_CA_CERT_ONLY: "true"
        PILOT_ENABLE_GATEWAY_API_COPY_LABELS_ANNOTATIONS: "false"
"""
    run(f"echo '{istio_cr}' | kubectl --context {context} apply -f -")
    run(
        f"kubectl --context {context} --namespace coraza-system wait "
        "--for=condition=Ready istio/coraza --timeout=300s"
    )


# ---------------------------------------------------------------------------
# Gateway
# ---------------------------------------------------------------------------


def create_gateway_class(context: str) -> None:
    print("Creating GatewayClass for Istio")
    gateway_class = """
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: istio
spec:
  controllerName: istio.io/gateway-controller
"""
    run(f"echo '{gateway_class}' | kubectl --context {context} apply -f -")


def create_gateway(context: str, loadbalancer: bool) -> None:
    """Create the sample Gateway, optionally with ClusterIP annotation."""
    run(f"kubectl --context {context} create namespace {DEFAULT_NAMESPACE}", check=False)

    print("Creating Gateway for Istio")
    gw_manifest = "config/samples/gateway.yaml"
    ns_ctx = f"--context {context} -n {DEFAULT_NAMESPACE}"
    if loadbalancer:
        run(f"kubectl {ns_ctx} apply -f {gw_manifest}")
    else:
        run(
            f"kubectl annotate -f {gw_manifest} "
            f"networking.istio.io/service-type=ClusterIP "
            f"--local -o yaml "
            f"| kubectl {ns_ctx} apply -f -"
        )

    run(
        f"kubectl --context {context} -n {DEFAULT_NAMESPACE} wait "
        "--for=condition=Programmed gateway/coraza-gateway --timeout=300s"
    )


# ---------------------------------------------------------------------------
# Operator Deployment
# ---------------------------------------------------------------------------


def deploy_coraza_operator(context: str) -> None:
    """Deploy the Coraza operator via Helm and wait for readiness."""
    print("Deploying Coraza Operator")

    image_repo = os.environ.get(
        "CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE",
        "ghcr.io/networking-incubator/coraza-kubernetes-operator",
    )
    image_tag = os.environ.get("CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG", "v0.0.0-dev")

    run(
        f"helm upgrade --install {HELM_RELEASE_NAME} {HELM_CHART_DIR} "
        f"--namespace coraza-system "
        f"--create-namespace "
        f"--set image.repository={image_repo} "
        f"--set image.tag={image_tag} "
        f"--set createNamespace=false "
        f"--set istio.revision=coraza "
        f"--kube-context {context}"
    )

    run(
        f"kubectl --context {context} --namespace coraza-system wait "
        "--for=condition=Available "
        "deployment/coraza-kubernetes-operator --timeout=300s"
    )


# ---------------------------------------------------------------------------
# Full Cluster Setup
# ---------------------------------------------------------------------------


def setup_cluster(name: str) -> None:
    """End-to-end cluster setup: create, load images, install components."""
    docker_available = detect_container_runtime() == "docker"
    if os.environ.get("SKIP_IMAGE_BUILD", "").lower() != "true":
        build_images()
    create_cluster(name)
    load_images(name)

    context = get_kind_context(name)

    deploy_gateway_api_crds(context)

    metallb_enabled = False
    if docker_available:
        if deploy_metallb(context):
            create_metallb_manifests(context, get_kind_network_range())
            metallb_enabled = True

    deploy_istio_sail(context)
    create_istio_control_plane(context)
    create_gateway_class(context)
    create_gateway(context, metallb_enabled)
    deploy_coraza_operator(context)

    print("Cluster setup complete")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    parser = argparse.ArgumentParser(description="Manage KIND integration clusters")
    parser.add_argument("action", choices=["create", "delete", "setup"])
    parser.add_argument("--name", default="coraza-kubernetes-operator-integration")
    args = parser.parse_args()

    if args.action == "create":
        create_cluster(args.name)
    elif args.action == "setup":
        setup_cluster(args.name)
    elif args.action == "delete":
        delete_cluster(args.name)


if __name__ == "__main__":
    main()
