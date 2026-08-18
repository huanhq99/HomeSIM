#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DIST_DIR="${ROOT_DIR}/dist"

mkdir -p "${DIST_DIR}"

cd "${ROOT_DIR}"

ARCH=${DJONEHUB_BUILD_ARCH:-$(uname -m)}
case "${ARCH}" in
  arm64) FILE_ARCH=arm64 ;;
  amd64) FILE_ARCH=x86_64 ;;
  x86_64) ARCH=amd64; FILE_ARCH=x86_64 ;;
  *)
    echo "Unsupported macOS build architecture: ${ARCH}" >&2
    exit 1
    ;;
esac
PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig:/usr/local/lib/pkgconfig}"
export PKG_CONFIG_PATH

CGO_ENABLED=1 GOOS=darwin GOARCH="${ARCH}" go build \
  -p 2 \
  -trimpath -buildvcs=false -ldflags="-s -w" \
  -o "${DIST_DIR}/djonehub-macos-${ARCH}" ./cmd/djonehub-macos

codesign --force --sign - "${DIST_DIR}/djonehub-macos-${ARCH}"
codesign --verify --strict "${DIST_DIR}/djonehub-macos-${ARCH}"
file "${DIST_DIR}/djonehub-macos-${ARCH}" | grep -q "${FILE_ARCH}" || {
  echo "Built binary does not contain the requested ${ARCH} architecture." >&2
  exit 1
}

cp "${DIST_DIR}/djonehub-macos-${ARCH}" "${DIST_DIR}/djonehub-macos"

echo "macOS binaries written to ${DIST_DIR}"
