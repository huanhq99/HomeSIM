#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v1.0.0-rc.4}
DMG_NAME="MacCellular-macOS-arm64-${VERSION}.dmg"
DMG="${ROOT_DIR}/dist/${DMG_NAME}"
STAGE=$(mktemp -d "${TMPDIR:-/tmp}/maccellular-dmg-stage.XXXXXX")
VERIFY_MOUNT=""
VERIFY_DEVICE=""
cleanup() {
  if [ -n "${VERIFY_DEVICE}" ]; then
    hdiutil detach "${VERIFY_DEVICE}" >/dev/null 2>&1 || true
  fi
  if [ -n "${VERIFY_MOUNT}" ]; then
    rmdir "${VERIFY_MOUNT}" 2>/dev/null || true
  fi
  rm -rf -- "${STAGE}"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

echo "==> 1/4 构建 Go 主程序与 libusb"
"${ROOT_DIR}/scripts/package-macos-arm64.sh" "${VERSION}"

echo "==> 2/4 构建通知助手（含自检）"
(cd "${ROOT_DIR}/macos/DJOneHubNotifier" && ./build-app.sh)

echo "==> 3/4 组装安装目录"
ditto --norsrc --noextattr --noqtn --noacl "${ROOT_DIR}/dist/release/MacCellular-macOS-arm64-${VERSION}" "${STAGE}/maccellular"
ditto --norsrc --noextattr --noqtn --noacl "${ROOT_DIR}/macos/DJOneHubNotifier/.build-output/DJOneHubNotifier.app" "${STAGE}/MacCellular.app"
cp "${ROOT_DIR}/scripts/dmg/安装 MacCellular.command" "${STAGE}/安装 MacCellular.command"
cp "${ROOT_DIR}/scripts/dmg/配置手机访问.command" "${STAGE}/配置手机访问.command"
cp "${ROOT_DIR}/scripts/dmg/卸载 MacCellular.command" "${STAGE}/卸载 MacCellular.command"
cp "${ROOT_DIR}/scripts/dmg/使用说明.txt" "${STAGE}/使用说明.txt"
chmod 755 "${STAGE}/安装 MacCellular.command" "${STAGE}/配置手机访问.command" "${STAGE}/卸载 MacCellular.command"
EXPECTED_APP_VERSION=${VERSION#v}
EXPECTED_APP_VERSION=${EXPECTED_APP_VERSION%%-*}
ACTUAL_APP_VERSION=$(plutil -extract CFBundleShortVersionString raw "${STAGE}/MacCellular.app/Contents/Info.plist")
if [ "${ACTUAL_APP_VERSION}" != "${EXPECTED_APP_VERSION}" ]; then
  echo "App version ${ACTUAL_APP_VERSION} does not match package version ${VERSION}." >&2
  exit 1
fi
codesign --verify --deep --strict "${STAGE}/MacCellular.app"
plutil -lint "${STAGE}/MacCellular.app/Contents/Info.plist"
"${ROOT_DIR}/scripts/check-release-runtime-policy.sh" "${STAGE}"

echo "==> 4/4 生成 DMG"
rm -f "${DMG}"
hdiutil create -volname "MacCellular" -srcfolder "${STAGE}" -ov -format UDZO "${DMG}"
hdiutil verify "${DMG}"
"${ROOT_DIR}/scripts/check-release-runtime-policy.sh" "${DMG}"

VERIFY_MOUNT=$(mktemp -d "${TMPDIR:-/tmp}/maccellular-dmg-verify.XXXXXX")
VERIFY_DEVICE=$(hdiutil attach -readonly -nobrowse -mountpoint "${VERIFY_MOUNT}" "${DMG}" | awk '/^\/dev\// { print $1; exit }')
if [ -z "${VERIFY_DEVICE}" ]; then
  echo "Unable to mount the completed MacCellular DMG." >&2
  exit 1
fi
if ! codesign --verify --deep --strict "${VERIFY_MOUNT}/MacCellular.app"; then
  echo "The App inside the completed DMG failed code-signature verification." >&2
  exit 1
fi
hdiutil detach "${VERIFY_DEVICE}" >/dev/null
VERIFY_DEVICE=""
rmdir "${VERIFY_MOUNT}"
VERIFY_MOUNT=""

echo
echo "完成：${DMG}"
