#!/bin/sh
set -eu
umask 077

ACTION=${1:-check}
SOURCE_REVISION=0443dfdaf8aec086fd76ba2ee9152fd908114524
SOURCE_BASE="https://raw.githubusercontent.com/moluncn/mavo/$SOURCE_REVISION/Resources/ModuleVoice"
TARGET="$HOME/Library/Application Support/MacCellular Runtime/ModuleVoice"
FILES='COPYING-GPL-2.0 MODULE-REPORT.md manifest.json mavo-pcm-bridge.armv7 qdc507_aprv3.ko qdc507_voice.ko'

complete_runtime() {
  [ -d "$TARGET" ] || return 1
  for name in $FILES; do
    [ -s "$TARGET/$name" ] || return 1
  done
}

if complete_runtime; then
  printf 'QDC507 voice runtime is ready: %s\n' "$TARGET"
  exit 0
fi

case "$ACTION" in
  check)
    printf '%s\n' 'QDC507 voice runtime is not prepared.'
    printf '%s\n' 'Run this command with install to download it directly from the original MaVo repository.'
    exit 2
    ;;
  install) ;;
  *)
    printf '%s\n' 'Usage: prepare-module-voice.sh [check|install]' >&2
    exit 64
    ;;
esac

command -v curl >/dev/null 2>&1 || {
  printf '%s\n' 'curl is required to download the QDC507 voice runtime.' >&2
  exit 69
}

PARENT=$(dirname "$TARGET")
mkdir -p "$PARENT"
chmod 700 "$PARENT"
STAGE="$PARENT/.ModuleVoice.download.$$"
cleanup() {
  if [ -d "$STAGE" ]; then
    rm -rf -- "$STAGE"
  fi
}
trap cleanup EXIT HUP INT TERM
mkdir "$STAGE"

for name in $FILES; do
  printf 'Downloading %s from the original MaVo repository…\n' "$name"
  curl -fsSL --retry 2 "$SOURCE_BASE/$name" -o "$STAGE/$name"
  [ -s "$STAGE/$name" ] || {
    printf 'Downloaded file is empty: %s\n' "$name" >&2
    exit 65
  }
done
chmod 600 "$STAGE"/*
chmod 700 "$STAGE/mavo-pcm-bridge.armv7"

if [ -e "$TARGET" ]; then
  printf 'Existing incomplete runtime was not replaced: %s\n' "$TARGET" >&2
  exit 73
fi
mv "$STAGE" "$TARGET"
trap - EXIT HUP INT TERM
printf 'QDC507 voice runtime prepared: %s\n' "$TARGET"
printf '%s\n' 'The files came directly from the pinned MaVo source revision and are not part of the MacCellular release.'
