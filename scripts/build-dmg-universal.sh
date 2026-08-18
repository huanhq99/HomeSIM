#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${1:-v1.0.0-rc.4}
DMG_NAME="MacCellular-macOS-universal-${VERSION}.dmg"
STAGE="${ROOT_DIR}/dist/dmg-stage-universal"
DMG="${ROOT_DIR}/dist/${DMG_NAME}"
CHECKSUM="${DMG}.sha256"
NOTIFIER_SRC="${ROOT_DIR}/macos/DJOneHubNotifier"
BUILD_ROOT="${TMPDIR:-/tmp}/djonehub-macos-package-universal"

echo "==> 1/4 构建通用主程序（arm64 + x86_64）"
"${ROOT_DIR}/scripts/package-macos-universal.sh" "${VERSION}"

echo "==> 2/4 构建通用通知助手"
mkdir -p "${BUILD_ROOT}/local-cache/clang" "${BUILD_ROOT}/local-cache/swiftpm"
export CLANG_MODULE_CACHE_PATH="${BUILD_ROOT}/local-cache/clang"
export SWIFTPM_MODULECACHE_OVERRIDE="${BUILD_ROOT}/local-cache/clang"
export SWIFTPM_CUSTOM_CACHE_PATH="${BUILD_ROOT}/local-cache/swiftpm"
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
if [ ! -x "${DEVELOPER_DIR}/Toolchains/XcodeDefault.xctoolchain/usr/bin/swiftc" ]; then
  echo "A full Xcode installation is required to build the Intel notifier slice." >&2
  exit 1
fi
cd "${NOTIFIER_SRC}"
swift build --disable-sandbox -c release
"${NOTIFIER_SRC}/.build/release/DJOneHubNotifier" --self-test

# SwiftPM on an Apple-Silicon host normally emits only the native slice. Build
# the Intel slice explicitly, including the two local C targets used by the
# notifier. This avoids claiming a universal app while silently shipping arm64.
INTEL_ROOT="${BUILD_ROOT}/notifier-x86_64"
mkdir -p "${INTEL_ROOT}/module-cache"
cat > "${INTEL_ROOT}/CModemBridge.modulemap" <<EOF
module CModemBridge { header "${NOTIFIER_SRC}/Sources/CModemBridge/include/CModemBridge.h" export * }
EOF
cat > "${INTEL_ROOT}/CUACProbe.modulemap" <<EOF
module CUACProbe { header "${NOTIFIER_SRC}/Sources/CUACProbe/include/CUACProbe.h" export * }
EOF
xcrun clang -target x86_64-apple-macosx13.0 -O2 -fmodules \
  -fmodules-cache-path="${INTEL_ROOT}/module-cache" \
  -fmodule-map-file="${INTEL_ROOT}/CModemBridge.modulemap" \
  -I Sources/CModemBridge/include -c Sources/CModemBridge/ModemBridge.c \
  -o "${INTEL_ROOT}/ModemBridge.o"
xcrun clang -target x86_64-apple-macosx13.0 -O2 -fmodules \
  -fmodules-cache-path="${INTEL_ROOT}/module-cache" \
  -fmodule-map-file="${INTEL_ROOT}/CUACProbe.modulemap" \
  -I Sources/CUACProbe/include -c Sources/CUACProbe/CUACProbe.c \
  -o "${INTEL_ROOT}/CUACProbe.o"
xcrun swiftc -O -target x86_64-apple-macosx13.0 -sdk "$(xcrun --show-sdk-path)" \
  -Xcc -fmodules-cache-path="${INTEL_ROOT}/module-cache" \
  -Xcc -fmodule-map-file="${INTEL_ROOT}/CModemBridge.modulemap" \
  -Xcc -fmodule-map-file="${INTEL_ROOT}/CUACProbe.modulemap" \
  -I Sources/CModemBridge/include -I Sources/CUACProbe/include \
  Sources/DJOneHubNotifier/*.swift "${INTEL_ROOT}/ModemBridge.o" "${INTEL_ROOT}/CUACProbe.o" \
  -framework CoreAudio -framework CoreFoundation -framework IOKit -framework AVFoundation \
  -framework AppKit -framework UserNotifications -framework Contacts \
  -o "${INTEL_ROOT}/DJOneHubNotifier"
lipo -create "${NOTIFIER_SRC}/.build/release/DJOneHubNotifier" "${INTEL_ROOT}/DJOneHubNotifier" \
  -output "${BUILD_ROOT}/DJOneHubNotifier-universal"
file "${BUILD_ROOT}/DJOneHubNotifier-universal" | cut -c1-120
for arch in arm64 x86_64; do
  lipo "${BUILD_ROOT}/DJOneHubNotifier-universal" -verify_arch "${arch}"
done

echo "==> 3/4 组装安装目录"
rm -rf "${STAGE}"
mkdir -p "${STAGE}/MacCellular.app/Contents/MacOS" "${STAGE}/MacCellular.app/Contents/Resources"
ditto --norsrc --noextattr --noqtn --noacl "${ROOT_DIR}/dist/release/MacCellular-macOS-universal-${VERSION}" "${STAGE}/maccellular"
cp "${BUILD_ROOT}/DJOneHubNotifier-universal" "${STAGE}/MacCellular.app/Contents/MacOS/DJOneHubNotifier"
cp "${NOTIFIER_SRC}/Info.plist" "${STAGE}/MacCellular.app/Contents/Info.plist"
cp "${NOTIFIER_SRC}/Resources/AppIcon.icns" "${STAGE}/MacCellular.app/Contents/Resources/AppIcon.icns"
chmod 755 "${STAGE}/MacCellular.app/Contents/MacOS/DJOneHubNotifier"
codesign --force --deep --sign - "${STAGE}/MacCellular.app"
codesign --verify --deep --strict "${STAGE}/MacCellular.app"
plutil -lint "${STAGE}/MacCellular.app/Contents/Info.plist"
EXPECTED_APP_VERSION=${VERSION#v}
EXPECTED_APP_VERSION=${EXPECTED_APP_VERSION%%-*}
ACTUAL_APP_VERSION=$(plutil -extract CFBundleShortVersionString raw "${STAGE}/MacCellular.app/Contents/Info.plist")
if [ "${ACTUAL_APP_VERSION}" != "${EXPECTED_APP_VERSION}" ]; then
  echo "App version ${ACTUAL_APP_VERSION} does not match package version ${VERSION}." >&2
  exit 1
fi
for binary in \
  "${STAGE}/MacCellular.app/Contents/MacOS/DJOneHubNotifier" \
  "${STAGE}/maccellular/bin/djonehub-macos" \
  "${STAGE}/maccellular/lib/libusb-1.0.0.dylib"
do
  for arch in arm64 x86_64; do
    lipo "${binary}" -verify_arch "${arch}"
  done
done
cp "${ROOT_DIR}/scripts/dmg/安装 MacCellular.command" "${STAGE}/安装 MacCellular.command"
cp "${ROOT_DIR}/scripts/dmg/配置手机访问.command" "${STAGE}/配置手机访问.command"
cp "${ROOT_DIR}/scripts/dmg/卸载 MacCellular.command" "${STAGE}/卸载 MacCellular.command"
cp "${ROOT_DIR}/scripts/dmg/使用说明.txt" "${STAGE}/使用说明.txt"
chmod 755 "${STAGE}/安装 MacCellular.command" "${STAGE}/配置手机访问.command" "${STAGE}/卸载 MacCellular.command"

"${ROOT_DIR}/scripts/check-release-runtime-policy.sh" "${STAGE}"

echo "==> 4/4 生成 DMG"
rm -f "${DMG}" "${CHECKSUM}"
hdiutil create -volname "MacCellular" -srcfolder "${STAGE}" -ov -format UDZO "${DMG}"
hdiutil verify "${DMG}"
"${ROOT_DIR}/scripts/check-release-runtime-policy.sh" "${DMG}"
(
  cd "$(dirname -- "${DMG}")"
  shasum -a 256 "$(basename -- "${DMG}")" >"$(basename -- "${CHECKSUM}")"
)

echo
echo "完成：${DMG}"
echo "校验：${CHECKSUM}"
