#!/bin/zsh

set -eu

root="${0:A:h}"
build_root="${root}/.build"
output_root="${root}/.build-output"
app="${output_root}/DJOneHubNotifier.app"
standalone="${output_root}/DJOneHubNotifier"
cache_root="${build_root}/local-cache"
helper_identifier="io.maccellular.phone.media-helper"
helper_requirement='=designated => identifier "io.maccellular.phone.media-helper"'

cd "${root}"
mkdir -p "${cache_root}/clang" "${cache_root}/swiftpm" "${output_root}"
export CLANG_MODULE_CACHE_PATH="${cache_root}/clang"
export SWIFTPM_MODULECACHE_OVERRIDE="${cache_root}/clang"
export SWIFTPM_CUSTOM_CACHE_PATH="${cache_root}/swiftpm"
swift build --disable-sandbox -c release
"${build_root}/release/DJOneHubNotifier" --self-test

# The public direct-voice installer consumes a standalone helper rather than
# the GUI app bundle. Produce and verify that artifact explicitly so callers do
# not need to reach into SwiftPM's mutable .build directory.
cp "${build_root}/release/DJOneHubNotifier" "${standalone}"
chmod 755 "${standalone}"
xattr -c "${standalone}"
codesign --force --sign - \
  --identifier "${helper_identifier}" \
  --requirements "${helper_requirement}" \
  "${standalone}"
codesign --verify --strict --verbose=2 "${standalone}"
codesign -d -r- "${standalone}" 2>&1 | \
  grep -F 'designated => identifier "io.maccellular.phone.media-helper"' >/dev/null
"${standalone}" --self-test

rm -rf "${app}"
mkdir -p "${app}/Contents/MacOS" "${app}/Contents/Resources"
cp "${standalone}" "${app}/Contents/MacOS/DJOneHubNotifier"
cp "${root}/Info.plist" "${app}/Contents/Info.plist"
cp "${root}/Resources/AppIcon.icns" "${app}/Contents/Resources/AppIcon.icns"
chmod 755 "${app}/Contents/MacOS/DJOneHubNotifier"
# Finder/File Provider metadata inherited from a synced workspace is not part
# of the application and causes codesign to reject an otherwise valid bundle.
# A File Provider workspace can restore FinderInfo immediately after the first
# clear, so retry the clear-and-sign operation as one bounded build step.
signed=0
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  xattr -cr "${app}"
  if codesign --force --deep --sign - "${app}" && \
      codesign --verify --deep --strict --verbose=2 "${app}"; then
    signed=1
    break
  fi
  sleep 0.05
done
if (( signed != 1 )); then
  print -u2 -- "Unable to sign the MacCellular app bundle after clearing Finder metadata."
  exit 1
fi
plutil -lint "${app}/Contents/Info.plist"

print -r -- "${app}"
print -r -- "${standalone}"
