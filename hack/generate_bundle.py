#!/usr/bin/env python3
"""
Generate an OLM operator bundle from the Helm chart.

Runs helm template, extracts the Deployment spec, ClusterRole rules,
and ServiceAccount name, then injects them into the CSV template.
Populates metadata.annotations.alm-examples from owned CRs in config/samples.
Additional manifests (Service, CRDs) are copied into bundle/manifests/.
"""

import argparse
import copy
import json
import os
import shutil
import sys
from pathlib import Path

import yaml

from lib import SemverYAML, die, load_yaml_docs, run, write_yaml


class LiteralBlockStr(str):
    """Multiline string that PyYAML emits as a literal block (|) for readability."""


def _represent_literal_block_str(dumper, data):
    return dumper.represent_scalar("tag:yaml.org,2002:str", str(data), style="|")


yaml.add_representer(LiteralBlockStr, _represent_literal_block_str)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

# Resource kinds to include as extra bundle manifests (beyond CRDs and the CSV).
BUNDLE_RESOURCE_KINDS = {"Service"}

# Kinds rendered by Helm that are NOT included in the bundle.
EXCLUDED_KINDS = {"Namespace", "PodDisruptionBudget", "ServiceMonitor",
                  "ServiceAccount", "ClusterRole", "ClusterRoleBinding"}

# Default when --min-kube-version is omitted. Keep in sync with KUBE_VERSION in the Makefile.
DEFAULT_MIN_KUBE_VERSION = "1.32.0"

# CRD apiVersion prefix for OLM alm-examples (owned APIs only).
OWNED_API_PREFIX = "waf.k8s.coraza.io/"

# Display order for alm-examples (RuleSet before Engine: engine references the ruleset).
_ALM_EXAMPLE_KIND_ORDER = {"RuleSet": 0, "Engine": 1}

# ---------------------------------------------------------------------------
# Helm Rendering
# ---------------------------------------------------------------------------


def helm_template(chart_dir: str, release_name: str, namespace: str,
                  version: str, kube_version: str) -> list:
    """Render the Helm chart and return parsed YAML documents."""
    cmd = (
        f"helm template {release_name} {chart_dir} "
        f"--namespace {namespace} "
        f"--kube-version {kube_version} "
        f"--version {version} "
        f"--set openshift.enabled=true "
        f"--set istio.revision=openshift-gateway"
    )
    result = run(cmd, capture_output=True)
    docs = list(yaml.safe_load_all(result.stdout))
    return [d for d in docs if d is not None]


# ---------------------------------------------------------------------------
# Document Helpers
# ---------------------------------------------------------------------------


def find_by_kind(docs: list, kind: str) -> dict:
    """Return the first document matching the given kind, or None."""
    for doc in docs:
        if doc.get("kind") == kind:
            return doc
    return None


def find_all_by_kind(docs: list, kind: str) -> list:
    """Return all documents matching the given kind."""
    return [d for d in docs if d.get("kind") == kind]


def strip_helm_labels(doc: dict) -> dict:
    """Remove Helm-specific labels and namespace from a manifest."""
    doc = copy.deepcopy(doc)
    labels = doc.get("metadata", {}).get("labels", {})
    for key in list(labels.keys()):
        if (key.startswith("helm.sh/")
                or key == "app.kubernetes.io/managed-by"
                or key == "app.kubernetes.io/version"):
            del labels[key]
    if "namespace" in doc.get("metadata", {}):
        del doc["metadata"]["namespace"]
    return doc


def load_owned_cr_examples(samples_dir: Path) -> list:
    """
    Load Engine / RuleSet documents from config/samples for CSV alm-examples.
    Only includes APIs owned by this operator (waf.k8s.coraza.io).
    """
    if not samples_dir.is_dir():
        die(f"Samples directory not found: {samples_dir}")

    out = []
    for path in sorted(samples_dir.glob("*.yaml")):
        for doc in load_yaml_docs(str(path)):
            av = doc.get("apiVersion")
            if isinstance(av, str) and av.startswith(OWNED_API_PREFIX):
                out.append(copy.deepcopy(doc))

    def sort_key(d):
        kind = d.get("kind", "")
        name = (d.get("metadata") or {}).get("name") or ""
        return (_ALM_EXAMPLE_KIND_ORDER.get(kind, 99), name)

    out.sort(key=sort_key)
    return out


# ---------------------------------------------------------------------------
# CSV Builder
# ---------------------------------------------------------------------------


def override_container_image(deployment: dict, image: str):
    """Overwrite all container images in a Deployment with the given ref."""
    for container in deployment["spec"]["template"]["spec"]["containers"]:
        container["image"] = image


def build_csv(template_path: str, deployment: dict, cluster_role: dict,
              sa_name: str, version: str, image: str,
              replaces: str, channels: str, default_channel: str,
              package_name: str, min_kube_version: str,
              alm_examples: list) -> dict:
    """Build the CSV by injecting Helm-rendered resources into the template."""
    with open(template_path) as f:
        csv = yaml.safe_load(f)

    csv["spec"]["minKubeVersion"] = min_kube_version

    csv["metadata"]["name"] = f"{package_name}.v{version}"
    csv["metadata"]["annotations"]["containerImage"] = image
    alm_json = json.dumps(alm_examples, indent=2, ensure_ascii=False) + "\n"
    csv["metadata"]["annotations"]["alm-examples"] = LiteralBlockStr(alm_json)
    csv["spec"]["version"] = SemverYAML(version)

    if replaces:
        csv["spec"]["replaces"] = replaces

    deploy_spec = copy.deepcopy(deployment["spec"])
    csv["spec"]["install"]["spec"]["deployments"] = [
        {"name": deployment["metadata"]["name"], "spec": deploy_spec},
    ]

    rules = copy.deepcopy(cluster_role["rules"])
    csv["spec"]["install"]["spec"]["clusterPermissions"] = [
        {"serviceAccountName": sa_name, "rules": rules},
    ]

    return csv


# ---------------------------------------------------------------------------
# Bundle Writer
# ---------------------------------------------------------------------------


def sanitize_manifest_name(doc: dict) -> str:
    """Generate a file name for a bundle manifest from its metadata."""
    kind = doc["kind"].lower()
    name = doc["metadata"]["name"]
    return f"{name}-{kind}.yaml"


def write_bundle(bundle_dir: str, csv: dict, extra_manifests: list,
                 crd_dir: str, version: str, channels: str,
                 default_channel: str, package_name: str):
    """Write all bundle artifacts (manifests, metadata, scorecard, Dockerfile)."""
    manifests_dir = os.path.join(bundle_dir, "manifests")
    metadata_dir = os.path.join(bundle_dir, "metadata")
    scorecard_dir = os.path.join(bundle_dir, "tests", "scorecard")

    for d in (manifests_dir, metadata_dir, scorecard_dir):
        if os.path.isdir(d):
            shutil.rmtree(d)
        os.makedirs(d)

    # CSV
    csv_path = os.path.join(manifests_dir, f"{package_name}.clusterserviceversion.yaml")
    write_yaml(csv_path, csv)
    print(f"  wrote {csv_path}", file=sys.stderr)

    # Extra manifests (e.g. Services)
    for doc in extra_manifests:
        path = os.path.join(manifests_dir, sanitize_manifest_name(doc))
        write_yaml(path, doc)
        print(f"  wrote {path}", file=sys.stderr)

    # CRDs
    for crd_file in sorted(Path(crd_dir).glob("*.yaml")):
        dest = os.path.join(manifests_dir, crd_file.name)
        shutil.copy2(str(crd_file), dest)
        print(f"  wrote {dest}", file=sys.stderr)

    # OLM annotations
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
    write_yaml(annotations_path, annotations)
    print(f"  wrote {annotations_path}", file=sys.stderr)

    # Scorecard config
    scorecard_config = {
        "apiVersion": "scorecard.operatorframework.io/v1alpha3",
        "kind": "Configuration",
        "metadata": {"name": "config"},
        "stages": [{
            "parallel": True,
            "tests": [
                {
                    "entrypoint": ["scorecard-test", "basic-check-spec"],
                    "image": "quay.io/operator-framework/scorecard-test:v1.42.0",
                    "labels": {"suite": "basic", "test": "basic-check-spec-test"},
                },
                {
                    "entrypoint": ["scorecard-test", "olm-bundle-validation"],
                    "image": "quay.io/operator-framework/scorecard-test:v1.42.0",
                    "labels": {"suite": "olm", "test": "olm-bundle-validation-test"},
                },
            ],
        }],
    }
    scorecard_path = os.path.join(scorecard_dir, "config.yaml")
    write_yaml(scorecard_path, scorecard_config)
    print(f"  wrote {scorecard_path}", file=sys.stderr)

    # Dockerfile
    dockerfile_path = os.path.join(bundle_dir, "bundle.Dockerfile")
    with open(dockerfile_path, "w") as f:
        f.write(f"""FROM scratch

LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1={package_name}
LABEL operators.operatorframework.io.bundle.channels.v1={channels}
LABEL operators.operatorframework.io.bundle.channel.default.v1={default_channel}

COPY manifests /manifests/
COPY metadata /metadata/
COPY tests/scorecard /tests/scorecard/
""")
    print(f"  wrote {dockerfile_path}", file=sys.stderr)

    # OperatorHub repo expects operators/<name>/ci.yaml; source lives under bundle/base/.
    ci_src = os.path.join(bundle_dir, "base", "ci.yaml")
    ci_dest = os.path.join(bundle_dir, "ci.yaml")
    if not os.path.isfile(ci_src):
        die(f"OperatorHub ci.yaml not found at {ci_src}")
    shutil.copy2(ci_src, ci_dest)
    print(f"  wrote {ci_dest}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


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
    parser.add_argument(
        "--min-kube-version",
        default=None,
        metavar="SEMVER",
        help=f"Kubernetes version for Helm --kube-version and CSV minKubeVersion (default: {DEFAULT_MIN_KUBE_VERSION})",
    )
    default_samples = Path(__file__).resolve().parent.parent / "config/samples"
    parser.add_argument(
        "--samples-dir",
        type=Path,
        default=default_samples,
        help=f"Directory with sample YAML for CSV alm-examples (default: {default_samples})",
    )
    args = parser.parse_args()

    version = args.version.lstrip("v")
    chart_dir = args.chart_dir
    crd_dir = os.path.join(chart_dir, "crds")
    template_path = os.path.join(args.bundle_dir, "base", "csv-template.yaml")

    if not os.path.isfile(template_path):
        die(f"CSV template not found at {template_path}")

    min_kube_version = args.min_kube_version or DEFAULT_MIN_KUBE_VERSION
    kube_version = min_kube_version

    # Render Helm chart
    print("Rendering Helm chart...", file=sys.stderr)
    docs = helm_template(chart_dir, args.release_name, args.namespace, version, kube_version)
    print(f"  got {len(docs)} documents", file=sys.stderr)

    deployment = find_by_kind(docs, "Deployment")
    cluster_role = find_by_kind(docs, "ClusterRole")
    service_account = find_by_kind(docs, "ServiceAccount")

    if not deployment:
        die("No Deployment found in Helm output")
    if not cluster_role:
        die("No ClusterRole found in Helm output")
    if not service_account:
        die("No ServiceAccount found in Helm output")

    override_container_image(deployment, args.image)
    sa_name = service_account["metadata"]["name"]

    extra_manifests = [strip_helm_labels(d) for d in docs if d.get("kind") in BUNDLE_RESOURCE_KINDS]

    alm_examples = load_owned_cr_examples(args.samples_dir)
    print(f"  alm-examples: {len(alm_examples)} owned CR(s) from {args.samples_dir}", file=sys.stderr)

    # Build and write bundle
    print("Building CSV...", file=sys.stderr)
    csv = build_csv(
        template_path=template_path,
        deployment=deployment,
        cluster_role=cluster_role,
        sa_name=sa_name,
        version=version,
        image=args.image,
        replaces=args.replaces,
        channels=args.channels,
        default_channel=args.default_channel,
        package_name=args.package_name,
        min_kube_version=min_kube_version,
        alm_examples=alm_examples,
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
