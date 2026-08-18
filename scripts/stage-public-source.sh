#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -P -- "$(dirname -- "$0")/.." && pwd)
VERSION=$(tr -d '\r\n' < "$ROOT/VERSION")
DEFAULT_OUTPUT="$ROOT/local/public-release/MacCellular-$VERSION"
OUTPUT=${1:-$DEFAULT_OUTPUT}

case "$OUTPUT" in
  /*) ;;
  *) OUTPUT="$PWD/$OUTPUT" ;;
esac

case "$OUTPUT" in
  "$ROOT/local/public-release/"*) ;;
  *)
    printf '%s\n' 'ERROR: output must be below local/public-release' >&2
    exit 1
    ;;
esac

if [ -e "$OUTPUT" ]; then
  printf 'ERROR: output already exists: %s\n' "$OUTPUT" >&2
  exit 1
fi

command -v rsync >/dev/null 2>&1 || {
  printf '%s\n' 'ERROR: rsync is required' >&2
  exit 1
}

"$ROOT/scripts/check-public-source.sh"

PARENT=$(dirname "$OUTPUT")
mkdir -p "$PARENT"
chmod 700 "$ROOT/local" "$PARENT" 2>/dev/null || true

STAGE="$PARENT/.MacCellular-stage.$$"
cleanup() {
  if [ -d "$STAGE" ]; then
    rm -rf -- "$STAGE"
  fi
}
trap cleanup EXIT HUP INT TERM
mkdir "$STAGE"

rsync -a \
  --exclude '/.git/' \
  --exclude '/.ai/' \
  --exclude '/.codex/' \
  --exclude '/AGENTS.md' \
  --exclude '/docs/history/' \
  --exclude '/local/' \
  --exclude '/dist/' \
  --exclude '/recordings/' \
  --exclude '/runtime-data/' \
  --exclude '/backups/' \
  --exclude '/node_modules/' \
  --exclude '/DerivedData/' \
  --exclude '**/.build*/' \
  --exclude '**/.gradle/' \
  --exclude '**/build/' \
  --exclude '**/dist/' \
  --exclude '**/__pycache__/' \
  --exclude '*.pyc' \
  --exclude '*.log' \
  --exclude '*.pid' \
  --exclude '*.pcap' \
  --exclude '*.pcapng' \
  --exclude '*.atsession' \
  --include '.env.example' \
  --include '/third_party/euicc-go/http/rootci/bundle.pem' \
  --include '/third_party/euicc-go/http/rootci/bundle-tests.pem' \
  --include '/internal/config/imei.go' \
  --include '/internal/config/imei_test.go' \
  --include '/internal/config/persist_devices_imei_test.go' \
  --include '/internal/modem/imei_probe.go' \
  --exclude '.env' \
  --exclude '.env.*' \
  --exclude '*.pem' \
  --exclude '*.key' \
  --exclude '*.p12' \
  --exclude '*.mobileprovision' \
  --exclude '*.jks' \
  --exclude '*.keystore' \
  --exclude '*imei*' \
  --exclude '*imsi*' \
  --exclude '*iccid*' \
  --exclude '*phone-number*' \
  --exclude '*sms-export*' \
  --exclude '*call-recording*' \
  --exclude '.DS_Store' \
  --exclude '*HANDOFF*' \
  "$ROOT/" "$STAGE/"

"$STAGE/scripts/check-public-source.sh"

if find "$STAGE" -type l -print | grep -q .; then
  printf '%s\n' 'ERROR: public candidate contains symbolic links' >&2
  exit 1
fi

mv "$STAGE" "$OUTPUT"
trap - EXIT HUP INT TERM
printf 'public source candidate staged: %s\n' "$OUTPUT"
