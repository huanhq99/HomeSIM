#!/bin/bash

set -euo pipefail
umask 077

readonly TEST_DIR="$(CDPATH= cd -P -- "$(dirname -- "$0")" && pwd)"
readonly ROOT="$(CDPATH= cd -P -- "$TEST_DIR/.." && pwd)"
readonly INSTALLER="$ROOT/install-local.sh"
readonly FIXTURES="$TEST_DIR/fixtures"

if [[ "$(/usr/bin/uname -s)" != "Darwin" ]]; then
  printf '%s\n' 'installer fixture: SKIP (macOS-only contract)'
  exit 0
fi

cloudflared_source="/opt/homebrew/opt/cloudflared/bin/cloudflared"
if [[ ! -x "$cloudflared_source" ]] || \
   ! "$cloudflared_source" --version 2>/dev/null | /usr/bin/grep -qi 'cloudflared version'; then
  printf '%s\n' 'installer fixture: SKIP (native cloudflared unavailable)'
  exit 0
fi

fixture_root="$(/usr/bin/mktemp -d "/tmp/dji.XXXXXX")"
fixture_root="$(CDPATH= cd -P -- "$fixture_root" && pwd)"
test_finished=0
cleanup() {
  local result=$?
  if (( test_finished == 0 && result == 0 )); then
    result=1
  fi
  if (( result != 0 )); then
    printf 'installer fixture failed; diagnostic root: %s\n' "$fixture_root" >&2
  fi
  if [[ ( "$fixture_root" == /tmp/dji.* || "$fixture_root" == /private/tmp/dji.* ) && -d "$fixture_root" ]]; then
    if (( result == 0 )); then
      /bin/rm -rf -- "$fixture_root"
    fi
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

fixture_home="$fixture_root/home"
fixture_tmp="$fixture_root/tmp"
legacy="$fixture_home/Library/Application Support/DJOneHub"
runtime="$fixture_home/Library/Application Support/MacCellular"
launch_agents="$fixture_home/Library/LaunchAgents"
state_dir="$fixture_root/launchctl-state"
tools_dir="$fixture_root/tools"
media_control_socket="$runtime/remote/media-control.sock"

/usr/bin/install -d -m 700 \
  "$fixture_home" "$fixture_tmp" "$state_dir" "$tools_dir" \
  "$legacy/bin" "$legacy/config" "$legacy/logs" "$legacy/data/sms-store/nested" \
  "$runtime" "$runtime/data" "$runtime/data/sms-store" "$launch_agents"

/bin/cp /usr/bin/true "$legacy/bin/djonehub-macos.arm64-cgo"
/bin/chmod 755 "$legacy/bin/djonehub-macos.arm64-cgo"
printf '%s\n' 'owner@example.com' > "$legacy/config/access-allowed-emails"
/bin/chmod 600 "$legacy/config/access-allowed-emails"
/usr/bin/awk 'BEGIN { for (i = 0; i < 160; i++) printf "B"; printf "\n" }' > "$legacy/config/cloudflared-token"
/bin/chmod 600 "$legacy/config/cloudflared-token"

printf '%s\n' 'same-message' > "$legacy/data/sms-store/common.json"
printf '%s\n' 'legacy-only-message' > "$legacy/data/sms-store/nested/from-legacy.json"
/bin/chmod 600 "$legacy/data/sms-store/common.json" "$legacy/data/sms-store/nested/from-legacy.json"
printf '%s\n' 'same-message' > "$runtime/data/sms-store/common.json"
printf '%s\n' 'installed-only-message' > "$runtime/data/sms-store/installed-only.json"
/bin/chmod 600 "$runtime/data/sms-store/common.json" "$runtime/data/sms-store/installed-only.json"

for fixture_tool in fake-launchctl.sh fake-curl.sh fake-sleep.sh; do
  /bin/cp "$FIXTURES/$fixture_tool" "$tools_dir/$fixture_tool"
  /bin/chmod 700 "$tools_dir/$fixture_tool"
done

fake_launchctl="$tools_dir/fake-launchctl.sh"
fake_curl="$tools_dir/fake-curl.sh"
fake_sleep="$tools_dir/fake-sleep.sh"
binary_help_fixture="$tools_dir/djonehub-help.txt"
printf '%s\n' \
  '  -sms-only-runtime' \
  '  -phone-relay-runtime' \
  '  -public-web-direct-voice' \
  '  -public-web-push' \
  '  -public-web-push-vapid-private-key-file' \
  '  -public-web-push-vapid-subject' \
  '  -public-web-push-subscriptions-file' \
  '  -public-web-external-voice' \
  '  -public-web-turn-host' \
  '  -public-web-turn-secret-file' \
  '  -public-web-turn-udp-port' \
  '  -public-web-turn-tls-port' \
  '  -public-web-turn-credential-ttl' \
  '  -voice-provider' \
  '  -voice-gateway-id' \
  '  -asterisk-ari-url' \
  '  -asterisk-ari-application' \
  '  -asterisk-ari-incoming-argument' \
  '  -asterisk-ari-incoming-context' \
  '  -asterisk-ari-incoming-endpoint' \
  '  -asterisk-ari-outgoing-endpoint' \
  '  -asterisk-ari-username' \
  '  -asterisk-ari-password-file' \
  '  -asterisk-recovery-ari-username' \
  '  -asterisk-recovery-ari-password-file' \
  '  -asterisk-expected-entity-id' \
  '  -asterisk-expected-version' \
  '  -asterisk-incoming-policy-id' \
  '  -sip-recovery-store' \
  '  -sip-media-udp-min' \
  '  -sip-media-udp-max' \
  '  -sip-dial-outgoing' \
  '  -sip-answer-incoming' \
  '  -sip-reject-incoming' \
  '  -sip-send-dtmf' \
  '  -sip-end-active' > "$binary_help_fixture"
/bin/chmod 600 "$binary_help_fixture"

# Model the currently installed repo-local LaunchAgents. They are marked loaded
# directly in the fake state because the fake bootstrap intentionally rejects
# any new plist that still points into Documents.
legacy_backend_plist="$launch_agents/io.maccellular.phone.plist"
legacy_tunnel_plist="$launch_agents/io.maccellular.phone.cloudflared.plist"
/usr/bin/sed \
  -e "s|__DJONEHUB_BINARY__|$legacy/bin/djonehub-macos.arm64-cgo|g" \
  -e 's|__CLOUDFLARE_TEAM_DOMAIN__|test-team.cloudflareaccess.com|g' \
  -e 's|__CLOUDFLARE_ACCESS_AUD__|aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa|g' \
  -e "s|__ALLOWED_EMAILS_FILE__|$legacy/config/access-allowed-emails|g" \
  -e "s|__LOG_DIR__|$legacy/logs|g" \
  -e "s|__SMS_STORE_DIR__|$legacy/data/sms-store|g" \
  "$ROOT/io.maccellular.phone.plist.example" > "$legacy_backend_plist"
/usr/bin/sed \
  -e "s|__CLOUDFLARED_BINARY__|$cloudflared_source|g" \
  -e "s|__CLOUDFLARED_TOKEN_FILE__|$legacy/config/cloudflared-token|g" \
  -e "s|__LOG_DIR__|$legacy/logs|g" \
  "$ROOT/io.maccellular.phone.cloudflared.plist.example" > "$legacy_tunnel_plist"
/bin/chmod 600 "$legacy_backend_plist" "$legacy_tunnel_plist"
/usr/bin/plutil -lint "$legacy_backend_plist" "$legacy_tunnel_plist" >/dev/null
printf '%s\n' "$legacy_backend_plist" > "$state_dir/io.maccellular.phone"
printf '%s\n' "$legacy_tunnel_plist" > "$state_dir/io.maccellular.phone.cloudflared"

hash_file() {
  /usr/bin/shasum -a 256 "$1" | /usr/bin/awk '{print $1}'
}

run_installer() {
  HOME="$fixture_home" \
  TMPDIR="$fixture_tmp" \
  DJONEHUB_INSTALLER_TESTING=1 \
  DJONEHUB_INSTALLER_TEST_LAUNCHCTL="$fake_launchctl" \
  DJONEHUB_INSTALLER_TEST_CURL="$fake_curl" \
  DJONEHUB_INSTALLER_TEST_SLEEP="$fake_sleep" \
  DJONEHUB_INSTALLER_TEST_BINARY_HELP_FILE="$binary_help_fixture" \
  FAKE_LAUNCHCTL_STATE_DIR="$state_dir" \
  FAKE_LAUNCHCTL_FAIL_ONCE_LABEL="${FAIL_ONCE_LABEL:-}" \
  FAKE_REMOTE_MEDIA_SOCKET="$media_control_socket" \
    "$INSTALLER" "$@"
}

invoke_installer() {
  local mode="$1"
  shift
  case " $* " in
    *" --profile direct-voice "*|*" --profile external-sip "*)
      run_installer "$mode" --non-interactive \
        --public-host phone.example.com \
        --turn-host turn.example.com \
        --team-domain test-team.cloudflareaccess.com \
        --aud aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
        --email operator@sample.test \
        --cloudflared "$cloudflared_source" \
        "$@"
      return
      ;;
  esac
  run_installer "$mode" --non-interactive \
    --public-host phone.example.com \
    --team-domain test-team.cloudflareaccess.com \
    --aud aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    --email operator@sample.test \
    --cloudflared "$cloudflared_source" \
    "$@"
}

legacy_binary_before="$(hash_file "$legacy/bin/djonehub-macos.arm64-cgo")"
legacy_allowlist_before="$(hash_file "$legacy/config/access-allowed-emails")"
legacy_token_before="$(hash_file "$legacy/config/cloudflared-token")"
legacy_sms_before="$(hash_file "$legacy/data/sms-store/common.json")"
installed_common_identity="$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/sms-store/common.json")"
legacy_backend_plist_before="$(hash_file "$legacy_backend_plist")"
legacy_tunnel_plist_before="$(hash_file "$legacy_tunnel_plist")"

dry_run_output="$fixture_root/dry-run.out"
invoke_installer check > "$dry_run_output" 2>&1
/usr/bin/grep -Fq 'CHECK PASS:' "$dry_run_output"
/usr/bin/grep -Fq 'DRY RUN ONLY:' "$dry_run_output"
/usr/bin/grep -Fq '1 missing file(s) would be copied; 1 existing file(s) matched' "$dry_run_output"
[[ ! -e "$runtime/bin" ]]
[[ ! -e "$runtime/config" ]]
[[ ! -e "$runtime/logs" ]]
[[ "$legacy_backend_plist_before" == "$(hash_file "$legacy_backend_plist")" ]]
[[ "$legacy_tunnel_plist_before" == "$(hash_file "$legacy_tunnel_plist")" ]]
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared" >/dev/null
[[ "$installed_common_identity" == "$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/sms-store/common.json")" ]]
[[ "$legacy_binary_before" == "$(hash_file "$legacy/bin/djonehub-macos.arm64-cgo")" ]]
[[ "$legacy_allowlist_before" == "$(hash_file "$legacy/config/access-allowed-emails")" ]]
[[ "$legacy_token_before" == "$(hash_file "$legacy/config/cloudflared-token")" ]]
[[ "$legacy_sms_before" == "$(hash_file "$legacy/data/sms-store/common.json")" ]]

first_apply_output="$fixture_root/first-apply.out"
invoke_installer apply > "$first_apply_output" 2>&1
/usr/bin/grep -Fq 'APPLY PASS:' "$first_apply_output"

for private_dir in "$runtime" "$runtime/bin" "$runtime/config" "$runtime/data" \
  "$runtime/data/sms-store" "$runtime/logs"; do
  [[ "$(/usr/bin/stat -f '%Lp' "$private_dir")" == "700" ]]
done
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/bin/djonehub-macos.arm64-cgo")" == "700" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/bin/cloudflared")" == "700" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/config/access-allowed-emails")" == "600" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/config/cloudflared-token")" == "600" ]]
[[ ! -e "$runtime/config/public-web-vapid-private-key" ]]
[[ ! -e "$runtime/data/push" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$launch_agents/io.maccellular.phone.plist")" == "600" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$launch_agents/io.maccellular.phone.cloudflared.plist")" == "600" ]]
[[ "$(hash_file /usr/bin/true)" == "$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")" ]]
[[ "$(hash_file "$cloudflared_source")" == "$(hash_file "$runtime/bin/cloudflared")" ]]
[[ "$(/bin/cat "$runtime/config/access-allowed-emails")" == 'operator@sample.test' ]]
[[ "$(hash_file "$legacy/config/cloudflared-token")" == "$(hash_file "$runtime/config/cloudflared-token")" ]]

[[ "$installed_common_identity" == "$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/sms-store/common.json")" ]]
[[ "$(/bin/cat "$runtime/data/sms-store/installed-only.json")" == 'installed-only-message' ]]
[[ "$(/bin/cat "$runtime/data/sms-store/nested/from-legacy.json")" == 'legacy-only-message' ]]
[[ "$legacy_sms_before" == "$(hash_file "$legacy/data/sms-store/common.json")" ]]
[[ -f "$legacy/data/sms-store/nested/from-legacy.json" ]]

backend_plist="$launch_agents/io.maccellular.phone.plist"
tunnel_plist="$launch_agents/io.maccellular.phone.cloudflared.plist"
[[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:2' "$backend_plist")" == "$runtime/bin/djonehub-macos.arm64-cgo" ]]
[[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$tunnel_plist")" == "$runtime/bin/cloudflared" ]]
[[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:7' "$tunnel_plist")" == "$runtime/config/cloudflared-token" ]]
/usr/bin/grep -Fq -- "$runtime/config/access-allowed-emails" "$backend_plist"
/usr/bin/grep -Fq -- "$runtime/data/sms-store" "$backend_plist"
/usr/bin/grep -Fq -- "$runtime/logs" "$backend_plist" "$tunnel_plist"
! /usr/bin/grep -Fq -- '-public-web-push' "$backend_plist"
! /usr/bin/grep -Fq -- "$legacy" "$backend_plist" "$tunnel_plist"
! /usr/bin/grep -Fq '/Documents/' "$backend_plist" "$tunnel_plist"

# Install a binary/config update from arbitrary source paths. The launched files
# must remain private copies under Application Support.
update_sources="$fixture_root/update-sources"
/usr/bin/install -d -m 700 "$update_sources"
/bin/cp /usr/bin/false "$update_sources/djonehub-macos.arm64-cgo"
/bin/chmod 700 "$update_sources/djonehub-macos.arm64-cgo"
printf '%s\n%s\n' 'operator@sample.test' 'backup@sample.test' > "$update_sources/access-allowed-emails"
/bin/chmod 600 "$update_sources/access-allowed-emails"
/usr/bin/awk 'BEGIN { for (i = 0; i < 160; i++) printf "C"; printf "\n" }' > "$update_sources/cloudflared-token"
/bin/chmod 600 "$update_sources/cloudflared-token"

update_output="$fixture_root/update.out"
invoke_installer apply \
  --binary "$update_sources/djonehub-macos.arm64-cgo" \
  --allowlist-file "$update_sources/access-allowed-emails" \
  --token-file "$update_sources/cloudflared-token" > "$update_output" 2>&1
/usr/bin/grep -Fq 'APPLY PASS:' "$update_output"
[[ "$(hash_file /usr/bin/false)" == "$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")" ]]
[[ "$(hash_file "$update_sources/access-allowed-emails")" == "$(hash_file "$runtime/config/access-allowed-emails")" ]]
[[ "$(hash_file "$update_sources/cloudflared-token")" == "$(hash_file "$runtime/config/cloudflared-token")" ]]

# A bare repeat apply prefers the installed production binary. The still
# present legacy binary is only a first-install fallback and must not downgrade
# the explicit update above.
installed_update_hash="$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")"
legacy_stale_hash="$(hash_file "$legacy/bin/djonehub-macos.arm64-cgo")"
[[ "$installed_update_hash" != "$legacy_stale_hash" ]]
installed_priority_output="$fixture_root/installed-priority.out"
invoke_installer apply > "$installed_priority_output" 2>&1
/usr/bin/grep -Fq 'APPLY PASS:' "$installed_priority_output"
[[ "$installed_update_hash" == "$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")" ]]

# Make every Documents/update source unavailable, then emulate a background
# reopen from the installed LaunchAgents and repeat apply from installed state.
legacy_offline="$fixture_root/legacy-preserved-offline"
updates_offline="$fixture_root/update-sources-offline"
/bin/mv "$legacy" "$legacy_offline"
/bin/mv "$update_sources" "$updates_offline"

FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" bootout "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared"
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" bootout "gui/$(/usr/bin/id -u)/io.maccellular.phone"
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" bootstrap "gui/$(/usr/bin/id -u)" "$backend_plist"
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" bootstrap "gui/$(/usr/bin/id -u)" "$tunnel_plist"
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared" >/dev/null

repeat_binary_before="$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")"
repeat_allowlist_before="$(hash_file "$runtime/config/access-allowed-emails")"
repeat_token_before="$(hash_file "$runtime/config/cloudflared-token")"
repeat_sms_before="$(hash_file "$runtime/data/sms-store/installed-only.json")"
repeat_output="$fixture_root/repeat.out"
invoke_installer apply --legacy-dir "$legacy_offline" > "$repeat_output" 2>&1
/usr/bin/grep -Fq 'APPLY PASS:' "$repeat_output"
[[ "$repeat_binary_before" == "$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")" ]]
[[ "$repeat_allowlist_before" == "$(hash_file "$runtime/config/access-allowed-emails")" ]]
[[ "$repeat_token_before" == "$(hash_file "$runtime/config/cloudflared-token")" ]]
[[ "$repeat_sms_before" == "$(hash_file "$runtime/data/sms-store/installed-only.json")" ]]

# Direct voice is an opt-in third-service profile. It requires four explicit
# sources, validates capabilities without touching the installed SMS profile in
# check mode, and never prints either private key material or the TURN secret.
voice_sources="$fixture_root/voice-sources"
/usr/bin/install -d -m 700 "$voice_sources"
/bin/cp /usr/bin/true "$voice_sources/djonehub-macos.arm64-cgo"
/bin/cp /usr/bin/true "$voice_sources/DJOneHubNotifier"
/bin/chmod 700 "$voice_sources/djonehub-macos.arm64-cgo" "$voice_sources/DJOneHubNotifier"
/usr/bin/awk 'BEGIN { for (i = 0; i < 96; i++) printf "V"; printf "\n" }' > "$voice_sources/public-web-turn-secret"
/bin/chmod 600 "$voice_sources/public-web-turn-secret"
printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE' > "$voice_sources/public-web-vapid-private-key"
/bin/chmod 600 "$voice_sources/public-web-vapid-private-key"
voice_secret_hash="$(hash_file "$voice_sources/public-web-turn-secret")"
vapid_private_key_hash="$(hash_file "$voice_sources/public-web-vapid-private-key")"

missing_voice_source_output="$fixture_root/missing-voice-source.out"
if invoke_installer check --legacy-dir "$legacy_offline" --profile direct-voice \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" > "$missing_voice_source_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted an incomplete direct-voice profile' >&2
  exit 1
fi
/usr/bin/grep -Fq 'direct-voice requires an explicit --helper-binary source' "$missing_voice_source_output"

missing_vapid_source_output="$fixture_root/missing-vapid-source.out"
if invoke_installer check --legacy-dir "$legacy_offline" --profile direct-voice \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --helper-binary "$voice_sources/DJOneHubNotifier" \
  --turn-secret-file "$voice_sources/public-web-turn-secret" > "$missing_vapid_source_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted direct-voice without a VAPID private key' >&2
  exit 1
fi
/usr/bin/grep -Fq 'direct-voice requires an explicit --vapid-private-key-file source' "$missing_vapid_source_output"

printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAE=' > "$voice_sources/vapid-padded"
printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' > "$voice_sources/vapid-zero"
printf '%s\n' '_____wAAAAD__________7zm-q2nF56E87nKwvxjJVE' > "$voice_sources/vapid-order"
/bin/chmod 600 "$voice_sources/vapid-padded" "$voice_sources/vapid-zero" "$voice_sources/vapid-order"
for invalid_vapid_name in vapid-padded vapid-zero vapid-order; do
  invalid_vapid_output="$fixture_root/$invalid_vapid_name.out"
  if invoke_installer check --legacy-dir "$legacy_offline" \
    --profile direct-voice \
    --binary "$voice_sources/djonehub-macos.arm64-cgo" \
    --helper-binary "$voice_sources/DJOneHubNotifier" \
    --turn-secret-file "$voice_sources/public-web-turn-secret" \
    --vapid-private-key-file "$voice_sources/$invalid_vapid_name" > "$invalid_vapid_output" 2>&1; then
    printf 'installer unexpectedly accepted invalid VAPID key fixture: %s\n' "$invalid_vapid_name" >&2
    exit 1
  fi
  /usr/bin/grep -Fq 'VAPID private-key file must contain one LF-terminated, unpadded base64url canonical P-256 scalar in the range 1..N-1' "$invalid_vapid_output"
  ! /usr/bin/grep -Fq -- "$(/bin/cat "$voice_sources/$invalid_vapid_name")" "$invalid_vapid_output"
done

direct_dry_run_output="$fixture_root/direct-dry-run.out"
invoke_installer check --legacy-dir "$legacy_offline" \
  --profile direct-voice \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --helper-binary "$voice_sources/DJOneHubNotifier" \
  --turn-secret-file "$voice_sources/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources/public-web-vapid-private-key" > "$direct_dry_run_output" 2>&1
/usr/bin/grep -Fq 'CHECK PASS: direct-voice profile' "$direct_dry_run_output"
/usr/bin/grep -Fq 'DRY RUN ONLY:' "$direct_dry_run_output"
! /usr/bin/grep -Fq -- "$(/bin/cat "$voice_sources/public-web-turn-secret")" "$direct_dry_run_output"
! /usr/bin/grep -Fq -- "$(/bin/cat "$voice_sources/public-web-vapid-private-key")" "$direct_dry_run_output"
[[ ! -e "$runtime/bin/DJOneHubNotifier" ]]
[[ ! -e "$runtime/config/public-web-turn-secret" ]]
[[ ! -e "$runtime/config/public-web-vapid-private-key" ]]
[[ ! -e "$runtime/data/push" ]]
[[ ! -e "$launch_agents/io.maccellular.phone.media-helper.plist" ]]

# A helper startup failure restores the exact prior two-service SMS profile and
# removes newly staged voice-only runtime files.
direct_rollback_output="$fixture_root/direct-rollback.out"
FAIL_ONCE_LABEL='io.maccellular.phone.media-helper'
if invoke_installer apply --legacy-dir "$legacy_offline" \
  --profile direct-voice \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --helper-binary "$voice_sources/DJOneHubNotifier" \
  --turn-secret-file "$voice_sources/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources/public-web-vapid-private-key" > "$direct_rollback_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted the injected media helper failure' >&2
  exit 1
fi
unset FAIL_ONCE_LABEL
/bin/rm -f -- "$state_dir/.failed-once-io.maccellular.phone.media-helper"
/usr/bin/grep -Fq 'Rollback complete:' "$direct_rollback_output"
[[ ! -e "$runtime/bin/DJOneHubNotifier" ]]
[[ ! -e "$runtime/config/public-web-turn-secret" ]]
[[ ! -e "$runtime/config/public-web-vapid-private-key" ]]
[[ -d "$runtime/data/push" && ! -L "$runtime/data/push" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/data/push")" == "700" ]]
[[ ! -e "$launch_agents/io.maccellular.phone.media-helper.plist" ]]
! FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.media-helper" >/dev/null 2>&1
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared" >/dev/null

# The subscriptions database is runtime-owned forward data. Once present, no
# later successful install, failed install, rollback, or SMS switch may replace
# or delete it.
printf '%s\n' '{"version":1,"subscriptions":[]}' > "$runtime/data/push/subscriptions.json"
/bin/chmod 600 "$runtime/data/push/subscriptions.json"
push_subscriptions_hash="$(hash_file "$runtime/data/push/subscriptions.json")"
push_subscriptions_identity="$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/push/subscriptions.json")"

direct_data_rollback_output="$fixture_root/direct-data-rollback.out"
FAIL_ONCE_LABEL='io.maccellular.phone.media-helper'
if invoke_installer apply --legacy-dir "$legacy_offline" \
  --profile direct-voice \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --helper-binary "$voice_sources/DJOneHubNotifier" \
  --turn-secret-file "$voice_sources/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources/public-web-vapid-private-key" > "$direct_data_rollback_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted the second injected media helper failure' >&2
  exit 1
fi
unset FAIL_ONCE_LABEL
/bin/rm -f -- "$state_dir/.failed-once-io.maccellular.phone.media-helper"
/usr/bin/grep -Fq 'Rollback complete:' "$direct_data_rollback_output"
[[ ! -e "$runtime/config/public-web-vapid-private-key" ]]
[[ "$push_subscriptions_hash" == "$(hash_file "$runtime/data/push/subscriptions.json")" ]]
[[ "$push_subscriptions_identity" == "$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/push/subscriptions.json")" ]]

: > "$state_dir/operations.log"
direct_apply_output="$fixture_root/direct-apply.out"
if ! invoke_installer apply --legacy-dir "$legacy_offline" \
  --profile direct-voice \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --helper-binary "$voice_sources/DJOneHubNotifier" \
  --turn-secret-file "$voice_sources/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources/public-web-vapid-private-key" > "$direct_apply_output" 2>&1; then
  /bin/cat "$direct_apply_output" >&2
  exit 1
fi
/usr/bin/grep -Fq 'APPLY PASS:' "$direct_apply_output"
/usr/bin/grep -Fq 'profile: direct-voice' "$direct_apply_output"
expected_direct_operations="$(printf '%s\n' \
  'bootout io.maccellular.phone.cloudflared' \
  'bootout io.maccellular.phone.media-helper' \
  'bootout io.maccellular.phone' \
  'bootstrap io.maccellular.phone' \
  'bootstrap io.maccellular.phone.media-helper' \
  'bootstrap io.maccellular.phone.cloudflared')"
[[ "$(/bin/cat "$state_dir/operations.log")" == "$expected_direct_operations" ]]

helper_plist="$launch_agents/io.maccellular.phone.media-helper.plist"
[[ "$voice_secret_hash" == "$(hash_file "$runtime/config/public-web-turn-secret")" ]]
[[ "$vapid_private_key_hash" == "$(hash_file "$runtime/config/public-web-vapid-private-key")" ]]
[[ "$(hash_file /usr/bin/true)" == "$(hash_file "$runtime/bin/DJOneHubNotifier")" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/bin/DJOneHubNotifier")" == "700" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/config/public-web-turn-secret")" == "600" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/config/public-web-vapid-private-key")" == "600" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/data/push")" == "700" ]]
[[ "$push_subscriptions_hash" == "$(hash_file "$runtime/data/push/subscriptions.json")" ]]
[[ "$push_subscriptions_identity" == "$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/push/subscriptions.json")" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$helper_plist")" == "600" ]]
[[ -S "$media_control_socket" && ! -L "$media_control_socket" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$media_control_socket")" == "600" ]]
/usr/bin/grep -Fq -- '-phone-relay-runtime' "$backend_plist"
/usr/bin/grep -Fq -- '-public-web-direct-voice' "$backend_plist"
/usr/bin/grep -Fq -- "$runtime/config/public-web-turn-secret" "$backend_plist"
/usr/bin/grep -Fq -- '-public-web-push' "$backend_plist"
/usr/bin/grep -Fq -- "$runtime/config/public-web-vapid-private-key" "$backend_plist"
/usr/bin/grep -Fq -- '-public-web-push-vapid-subject' "$backend_plist"
/usr/bin/grep -Fq -- 'https://phone.example.com' "$backend_plist"
/usr/bin/grep -Fq -- "$runtime/data/push/subscriptions.json" "$backend_plist"
! /usr/bin/grep -Fq -- '-sms-only-runtime' "$backend_plist"
[[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$helper_plist")" == "$runtime/bin/DJOneHubNotifier" ]]
[[ "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:1' "$helper_plist")" == '--remote-media-helper' ]]
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.media-helper" >/dev/null

# Replacing an installed VAPID key is part of the same service transaction. A
# late Tunnel failure must restore the prior key byte-for-byte while leaving
# runtime-owned subscriptions untouched.
printf '%s\n' '_____wAAAAD__________7zm-q2nF56E87nKwvxjJVA' > "$voice_sources/public-web-vapid-private-key-next"
/bin/chmod 600 "$voice_sources/public-web-vapid-private-key-next"
direct_key_rollback_output="$fixture_root/direct-key-rollback.out"
FAIL_ONCE_LABEL='io.maccellular.phone.cloudflared'
if invoke_installer apply --legacy-dir "$legacy_offline" \
  --profile direct-voice \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --helper-binary "$voice_sources/DJOneHubNotifier" \
  --turn-secret-file "$voice_sources/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources/public-web-vapid-private-key-next" > "$direct_key_rollback_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted the injected Tunnel failure during VAPID replacement' >&2
  exit 1
fi
unset FAIL_ONCE_LABEL
/bin/rm -f -- "$state_dir/.failed-once-io.maccellular.phone.cloudflared"
/usr/bin/grep -Fq 'Rollback complete:' "$direct_key_rollback_output"
! /usr/bin/grep -Fq -- "$(/bin/cat "$voice_sources/public-web-vapid-private-key-next")" "$direct_key_rollback_output"
[[ "$vapid_private_key_hash" == "$(hash_file "$runtime/config/public-web-vapid-private-key")" ]]
[[ "$push_subscriptions_hash" == "$(hash_file "$runtime/data/push/subscriptions.json")" ]]
[[ "$push_subscriptions_identity" == "$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/push/subscriptions.json")" ]]
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.media-helper" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared" >/dev/null

# A failed direct-voice -> SMS switch restores the helper, secret, three plists,
# socket, and loaded state. A successful default SMS apply then removes only the
# voice LaunchAgent/activation while retaining private update sources.
voice_backend_hash="$(hash_file "$backend_plist")"
voice_helper_plist_hash="$(hash_file "$helper_plist")"
voice_tunnel_hash="$(hash_file "$tunnel_plist")"
voice_helper_hash="$(hash_file "$runtime/bin/DJOneHubNotifier")"
voice_installed_secret_hash="$(hash_file "$runtime/config/public-web-turn-secret")"
voice_installed_vapid_hash="$(hash_file "$runtime/config/public-web-vapid-private-key")"
voice_to_sms_rollback_output="$fixture_root/voice-to-sms-rollback.out"
FAIL_ONCE_LABEL='io.maccellular.phone.cloudflared'
if invoke_installer apply --legacy-dir "$legacy_offline" > "$voice_to_sms_rollback_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted the injected SMS switch failure' >&2
  exit 1
fi
unset FAIL_ONCE_LABEL
/bin/rm -f -- "$state_dir/.failed-once-io.maccellular.phone.cloudflared"
/usr/bin/grep -Fq 'Rollback complete:' "$voice_to_sms_rollback_output"
[[ "$voice_backend_hash" == "$(hash_file "$backend_plist")" ]]
[[ "$voice_helper_plist_hash" == "$(hash_file "$helper_plist")" ]]
[[ "$voice_tunnel_hash" == "$(hash_file "$tunnel_plist")" ]]
[[ "$voice_helper_hash" == "$(hash_file "$runtime/bin/DJOneHubNotifier")" ]]
[[ "$voice_installed_secret_hash" == "$(hash_file "$runtime/config/public-web-turn-secret")" ]]
[[ "$voice_installed_vapid_hash" == "$(hash_file "$runtime/config/public-web-vapid-private-key")" ]]
[[ "$push_subscriptions_hash" == "$(hash_file "$runtime/data/push/subscriptions.json")" ]]
[[ "$push_subscriptions_identity" == "$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/push/subscriptions.json")" ]]
[[ -S "$media_control_socket" ]]
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.media-helper" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared" >/dev/null

sms_switch_output="$fixture_root/sms-switch.out"
invoke_installer apply --legacy-dir "$legacy_offline" > "$sms_switch_output" 2>&1
/usr/bin/grep -Fq 'profile: sms' "$sms_switch_output"
/usr/bin/grep -Fq -- '-sms-only-runtime' "$backend_plist"
! /usr/bin/grep -Fq -- '-public-web-direct-voice' "$backend_plist"
! /usr/bin/grep -Fq -- '-public-web-push' "$backend_plist"
[[ ! -e "$helper_plist" ]]
[[ ! -e "$media_control_socket" ]]
[[ "$voice_helper_hash" == "$(hash_file "$runtime/bin/DJOneHubNotifier")" ]]
[[ "$voice_installed_secret_hash" == "$(hash_file "$runtime/config/public-web-turn-secret")" ]]
[[ "$voice_installed_vapid_hash" == "$(hash_file "$runtime/config/public-web-vapid-private-key")" ]]
[[ "$push_subscriptions_hash" == "$(hash_file "$runtime/data/push/subscriptions.json")" ]]
[[ "$push_subscriptions_identity" == "$(/usr/bin/stat -f '%i:%m:%z' "$runtime/data/push/subscriptions.json")" ]]
! FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.media-helper" >/dev/null 2>&1
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared" >/dev/null

# External SIP is a separate two-service profile. Its Asterisk deployment and
# both ARI credentials are staged and installed as private runtime files; the
# QDC helper remains absent. This exercises the path that is distinct from the
# direct-QDC voice fixture above.
external_sources="$fixture_root/external-sources"
external_asterisk="$external_sources/asterisk"
external_secrets="$external_asterisk/secrets"
voice_sources_physical="$(CDPATH= cd -P -- "$voice_sources" && pwd)"
/usr/bin/install -d -m 700 "$external_sources" "$external_asterisk" "$external_secrets"
external_sources_physical="$(CDPATH= cd -P -- "$external_sources" && pwd)"
external_asterisk_physical="$external_sources_physical/asterisk"
external_secrets_physical="$external_asterisk_physical/secrets"
printf '%s\n' \
  'MODE=production' \
  'ASTERISK_VERSION=22.10.1' \
  'ASTERISK_SHA256=0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353' \
  'ASTERISK_IMAGE_REF=maccellular-asterisk@sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51' \
  'ENTITY_ID=a2:11:22:33:44:55' \
  'ARI_PORT=18088' \
  'SIP_PORT=5060' \
  'RTP_MIN=10000' \
  'RTP_MAX=10031' \
  'MAX_CALL_SECONDS=900' \
  'APPLICATION=djonehub' \
  'ARGUMENT=incoming' \
  'POLICY_ID=djonehub-incoming-v1' \
  'CONTEXT=djonehub-incoming' \
  'ENDPOINT=gateway' \
  'CONTROL_USER=control' \
  'INSPECT_USER=inspect' \
  'GATEWAY_IP=192.168.250.2' \
  'SIP_BIND_IP=192.168.250.1' \
  'LAN_CIDR=192.168.250.0/30' \
  'PBX_CONTAINER_SUBNET=172.30.247.0/28' \
  'PBX_CONTAINER_IPV4=172.30.247.2' > "$external_asterisk/deployment.env"
printf '%s\n' 'control-password-fixture' > "$external_secrets/ari-control.password"
printf '%s\n' 'inspect-password-fixture' > "$external_secrets/ari-inspect.password"
/bin/chmod 600 "$external_asterisk/deployment.env" "$external_secrets/ari-control.password" "$external_secrets/ari-inspect.password"

external_recovery_store="$runtime/data/external-voice/recovery.json"
external_check_output="$fixture_root/external-check.out"
if ! invoke_installer check --legacy-dir "$legacy_offline" \
  --profile external-sip \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --turn-secret-file "$voice_sources_physical/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources_physical/public-web-vapid-private-key" \
  --voice-gateway-id sc211 \
  --asterisk-deployment-env "$external_asterisk_physical/deployment.env" \
  --asterisk-ari-password-file "$external_secrets_physical/ari-control.password" \
  --asterisk-recovery-ari-password-file "$external_secrets_physical/ari-inspect.password" \
  --sip-recovery-store "$external_recovery_store" \
  --sip-media-udp-min 55000 \
  --sip-media-udp-max 55063 \
  --asterisk-ari-outgoing-endpoint gateway-out \
  --sip-dial-outgoing > "$external_check_output" 2>&1; then
  /bin/cat "$external_check_output" >&2
  exit 1
fi
/usr/bin/grep -Fq 'CHECK PASS: external-sip profile' "$external_check_output"
/usr/bin/grep -Fq 'QDC media helper: disabled' "$external_check_output"
[[ ! -e "$runtime/config/asterisk-deployment.env" ]]
[[ ! -e "$runtime/config/asterisk-ari-control.password" ]]
[[ ! -e "$runtime/config/asterisk-ari-inspect.password" ]]

external_apply_output="$fixture_root/external-apply.out"
if ! invoke_installer apply --legacy-dir "$legacy_offline" \
  --profile external-sip \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --turn-secret-file "$voice_sources_physical/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources_physical/public-web-vapid-private-key" \
  --voice-gateway-id sc211 \
  --asterisk-deployment-env "$external_asterisk_physical/deployment.env" \
  --asterisk-ari-password-file "$external_secrets_physical/ari-control.password" \
  --asterisk-recovery-ari-password-file "$external_secrets_physical/ari-inspect.password" \
  --sip-recovery-store "$external_recovery_store" \
  --sip-media-udp-min 55000 \
  --sip-media-udp-max 55063 \
  --asterisk-ari-outgoing-endpoint gateway-out \
  --sip-dial-outgoing > "$external_apply_output" 2>&1; then
  /bin/cat "$external_apply_output" >&2
  exit 1
fi
/usr/bin/grep -Fq 'APPLY PASS:' "$external_apply_output"
/usr/bin/grep -Fq 'profile: external-sip' "$external_apply_output"
[[ -f "$runtime/config/asterisk-deployment.env" ]]
[[ -f "$runtime/config/asterisk-ari-control.password" ]]
[[ -f "$runtime/config/asterisk-ari-inspect.password" ]]
[[ "$(hash_file "$external_asterisk_physical/deployment.env")" == "$(hash_file "$runtime/config/asterisk-deployment.env")" ]]
[[ "$(hash_file "$external_secrets_physical/ari-control.password")" == "$(hash_file "$runtime/config/asterisk-ari-control.password")" ]]
[[ "$(hash_file "$external_secrets_physical/ari-inspect.password")" == "$(hash_file "$runtime/config/asterisk-ari-inspect.password")" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/config/asterisk-deployment.env")" == "600" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/config/asterisk-ari-control.password")" == "600" ]]
[[ "$(/usr/bin/stat -f '%Lp' "$runtime/config/asterisk-ari-inspect.password")" == "600" ]]
[[ ! -e "$helper_plist" ]]
! /usr/bin/grep -Fq -- '-public-web-direct-voice' "$backend_plist"
/usr/bin/grep -Fq -- '-public-web-external-voice' "$backend_plist"
/usr/bin/grep -Fq -- '-voice-provider' "$backend_plist"
/usr/bin/grep -Fq -- '-asterisk-ari-outgoing-endpoint' "$backend_plist"
/usr/bin/grep -Fq -- 'gateway-out' "$backend_plist"
/usr/bin/grep -Fq -- '-sip-dial-outgoing=1' "$backend_plist"
/usr/bin/grep -Fq -- "$runtime/config/asterisk-ari-control.password" "$backend_plist"
/usr/bin/grep -Fq -- "$runtime/config/asterisk-ari-inspect.password" "$backend_plist"

# A late Tunnel failure must restore the complete external-SIP runtime files
# byte-for-byte while leaving Web Push forward data untouched.
external_deployment_hash="$(hash_file "$runtime/config/asterisk-deployment.env")"
external_control_hash="$(hash_file "$runtime/config/asterisk-ari-control.password")"
external_inspect_hash="$(hash_file "$runtime/config/asterisk-ari-inspect.password")"
external_turn_hash="$(hash_file "$runtime/config/public-web-turn-secret")"
external_vapid_hash="$(hash_file "$runtime/config/public-web-vapid-private-key")"
external_rollback_output="$fixture_root/external-rollback.out"
FAIL_ONCE_LABEL='io.maccellular.phone.cloudflared'
if invoke_installer apply --legacy-dir "$legacy_offline" \
  --profile external-sip \
  --binary "$voice_sources/djonehub-macos.arm64-cgo" \
  --turn-secret-file "$voice_sources_physical/public-web-turn-secret" \
  --vapid-private-key-file "$voice_sources_physical/public-web-vapid-private-key" \
  --voice-gateway-id sc211 \
  --asterisk-deployment-env "$external_asterisk_physical/deployment.env" \
  --asterisk-ari-password-file "$external_secrets_physical/ari-control.password" \
  --asterisk-recovery-ari-password-file "$external_secrets_physical/ari-inspect.password" \
  --sip-recovery-store "$external_recovery_store" \
  --sip-media-udp-min 55000 \
  --sip-media-udp-max 55063 \
  --asterisk-ari-outgoing-endpoint gateway-out \
  --sip-dial-outgoing > "$external_rollback_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted the injected external-sip Tunnel failure' >&2
  exit 1
fi
unset FAIL_ONCE_LABEL
/bin/rm -f -- "$state_dir/.failed-once-io.maccellular.phone.cloudflared"
/usr/bin/grep -Fq 'Rollback complete:' "$external_rollback_output"
[[ "$external_deployment_hash" == "$(hash_file "$runtime/config/asterisk-deployment.env")" ]]
[[ "$external_control_hash" == "$(hash_file "$runtime/config/asterisk-ari-control.password")" ]]
[[ "$external_inspect_hash" == "$(hash_file "$runtime/config/asterisk-ari-inspect.password")" ]]
[[ "$external_turn_hash" == "$(hash_file "$runtime/config/public-web-turn-secret")" ]]
[[ "$external_vapid_hash" == "$(hash_file "$runtime/config/public-web-vapid-private-key")" ]]

# Return to the default SMS profile after the external-SIP transaction. The
# external credentials are retained as private update sources, while the live
# plist returns to SMS-only and no voice helper is loaded.
external_to_sms_output="$fixture_root/external-to-sms.out"
invoke_installer apply --legacy-dir "$legacy_offline" > "$external_to_sms_output" 2>&1
/usr/bin/grep -Fq 'profile: sms' "$external_to_sms_output"
/usr/bin/grep -Fq -- '-sms-only-runtime' "$backend_plist"
! /usr/bin/grep -Fq -- '-public-web-external-voice' "$backend_plist"
[[ -f "$runtime/config/asterisk-deployment.env" ]]
[[ ! -e "$helper_plist" ]]

# Force the second service bootstrap to fail once. Runtime files and plist state
# must roll back byte-for-byte. SMS migration is forward-only and both copies
# must survive the rollback.
rollback_sources="$fixture_root/rollback-sources"
/usr/bin/install -d -m 700 "$rollback_sources"
/bin/cp /usr/bin/true "$rollback_sources/djonehub-macos.arm64-cgo"
/bin/chmod 700 "$rollback_sources/djonehub-macos.arm64-cgo"
printf '%s\n' 'operator@sample.test' > "$rollback_sources/access-allowed-emails"
/bin/chmod 600 "$rollback_sources/access-allowed-emails"
/usr/bin/awk 'BEGIN { for (i = 0; i < 160; i++) printf "D"; printf "\n" }' > "$rollback_sources/cloudflared-token"
/bin/chmod 600 "$rollback_sources/cloudflared-token"
printf '%s\n' 'copied-before-service-failure' > "$legacy_offline/data/sms-store/forward-only.json"
/bin/chmod 600 "$legacy_offline/data/sms-store/forward-only.json"

baseline_binary="$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")"
baseline_cloudflared="$(hash_file "$runtime/bin/cloudflared")"
baseline_allowlist="$(hash_file "$runtime/config/access-allowed-emails")"
baseline_token="$(hash_file "$runtime/config/cloudflared-token")"
baseline_backend_plist="$(hash_file "$backend_plist")"
baseline_tunnel_plist="$(hash_file "$tunnel_plist")"
forward_source_hash="$(hash_file "$legacy_offline/data/sms-store/forward-only.json")"

rollback_output="$fixture_root/rollback.out"
FAIL_ONCE_LABEL='io.maccellular.phone.cloudflared'
if invoke_installer apply \
  --legacy-dir "$legacy_offline" \
  --binary "$rollback_sources/djonehub-macos.arm64-cgo" \
  --allowlist-file "$rollback_sources/access-allowed-emails" \
  --token-file "$rollback_sources/cloudflared-token" > "$rollback_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted the injected service failure' >&2
  exit 1
fi
unset FAIL_ONCE_LABEL
/usr/bin/grep -Fq 'APPLY FAILED:' "$rollback_output"
/usr/bin/grep -Fq 'Rollback complete:' "$rollback_output"
[[ "$baseline_binary" == "$(hash_file "$runtime/bin/djonehub-macos.arm64-cgo")" ]]
[[ "$baseline_cloudflared" == "$(hash_file "$runtime/bin/cloudflared")" ]]
[[ "$baseline_allowlist" == "$(hash_file "$runtime/config/access-allowed-emails")" ]]
[[ "$baseline_token" == "$(hash_file "$runtime/config/cloudflared-token")" ]]
[[ "$baseline_backend_plist" == "$(hash_file "$backend_plist")" ]]
[[ "$baseline_tunnel_plist" == "$(hash_file "$tunnel_plist")" ]]
[[ "$forward_source_hash" == "$(hash_file "$legacy_offline/data/sms-store/forward-only.json")" ]]
[[ "$forward_source_hash" == "$(hash_file "$runtime/data/sms-store/forward-only.json")" ]]
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone" >/dev/null
FAKE_LAUNCHCTL_STATE_DIR="$state_dir" "$fake_launchctl" print "gui/$(/usr/bin/id -u)/io.maccellular.phone.cloudflared" >/dev/null

# Different bytes at the same relative SMS path are a hard preflight failure;
# neither side may be overwritten.
printf '%s\n' 'source-conflict' > "$legacy_offline/data/sms-store/conflict.json"
printf '%s\n' 'destination-conflict' > "$runtime/data/sms-store/conflict.json"
/bin/chmod 600 "$legacy_offline/data/sms-store/conflict.json" "$runtime/data/sms-store/conflict.json"
source_conflict_hash="$(hash_file "$legacy_offline/data/sms-store/conflict.json")"
destination_conflict_hash="$(hash_file "$runtime/data/sms-store/conflict.json")"
conflict_output="$fixture_root/conflict.out"
if invoke_installer check --legacy-dir "$legacy_offline" > "$conflict_output" 2>&1; then
  printf '%s\n' 'installer unexpectedly accepted conflicting SMS data' >&2
  exit 1
fi
/usr/bin/grep -Fq 'SMS store conflict; refusing to overwrite' "$conflict_output"
[[ "$source_conflict_hash" == "$(hash_file "$legacy_offline/data/sms-store/conflict.json")" ]]
[[ "$destination_conflict_hash" == "$(hash_file "$runtime/data/sms-store/conflict.json")" ]]

test_finished=1
printf '%s\n' 'installer fixture: PASS'
