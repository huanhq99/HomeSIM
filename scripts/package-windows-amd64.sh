#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v1.0.0-rc.4}
case "${VERSION}" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "Version must start with vMAJOR.MINOR.PATCH." >&2; exit 64 ;; esac
PACKAGE_NAME="MacCellular-Windows-amd64-${VERSION}"
STAGE_ROOT="${ROOT_DIR}/dist/release"
STAGE_DIR="${STAGE_ROOT}/${PACKAGE_NAME}"
ARCHIVE="${STAGE_ROOT}/${PACKAGE_NAME}.zip"
CHECKSUM="${ARCHIVE}.sha256"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-windows-package"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required to build the Windows package." >&2
  exit 1
fi
if ! command -v zip >/dev/null 2>&1; then
  echo "zip is required to build the Windows package." >&2
  exit 1
fi

"${ROOT_DIR}/scripts/check-release-runtime-policy.sh" --git-tracked "${ROOT_DIR}"

rm -rf "${STAGE_DIR}"
mkdir -p "${STAGE_DIR}/licenses" "${BUILD_ROOT}" "${STAGE_ROOT}"

cd "${ROOT_DIR}"
GOCACHE="${BUILD_ROOT}/go-cache" CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags="-s -w -H=windowsgui" \
  -o "${STAGE_DIR}/MacCellular.exe" ./cmd/djonehub-macos

cp "${ROOT_DIR}/windows/install.ps1" "${STAGE_DIR}/install.ps1"
cp "${ROOT_DIR}/windows/uninstall.ps1" "${STAGE_DIR}/uninstall.ps1"
cp "${ROOT_DIR}/windows/Install MacCellular.cmd" "${STAGE_DIR}/Install MacCellular.cmd"
cp "${ROOT_DIR}/windows/Stop MacCellular.cmd" "${STAGE_DIR}/Stop MacCellular.cmd"
cp "${ROOT_DIR}/windows/README-Windows.txt" "${STAGE_DIR}/README-Windows.txt"
cp "${ROOT_DIR}/LICENSE" "${STAGE_DIR}/LICENSE"
cp "${ROOT_DIR}/LEGAL_AND_RESPONSIBLE_USE.md" "${STAGE_DIR}/合法与负责任使用.md"
cp "${ROOT_DIR}/PRIVACY.md" "${STAGE_DIR}/数据与隐私说明.md"
printf '%s\n' "${VERSION}" >"${STAGE_DIR}/VERSION"
cp "${ROOT_DIR}/THIRD_PARTY_NOTICES.md" "${STAGE_DIR}/THIRD_PARTY_NOTICES.md"
cp "${ROOT_DIR}/licenses/MaVo-LICENSE" "${STAGE_DIR}/licenses/MaVo-LICENSE"
sh "${ROOT_DIR}/scripts/copy-third-party-licenses.sh" "${STAGE_DIR}"

"${ROOT_DIR}/scripts/check-release-runtime-policy.sh" "${STAGE_DIR}"

rm -f "${ARCHIVE}" "${CHECKSUM}"
(
  cd "${STAGE_ROOT}"
  zip -q -r "$(basename -- "${ARCHIVE}")" "${PACKAGE_NAME}"
  "${ROOT_DIR}/scripts/check-release-runtime-policy.sh" "${ARCHIVE}"
  shasum -a 256 "$(basename -- "${ARCHIVE}")" >"$(basename -- "${CHECKSUM}")"
)

echo "Release directory: ${STAGE_DIR}"
echo "Release archive:   ${ARCHIVE}"
echo "Checksum:          ${CHECKSUM}"
