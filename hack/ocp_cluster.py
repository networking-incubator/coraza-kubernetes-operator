#!/usr/bin/env python3
import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path

helm_chart_dir: str = "charts/coraza-kubernetes-operator"
helm_release_name: str = "coraza-kubernetes-operator"

# Cached container runtime, detected once
_container_runtime: str = ""


def detect_container_runtime() -> str:
    """Detect if Docker is available, otherwise fall back to Podman."""
    global _container_runtime
    if _container_runtime:
        return _container_runtime

    print("Detecting container runtime (docker or podman)...")

    # Try docker first
    result = subprocess.run(
        "docker version --format json",
        shell=True, check=False, capture_output=True, text=True
    )
    if result.returncode == 0:
        try:
            version_info = json.loads(result.stdout)
            platform = version_info.get("Client", {}).get("Platform", {}).get("Name", "")
            if "Docker Engine" in platform:
                print("Detected: Docker")
                _container_runtime = "docker"
                return _container_runtime
        except (json.JSONDecodeError, KeyError, AttributeError):
            pass

    # Try podman
    result = subprocess.run(
        "podman version",
        shell=True, check=False, capture_output=True, text=True
    )
    if result.returncode == 0:
        print("Detected: Podman")
        _container_runtime = "podman"
        return _container_runtime

    print("ERROR: Neither docker nor podman is available", file=sys.stderr)
    print("Please install either docker or podman to continue", file=sys.stderr)
    sys.exit(1)


def run(cmd, input_str=None, capture_output=True, check=True):
    """Unified execution helper with logging"""
    print(f"+ {cmd}")
    try:
        result = subprocess.run(
            cmd, shell=True, check=check, text=True,
            input=input_str, capture_output=capture_output
        )
        if result.stdout and not capture_output:
            print(result.stdout.strip())
        return result
    except subprocess.CalledProcessError as e:
        print(f"ERROR: {e.stderr if e.stderr else e}")
        if check: sys.exit(e.returncode)
        return e

def get_version(args) -> str:
    istio_version = args.istio_version or os.environ.get("ISTIO_VERSION") or "v1.27.5"
    ossm_version = args.ossm_version or os.environ.get("OSSM_VERSION") or "v3.2.2"

    if not istio_version.startswith('v'):
        istio_version = f"v{istio_version}"
    if not ossm_version.startswith('v'):
        ossm_version = f"v{ossm_version}"
    return istio_version, ossm_version

def setup_internal_registry(args):
    print(f"--- Setting up OCP Internal Registry in {args.coraza_ns} ---")
    run("oc patch configs.imageregistry.operator.openshift.io/cluster --patch '{\"spec\":{\"defaultRoute\":true}}' --type=merge")
    url = ""
    start = time.time()
    while time.time() - start < args.timeout:
        res = run("oc get route default-route -n openshift-image-registry --template='{{ .spec.host }}'", check=False)
        if res.returncode == 0:
            url = res.stdout.strip()
            break
        time.sleep(5)

    run(f"oc create namespace {args.coraza_ns} --dry-run=client -o yaml | oc apply -f -")

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

def deploy_gateway_class(args, istio_version, ossm_version):    
    # Safeguard: Ensure the OSSM version has the required prefix for the annotation
    if not ossm_version.startswith("servicemeshoperator3."):
        ossm_version = f"servicemeshoperator3.{ossm_version}"

    print(f"--- Creating GatewayClass (Istio: {istio_version}, OSSM: {ossm_version}) ---")
    
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

    # Poll the OpenShift Operators namespace until the servicemeshoperator3 CSV reports 'Succeeded'
    check_csv_cmd = (
        f"timeout {args.timeout}s bash -c '"
        f"until oc get csv -n openshift-operators 2>/dev/null | grep -i servicemeshoperator3 | grep -q Succeeded; "
        f"do echo \"Waiting for operator CSV...\"; sleep 5; done'"
    )
    run(check_csv_cmd)

def create_istio_resources(args, version):
    print(f"--- Creating Istio Control Plane ({version}) ---")
    run(f"oc create namespace {args.coraza_ns} --dry-run=client -o yaml | oc apply -f -")
    istio_cr = f"""
apiVersion: sailoperator.io/v1
kind: Istio
metadata: {{namespace: {args.coraza_ns}, name: coraza}}
spec:
  namespace: {args.coraza_ns}
  version: {version}
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
    run("oc apply -f -", input_str=istio_cr)
    run(f"oc wait --for=condition=Ready istio/coraza -n {args.coraza_ns} --timeout={args.timeout}s")


def deploy_coraza_operator(args):
    print(f"--- Deploying Coraza Operator ---")

    project_root = Path(__file__).parent.parent.absolute()
    os.chdir(project_root)

    # Detect runtime before building so CONTAINER_TOOL matches
    runtime = detect_container_runtime()

    run(f"make build.image CONTAINER_TOOL={runtime}")

    # Get source image from environment variables (same as kind_cluster.py)
    source_image_repo = os.environ.get(
        "CONTROLLER_MANAGER_CONTAINER_IMAGE_BASE",
        "ghcr.io/networking-incubator/coraza-kubernetes-operator",
    )
    source_image_tag = os.environ.get("CONTROLLER_MANAGER_CONTAINER_IMAGE_TAG", "v0.0.0-dev")
    source_image = f"{source_image_repo}:{source_image_tag}"

    # --- REGISTRY ---
    res = run("oc get route default-route -n openshift-image-registry --template='{{ .spec.host }}'")
    registry_host_external = res.stdout.strip()
    push_image = f"{registry_host_external}/{args.coraza_ns}/coraza-operator:{source_image_tag}"
    pull_image_repo = f"image-registry.openshift-image-registry.svc:5000/{args.coraza_ns}/coraza-operator"
    pull_image_tag = source_image_tag

    print(f"Logging in to OpenShift registry at {registry_host_external}...")
    run(f"{runtime} login -u kubeadmin -p $(oc whoami -t) {registry_host_external}")

    print(f"Tagging and Pushing image to external route: {push_image}...")
    run(f"{runtime} tag {source_image} {push_image}")
    run(f"{runtime} push {push_image}")

    print(f"Deploying operator via Helm (pulling from internal registry: {pull_image_repo}:{pull_image_tag})...")
    run(
        f"helm upgrade --install {helm_release_name} {helm_chart_dir} "
        f"--namespace {args.coraza_ns} "
        f"--create-namespace "
        f"--set image.repository={pull_image_repo} "
        f"--set image.tag={pull_image_tag} "
        f"--set openshift.enabled=true "
        f"--set createNamespace=false"
    )

    # seccompProfile: RuntimeDefault may conflict with some OCP SCC configurations
    print("Patching deployment to remove seccompProfile (SCC compatibility)...")
    patch = "'[{\"op\": \"remove\", \"path\": \"/spec/template/spec/securityContext/seccompProfile\"}]'"
    run(f"oc patch deployment {helm_release_name} -n {args.coraza_ns} --type=json -p={patch}", check=False)

    print(f"Waiting for Coraza Operator to become Available (Timeout: {args.timeout}s)...")
    run(f"oc wait --for=condition=Available deployment/{helm_release_name} -n {args.coraza_ns} --timeout={args.timeout}s")


def create_gateway(args, use_lb):
    print(f"--- Creating Gateway in {args.test_ns} ---")
    run(f"oc create namespace {args.test_ns} --dry-run=client -o yaml | oc apply -f -")
    project_root = Path(__file__).parent.parent.absolute()
    gw_path = project_root / "config" / "samples" / "gateway.yaml"
    
    if use_lb:
        run(f"oc apply -f {gw_path} -n {args.test_ns}")
    else:
        run(f"oc annotate -f {gw_path} networking.istio.io/service-type=ClusterIP --local -o yaml | oc apply -f - -n {args.test_ns}")
    
    run(f"oc wait --for=condition=Programmed gateway/coraza-gateway -n {args.test_ns} --timeout={args.timeout}s")

def main():
    parser = argparse.ArgumentParser(
        description="Coraza OCP Integration Setup: Automates deployment of MetalLB, Sail Operator, and Istio on OpenShift.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
        epilog="Priority Logic: CLI arguments override Environment Variables, which override hardcoded defaults."
    )
    parser.add_argument("action", choices=["setup", "cleanup"], 
                        help="Action to perform: 'setup' for deploy. cleanup to remove created resources.")
    
    parser.add_argument("--coraza-ns", 
                        default=os.getenv("CORAZA_NS", "coraza-system"),
                        help="Primary namespace for Coraza Operator and Istio Control Plane. (Env: CORAZA_NS)")
    
    parser.add_argument("--test-ns", 
                        default=os.getenv("TEST_NS", "integration-tests"),
                        help="Namespace where the test gateway and sample apps are deployed. (Env: TEST_NS)")
    
    parser.add_argument("--istio-version", 
                        default=os.getenv("ISTIO_VERSION", "v1.27.5"),
                        help="Istio version string. Must be supported by the Sail Operator catalog. (Env: ISTIO_VERSION)")

    parser.add_argument("--ossm-version", 
                        default=os.getenv("OSSM_VERSION", "v3.2.2"),
                        help="OSSM version string. (Env: OSSM_VERSION)")
    
    parser.add_argument("--timeout", 
                        type=int, 
                        default=int(os.getenv("TIMEOUT", 300)),
                        help="Seconds to wait for deployments and CSVs to become ready. (Env: TIMEOUT)")
    
    parser.add_argument("--sail-repo-url", 
                        default=os.getenv("SAIL_REPO_URL", "https://github.com/istio-ecosystem/sail-operator.git"),
                        help="Git URL for the Sail Operator repository. (Env: SAIL_REPO_URL)")

    parser.add_argument("--deploy-metallb", action="store_true", 
                        default=False,
                        help="Whether to deploy and configure MetalLB for LoadBalancer support.")
    
    parser.add_argument("--working-dir", 
                        default=os.getenv("WORKING_DIR", Path.cwd()),
                        help="Base directory for temporary clones and file path resolution. (Env: WORKING_DIR)")

    args = parser.parse_args()
    args.working_dir = Path(args.working_dir)

    if args.action in ["setup"]:
        istio_version, ossm_version = get_version(args)
        setup_internal_registry(args)
        deploy_gateway_class(args,istio_version, ossm_version)
        create_istio_resources(args, istio_version)
        deploy_coraza_operator(args)
        create_gateway(args, use_lb=args.deploy_metallb)
        print("\n=======================================================")
        print("✅ SUCCESS! Coraza Operator and Istio are ready on OCP!")
        print("=======================================================")

    if args.action == "cleanup":
        print("\n=======================================================")
        print("--- Initiating Cleanup ---")
        print("=======================================================")

        print("Cleaning up Coraza WAF instances (clearing finalizers)...")
        run("oc delete engines.waf.k8s.coraza.io --all -A", check=False)
        run("oc delete rulesets.waf.k8s.coraza.io --all -A", check=False)

        print("Cleaning up Istio control planes...")
        run(f"oc delete istio --all -n {args.coraza_ns}", check=False)

        print("Removing Coraza Operator...")
        run(f"helm uninstall {helm_release_name} --namespace {args.coraza_ns}", check=False)

        print("Removing GatewayClasses...")
        run("oc delete gatewayclass openshift-default", check=False)

        print("Removing OpenShift Service Mesh Operator (OSSM)...")
        # Delete the subscription so OLM stops managing it
        run("oc delete subscription servicemeshoperator3 -n openshift-operators", check=False)

        csv_delete_cmd = "oc get clusterserviceversion -n openshift-operators | grep servicemeshoperator3 | awk '{print $1}' | xargs -r oc delete clusterserviceversion -n openshift-operators"
        run(csv_delete_cmd, check=False)
        
        print("Deleting namespaces...")
        namespaces = f"{args.coraza_ns} {args.test_ns} sail-operator"
        if args.deploy_metallb:
            namespaces += " metallb-system"

        run(f"oc delete ns {namespaces}", check=False)
        
        print("\n✅ Cleanup completed!")

if __name__ == "__main__":
    main()