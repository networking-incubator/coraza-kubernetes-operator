#!/usr/bin/env python3
# pylint: disable=missing-function-docstring,missing-module-docstring
# flake8: noqa: E501
#
# NOTE: generally you should run this from the Makefile ("make cluster.kind")

import subprocess
import argparse
import os
import sys
import ipaddress
import json

default_namespace: str = "integration-tests"
gateway_api: str = (
    "https://github.com/kubernetes-sigs/gateway-api/"
    "releases/download/v1.4.1/standard-install.yaml"
)
sail_repo: str = "https://istio-ecosystem.github.io/sail-operator"
helm_chart_dir: str = "charts/coraza-kubernetes-operator"
helm_release_name: str = "coraza-kubernetes-operator"
helm_release_namespace: str = "coraza-system"


def get_istio_version() -> str:
    """Get ISTIO_VERSION from environment, required for cluster setup operations."""
    istio_version = os.environ.get("ISTIO_VERSION")
    if not istio_version:
        print("ERROR: ISTIO_VERSION environment variable is required", file=sys.stderr)
        print("Please set ISTIO_VERSION to the desired Istio version (e.g., 1.28.2)", file=sys.stderr)
        print("You can set a default in the Makefile or export it in your shell", file=sys.stderr)
        sys.exit(1)
    return istio_version


def get_kind_network_range() -> str:
    """Get the Network range used by kind network, to be used during LoadBalancer deployment"""
    result = run("docker network inspect kind", check=False, capture_output=True)
    if result.returncode != 0:
        print("ERROR: Could not get the kind network range", file=sys.stderr)
        sys.exit(1)
    try:
        metallb_pool_size = os.environ.get("METALLB_POOL_SIZE")
        if not metallb_pool_size:
            metallb_pool_size = "128"
        metallb_pool_size_int = int(metallb_pool_size)
        if metallb_pool_size_int > 255 or metallb_pool_size_int < 1:
            print(f"WARNING: Unusual METALLB_POOL_SIZE: {metallb_pool_size_int}", file=sys.stderr)
        # result.stdout is str because capture_output=True uses text=True
        kind_network = json.loads(result.stdout)
        ipam_config = kind_network[0].get("IPAM", {}).get("Config", [])
        if not ipam_config:
            raise ValueError(f"No IPAM configuration found for network: {kind_network}")
        ipv4_config = next((c for c in ipam_config if ":" not in c.get("Subnet", "")), None)
        if not ipv4_config:
            raise ValueError("No IPv4 configuration found.")
        cidr = ipv4_config.get("IPRange") or ipv4_config.get("Subnet")
        net = ipaddress.ip_network(cidr)
        broadcast = net.broadcast_address
        last_pool_ip = broadcast - 1 # eg.: 172.18.255.254
        first_pool_ip = broadcast - metallb_pool_size_int # eg.: 172.18.255.244
        return f"{first_pool_ip}-{last_pool_ip}"
    except Exception as e:
        print(f"ERROR: Invalid IP address range: {e}", file=sys.stderr)
        sys.exit(1)


def run(cmd: str, check: bool = True, capture_output: bool = False) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd,
        shell=True,
        check=check,
        capture_output=capture_output,
        text=capture_output  # Use text mode when capturing output for easier handling
    )


def get_kind_context(name: str) -> str:
    return f"kind-{name}"


def build_images() -> None:
    print("Building container images")
    run("make build.image")


def create_cluster(name: str) -> None:
    print(f"Creating kind cluster: {name}")
    result = run(f"kind get clusters | grep -q '^{name}$'", check=False)
    if result.returncode == 0:
        print(f"Cluster {name} already exists, skipping creation")
    else:
        run(f"kind create cluster --name {name}")


def load_images(name: str) -> None:
    print(f"Loading images into kind cluster: {name}")
    run("make cluster.load-images")


def deploy_gateway_api_crds(context: str) -> None:
    print("Deploying Gateway API CRDs")
    run(f"kubectl --context {context} apply -f {gateway_api}")

def deploy_metallb(context: str) -> bool:
    metallb_version = os.environ.get("METALLB_VERSION")
    if not metallb_version:
        print("WARNING: METALLB_VERSION is not set, skipping MetalLB deployment", file=sys.stderr)
        return False
    print("Deploying MetalLB")
    try:
        run(
            f"kubectl --context {context} apply --server-side "
            f"-f https://raw.githubusercontent.com/metallb/metallb/v{metallb_version}/config/manifests/metallb-native.yaml",
            capture_output=True
        )
        run(
            f"kubectl --context {context} wait --for=condition=Available "
            f"deployment/controller -n metallb-system --timeout=300s",
            capture_output=True
        )
        # Wait for webhook to be ready to avoid race condition with CRD creation
        run(
            f"kubectl --context {context} wait --for=condition=Ready "
            f"pod -l component=webhook-server -n metallb-system --timeout=300s",
            check=False,  # Webhook might not exist in all versions
            capture_output=True
        )
        return True

    except subprocess.CalledProcessError as e:
        print("ERROR: deploying MetalLB: kubectl command failed", file=sys.stderr)
        if e.stdout:
            print("kubectl stdout:", e.stdout, file=sys.stderr)
        if e.stderr:
            print("kubectl stderr:", e.stderr, file=sys.stderr)
        sys.exit(1)

def create_metallb_manifests(context: str, iprange: str) -> None:
    print("Creating MetalLB pool and L2Advertisement")
    metallb_manifests = f"""
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
    run(
        f"echo '{metallb_manifests}' | kubectl "
        f"--context {context} apply --server-side -f -"
    )

def deploy_istio_sail(context: str) -> None:
    istio_version = get_istio_version()

    print("Deploying Istio Sail Operator")
    run(f"helm repo add sail-operator {sail_repo}")
    run("helm repo update")
    run(f"kubectl --context {context} create namespace sail-operator", check=False)

    result = run(
        f"helm list --namespace sail-operator --kube-context {context} "
        f"-o json | grep -q sail-operator",
        check=False
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
    run(
        f"echo '{gateway_class}' | kubectl "
        f"--context {context} apply -f -"
    )


def create_gateway(context: str, loadbalancer: bool) -> None:
    run(
        f"kubectl --context {context} "
        f" create namespace {default_namespace}", check=False
    )

    print("Creating Gateway for Istio")
    if loadbalancer:
        run(f"kubectl --context {context} -n {default_namespace} apply -f config/samples/gateway.yaml")
    else:
        run(f"kubectl annotate -f config/samples/gateway.yaml networking.istio.io/service-type=ClusterIP --local -o yaml |kubectl --context {context} -n {default_namespace} apply -f -")

    run(
        f"kubectl --context {context} -n {default_namespace} wait "
        "--for=condition=Programmed gateway/coraza-gateway --timeout=300s"
    )


def create_istio_control_plane(context: str) -> None:
    istio_version = get_istio_version()

    run(
        f"kubectl --context {context} "
        " create namespace coraza-system", check=False
    )

    print("Creating Istio control-plane")
    istio = f"""
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
    run(
        f"echo '{istio}' | kubectl "
        f"--context {context} apply -f -"
    )
    run(
        f"kubectl --context {context} --namespace coraza-system wait "
        "--for=condition=Ready istio/coraza --timeout=300s"
    )


def deploy_coraza_operator(context: str) -> None:
    print("Deploying Coraza Operator")

    image_repo = os.environ.get(
        "CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE",
        "ghcr.io/networking-incubator/coraza-kubernetes-operator",
    )
    image_tag = os.environ.get("CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG", "v0.0.0-dev")

    run(
        f"helm upgrade --install {helm_release_name} {helm_chart_dir} "
        f"--namespace {helm_release_namespace} "
        f"--create-namespace "
        f"--set image.repository={image_repo} "
        f"--set image.tag={image_tag} "
        f"--set createNamespace=false "
        f"--kube-context {context}"
    )

    run(
        f"kubectl --context {context} --namespace {helm_release_namespace} wait "
        "--for=condition=Available "
        "deployment/coraza-kubernetes-operator --timeout=300s"
    )

def detect_docker() -> bool:
    """Detect if Docker is available (as opposed to Podman)."""
    print("detecting if Docker is available, otherwise assuming this is a Podman cluster")
    result = run("docker version -f json", check=False, capture_output=True)
    if result.returncode != 0:
        # Docker not available, check for podman
        print("Docker not found, checking for podman")
        podman_result = run("podman version", check=False, capture_output=True)
        if podman_result.returncode != 0:
            print("ERROR: Neither docker nor podman is available", file=sys.stderr)
            print("Please install either docker or podman to continue", file=sys.stderr)
            sys.exit(1)
        # Podman is available
        return False
    try:
        # result.stdout is str because capture_output=True uses text=True
        client_decoded = json.loads(result.stdout)
        platform = client_decoded.get("Client", {}).get("Platform", {}).get("Name", '')
        if "Docker Engine" in platform:
            return True
        return False
    except (json.JSONDecodeError, KeyError, AttributeError):
        return False


def delete_cluster(name: str) -> None:
    print(f"Deleting kind cluster: {name}")
    run(f"kind delete cluster --name {name}", check=False)


def setup_cluster(name: str) -> None:
    docker_available = detect_docker()
    build_images()
    create_cluster(name)
    load_images(name)

    context = get_kind_context(name)

    deploy_gateway_api_crds(context)
    metallb_enabled = False
    if docker_available:
        if deploy_metallb(context):
            metallb_ip_range = get_kind_network_range()
            create_metallb_manifests(context, metallb_ip_range)
            metallb_enabled = True
    deploy_istio_sail(context)
    create_istio_control_plane(context)
    create_gateway_class(context)
    create_gateway(context, metallb_enabled)
    deploy_coraza_operator(context)

    print("Cluster setup complete")


def main() -> None:
    parser = argparse.ArgumentParser()
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
