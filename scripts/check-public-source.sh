#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -P -- "$(dirname -- "$0")/.." && pwd)

forbidden='evancao|yifengcao|yuexiazhuojiu|119\.28\.32\.229|Evan Phone|Evan 电话|EVAN\.CAO|com\.evan'

if rg -n -i --hidden \
  --glob '!.git/**' \
  --glob '!.ai/**' \
  --glob '!local/**' \
  --glob '!dist/**' \
  --glob '!third_party/**' \
  --glob '!scripts/check-public-source.sh' \
  --glob '!android/**/build/**' \
  --glob '!macos/DJOneHubNotifier/.build*/**' \
  "$forbidden" "$ROOT"; then
  printf '%s\n' 'ERROR: public source contains a private deployment or personal marker' >&2
  exit 1
fi

if find "$ROOT" \
  \( -path "$ROOT/.git" -o -path "$ROOT/.ai" -o -path "$ROOT/local" \
     -o -path "$ROOT/dist" -o -path '*/build' -o -path '*/.build' \) -prune -o \
  \( -iname '*evancao*' -o -iname '*yifengcao*' -o -iname '*yuexiazhuojiu*' \
     -o -iname '*evan phone*' -o -iname '*evan-4g-phone*' \) -print | grep -q .; then
  printf '%s\n' 'ERROR: public source contains a private or personal filename' >&2
  exit 1
fi

if find "$ROOT/docs/images" -type f ! -name 'maccellular-pwa-mobile.png' -print 2>/dev/null | grep -q .; then
  printf '%s\n' 'ERROR: public source contains an unapproved documentation screenshot' >&2
  exit 1
fi

printf '%s\n' 'public source privacy check: PASS'
