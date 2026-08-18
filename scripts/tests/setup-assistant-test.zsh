#!/bin/zsh
set -eu

ROOT=${0:A:h:h:h}
TEMP=$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/maccellular-setup-test.XXXXXX")
trap '/bin/rm -rf -- "$TEMP"' EXIT HUP INT TERM

mkdir -p "$TEMP/maccellular/setup/public-web-mac" "$TEMP/maccellular/bin" \
  "$TEMP/MacCellular.app/Contents/MacOS"
cp "$ROOT/scripts/dmg/配置手机访问.command" "$TEMP/配置手机访问.command"
for file in \
  "$TEMP/maccellular/bin/djonehub-macos" \
  "$TEMP/maccellular/bin/cloudflared" \
  "$TEMP/MacCellular.app/Contents/MacOS/DJOneHubNotifier"
do
  print -r -- '#!/bin/sh' > "$file"
  chmod 755 "$file"
done

cat > "$TEMP/maccellular/setup/public-web-mac/install-local.sh" <<'SH'
#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$MACCELLULAR_SETUP_TEST_LOG"
SH
chmod 755 "$TEMP/maccellular/setup/public-web-mac/install-local.sh"
cat > "$TEMP/maccellular/setup/prepare-module-voice.sh" <<'SH'
#!/bin/sh
exit 0
SH
chmod 755 "$TEMP/maccellular/setup/prepare-module-voice.sh"

SMS_LOG="$TEMP/sms.log"
MACCELLULAR_SETUP_TEST_LOG="$SMS_LOG" \
  printf '%s\n' \
    '1' 'phone.example.com' 'team.cloudflareaccess.com' 'audience_123' \
    'owner@sample.test' 'tunnel-secret-value' 'n' | \
  MACCELLULAR_SETUP_TEST_LOG="$SMS_LOG" zsh "$TEMP/配置手机访问.command" >/dev/null

grep -Fxq -- '--profile' "$SMS_LOG"
grep -Fxq -- 'sms' "$SMS_LOG"
grep -Fxq -- '--public-host' "$SMS_LOG"
grep -Fxq -- 'phone.example.com' "$SMS_LOG"
if grep -Fq -- 'tunnel-secret-value' "$SMS_LOG"; then
  print -u2 -- 'setup assistant exposed the Tunnel token in argv'
  exit 1
fi

VOICE_LOG="$TEMP/voice.log"
MACCELLULAR_SETUP_TEST_LOG="$VOICE_LOG" \
  printf '%s\n' \
    '2' 'phone.example.com' 'team.cloudflareaccess.com' 'audience_123' \
    'owner@sample.test' 'tunnel-secret-value' 'turn.example.com' \
    'turn-secret-value' 'n' | \
  MACCELLULAR_SETUP_TEST_LOG="$VOICE_LOG" zsh "$TEMP/配置手机访问.command" >/dev/null

grep -Fxq -- 'direct-voice' "$VOICE_LOG"
grep -Fxq -- '--turn-host' "$VOICE_LOG"
grep -Fxq -- 'turn.example.com' "$VOICE_LOG"
grep -Fxq -- '--helper-binary' "$VOICE_LOG"
grep -Fxq -- '--vapid-private-key-file' "$VOICE_LOG"
if grep -Eq -- 'tunnel-secret-value|turn-secret-value' "$VOICE_LOG"; then
  print -u2 -- 'setup assistant exposed a secret in argv'
  exit 1
fi

PREP_HOME="$TEMP/prepare-home"
FAKE_BIN="$TEMP/fake-bin"
mkdir -p "$PREP_HOME" "$FAKE_BIN"
cat > "$FAKE_BIN/curl" <<'SH'
#!/bin/sh
set -eu
output=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    output=$2
    shift 2
  else
    shift
  fi
done
[ -n "$output" ]
printf '%s\n' 'fixture' > "$output"
SH
chmod 755 "$FAKE_BIN/curl"
PATH="$FAKE_BIN:/usr/bin:/bin" HOME="$PREP_HOME" \
  sh "$ROOT/scripts/prepare-module-voice.sh" install >/dev/null
PATH="$FAKE_BIN:/usr/bin:/bin" HOME="$PREP_HOME" \
  sh "$ROOT/scripts/prepare-module-voice.sh" check >/dev/null
[[ -s "$PREP_HOME/Library/Application Support/MacCellular Runtime/ModuleVoice/qdc507_voice.ko" ]]

print -- 'MacCellular setup assistant tests: PASS'
