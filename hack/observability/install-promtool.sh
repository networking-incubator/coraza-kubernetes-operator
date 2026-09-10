#!/usr/bin/env bash
# Install promtool for local dev and CI (make observability.promtool.install).
# Version is pinned in the Makefile (PROM_VERSION); keep SHA256 in sync when bumping.
set -euo pipefail

: "${PROM_VERSION:?PROM_VERSION must be set}"

INSTALL_DIR="${INSTALL_DIR:-${GOBIN:-${HOME}/.local/bin}}"
mkdir -p "${INSTALL_DIR}"

OS="$(go env GOOS)"
ARCH="$(go env GOARCH)"
PROM_ARCHIVE="prometheus-${PROM_VERSION}.${OS}-${ARCH}.tar.gz"
PROM_URL="https://github.com/prometheus/prometheus/releases/download/v${PROM_VERSION}/${PROM_ARCHIVE}"

# GitHub release downloads occasionally reset mid-transfer in CI (curl 35).
# --retry-all-errors covers connection resets; -C - resumes a partial file.
curl --retry 5 --retry-all-errors --retry-delay 2 -C - -fsSLo "${PROM_ARCHIVE}" "${PROM_URL}"

# PROM_SHA256 is verified only on linux/amd64 (CI pin). Other platforms skip checksum.
if [ -n "${PROM_SHA256:-}" ] && [ "${OS}" = "linux" ] && [ "${ARCH}" = "amd64" ]; then
  echo "${PROM_SHA256}  ${PROM_ARCHIVE}" | sha256sum -c -
fi

tar -xzf "${PROM_ARCHIVE}" --strip-components=1 "prometheus-${PROM_VERSION}.${OS}-${ARCH}/promtool"
install promtool "${INSTALL_DIR}/promtool"
rm -f "${PROM_ARCHIVE}" promtool

echo "promtool ${PROM_VERSION} (${OS}/${ARCH}) installed to ${INSTALL_DIR}/promtool"
