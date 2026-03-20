#!/usr/bin/env python3
"""
Generate an OLM operator bundle from the Helm chart.

Runs helm template, extracts the Deployment spec, ClusterRole rules,
and ServiceAccount name, then injects them into the CSV template.
Additional manifests (Service, CRDs) are copied into bundle/manifests/.
"""

import argparse
import copy
import os
import shutil
import subprocess
import sys
from pathlib import Path

import yaml


BUNDLE_RESOURCE_KINDS = {"Service"}

EXCLUDED_KINDS = {"Namespace", "PodDisruptionBudget", "ServiceMonitor",
                  "ServiceAccount", "ClusterRole", "ClusterRoleBinding"}


def helm_template(chart_dir: str, release_name: str, namespace: str,
                  version: str, kube_version: str) -> list:
    """Render the Helm chart and return parsed YAML documents."""
    cmd = [
        "helm", "template",
        release_name,
        chart_dir,
        "--namespace", namespace,
        "--kube-version", kube_version,
        "--version", version,
        "--set", "openshift.enabled=true",
    ]
    result = subprocess.run(cmd, capture_output=True, text=True, check=True)
    docs = list(yaml.safe_load_all(result.stdout))
    return [d for d in docs if d is not None]


def override_container_image(deployment: dict, image: str):
    """Overwrite container images in a Deployment spec with the given image ref."""
    for container in deployment["spec"]["template"]["spec"]["containers"]:
        container["image"] = image


def find_by_kind(docs: list, kind: str) -> dict:
    """Return the first document matching the given kind."""
    for doc in docs:
        if doc.get("kind") == kind:
            return doc
    return None


def find_all_by_kind(docs: list, kind: str) -> list:
    """Return all documents matching the given kind."""
    return [d for d in docs if d.get("kind") == kind]


def build_csv(template_path: str, deployment: dict, cluster_role: dict,
              sa_name: str, version: str, image: str,
              replaces: str, channels: str, default_channel: str,
              package_name: str) -> dict:
    """Build the CSV by injecting Helm-rendered resources into the template."""
    with open(template_path) as f:
        csv = yaml.safe_load(f)

    csv["metadata"]["name"] = f"{package_name}.v{version}"
    csv["metadata"]["annotations"]["containerImage"] = image
    csv["spec"]["version"] = version

    if replaces:
        csv["spec"]["replaces"] = replaces

    deploy_spec = copy.deepcopy(deployment["spec"])
    csv["spec"]["install"]["spec"]["deployments"] = [
        {
            "name": deployment["metadata"]["name"],
            "spec": deploy_spec,
        }
    ]

    rules = copy.deepcopy(cluster_role["rules"])
    csv["spec"]["install"]["spec"]["clusterPermissions"] = [
        {
            "serviceAccountName": sa_name,
            "rules": rules,
        }
    ]

    return csv


def sanitize_manifest_name(doc: dict) -> str:
    """Generate a file name for a bundle manifest from its metadata."""
    kind = doc["kind"].lower()
    name = doc["metadata"]["name"]
    return f"{name}-{kind}.yaml"


def write_bundle(bundle_dir: str, csv: dict, extra_manifests: list,
                 crd_dir: str, version: str, channels: str,
                 default_channel: str, package_name: str):
    """Write all bundle artifacts to disk."""
    manifests_dir = os.path.join(bundle_dir, "manifests")
    metadata_dir = os.path.join(bundle_dir, "metadata")
    scorecard_dir = os.path.join(bundle_dir, "tests", "scorecard")

    for d in (manifests_dir, metadata_dir, scorecard_dir):
        if os.path.isdir(d):
            shutil.rmtree(d)
        os.makedirs(d)

    csv_path = os.path.join(manifests_dir, f"{package_name}.clusterserviceversion.yaml")
    with open(csv_path, "w") as f:
        yaml.dump(csv, f, default_flow_style=False, sort_keys=False, width=1000)
    print(f"  wrote {csv_path}", file=sys.stderr)

    for doc in extra_manifests:
        filename = sanitize_manifest_name(doc)
        path = os.path.join(manifests_dir, filename)
        with open(path, "w") as f:
            yaml.dump(doc, f, default_flow_style=False, sort_keys=False, width=1000)
        print(f"  wrote {path}", file=sys.stderr)

    for crd_file in sorted(Path(crd_dir).glob("*.yaml")):
        dest = os.path.join(manifests_dir, crd_file.name)
        shutil.copy2(str(crd_file), dest)
        print(f"  wrote {dest}", file=sys.stderr)

    annotations = {
        "annotations": {
            "operators.operatorframework.io.bundle.mediatype.v1": "registry+v1",
            "operators.operatorframework.io.bundle.manifests.v1": "manifests/",
            "operators.operatorframework.io.bundle.metadata.v1": "metadata/",
            "operators.operatorframework.io.bundle.package.v1": package_name,
            "operators.operatorframework.io.bundle.channels.v1": channels,
            "operators.operatorframework.io.bundle.channel.default.v1": default_channel,
        }
    }
    annotations_path = os.path.join(metadata_dir, "annotations.yaml")
    with open(annotations_path, "w") as f:
        yaml.dump(annotations, f, default_flow_style=False, sort_keys=False)
    print(f"  wrote {annotations_path}", file=sys.stderr)

    scorecard_config = {
        "apiVersion": "scorecard.operatorframework.io/v1alpha3",
        "kind": "Configuration",
        "metadata": {"name": "config"},
        "stages": [
            {
                "parallel": True,
                "tests": [
                    {
                        "entrypoint": [
                            "scorecard-test", "basic-check-spec",
                        ],
                        "image": "quay.io/operator-framework/scorecard-test:v1.42.0",
                        "labels": {
                            "suite": "basic",
                            "test": "basic-check-spec-test",
                        },
                    },
                    {
                        "entrypoint": [
                            "scorecard-test", "olm-bundle-validation",
                        ],
                        "image": "quay.io/operator-framework/scorecard-test:v1.42.0",
                        "labels": {
                            "suite": "olm",
                            "test": "olm-bundle-validation-test",
                        },
                    },
                ],
            },
        ],
    }
    scorecard_path = os.path.join(scorecard_dir, "config.yaml")
    with open(scorecard_path, "w") as f:
        yaml.dump(scorecard_config, f, default_flow_style=False, sort_keys=False)
    print(f"  wrote {scorecard_path}", file=sys.stderr)

    dockerfile_path = os.path.join(bundle_dir, "bundle.Dockerfile")
    dockerfile_content = f"""FROM scratch

LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1={package_name}
LABEL operators.operatorframework.io.bundle.channels.v1={channels}
LABEL operators.operatorframework.io.bundle.channel.default.v1={default_channel}

COPY manifests /manifests/
COPY metadata /metadata/
COPY tests/scorecard /tests/scorecard/
"""
    with open(dockerfile_path, "w") as f:
        f.write(dockerfile_content)
    print(f"  wrote {dockerfile_path}", file=sys.stderr)


def strip_helm_labels(doc: dict) -> dict:
    """Remove Helm-specific labels from a manifest for bundle use."""
    doc = copy.deepcopy(doc)
    labels = doc.get("metadata", {}).get("labels", {})
    for key in list(labels.keys()):
        if (
            key.startswith("helm.sh/")
            or key == "app.kubernetes.io/managed-by"
            or key == "app.kubernetes.io/version"
        ):
            del labels[key]
    if "namespace" in doc.get("metadata", {}):
        del doc["metadata"]["namespace"]
    return doc


def main():
    parser = argparse.ArgumentParser(description="Generate OLM bundle from Helm chart")
    parser.add_argument("--chart-dir", required=True, help="Path to the Helm chart directory")
    parser.add_argument("--bundle-dir", required=True, help="Path to the output bundle directory")
    parser.add_argument("--version", required=True, help="Operator version (semver, optional 'v' prefix is stripped)")
    parser.add_argument("--image", required=True, help="Operator container image ref (repo:tag or repo@sha256:digest)")
    parser.add_argument("--channels", default="alpha", help="Comma-separated OLM channel list")
    parser.add_argument("--default-channel", default="alpha", help="Default OLM channel")
    parser.add_argument("--replaces", default="", help="CSV version this replaces (e.g. coraza-kubernetes-operator.v0.2.0)")
    parser.add_argument("--package-name", default="coraza-kubernetes-operator", help="OLM package name")
    parser.add_argument("--release-name", default="coraza-kubernetes-operator", help="Helm release name for rendering")
    parser.add_argument("--namespace", default="coraza-system", help="Namespace for Helm rendering")
    args = parser.parse_args()

    version = args.version.lstrip("v")
    image = args.image

    chart_dir = args.chart_dir
    crd_dir = os.path.join(chart_dir, "crds")
    template_path = os.path.join(args.bundle_dir, "base", "csv-template.yaml")

    if not os.path.isfile(template_path):
        print(f"ERROR: CSV template not found at {template_path}", file=sys.stderr)
        sys.exit(1)

    with open(template_path) as f:
        csv_template = yaml.safe_load(f)
    kube_version = csv_template.get("spec", {}).get("minKubeVersion", "1.33.0")

    print("Rendering Helm chart...", file=sys.stderr)
    docs = helm_template(chart_dir, args.release_name, args.namespace, version,
                         kube_version)
    print(f"  got {len(docs)} documents", file=sys.stderr)

    deployment = find_by_kind(docs, "Deployment")
    cluster_role = find_by_kind(docs, "ClusterRole")
    service_account = find_by_kind(docs, "ServiceAccount")

    if not deployment:
        print("ERROR: No Deployment found in Helm output", file=sys.stderr)
        sys.exit(1)
    if not cluster_role:
        print("ERROR: No ClusterRole found in Helm output", file=sys.stderr)
        sys.exit(1)
    if not service_account:
        print("ERROR: No ServiceAccount found in Helm output", file=sys.stderr)
        sys.exit(1)

    override_container_image(deployment, image)
    sa_name = service_account["metadata"]["name"]

    extra_manifests = []
    for doc in docs:
        kind = doc.get("kind", "")
        if kind in BUNDLE_RESOURCE_KINDS:
            extra_manifests.append(strip_helm_labels(doc))

    print("Building CSV...", file=sys.stderr)
    csv = build_csv(
        template_path=template_path,
        deployment=deployment,
        cluster_role=cluster_role,
        sa_name=sa_name,
        version=version,
        image=image,
        replaces=args.replaces,
        channels=args.channels,
        default_channel=args.default_channel,
        package_name=args.package_name,
    )

    csv = strip_helm_labels(csv)

    print("Writing bundle...", file=sys.stderr)
    write_bundle(
        bundle_dir=args.bundle_dir,
        csv=csv,
        extra_manifests=extra_manifests,
        crd_dir=crd_dir,
        version=version,
        channels=args.channels,
        default_channel=args.default_channel,
        package_name=args.package_name,
    )

    print("\nBundle generation complete.", file=sys.stderr)


if __name__ == "__main__":
    main()
