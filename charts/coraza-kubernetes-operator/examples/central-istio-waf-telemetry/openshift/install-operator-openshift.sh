#!/usr/bin/env bash
# Build (optional), push, and Helm-install Coraza operator on OpenShift.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../../../.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/coraza-kubernetes-operator"
VALUES_FILE="${SCRIPT_DIR}/15-operator-openshift-values.yaml"
NAMESPACE="${NAMESPACE:-coraza-system}"
RELEASE="${RELEASE:-coraza-kubernetes-operator}"
BC_NAME="${BC_NAME:-coraza-kubernetes-operator}"

BUILD_IMAGE="${BUILD_IMAGE:-1}"
# registry: podman build + push via external registry route
# buildconfig: oc start-build --from-dir (no local registry login; works on CI clusters)
# auto: registry first, then buildconfig on failure
BUILD_METHOD="${BUILD_METHOD:-auto}"
# Image coordinates are deliberately supplied by the test runner. This example
# must not publish to or assume ownership of a registry repository or tag.
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-}"
IMAGE_TAG="${IMAGE_TAG:-}"
REGISTRY_ROUTE="${REGISTRY_ROUTE:-$(oc get route -n openshift-image-registry default-route -o jsonpath='{.spec.host}' 2>/dev/null || true)}"
INTERNAL_REPO="image-registry.openshift-image-registry.svc:5000/${NAMESPACE}/coraza-kubernetes-operator"

ensure_namespace() {
  oc get namespace "${NAMESPACE}" >/dev/null 2>&1 || oc create namespace "${NAMESPACE}"
}

require_image_tag() {
  if [[ -z "${IMAGE_TAG}" ]]; then
    echo "ERROR: set IMAGE_TAG to the tag you want to build or deploy" >&2
    exit 1
  fi
}

expose_registry_route() {
  if [[ -n "${REGISTRY_ROUTE}" ]]; then
    return 0
  fi
  echo "Exposing image registry default route..."
  oc patch configs.imageregistry cluster --type=merge -p '{"spec":{"defaultRoute":true}}' >/dev/null
  for _ in $(seq 1 30); do
    REGISTRY_ROUTE="$(oc get route -n openshift-image-registry default-route -o jsonpath='{.spec.host}' 2>/dev/null || true)"
    [[ -n "${REGISTRY_ROUTE}" ]] && break
    sleep 2
  done
}

registry_login() {
  local registry="$1"
  if oc registry login --registry="${registry}" >/dev/null 2>&1; then
    echo "Logged in via oc registry login"
    return 0
  fi
  local user token
  user="$(oc whoami)"
  token="$(oc whoami -t)"
  podman login --tls-verify=false -u "${user}" -p "${token}" "${registry}"
}

build_and_push_registry() {
  expose_registry_route
  if [[ -z "${REGISTRY_ROUTE}" ]]; then
    echo "ERROR: could not determine OpenShift image registry route" >&2
    return 1
  fi
  ensure_namespace

  local external_img="${REGISTRY_ROUTE}/${NAMESPACE}/coraza-kubernetes-operator:${IMAGE_TAG}"
  echo "Building ${external_img} ..."
  podman build -t "${external_img}" "${REPO_ROOT}"
  registry_login "${REGISTRY_ROUTE}"
  podman push --tls-verify=false "${external_img}"
  echo "Pushed ${external_img}; cluster pulls via ${INTERNAL_REPO}:${IMAGE_TAG}"
}

build_with_buildconfig() {
  ensure_namespace
  echo "Building in-cluster via BuildConfig/${BC_NAME} (no external registry push) ..."
  if ! oc get bc -n "${NAMESPACE}" "${BC_NAME}" >/dev/null 2>&1; then
    oc new-build --name="${BC_NAME}" \
      --strategy=docker \
      --binary \
      -n "${NAMESPACE}" \
      --to="coraza-kubernetes-operator:${IMAGE_TAG}"
  fi
  oc start-build "${BC_NAME}" --from-dir="${REPO_ROOT}" --follow -n "${NAMESPACE}"
  echo "Image available at ${INTERNAL_REPO}:${IMAGE_TAG}"
}

if [[ "${BUILD_IMAGE}" == "1" ]]; then
  require_image_tag
  case "${BUILD_METHOD}" in
    registry)
      build_and_push_registry
      ;;
    buildconfig)
      build_with_buildconfig
      ;;
    auto)
      if ! build_and_push_registry; then
        echo "WARN: registry push failed; falling back to OpenShift BuildConfig" >&2
        build_with_buildconfig
      fi
      ;;
    *)
      echo "ERROR: unknown BUILD_METHOD=${BUILD_METHOD} (use registry, buildconfig, or auto)" >&2
      exit 1
      ;;
  esac
  IMAGE_REPOSITORY="${INTERNAL_REPO}"
elif [[ -z "${IMAGE_REPOSITORY}" || -z "${IMAGE_TAG}" ]]; then
  echo "ERROR: set IMAGE_REPOSITORY and IMAGE_TAG when BUILD_IMAGE=0" >&2
  exit 1
fi

helm upgrade --install "${RELEASE}" "${CHART_DIR}" \
  --namespace "${NAMESPACE}" \
  --create-namespace \
  -f "${VALUES_FILE}" \
  --set createNamespace=false \
  --set image.repository="${IMAGE_REPOSITORY}" \
  --set image.tag="${IMAGE_TAG}"

echo "Waiting for operator pod..."
oc rollout status -n "${NAMESPACE}" deploy/"${RELEASE}" --timeout=300s
oc get pods -n "${NAMESPACE}"
