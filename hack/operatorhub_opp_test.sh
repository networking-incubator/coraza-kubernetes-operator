#!/usr/bin/env bash
# OPP (operator-test-playbooks) kiwi test against a staged community-operators tree.
# Pinned pipeline: community-operators-pipeline@fbac22ff0c713188bdcea36791b70fb3999e6e03
# Based on https://github.com/maistra/istio-workspace/blob/master/scripts/release/operatorhub/test.sh
# and https://github.com/redhat-openshift-ecosystem/community-operators-pipeline/blob/master/ci/scripts/opp.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

OPP_PINNED_COMMIT="fbac22ff0c713188bdcea36791b70fb3999e6e03"
OPP_SCRIPT_URL="https://raw.githubusercontent.com/redhat-openshift-ecosystem/community-operators-pipeline/${OPP_PINNED_COMMIT}/ci/scripts/opp.sh"
OPP_SCRIPT_SHA256="0536753bf819207c7d41c262b9b02fa62aa86adfe3298393e94b7b8b4bc30808"

OPERATOR_NAME="coraza-kubernetes-operator"
OWNER="${OWNER:-k8s-operatorhub}"
OPERATOR_HUB="${OPERATOR_HUB:-community-operators}"
HUB_BASE_BRANCH="${HUB_BASE_BRANCH:-main}"

TESTS="${TESTS:-kiwi}"

show_help() {
    cat <<EOF
operatorhub_opp_test.sh - run Operator Hub OPP tests (kiwi tier by default)

Requires: docker, git, curl, ansible and jmespath (pip install ansible jmespath)
Run from repo root after: make bundle

Usage:
  OPERATOR_VERSION=X.Y.Z ./hack/operatorhub_opp_test.sh [options]

Options:
  --version VERSION   Bare semver (no v), e.g. 1.0.0
  --test TIER         OPP tier: kiwi (default), orange, lemon, or all
  -h, --help          This help

Env:
  OPP_LOCAL_GITHUB_OUTPUT   When GITHUB_OUTPUT is unset, write here instead of a temp file (file is truncated).
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) show_help; exit 0 ;;
        --version) OPERATOR_VERSION="${2:-}"; shift 2 ;;
        --test)    TESTS="${2:-kiwi}"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; show_help; exit 1 ;;
    esac
done

OPERATOR_VERSION="${OPERATOR_VERSION:-}"
OPERATOR_VERSION="${OPERATOR_VERSION#v}"

if [[ -z "${OPERATOR_VERSION}" ]]; then
    echo "Error: set OPERATOR_VERSION or pass --version X.Y.Z" >&2
    exit 1
fi

if [[ ! "${OPERATOR_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: version '${OPERATOR_VERSION}' must be X.Y.Z (same as publish_operatorhub.sh)" >&2
    exit 1
fi

BUNDLE_DIR="${REPO_ROOT}/bundle"
BUNDLE_MANIFESTS="${BUNDLE_DIR}/manifests"
BUNDLE_METADATA="${BUNDLE_DIR}/metadata"

if [[ ! -d "${BUNDLE_MANIFESTS}" || ! -d "${BUNDLE_METADATA}" ]]; then
    echo "Error: bundle not found at ${BUNDLE_DIR}. Run 'make bundle' first." >&2
    exit 1
fi

HUB_REPO_URL="https://github.com/${OWNER}/${OPERATOR_HUB}.git"
BRANCH="${OPERATOR_NAME}-${OPERATOR_VERSION}"

TMP_DIR="$(mktemp -d -t "${OPERATOR_NAME}.opp.XXXXXXXXXX")"
trap 'rm -rf -- "${TMP_DIR}"' EXIT

echo "Cloning ${OWNER}/${OPERATOR_HUB}..."
git clone --depth 1 --branch "${HUB_BASE_BRANCH}" "${HUB_REPO_URL}" "${TMP_DIR}"

cd "${TMP_DIR}"
git checkout -b "${BRANCH}"

OPERATORS_DIR="operators/${OPERATOR_NAME}/${OPERATOR_VERSION}"
mkdir -p "${OPERATORS_DIR}/manifests"
mkdir -p "${OPERATORS_DIR}/metadata"
cp -a "${BUNDLE_MANIFESTS}/." "${OPERATORS_DIR}/manifests/"
cp -a "${BUNDLE_METADATA}/." "${OPERATORS_DIR}/metadata/"

CI_YAML_DEST="operators/${OPERATOR_NAME}/ci.yaml"
# Match publish_operatorhub.sh: bundle ci.yaml is the source of truth.
cp "${BUNDLE_DIR}/ci.yaml" "${CI_YAML_DEST}"

# Align with community OperatorHub k8s pipeline (see maistra istio-workspace test.sh).
export OPP_CONTAINER_OPT="${OPP_CONTAINER_OPT:--t}"
export OPP_IMAGE="${OPP_IMAGE:-quay.io/operator_testing/operator-test-playbooks:latest}"
export OPP_ANSIBLE_PULL_REPO="${OPP_ANSIBLE_PULL_REPO:-https://github.com/redhat-openshift-ecosystem/operator-test-playbooks/}"
export OPP_ANSIBLE_PULL_BRANCH="${OPP_ANSIBLE_PULL_BRANCH:-upstream-community}"
export OPP_THIS_REPO_BASE="${OPP_THIS_REPO_BASE:-https://github.com}"
export OPP_THIS_REPO="${OPP_THIS_REPO:-${OWNER}/${OPERATOR_HUB}}"
export OPP_THIS_BRANCH="${OPP_THIS_BRANCH:-${HUB_BASE_BRANCH}}"
export OPP_RELEASE_BUNDLE_REGISTRY="${OPP_RELEASE_BUNDLE_REGISTRY:-quay.io}"
export OPP_RELEASE_BUNDLE_ORGANIZATION="${OPP_RELEASE_BUNDLE_ORGANIZATION:-operatorhubio}"
export OPP_RELEASE_INDEX_REGISTRY="${OPP_RELEASE_INDEX_REGISTRY:-quay.io}"
export OPP_RELEASE_INDEX_ORGANIZATION="${OPP_RELEASE_INDEX_ORGANIZATION:-operatorhubio}"
export OPP_RELEASE_INDEX_NAME="${OPP_RELEASE_INDEX_NAME:-catalog}"
export OPP_MIRROR_INDEX_MULTIARCH_BASE="${OPP_MIRROR_INDEX_MULTIARCH_BASE:-quay.io/operator-framework/opm}"
export OPP_MIRROR_INDEX_MULTIARCH_POSTFIX="${OPP_MIRROR_INDEX_MULTIARCH_POSTFIX:-s}"
export OPP_PRODUCTION_TYPE="${OPP_PRODUCTION_TYPE:-k8s}"

OPP_SCRIPT_FILE="${TMP_DIR}/opp-pinned.sh"
curl -fsSL "${OPP_SCRIPT_URL}" -o "${OPP_SCRIPT_FILE}"
ACTUAL_SHA256="$(sha256sum "${OPP_SCRIPT_FILE}" | awk '{print $1}')"
if [[ "${ACTUAL_SHA256}" != "${OPP_SCRIPT_SHA256}" ]]; then
    echo "Error: SHA256 mismatch for opp.sh (expected ${OPP_SCRIPT_SHA256}, got ${ACTUAL_SHA256})" >&2
    exit 1
fi
chmod +x "${OPP_SCRIPT_FILE}"

# upstream opp.sh does `>> $GITHUB_OUTPUT` (e.g. line 620). Unset or empty => bash "ambiguous redirect" outside GitHub Actions.
# Use a real file locally so you can inspect opp_* metadata (override with GITHUB_OUTPUT or OPP_LOCAL_GITHUB_OUTPUT).
if [[ -z "${GITHUB_OUTPUT:-}" ]]; then
    if [[ -n "${OPP_LOCAL_GITHUB_OUTPUT:-}" ]]; then
        export GITHUB_OUTPUT="${OPP_LOCAL_GITHUB_OUTPUT}"
        : > "${GITHUB_OUTPUT}"
    else
        export GITHUB_OUTPUT="$(mktemp "${TMPDIR:-/tmp}/opp-github-output.XXXXXX")"
    fi
    echo "OPP appends GitHub Actions-style metadata to: ${GITHUB_OUTPUT}" >&2
fi

echo "Running OPP tests: ${TESTS} on operators/${OPERATOR_NAME}/${OPERATOR_VERSION}"
bash "${OPP_SCRIPT_FILE}" "${TESTS}" "operators/${OPERATOR_NAME}/${OPERATOR_VERSION}"

# Workaround until upstream playbooks exit non-zero reliably (maistra istio-workspace).
if [[ -f /tmp/test.out ]] && tail -n 4 /tmp/test.out | grep -q "Failed with rc"; then
    echo "OPP reported failure in /tmp/test.out" >&2
    exit 1
fi

echo "OPP ${TESTS} completed successfully."
