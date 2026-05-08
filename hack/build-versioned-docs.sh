#!/usr/bin/env bash
# Build multi-version documentation from docs/versions.yaml.
# Each version is built with Hugo inside the docs container image,
# then all outputs are consolidated into a single directory.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

VERSIONS_FILE="${REPO_ROOT}/docs/versions.yaml"
OUTPUT_DIR="${REPO_ROOT}/docs/public"
BASE_URL="https://networking-incubator.github.io/coraza-kubernetes-operator/"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
DOCS_IMG="${DOCS_IMG:-coraza-kubernetes-operator-docs}"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Build multi-version documentation site.

Options:
  --base-url URL      Base URL for the site (default: ${BASE_URL})
  --output-dir DIR    Output directory (default: ${OUTPUT_DIR})
  --versions-file F   Versions config file (default: ${VERSIONS_FILE})
  -h, --help          Show this help
EOF
}

require_arg() { [[ $# -ge 2 ]] || { echo "Error: $1 requires a value" >&2; usage >&2; exit 1; }; }
while [[ $# -gt 0 ]]; do
    case "$1" in
        --base-url)     require_arg "$@"; BASE_URL="$2"; shift 2 ;;
        --output-dir)   require_arg "$@"; OUTPUT_DIR="$2"; shift 2 ;;
        --versions-file) require_arg "$@"; VERSIONS_FILE="$2"; shift 2 ;;
        -h|--help)      usage; exit 0 ;;
        *)              echo "Unknown option: $1" >&2; usage >&2; exit 1 ;;
    esac
done

# Ensure base URL ends with /
[[ "${BASE_URL}" == */ ]] || BASE_URL="${BASE_URL}/"

command -v yq >/dev/null 2>&1 || { echo "Error: yq (mikefarah/yq) is required. Install from https://github.com/mikefarah/yq#install" >&2; exit 1; }

if [[ ! -f "${VERSIONS_FILE}" ]]; then
    echo "Error: versions file not found: ${VERSIONS_FILE}" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Parse versions.yaml
# ---------------------------------------------------------------------------
VERSION_COUNT=$(yq '.versions | length' "${VERSIONS_FILE}")
if [[ "${VERSION_COUNT}" -eq 0 ]]; then
    echo "Error: no versions defined in ${VERSIONS_FILE}" >&2
    exit 1
fi

DEFAULT_COUNT=$(yq '[.versions[] | select(.default == true)] | length' "${VERSIONS_FILE}")
if [[ "${DEFAULT_COUNT}" -ne 1 ]]; then
    echo "Error: exactly one version must have default: true (found ${DEFAULT_COUNT})" >&2
    exit 1
fi

DEFAULT_NAME=$(yq '.versions[] | select(.default == true) | .name' "${VERSIONS_FILE}")

# Read all version entries into arrays
declare -a V_NAMES V_REFS V_DISPLAYS V_DEFAULTS
for i in $(seq 0 $((VERSION_COUNT - 1))); do
    V_NAMES+=("$(yq ".versions[${i}].name" "${VERSIONS_FILE}")")
    V_REFS+=("$(yq ".versions[${i}].ref" "${VERSIONS_FILE}")")
    V_DISPLAYS+=("$(yq ".versions[${i}].display" "${VERSIONS_FILE}")")
    V_DEFAULTS+=("$(yq ".versions[${i}].default" "${VERSIONS_FILE}")")
done

# Validate: name format and no duplicates
declare -A seen_names
for name in "${V_NAMES[@]}"; do
    if [[ ! "${name}" =~ ^[a-zA-Z0-9._-]+$ ]]; then
        echo "Error: invalid version name '${name}': must match [a-zA-Z0-9._-]+" >&2
        exit 1
    fi
    if [[ -n "${seen_names[${name}]:-}" ]]; then
        echo "Error: duplicate version name: ${name}" >&2
        exit 1
    fi
    seen_names["${name}"]=1
done

# Validate: all non-HEAD refs exist
for i in "${!V_REFS[@]}"; do
    ref="${V_REFS[$i]}"
    if [[ "${ref}" != "HEAD" ]]; then
        if ! git -C "${REPO_ROOT}" rev-parse --verify "${ref}^{commit}" >/dev/null 2>&1; then
            echo "Error: git ref '${ref}' does not exist (version: ${V_NAMES[$i]})" >&2
            exit 1
        fi
        if [[ -z "$(git -C "${REPO_ROOT}" ls-tree --name-only "${ref}" -- docs/ 2>/dev/null)" ]]; then
            echo "Warning: ref '${ref}' has no docs/ directory, skipping version ${V_NAMES[$i]}" >&2
            V_REFS[$i]="SKIP"
        fi
    fi
done

# ---------------------------------------------------------------------------
# Build the shared versions dropdown list (used in every override config)
# ---------------------------------------------------------------------------
VERSIONS_LIST="[]"
for i in "${!V_NAMES[@]}"; do
    [[ "${V_REFS[$i]}" == "SKIP" ]] && continue
    export YQ_VDISPLAY="${V_DISPLAYS[$i]}"
    export YQ_VURL="${BASE_URL}${V_NAMES[$i]}/"
    VERSIONS_LIST=$(echo "${VERSIONS_LIST}" | \
        yq '. += [{"version": strenv(YQ_VDISPLAY), "url": strenv(YQ_VURL)}]')
done

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# Hugo runs as root inside the container, creating files the host user cannot
# delete. Use the container itself to fix ownership before removing.
clean_with_container() {
    local dir="$1"
    if [[ -d "${dir}" ]]; then
        ${CONTAINER_TOOL} run --rm -v "${dir}:/clean:z" alpine \
            chown -R "$(id -u):$(id -g)" /clean 2>/dev/null || true
        rm -rf "${dir}"
    fi
}

# ---------------------------------------------------------------------------
# Clean output directory
# ---------------------------------------------------------------------------
clean_with_container "${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"

# ---------------------------------------------------------------------------
# Build each version
# ---------------------------------------------------------------------------
TMPDIR_BASE=$(mktemp -d)
trap 'clean_with_container "${TMPDIR_BASE}"' EXIT

for i in "${!V_NAMES[@]}"; do
    name="${V_NAMES[$i]}"
    ref="${V_REFS[$i]}"
    display="${V_DISPLAYS[$i]}"
    is_default="${V_DEFAULTS[$i]}"

    [[ "${ref}" == "SKIP" ]] && continue

    echo "==> Building docs for ${name} (ref: ${ref})"

    # Determine source directory
    if [[ "${ref}" == "HEAD" ]]; then
        src_dir="${REPO_ROOT}/docs"
        work_dir="${REPO_ROOT}"
    else
        src_dir="${TMPDIR_BASE}/${name}/docs"
        work_dir="${TMPDIR_BASE}/${name}"
        mkdir -p "${work_dir}"
        git -C "${REPO_ROOT}" archive "${ref}" -- docs/ api/ hack/crd-ref-templates/ go.mod go.sum | tar -x -C "${work_dir}"
    fi

    # Generate CRD API reference from Go types for this version
    echo "    Generating API reference for ${name}..."
    ${CONTAINER_TOOL} run --rm \
        -v "${work_dir}:/repo:z" -w /repo "${DOCS_IMG}" \
        sh -c 'crd-ref-docs \
            --source-path=api \
            --config=hack/crd-ref-templates/config.yaml \
            --templates-dir=hack/crd-ref-templates \
            --renderer=markdown \
            --output-path=docs/content/reference/api.md \
        && chown '"$(id -u):$(id -g)"' docs/content/reference/api.md'

    # Generate override config via yq
    override_file="${TMPDIR_BASE}/${name}-override.yaml"
    # dev (HEAD) always shows the banner regardless of default status;
    # tagged versions show the banner only when they are not the default.
    is_archived="false"
    if [[ "${ref}" == "HEAD" || "${is_default}" != "true" ]]; then
        is_archived="true"
    fi

    export YQ_BASE="${BASE_URL}${name}/"
    export YQ_VER="${display}"
    export YQ_LATEST="${BASE_URL}${DEFAULT_NAME}/"
    export YQ_VERSIONS="${VERSIONS_LIST}"

    yq -n '.baseURL = strenv(YQ_BASE)' \
        | yq '.params.version = strenv(YQ_VER)' \
        | yq ".params.archived_version = ${is_archived}" \
        | yq '.params.url_latest_version = strenv(YQ_LATEST)' \
        | yq '.params.version_menu = "Versions"' \
        | yq '.params.version_menu_pagelinks = false' \
        | yq '.params.versions = env(YQ_VERSIONS)' \
        > "${override_file}"

    # Build output directory for this version
    version_output="${TMPDIR_BASE}/${name}-output"
    mkdir -p "${version_output}"

    # Run Hugo in the docs container. The container pre-fetches Hugo modules
    # from HEAD as a cache optimization; Hugo re-downloads if the tag's
    # go.mod requires different versions.
    ${CONTAINER_TOOL} run --rm \
        -v "${src_dir}:/src:z" \
        -v "${override_file}:/tmp/hugo-version-override.yaml:ro,z" \
        -v "${version_output}:/output:z" \
        "${DOCS_IMG}" \
        sh -c 'hugo --config hugo.yaml,/tmp/hugo-version-override.yaml --minify --destination /output && chown -R '"$(id -u):$(id -g)"' /output /src/resources'

    # Move to final output
    mv "${version_output}" "${OUTPUT_DIR}/${name}"
    echo "    Built ${name} -> ${OUTPUT_DIR}/${name}/"
done

# ---------------------------------------------------------------------------
# Generate root redirect
# ---------------------------------------------------------------------------
cat > "${OUTPUT_DIR}/index.html" <<EOF
<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta http-equiv="refresh" content="0; url=./${DEFAULT_NAME}/">
  <link rel="canonical" href="${BASE_URL}${DEFAULT_NAME}/">
  <title>Redirecting...</title>
</head>
<body>
  <p>Redirecting to <a href="./${DEFAULT_NAME}/">${DEFAULT_NAME}</a>...</p>
</body>
</html>
EOF

cat > "${OUTPUT_DIR}/404.html" <<EOF
<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Page Not Found</title>
</head>
<body>
  <h1>Page Not Found</h1>
  <p>The page you requested was not found. <a href="${BASE_URL}${DEFAULT_NAME}/">Go to the latest documentation.</a></p>
</body>
</html>
EOF

echo ""
echo "==> Multi-version docs built successfully in ${OUTPUT_DIR}/"
echo "    Default version: ${DEFAULT_NAME}"
ls -d "${OUTPUT_DIR}"/*/
