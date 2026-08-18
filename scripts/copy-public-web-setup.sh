#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -P -- "$(dirname -- "$0")/.." && pwd)
DESTINATION=${1:?destination package directory is required}
SOURCE="$ROOT/deploy/public-web-mac"
TARGET="$DESTINATION/setup/public-web-mac"

mkdir -p "$TARGET"
for name in \
  install-local.sh \
  io.maccellular.phone.cloudflared.plist.example \
  io.maccellular.phone.direct-voice.plist.example \
  io.maccellular.phone.external-sip.plist.example \
  io.maccellular.phone.media-helper.plist.example \
  io.maccellular.phone.plist.example \
  route-contract.json \
  README.md
do
  cp "$SOURCE/$name" "$TARGET/$name"
done
chmod 755 "$TARGET/install-local.sh"
cp "$ROOT/scripts/prepare-module-voice.sh" "$DESTINATION/setup/prepare-module-voice.sh"
chmod 755 "$DESTINATION/setup/prepare-module-voice.sh"
