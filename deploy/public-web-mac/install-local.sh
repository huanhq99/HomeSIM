#!/bin/bash

# User-scoped installer for MacCellular on the home Mac.
# The default mode is a dry-run. Only the explicit "apply" mode mutates state.

set +x
set -euo pipefail
umask 077

readonly BACKEND_LABEL="io.maccellular.phone"
readonly HELPER_LABEL="io.maccellular.phone.media-helper"
readonly TUNNEL_LABEL="io.maccellular.phone.cloudflared"
readonly SCRIPT_DIR="$(CDPATH= cd -P -- "$(dirname -- "$0")" && pwd)"
readonly REPO_ROOT="$(CDPATH= cd -P -- "$SCRIPT_DIR/../.." && pwd)"
readonly TEMPLATE_BACKEND="$SCRIPT_DIR/io.maccellular.phone.plist.example"
readonly TEMPLATE_DIRECT_BACKEND="$SCRIPT_DIR/io.maccellular.phone.direct-voice.plist.example"
readonly TEMPLATE_EXTERNAL_BACKEND="$SCRIPT_DIR/io.maccellular.phone.external-sip.plist.example"
readonly TEMPLATE_HELPER="$SCRIPT_DIR/io.maccellular.phone.media-helper.plist.example"
readonly TEMPLATE_TUNNEL="$SCRIPT_DIR/io.maccellular.phone.cloudflared.plist.example"
readonly USER_UID="$(/usr/bin/id -u)"
readonly LAUNCH_DOMAIN="gui/$USER_UID"

MODE="check"
MODE_SET=0
PROFILE="sms"
NON_INTERACTIVE=0
PUBLIC_HOST=""
TURN_HOST=""
TEAM_DOMAIN=""
AUDIENCE=""
EXPECTED_EMAIL=""
LEGACY_DIR="$HOME/Library/Application Support/DJOneHub"
if [[ ! -d "$LEGACY_DIR" ]]; then
  LEGACY_DIR="$REPO_ROOT/local/public-web-mac"
fi
BINARY_SOURCE_OPTION=""
CLOUDFLARED_SOURCE_OPTION=""
ALLOWLIST_SOURCE_OPTION=""
TOKEN_SOURCE_OPTION=""
HELPER_SOURCE_OPTION=""
TURN_SECRET_SOURCE_OPTION=""
VAPID_PRIVATE_KEY_SOURCE_OPTION=""
ASTERISK_DEPLOYMENT_ENV_SOURCE_OPTION=""
ASTERISK_CONTROL_PASSWORD_SOURCE_OPTION=""
ASTERISK_INSPECT_PASSWORD_SOURCE_OPTION=""
VOICE_GATEWAY_ID=""
SIP_RECOVERY_STORE_OPTION=""
SIP_MEDIA_UDP_MIN=""
SIP_MEDIA_UDP_MAX=""
ASTERISK_OUTGOING_ENDPOINT=""
SIP_DIAL_OUTGOING=0
PBX_VERSION=""
PBX_ENTITY_ID=""
PBX_ARI_PORT=""
PBX_SIP_PORT=""
PBX_RTP_MIN=""
PBX_RTP_MAX=""
PBX_APPLICATION=""
PBX_ARGUMENT=""
PBX_POLICY_ID=""
PBX_CONTEXT=""
PBX_ENDPOINT=""
PBX_CONTROL_USER=""
PBX_INSPECT_USER=""

# Production paths are deliberately independent from the repository. Tests use
# a temporary HOME, so these exact defaults are exercised without touching the
# real user's Library directory.
readonly RUNTIME_DIR="$HOME/Library/Application Support/MacCellular"
readonly BIN_DIR="$RUNTIME_DIR/bin"
readonly CONFIG_DIR="$RUNTIME_DIR/config"
readonly DATA_DIR="$RUNTIME_DIR/data"
readonly LOG_DIR="$RUNTIME_DIR/logs"
readonly SMS_STORE_DIR="$DATA_DIR/sms-store"
readonly PUSH_DATA_DIR="$DATA_DIR/push"
readonly PUSH_SUBSCRIPTIONS_FILE="$PUSH_DATA_DIR/subscriptions.json"
readonly EXTERNAL_VOICE_DATA_DIR="$DATA_DIR/external-voice"
readonly INSTALLED_DJONEHUB_BINARY="$BIN_DIR/djonehub-macos.arm64-cgo"
readonly INSTALLED_MEDIA_HELPER_BINARY="$BIN_DIR/DJOneHubNotifier"
readonly INSTALLED_CLOUDFLARED_BINARY="$BIN_DIR/cloudflared"
readonly INSTALLED_ALLOWLIST_FILE="$CONFIG_DIR/access-allowed-emails"
readonly INSTALLED_TOKEN_FILE="$CONFIG_DIR/cloudflared-token"
readonly INSTALLED_TURN_SECRET_FILE="$CONFIG_DIR/public-web-turn-secret"
readonly INSTALLED_VAPID_PRIVATE_KEY_FILE="$CONFIG_DIR/public-web-vapid-private-key"
readonly INSTALLED_ASTERISK_DEPLOYMENT_ENV="$CONFIG_DIR/asterisk-deployment.env"
readonly INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE="$CONFIG_DIR/asterisk-ari-control.password"
readonly INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE="$CONFIG_DIR/asterisk-ari-inspect.password"
readonly MEDIA_CONTROL_SOCKET="$RUNTIME_DIR/remote/media-control.sock"
readonly LAUNCH_AGENTS_DIR="$HOME/Library/LaunchAgents"

STAGE_DIR=""
ALLOWLIST_ACTION=""
TOKEN_ACTION=""
BINARY_ACTION=""
CLOUDFLARED_ACTION=""
HELPER_ACTION=""
TURN_SECRET_ACTION=""
VAPID_PRIVATE_KEY_ACTION=""
ASTERISK_DEPLOYMENT_ENV_ACTION=""
ASTERISK_CONTROL_PASSWORD_ACTION=""
ASTERISK_INSPECT_PASSWORD_ACTION=""
SMS_COPY_COUNT=0
SMS_MATCH_COUNT=0

usage() {
  /bin/cat <<'EOF'
Usage:
  ./deploy/public-web-mac/install-local.sh [check]
  ./deploy/public-web-mac/install-local.sh apply

Options:
  --profile PROFILE      sms (default), direct-voice, or external-sip
  --public-host HOST     public PWA hostname, for example phone.example.com
  --turn-host HOST       coturn hostname for a voice profile
  --team-domain DOMAIN   Cloudflare Access team domain
  --aud AUD              Cloudflare Access application AUD tag
  --email EMAIL          exact email allowed by the Access policy
  --binary PATH          MacCellular arm64 backend update source
  --helper-binary PATH   MacCellular arm64 audio helper source (direct-voice only)
  --cloudflared PATH     native cloudflared update source
  --allowlist-file PATH  mode-0600 allowlist update source
  --token-file PATH      mode-0600 Tunnel token update source; never the token itself
  --turn-secret-file PATH
                         mode-0600 coturn REST shared-secret source (voice profiles only)
  --vapid-private-key-file PATH
                         mode-0600 unpadded base64url P-256 private scalar
                         (voice profiles only)
  --voice-gateway-id ID  stable external SIP gateway ID (external-sip only)
  --asterisk-deployment-env PATH
                         mode-0600 production deployment.env (external-sip only)
  --asterisk-ari-password-file PATH
                         mode-0600 ARI control password source (external-sip only)
  --asterisk-recovery-ari-password-file PATH
                         mode-0600 read-only ARI password source (external-sip only)
  --sip-recovery-store PATH
                         exact private recovery.json path (external-sip only)
  --sip-media-udp-min PORT
  --sip-media-udp-max PORT
                         dedicated bounded Pion UDP range (external-sip only)
  --asterisk-ari-outgoing-endpoint ID
                         explicit PJSIP endpoint for browser outbound dialing
                         (external-sip only; requires --sip-dial-outgoing)
  --sip-dial-outgoing   enable browser outbound dialing through that endpoint
                         (external-sip only; default off)
  --legacy-dir PATH      old runtime to migrate (default: previous DJOneHub runtime when present,
                         otherwise repo/local/public-web-mac)
  --non-interactive      fail instead of prompting for missing values/token
  -h, --help             show this help

Installed runtime (fixed):
  ~/Library/Application Support/MacCellular/{bin,config,data,logs}

The repository is only an update/migration source. LaunchAgents never execute
or read mutable runtime files from Documents. The script never accepts a
Tunnel token in argv or an environment variable. When no token file exists it
reads one hidden value from the terminal.

The default sms profile keeps the existing two-service SMS deployment. The
direct-voice profile is opt-in and requires explicit --binary,
--helper-binary, --turn-secret-file, and --vapid-private-key-file sources on
every run.

The external-sip profile is a separate public external voice v2 deployment. It
requires every external SIP source/value above, keeps SMS polling, and never
installs or starts the QDC media helper.
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

note() {
  printf '%s\n' "$*"
}

need_value() {
  [[ $# -ge 2 && -n "$2" ]] || die "$1 requires a value"
}

set_mode() {
  local requested="$1"
  if (( MODE_SET == 1 )) && [[ "$MODE" != "$requested" ]]; then
    die "choose exactly one mode: check or apply"
  fi
  MODE="$requested"
  MODE_SET=1
}

while (( $# > 0 )); do
  case "$1" in
    check|--check)
      set_mode "check"
      shift
      ;;
    apply|--apply)
      set_mode "apply"
      shift
      ;;
    --profile)
      need_value "$1" "${2:-}"
      PROFILE="$2"
      shift 2
      ;;
    --public-host)
      need_value "$1" "${2:-}"
      PUBLIC_HOST="$2"
      shift 2
      ;;
    --turn-host)
      need_value "$1" "${2:-}"
      TURN_HOST="$2"
      shift 2
      ;;
    --team-domain)
      need_value "$1" "${2:-}"
      TEAM_DOMAIN="$2"
      shift 2
      ;;
    --aud)
      need_value "$1" "${2:-}"
      AUDIENCE="$2"
      shift 2
      ;;
    --email)
      need_value "$1" "${2:-}"
      EXPECTED_EMAIL="$2"
      shift 2
      ;;
    --binary)
      need_value "$1" "${2:-}"
      BINARY_SOURCE_OPTION="$2"
      shift 2
      ;;
    --helper-binary)
      need_value "$1" "${2:-}"
      HELPER_SOURCE_OPTION="$2"
      shift 2
      ;;
    --cloudflared)
      need_value "$1" "${2:-}"
      CLOUDFLARED_SOURCE_OPTION="$2"
      shift 2
      ;;
    --allowlist-file)
      need_value "$1" "${2:-}"
      ALLOWLIST_SOURCE_OPTION="$2"
      shift 2
      ;;
    --token-file)
      need_value "$1" "${2:-}"
      TOKEN_SOURCE_OPTION="$2"
      shift 2
      ;;
    --turn-secret-file)
      need_value "$1" "${2:-}"
      TURN_SECRET_SOURCE_OPTION="$2"
      shift 2
      ;;
    --vapid-private-key-file)
      need_value "$1" "${2:-}"
      VAPID_PRIVATE_KEY_SOURCE_OPTION="$2"
      shift 2
      ;;
    --voice-gateway-id)
      need_value "$1" "${2:-}"
      VOICE_GATEWAY_ID="$2"
      shift 2
      ;;
    --asterisk-deployment-env)
      need_value "$1" "${2:-}"
      ASTERISK_DEPLOYMENT_ENV_SOURCE_OPTION="$2"
      shift 2
      ;;
    --asterisk-ari-password-file)
      need_value "$1" "${2:-}"
      ASTERISK_CONTROL_PASSWORD_SOURCE_OPTION="$2"
      shift 2
      ;;
    --asterisk-recovery-ari-password-file)
      need_value "$1" "${2:-}"
      ASTERISK_INSPECT_PASSWORD_SOURCE_OPTION="$2"
      shift 2
      ;;
    --sip-recovery-store)
      need_value "$1" "${2:-}"
      SIP_RECOVERY_STORE_OPTION="$2"
      shift 2
      ;;
    --sip-media-udp-min)
      need_value "$1" "${2:-}"
      SIP_MEDIA_UDP_MIN="$2"
      shift 2
      ;;
    --sip-media-udp-max)
      need_value "$1" "${2:-}"
      SIP_MEDIA_UDP_MAX="$2"
      shift 2
      ;;
    --asterisk-ari-outgoing-endpoint)
      need_value "$1" "${2:-}"
      ASTERISK_OUTGOING_ENDPOINT="$2"
      shift 2
      ;;
    --sip-dial-outgoing)
      SIP_DIAL_OUTGOING=1
      shift
      ;;
    --legacy-dir)
      need_value "$1" "${2:-}"
      LEGACY_DIR="$2"
      shift 2
      ;;
    --base-dir|--runtime-dir)
      die "$1 is no longer supported; the production runtime is fixed under ~/Library/Application Support/MacCellular"
      ;;
    --non-interactive)
      NON_INTERACTIVE=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --token|--tunnel-token|--token=*)
      die "Tunnel token content is never accepted in argv; use --token-file or the hidden prompt"
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

valid_dns_name() {
  local value="$1"
  [[ ${#value} -le 253 && "$value" == *.* && "$value" != .* && "$value" != *. &&
     "$value" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ &&
     "$value" != *..* ]]
}

valid_dns_name "$PUBLIC_HOST" || die "--public-host must be a plain DNS hostname such as phone.example.com"
if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
  valid_dns_name "$TURN_HOST" || die "a voice profile requires --turn-host with a plain DNS hostname"
elif [[ -n "$TURN_HOST" ]]; then
  die "--turn-host is only used by a voice profile"
fi

case "$PROFILE" in
  sms) ;;
  direct-voice)
    [[ -n "$BINARY_SOURCE_OPTION" ]] || die "direct-voice requires an explicit --binary source"
    [[ -n "$HELPER_SOURCE_OPTION" ]] || die "direct-voice requires an explicit --helper-binary source"
    [[ -n "$TURN_SECRET_SOURCE_OPTION" ]] || die "direct-voice requires an explicit --turn-secret-file source"
    [[ -n "$VAPID_PRIVATE_KEY_SOURCE_OPTION" ]] || die "direct-voice requires an explicit --vapid-private-key-file source"
    ;;
  external-sip)
    [[ -n "$BINARY_SOURCE_OPTION" ]] || die "external-sip requires an explicit --binary source"
    [[ -n "$TURN_SECRET_SOURCE_OPTION" ]] || die "external-sip requires an explicit --turn-secret-file source"
    [[ -n "$VAPID_PRIVATE_KEY_SOURCE_OPTION" ]] || die "external-sip requires an explicit --vapid-private-key-file source"
    [[ -n "$VOICE_GATEWAY_ID" ]] || die "external-sip requires an explicit --voice-gateway-id"
    [[ -n "$ASTERISK_DEPLOYMENT_ENV_SOURCE_OPTION" ]] || die "external-sip requires an explicit --asterisk-deployment-env source"
    [[ -n "$ASTERISK_CONTROL_PASSWORD_SOURCE_OPTION" ]] || die "external-sip requires an explicit --asterisk-ari-password-file source"
    [[ -n "$ASTERISK_INSPECT_PASSWORD_SOURCE_OPTION" ]] || die "external-sip requires an explicit --asterisk-recovery-ari-password-file source"
    [[ -n "$SIP_RECOVERY_STORE_OPTION" ]] || die "external-sip requires an explicit --sip-recovery-store path"
    [[ -n "$SIP_MEDIA_UDP_MIN" ]] || die "external-sip requires an explicit --sip-media-udp-min"
    [[ -n "$SIP_MEDIA_UDP_MAX" ]] || die "external-sip requires an explicit --sip-media-udp-max"
    if (( SIP_DIAL_OUTGOING == 1 )); then
      [[ -n "$ASTERISK_OUTGOING_ENDPOINT" ]] || die "--sip-dial-outgoing requires --asterisk-ari-outgoing-endpoint"
    elif [[ -n "$ASTERISK_OUTGOING_ENDPOINT" ]]; then
      die "--asterisk-ari-outgoing-endpoint requires --sip-dial-outgoing"
    fi
    if [[ -n "$ASTERISK_OUTGOING_ENDPOINT" ]]; then
      [[ "$ASTERISK_OUTGOING_ENDPOINT" =~ ^[A-Za-z][A-Za-z0-9_.-]{0,63}$ ]] || \
        die "Asterisk outgoing endpoint must be one 1-64 character Asterisk token"
    fi
    [[ -z "$HELPER_SOURCE_OPTION" ]] || die "external-sip does not accept --helper-binary; the QDC media helper stays disabled"
    [[ "$SIP_RECOVERY_STORE_OPTION" == "$EXTERNAL_VOICE_DATA_DIR/recovery.json" ]] || \
      die "external-sip recovery store must be exactly $EXTERNAL_VOICE_DATA_DIR/recovery.json"
    ;;
  *) die "profile must be exactly sms, direct-voice, or external-sip" ;;
esac
if [[ "$PROFILE" != "external-sip" && ( -n "$VOICE_GATEWAY_ID" || -n "$ASTERISK_DEPLOYMENT_ENV_SOURCE_OPTION" ||
  -n "$ASTERISK_CONTROL_PASSWORD_SOURCE_OPTION" || -n "$ASTERISK_INSPECT_PASSWORD_SOURCE_OPTION" ||
  -n "$SIP_RECOVERY_STORE_OPTION" || -n "$SIP_MEDIA_UDP_MIN" || -n "$SIP_MEDIA_UDP_MAX" ||
  -n "$ASTERISK_OUTGOING_ENDPOINT" || "$SIP_DIAL_OUTGOING" == 1 ) ]]; then
  die "external SIP options require --profile external-sip"
fi
if [[ "$PROFILE" == "sms" && ( -n "$HELPER_SOURCE_OPTION" || -n "$TURN_SECRET_SOURCE_OPTION" || -n "$VAPID_PRIVATE_KEY_SOURCE_OPTION" ) ]]; then
  die "--helper-binary requires --profile direct-voice; TURN and VAPID sources require a voice profile"
fi

for path in "$RUNTIME_DIR" "$BIN_DIR" "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" \
  "$SMS_STORE_DIR" "$INSTALLED_DJONEHUB_BINARY" "$INSTALLED_MEDIA_HELPER_BINARY" \
  "$INSTALLED_CLOUDFLARED_BINARY" "$INSTALLED_ALLOWLIST_FILE" "$INSTALLED_TOKEN_FILE" \
  "$INSTALLED_TURN_SECRET_FILE" "$INSTALLED_VAPID_PRIVATE_KEY_FILE" "$PUSH_DATA_DIR" \
  "$PUSH_SUBSCRIPTIONS_FILE" "$INSTALLED_ASTERISK_DEPLOYMENT_ENV" \
  "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" \
  "$EXTERNAL_VOICE_DATA_DIR" "$SIP_RECOVERY_STORE_OPTION" "$MEDIA_CONTROL_SOCKET" \
  "$LAUNCH_AGENTS_DIR" "$LEGACY_DIR"; do
  [[ -z "$path" ]] && continue
  [[ "$path" == /* ]] || die "all installer paths must be absolute: $path"
  [[ "$path" != *$'\n'* && "$path" != *$'\r'* ]] || die "installer paths may not contain line breaks"
done
for plist_path in "$INSTALLED_DJONEHUB_BINARY" "$INSTALLED_CLOUDFLARED_BINARY" \
  "$INSTALLED_MEDIA_HELPER_BINARY" "$INSTALLED_ALLOWLIST_FILE" "$INSTALLED_TOKEN_FILE" \
  "$INSTALLED_TURN_SECRET_FILE" "$INSTALLED_VAPID_PRIVATE_KEY_FILE" "$LOG_DIR" \
  "$INSTALLED_ASTERISK_DEPLOYMENT_ENV" "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" \
  "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" "$SMS_STORE_DIR" "$PUSH_SUBSCRIPTIONS_FILE"; do
  [[ "$plist_path" != *'<'* && "$plist_path" != *'>'* && "$plist_path" != *'&'* ]] || \
    die "runtime paths may not contain XML metacharacters"
done
case "$RUNTIME_DIR/" in
  "$REPO_ROOT/"*) die "the production runtime may not be inside the repository" ;;
esac

prompt_value() {
  local variable_name="$1"
  local prompt="$2"
  local current_value="${!variable_name}"
  if [[ -n "$current_value" ]]; then
    return 0
  fi
  (( NON_INTERACTIVE == 0 )) || die "$prompt is required in non-interactive mode"
  [[ -t 0 ]] || die "$prompt is required; rerun in a terminal or pass its non-secret option"
  printf '%s: ' "$prompt" >&2
  IFS= read -r current_value
  [[ -n "$current_value" ]] || die "$prompt may not be empty"
  printf -v "$variable_name" '%s' "$current_value"
}

prompt_value TEAM_DOMAIN "Cloudflare Access team domain"
prompt_value AUDIENCE "Cloudflare Access AUD"
prompt_value EXPECTED_EMAIL "Access allowlist email"

[[ "$TEAM_DOMAIN" =~ ^[a-z0-9][a-z0-9.-]*\.cloudflareaccess\.com$ ]] || \
  die "team domain must be a canonical *.cloudflareaccess.com hostname"
[[ "$TEAM_DOMAIN" != *..* ]] || die "team domain contains an empty DNS label"
(( ${#AUDIENCE} >= 8 && ${#AUDIENCE} <= 256 )) || \
  die "Access AUD must be one 8-256 character tag without whitespace"
[[ "$AUDIENCE" =~ ^[A-Za-z0-9_-]+$ ]] || \
  die "Access AUD must be one 8-256 character tag without whitespace"
[[ "$EXPECTED_EMAIL" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || \
  die "email must be one exact address without whitespace"
[[ "$EXPECTED_EMAIL" != *@example.com && "$EXPECTED_EMAIL" != *@example.net && "$EXPECTED_EMAIL" != *@example.org ]] || \
  die "replace the example email with the real Cloudflare Access login email"

file_mode() {
  /usr/bin/stat -f '%Lp' "$1"
}

file_uid() {
  /usr/bin/stat -f '%u' "$1"
}

directory_is_mode_700() {
  local path="$1"
  [[ -d "$path" && ! -L "$path" ]] || return 1
  [[ "$(file_uid "$path")" == "$USER_UID" ]] || return 1
  [[ "$(file_mode "$path")" == "700" ]]
}

directory_is_private() {
  local path="$1"
  [[ -d "$path" && ! -L "$path" ]] || return 1
  [[ "$(file_uid "$path")" == "$USER_UID" ]] || return 1
  local mode
  mode="$(file_mode "$path")"
  (( (8#$mode & 077) == 0 ))
}

directory_is_safe_user_dir() {
  local path="$1"
  [[ -d "$path" && ! -L "$path" ]] || return 1
  [[ "$(file_uid "$path")" == "$USER_UID" ]] || return 1
  local mode
  mode="$(file_mode "$path")"
  (( (8#$mode & 022) == 0 ))
}

validate_target_dir() {
  local path="$1"
  if [[ -e "$path" ]]; then
    directory_is_mode_700 "$path" || die "runtime directory must be owned by the current user and mode 0700: $path"
  elif [[ "$MODE" == "check" ]]; then
    note "would create private runtime directory: $path"
  fi
}

validate_private_file() {
  local path="$1"
  local description="$2"
  [[ -f "$path" && ! -L "$path" ]] || die "$description must be a regular non-symlink file: $path"
  [[ "$(file_uid "$path")" == "$USER_UID" ]] || die "$description must be owned by the current user: $path"
  [[ "$(file_mode "$path")" == "600" ]] || die "$description must have exact mode 0600: $path"
}

validate_clean_absolute_existing_path() {
  local path="$1"
  local description="$2"
  local parent physical_parent base
  [[ "$path" == /* && "$path" != *$'\n'* && "$path" != *$'\r'* ]] || \
    die "$description must use a clean absolute path"
  parent="$(/usr/bin/dirname "$path")"
  base="$(/usr/bin/basename "$path")"
  physical_parent="$(CDPATH= cd -P -- "$parent" 2>/dev/null && pwd)" || \
    die "$description parent directory is unavailable: $parent"
  [[ "$physical_parent/$base" == "$path" ]] || \
    die "$description must not use symlinked parents, dot segments, or a non-canonical path: $path"
}

validate_private_single_link_file() {
  local path="$1"
  local description="$2"
  validate_clean_absolute_existing_path "$path" "$description"
  validate_private_file "$path" "$description"
  [[ "$(/usr/bin/stat -f '%l' "$path")" == "1" ]] || \
    die "$description must have exactly one hard link: $path"
  directory_is_mode_700 "$(/usr/bin/dirname "$path")" || \
    die "$description parent directory must be current-user owned and mode 0700: $(/usr/bin/dirname "$path")"
}

validate_regular_user_file() {
  local path="$1"
  local description="$2"
  [[ -f "$path" && ! -L "$path" ]] || die "$description must be a regular non-symlink file: $path"
  [[ "$(file_uid "$path")" == "$USER_UID" ]] || die "$description must be owned by the current user: $path"
}

validate_executable() {
  local path="$1"
  local description="$2"
  local require_user_owner="$3"
  [[ -f "$path" && -x "$path" && ! -L "$path" ]] || die "$description must be an executable regular non-symlink file: $path"
  if [[ "$require_user_owner" == "yes" ]]; then
    [[ "$(file_uid "$path")" == "$USER_UID" ]] || die "$description must be owned by the current user: $path"
  fi
  local mode
  mode="$(file_mode "$path")"
  (( (8#$mode & 022) == 0 )) || die "$description may not be group/world writable: $path"
  /usr/bin/file -b "$path" | /usr/bin/grep -Eq 'arm64(e)?' || die "$description is not a native arm64 Mach-O executable: $path"
  /usr/bin/codesign --verify "$path" >/dev/null 2>&1 || die "$description does not pass codesign verification: $path"
}

validate_direct_voice_binary_contract() {
  local path="$1"
  local help_output="$STAGE_DIR/djonehub-help.txt"
  if [[ "${DJONEHUB_INSTALLER_TESTING:-0}" == "1" && -n "${DJONEHUB_INSTALLER_TEST_BINARY_HELP_FILE:-}" ]]; then
    local fixture_help="$DJONEHUB_INSTALLER_TEST_BINARY_HELP_FILE"
    [[ "$fixture_help" == /* ]] || die "test binary help fixture must use an absolute path"
    validate_regular_user_file "$fixture_help" "test binary help fixture"
    /bin/cp "$fixture_help" "$help_output"
  elif ! "$path" -h > "$help_output" 2>&1; then
    die "DJOneHub -h failed; refusing to enable direct-voice"
  fi
  /usr/bin/grep -Fq -- '-public-web-direct-voice' "$help_output" || \
    die "DJOneHub binary does not support -public-web-direct-voice"
  /usr/bin/grep -Fq -- '-phone-relay-runtime' "$help_output" || \
    die "DJOneHub binary does not support -phone-relay-runtime"
  /usr/bin/grep -Eq -- '(^|[[:space:]])-public-web-push([[:space:]]|$)' "$help_output" || \
    die "DJOneHub binary does not support -public-web-push"
  /usr/bin/grep -Fq -- '-public-web-push-vapid-private-key-file' "$help_output" || \
    die "DJOneHub binary does not support -public-web-push-vapid-private-key-file"
  /usr/bin/grep -Fq -- '-public-web-push-vapid-subject' "$help_output" || \
    die "DJOneHub binary does not support -public-web-push-vapid-subject"
  /usr/bin/grep -Fq -- '-public-web-push-subscriptions-file' "$help_output" || \
    die "DJOneHub binary does not support -public-web-push-subscriptions-file"
  /bin/rm -f -- "$help_output"
}

validate_external_sip_binary_contract() {
  local path="$1"
  local help_output="$STAGE_DIR/djonehub-external-sip-help.txt"
  if [[ "${DJONEHUB_INSTALLER_TESTING:-0}" == "1" && -n "${DJONEHUB_INSTALLER_TEST_BINARY_HELP_FILE:-}" ]]; then
    local fixture_help="$DJONEHUB_INSTALLER_TEST_BINARY_HELP_FILE"
    [[ "$fixture_help" == /* ]] || die "test binary help fixture must use an absolute path"
    validate_regular_user_file "$fixture_help" "test binary help fixture"
    /bin/cp "$fixture_help" "$help_output"
  elif ! "$path" -h > "$help_output" 2>&1; then
    die "DJOneHub -h failed; refusing to enable external-sip"
  fi
  local required_flag
  for required_flag in \
    -sms-only-runtime \
    -public-web-external-voice \
    -public-web-turn-host \
    -public-web-turn-secret-file \
    -public-web-turn-udp-port \
    -public-web-turn-tls-port \
    -public-web-turn-credential-ttl \
    -public-web-push \
    -public-web-push-vapid-private-key-file \
    -public-web-push-vapid-subject \
    -public-web-push-subscriptions-file \
    -voice-provider \
    -voice-gateway-id \
    -asterisk-ari-url \
    -asterisk-ari-application \
    -asterisk-ari-incoming-argument \
    -asterisk-ari-incoming-context \
    -asterisk-ari-incoming-endpoint \
    -asterisk-ari-outgoing-endpoint \
    -asterisk-ari-username \
    -asterisk-ari-password-file \
    -asterisk-recovery-ari-username \
    -asterisk-recovery-ari-password-file \
    -asterisk-expected-entity-id \
    -asterisk-expected-version \
    -asterisk-incoming-policy-id \
    -sip-recovery-store \
    -sip-media-udp-min \
    -sip-media-udp-max \
    -sip-dial-outgoing \
    -sip-answer-incoming \
    -sip-reject-incoming \
    -sip-send-dtmf \
    -sip-end-active; do
    /usr/bin/grep -Fq -- "$required_flag" "$help_output" || \
      die "DJOneHub binary does not support $required_flag"
  done
  /bin/rm -f -- "$help_output"
}

[[ "$(/usr/bin/uname -s)" == "Darwin" ]] || die "this installer only runs on macOS"
[[ -x /usr/bin/caffeinate ]] || die "/usr/bin/caffeinate is unavailable"

for target_dir in "$RUNTIME_DIR" "$BIN_DIR" "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" "$SMS_STORE_DIR"; do
  validate_target_dir "$target_dir"
done
if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
  validate_target_dir "$PUSH_DATA_DIR"
  if [[ -e "$PUSH_SUBSCRIPTIONS_FILE" ]]; then
    validate_private_file "$PUSH_SUBSCRIPTIONS_FILE" "Web Push subscriptions database"
  fi
fi
if [[ "$PROFILE" == "external-sip" ]]; then
  validate_target_dir "$EXTERNAL_VOICE_DATA_DIR"
  for recovery_file in \
    "$SIP_RECOVERY_STORE_OPTION" \
    "$SIP_RECOVERY_STORE_OPTION.lock" \
    "$SIP_RECOVERY_STORE_OPTION.meta" \
    "$SIP_RECOVERY_STORE_OPTION.tmp" \
    "$SIP_RECOVERY_STORE_OPTION.meta.tmp"; do
    if [[ -e "$recovery_file" ]]; then
      validate_private_single_link_file "$recovery_file" "external voice recovery file"
    fi
  done
fi
if [[ -e "$LAUNCH_AGENTS_DIR" ]]; then
  directory_is_safe_user_dir "$LAUNCH_AGENTS_DIR" || \
    die "LaunchAgents directory must be user-owned and not group/world writable: $LAUNCH_AGENTS_DIR"
elif [[ "$MODE" == "check" ]]; then
  note "would create user LaunchAgents directory: $LAUNCH_AGENTS_DIR"
fi

STAGE_DIR="$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/djonehub-install.XXXXXX")"
cleanup() {
  if [[ -n "${STAGE_DIR:-}" && "$STAGE_DIR" == "${TMPDIR:-/tmp}"/djonehub-install.* && -d "$STAGE_DIR" ]]; then
    /bin/rm -rf -- "$STAGE_DIR"
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

choose_binary_source() {
  if [[ -n "$BINARY_SOURCE_OPTION" ]]; then
    DJONEHUB_SOURCE="$BINARY_SOURCE_OPTION"
  elif [[ -f "$INSTALLED_DJONEHUB_BINARY" ]]; then
    DJONEHUB_SOURCE="$INSTALLED_DJONEHUB_BINARY"
  elif [[ -f "$LEGACY_DIR/bin/djonehub-macos.arm64-cgo" ]]; then
    # Legacy is a first-install migration fallback only. Once a production
    # copy exists, a bare repeat apply must never downgrade it from Documents.
    DJONEHUB_SOURCE="$LEGACY_DIR/bin/djonehub-macos.arm64-cgo"
  else
    die "DJOneHub binary is missing; build it or pass --binary"
  fi
  [[ "$DJONEHUB_SOURCE" == /* ]] || die "DJOneHub binary source must be an absolute path"
  validate_executable "$DJONEHUB_SOURCE" "DJOneHub arm64-cgo binary" "yes"
  /bin/cp "$DJONEHUB_SOURCE" "$STAGE_DIR/djonehub-macos.arm64-cgo"
  /bin/chmod 700 "$STAGE_DIR/djonehub-macos.arm64-cgo"
  validate_executable "$STAGE_DIR/djonehub-macos.arm64-cgo" "staged DJOneHub arm64-cgo binary" "yes"
  if [[ "$PROFILE" == "direct-voice" ]]; then
    validate_direct_voice_binary_contract "$STAGE_DIR/djonehub-macos.arm64-cgo"
  elif [[ "$PROFILE" == "external-sip" ]]; then
    validate_external_sip_binary_contract "$STAGE_DIR/djonehub-macos.arm64-cgo"
  fi
}

stage_media_helper() {
  [[ "$PROFILE" == "direct-voice" ]] || return 0
  [[ "$HELPER_SOURCE_OPTION" == /* ]] || die "media helper source must be an absolute path"
  validate_executable "$HELPER_SOURCE_OPTION" "DJOneHubNotifier media helper" "yes"
  "$HELPER_SOURCE_OPTION" --self-test >/dev/null 2>&1 || \
    die "DJOneHubNotifier media helper self-test failed"
  /bin/cp "$HELPER_SOURCE_OPTION" "$STAGE_DIR/DJOneHubNotifier"
  /bin/chmod 700 "$STAGE_DIR/DJOneHubNotifier"
  validate_executable "$STAGE_DIR/DJOneHubNotifier" "staged DJOneHubNotifier media helper" "yes"
  "$STAGE_DIR/DJOneHubNotifier" --self-test >/dev/null 2>&1 || \
    die "staged DJOneHubNotifier media helper self-test failed"
}

choose_cloudflared_source() {
  if [[ -n "$CLOUDFLARED_SOURCE_OPTION" ]]; then
    CLOUDFLARED_SOURCE="$CLOUDFLARED_SOURCE_OPTION"
  elif [[ -x /opt/homebrew/opt/cloudflared/bin/cloudflared ]]; then
    CLOUDFLARED_SOURCE="/opt/homebrew/opt/cloudflared/bin/cloudflared"
  elif [[ -x "$INSTALLED_CLOUDFLARED_BINARY" ]]; then
    CLOUDFLARED_SOURCE="$INSTALLED_CLOUDFLARED_BINARY"
  else
    die "native cloudflared is missing; install it or pass --cloudflared"
  fi
  [[ "$CLOUDFLARED_SOURCE" == /* ]] || die "cloudflared source must be an absolute path"
  validate_executable "$CLOUDFLARED_SOURCE" "cloudflared binary" "no"
  "$CLOUDFLARED_SOURCE" --version 2>/dev/null | /usr/bin/grep -qi 'cloudflared version' || \
    die "cloudflared --version did not identify a cloudflared build"
  /bin/cp "$CLOUDFLARED_SOURCE" "$STAGE_DIR/cloudflared"
  /bin/chmod 700 "$STAGE_DIR/cloudflared"
  validate_executable "$STAGE_DIR/cloudflared" "staged cloudflared binary" "yes"
}

validate_email_file_content() {
  local path="$1"
  local count=0
  local line
  [[ "$(/usr/bin/wc -c < "$path" | /usr/bin/tr -d ' ')" -le 16384 ]] || die "email allowlist exceeds 16 KiB"
  [[ "$(/usr/bin/tail -c 1 "$path" | /usr/bin/od -An -tuC | /usr/bin/tr -d ' ')" == "10" ]] || \
    die "email allowlist must end with one LF newline"
  while IFS= read -r line; do
    (( count += 1 ))
    [[ -n "$line" && "$line" != *$'\r'* ]] || die "email allowlist contains a blank or CRLF line"
    [[ "$line" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || \
      die "email allowlist contains an invalid address"
  done < "$path"
  (( count > 0 )) || die "email allowlist must contain at least one address"
}

stage_allowlist() {
  local source=""
  if [[ -n "$ALLOWLIST_SOURCE_OPTION" ]]; then
    source="$ALLOWLIST_SOURCE_OPTION"
  elif [[ -f "$INSTALLED_ALLOWLIST_FILE" ]]; then
    source="$INSTALLED_ALLOWLIST_FILE"
  elif [[ -f "$LEGACY_DIR/config/access-allowed-emails" ]]; then
    source="$LEGACY_DIR/config/access-allowed-emails"
  fi

  if [[ -n "$source" ]]; then
    [[ "$source" == /* ]] || die "allowlist source must be an absolute path"
    validate_private_file "$source" "email allowlist source"
    validate_email_file_content "$source"
    /bin/cp "$source" "$STAGE_DIR/access-allowed-emails"
    /bin/chmod 600 "$STAGE_DIR/access-allowed-emails"
    if ! /usr/bin/grep -Fqx -- "$EXPECTED_EMAIL" "$STAGE_DIR/access-allowed-emails"; then
      if /usr/bin/grep -Eq '^[^@[:space:]]+@example\.(com|net|org)$' "$STAGE_DIR/access-allowed-emails" && \
         ! /usr/bin/grep -Evq '^[^@[:space:]]+@example\.(com|net|org)$' "$STAGE_DIR/access-allowed-emails"; then
        printf '%s\n' "$EXPECTED_EMAIL" > "$STAGE_DIR/access-allowed-emails"
        /bin/chmod 600 "$STAGE_DIR/access-allowed-emails"
      else
        die "allowlist source does not contain the requested email"
      fi
    fi
  else
    printf '%s\n' "$EXPECTED_EMAIL" > "$STAGE_DIR/access-allowed-emails"
    /bin/chmod 600 "$STAGE_DIR/access-allowed-emails"
  fi
  validate_email_file_content "$STAGE_DIR/access-allowed-emails"
}

validate_token_value() {
  local token="$1"
  (( ${#token} >= 80 && ${#token} <= 4096 )) || die "Tunnel token length is invalid"
  [[ "$token" =~ ^[A-Za-z0-9._~+/=-]+$ ]] || die "Tunnel token contains whitespace or invalid characters"
}

validate_token_file_content() {
  local path="$1"
  local token
  [[ "$(/usr/bin/wc -l < "$path" | /usr/bin/tr -d ' ')" == "1" ]] || \
    die "Tunnel token file must contain exactly one LF-terminated line"
  [[ "$(/usr/bin/tail -c 1 "$path" | /usr/bin/od -An -tuC | /usr/bin/tr -d ' ')" == "10" ]] || \
    die "Tunnel token file must end with one LF newline"
  IFS= read -r token < "$path"
  validate_token_value "$token"
  unset token
}

source_path_is_git_ignored_if_needed() {
  local path="$1"
  case "$path" in
    "$SCRIPT_DIR"|"$SCRIPT_DIR"/*)
      die "secret configuration may not be stored under deploy/public-web-mac"
      ;;
  esac
  if [[ "$path" == "$REPO_ROOT"/* ]]; then
    local relative="${path#"$REPO_ROOT"/}"
    /usr/bin/git -C "$REPO_ROOT" check-ignore -q -- "$relative" || \
      die "secret configuration source is inside the repository but is not Git-ignored"
  fi
}

stage_token() {
  local source=""
  if [[ -n "$TOKEN_SOURCE_OPTION" ]]; then
    source="$TOKEN_SOURCE_OPTION"
  elif [[ -f "$INSTALLED_TOKEN_FILE" ]]; then
    source="$INSTALLED_TOKEN_FILE"
  elif [[ -f "$LEGACY_DIR/config/cloudflared-token" ]]; then
    source="$LEGACY_DIR/config/cloudflared-token"
  fi

  if [[ -n "$source" ]]; then
    [[ "$source" == /* ]] || die "Tunnel token source must be an absolute path"
    source_path_is_git_ignored_if_needed "$source"
    validate_private_file "$source" "Tunnel token source"
    validate_token_file_content "$source"
    /bin/cp "$source" "$STAGE_DIR/cloudflared-token"
    /bin/chmod 600 "$STAGE_DIR/cloudflared-token"
  else
    (( NON_INTERACTIVE == 0 )) || \
      die "Tunnel token is missing and hidden prompting is disabled"
    [[ -t 0 ]] || die "Tunnel token is missing; rerun in a terminal for hidden input"
    printf 'Cloudflare Tunnel token (hidden): ' >&2
    IFS= read -r -s tunnel_token
    printf '\n' >&2
    validate_token_value "$tunnel_token"
    printf '%s\n' "$tunnel_token" > "$STAGE_DIR/cloudflared-token"
    /bin/chmod 600 "$STAGE_DIR/cloudflared-token"
    unset tunnel_token
  fi
  validate_token_file_content "$STAGE_DIR/cloudflared-token"
}

validate_turn_secret_file_content() {
  local path="$1"
  local size
  size="$(/usr/bin/wc -c < "$path" | /usr/bin/tr -d ' ')"
  (( size >= 1 && size <= 4096 )) || die "TURN shared-secret file size is invalid"
  LC_ALL=C /usr/bin/awk '
    NR > 1 { exit 1 }
    length($0) == 0 { exit 1 }
    $0 ~ /^[[:space:]]/ || $0 ~ /[[:space:]]$/ || index($0, "\r") != 0 { exit 1 }
    END { if (NR != 1) exit 1 }
  ' "$path" || die "TURN shared-secret file must contain one non-empty exact line"
}

stage_turn_secret() {
  [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]] || return 0
  local source="$TURN_SECRET_SOURCE_OPTION"
  [[ "$source" == /* ]] || die "TURN shared-secret source must be an absolute path"
  source_path_is_git_ignored_if_needed "$source"
  if [[ "$PROFILE" == "external-sip" ]]; then
    validate_private_single_link_file "$source" "TURN shared-secret source"
  else
    validate_private_file "$source" "TURN shared-secret source"
  fi
  validate_turn_secret_file_content "$source"
  /bin/cp "$source" "$STAGE_DIR/public-web-turn-secret"
  /bin/chmod 600 "$STAGE_DIR/public-web-turn-secret"
  validate_private_file "$STAGE_DIR/public-web-turn-secret" "staged TURN shared-secret"
  validate_turn_secret_file_content "$STAGE_DIR/public-web-turn-secret"
}

validate_vapid_private_key_file_content() {
  local path="$1"
  [[ -x /usr/bin/python3 ]] || die "/usr/bin/python3 is required to validate the VAPID private key"
  [[ "$(/usr/bin/wc -c < "$path" | /usr/bin/tr -d ' ')" == "44" ]] || \
    die "VAPID private-key file must contain one LF-terminated, unpadded base64url canonical P-256 scalar in the range 1..N-1"
  /usr/bin/python3 - "$path" <<'PY' || \
    die "VAPID private-key file must contain one LF-terminated, unpadded base64url canonical P-256 scalar in the range 1..N-1"
import base64
import binascii
import pathlib
import re
import sys

payload = pathlib.Path(sys.argv[1]).read_bytes()
if not payload.endswith(b"\n") or payload.count(b"\n") != 1:
    raise SystemExit(1)
encoded = payload[:-1]
if not re.fullmatch(rb"[A-Za-z0-9_-]+", encoded):
    raise SystemExit(1)
try:
    raw = base64.b64decode(
        encoded + (b"=" * ((4 - len(encoded) % 4) % 4)),
        altchars=b"-_",
        validate=True,
    )
except (binascii.Error, ValueError):
    raise SystemExit(1)
if len(raw) != 32:
    raise SystemExit(1)
if base64.urlsafe_b64encode(raw).rstrip(b"=") != encoded:
    raise SystemExit(1)
scalar = int.from_bytes(raw, "big")
p256_order = int("FFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551", 16)
if not 1 <= scalar < p256_order:
    raise SystemExit(1)
PY
}

stage_vapid_private_key() {
  [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]] || return 0
  local source="$VAPID_PRIVATE_KEY_SOURCE_OPTION"
  [[ "$source" == /* ]] || die "VAPID private-key source must be an absolute path"
  source_path_is_git_ignored_if_needed "$source"
  if [[ "$PROFILE" == "external-sip" ]]; then
    validate_private_single_link_file "$source" "VAPID private-key source"
  else
    validate_private_file "$source" "VAPID private-key source"
  fi
  validate_vapid_private_key_file_content "$source"
  /bin/cp "$source" "$STAGE_DIR/public-web-vapid-private-key"
  /bin/chmod 600 "$STAGE_DIR/public-web-vapid-private-key"
  validate_private_file "$STAGE_DIR/public-web-vapid-private-key" "staged VAPID private key"
  validate_vapid_private_key_file_content "$STAGE_DIR/public-web-vapid-private-key"
}

deployment_env_value() {
  local path="$1"
  local key="$2"
  LC_ALL=C /usr/bin/awk -F= -v wanted="$key" '
    $1 == wanted { count += 1; value = substr($0, length(wanted) + 2) }
    END { if (count != 1 || value == "") exit 1; print value }
  ' "$path"
}

validate_asterisk_deployment_env_content() {
  local path="$1"
  [[ -x /usr/bin/python3 ]] || die "/usr/bin/python3 is required to validate Asterisk deployment.env"
  /usr/bin/python3 - "$path" <<'PY' || \
    die "Asterisk deployment.env does not match the exact production contract"
import ipaddress
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
payload = path.read_bytes()
if not payload.endswith(b"\n") or b"\r" in payload or b"\x00" in payload:
    raise SystemExit(1)
try:
    lines = payload.decode("ascii").splitlines()
except UnicodeDecodeError:
    raise SystemExit(1)

expected_keys = [
    "MODE", "ASTERISK_VERSION", "ASTERISK_SHA256", "ASTERISK_IMAGE_REF",
    "ENTITY_ID", "ARI_PORT", "SIP_PORT", "RTP_MIN", "RTP_MAX",
    "MAX_CALL_SECONDS", "APPLICATION", "ARGUMENT", "POLICY_ID", "CONTEXT",
    "ENDPOINT", "CONTROL_USER", "INSPECT_USER", "GATEWAY_IP", "SIP_BIND_IP",
    "LAN_CIDR", "PBX_CONTAINER_SUBNET", "PBX_CONTAINER_IPV4",
]
if len(lines) != len(expected_keys):
    raise SystemExit(1)
values = {}
for expected, line in zip(expected_keys, lines):
    if "=" not in line:
        raise SystemExit(1)
    key, value = line.split("=", 1)
    if key != expected or not value or any(ch.isspace() for ch in value):
        raise SystemExit(1)
    values[key] = value

if values["MODE"] != "production":
    raise SystemExit(1)
if values["ASTERISK_VERSION"] != "22.10.1":
    raise SystemExit(1)
if values["ASTERISK_SHA256"] != "0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353":
    raise SystemExit(1)
if values["ASTERISK_IMAGE_REF"] != "maccellular-asterisk@sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51":
    raise SystemExit(1)
if not re.fullmatch(r"[0-9a-f][26ae](?::[0-9a-f]{2}){5}", values["ENTITY_ID"]):
    raise SystemExit(1)
token = re.compile(r"[A-Za-z][A-Za-z0-9_.-]{0,63}")
for key in ("APPLICATION", "ARGUMENT", "CONTEXT", "ENDPOINT", "CONTROL_USER", "INSPECT_USER"):
    if not token.fullmatch(values[key]):
        raise SystemExit(1)
if values["CONTROL_USER"] == values["INSPECT_USER"] or values["POLICY_ID"] != "djonehub-incoming-v1":
    raise SystemExit(1)

def decimal(key, minimum, maximum):
    value = values[key]
    if not re.fullmatch(r"0|[1-9][0-9]*", value):
        raise SystemExit(1)
    number = int(value)
    if not minimum <= number <= maximum:
        raise SystemExit(1)
    return number

ari_port = decimal("ARI_PORT", 1024, 65535)
sip_port = decimal("SIP_PORT", 1024, 65535)
rtp_min = decimal("RTP_MIN", 1024, 65535)
rtp_max = decimal("RTP_MAX", 1024, 65535)
decimal("MAX_CALL_SECONDS", 30, 3600)
if ari_port == sip_port or rtp_min > rtp_max or rtp_min % 2 or (rtp_max - rtp_min + 1) % 2:
    raise SystemExit(1)
if not 2 <= rtp_max - rtp_min + 1 <= 64:
    raise SystemExit(1)
if rtp_min <= ari_port <= rtp_max or rtp_min <= sip_port <= rtp_max:
    raise SystemExit(1)

private_ranges = [
    ipaddress.ip_network("10.0.0.0/8"), ipaddress.ip_network("172.16.0.0/12"),
    ipaddress.ip_network("192.168.0.0/16"), ipaddress.ip_network("100.64.0.0/10"),
]
gateway = ipaddress.ip_address(values["GATEWAY_IP"])
sip_bind = ipaddress.ip_address(values["SIP_BIND_IP"])
lan = ipaddress.ip_network(values["LAN_CIDR"], strict=True)
container_net = ipaddress.ip_network(values["PBX_CONTAINER_SUBNET"], strict=True)
container_ip = ipaddress.ip_address(values["PBX_CONTAINER_IPV4"])
if any(item.version != 4 for item in (gateway, sip_bind, lan, container_net, container_ip)):
    raise SystemExit(1)
if str(gateway) != values["GATEWAY_IP"] or str(sip_bind) != values["SIP_BIND_IP"]:
    raise SystemExit(1)
if str(lan) != values["LAN_CIDR"] or str(container_net) != values["PBX_CONTAINER_SUBNET"]:
    raise SystemExit(1)
if not all(any(item.subnet_of(net) if isinstance(item, ipaddress.IPv4Network) else item in net for net in private_ranges)
           for item in (lan, container_net, gateway, sip_bind, container_ip)):
    raise SystemExit(1)
if gateway == sip_bind or gateway not in lan or sip_bind not in lan:
    raise SystemExit(1)
if gateway in (lan.network_address, lan.broadcast_address) or sip_bind in (lan.network_address, lan.broadcast_address):
    raise SystemExit(1)
if container_ip not in container_net or container_ip in (container_net.network_address, container_net.broadcast_address):
    raise SystemExit(1)
if lan.overlaps(container_net):
    raise SystemExit(1)
PY
}

validate_asterisk_password_content() {
  local path="$1"
  local description="$2"
  local size
  size="$(/usr/bin/wc -c < "$path" | /usr/bin/tr -d ' ')"
  (( size >= 2 && size <= 1025 )) || die "$description must contain one 1-1024 byte LF-terminated password"
  [[ "$(/usr/bin/wc -l < "$path" | /usr/bin/tr -d ' ')" == "1" ]] || \
    die "$description must contain exactly one LF-terminated password"
  [[ "$(/usr/bin/tail -c 1 "$path" | /usr/bin/od -An -tuC | /usr/bin/tr -d ' ')" == "10" ]] || \
    die "$description must end with one LF newline"
  LC_ALL=C /usr/bin/awk '
    NR != 1 || length($0) == 0 || length($0) > 1024 || index($0, "\r") || index($0, ":") { exit 1 }
    END { if (NR != 1) exit 1 }
  ' "$path" || die "$description must contain one safe non-empty password line"
}

canonical_port() {
  local value="$1"
  [[ "$value" =~ ^[1-9][0-9]{0,4}$ ]] || return 1
  (( 10#$value >= 1024 && 10#$value <= 65535 ))
}

stage_external_sip_configuration() {
  [[ "$PROFILE" == "external-sip" ]] || return 0
  local deployment="$ASTERISK_DEPLOYMENT_ENV_SOURCE_OPTION"
  local control="$ASTERISK_CONTROL_PASSWORD_SOURCE_OPTION"
  local inspect="$ASTERISK_INSPECT_PASSWORD_SOURCE_OPTION"
  local deployment_root

  for source in "$deployment" "$control" "$inspect"; do
    [[ "$source" == /* ]] || die "external SIP Asterisk sources must use absolute paths"
    source_path_is_git_ignored_if_needed "$source"
  done
  validate_private_single_link_file "$deployment" "Asterisk deployment.env source"
  validate_private_single_link_file "$control" "Asterisk ARI control password source"
  validate_private_single_link_file "$inspect" "Asterisk ARI inspect password source"
  deployment_root="$(/usr/bin/dirname "$deployment")"
  [[ "$control" == "$deployment_root/secrets/ari-control.password" ]] || \
    die "Asterisk control password must be the canonical sibling of deployment.env"
  [[ "$inspect" == "$deployment_root/secrets/ari-inspect.password" ]] || \
    die "Asterisk inspect password must be the canonical sibling of deployment.env"
  validate_asterisk_deployment_env_content "$deployment"
  validate_asterisk_password_content "$control" "Asterisk ARI control password"
  validate_asterisk_password_content "$inspect" "Asterisk ARI inspect password"
  /usr/bin/cmp -s "$control" "$inspect" && \
    die "Asterisk control and inspect passwords must differ"

  [[ "$VOICE_GATEWAY_ID" =~ ^[A-Za-z][A-Za-z0-9_.-]{0,63}$ ]] || \
    die "voice gateway ID must be one 1-64 character Asterisk token"
  canonical_port "$SIP_MEDIA_UDP_MIN" || die "SIP media UDP minimum must be a canonical port in 1024..65535"
  canonical_port "$SIP_MEDIA_UDP_MAX" || die "SIP media UDP maximum must be a canonical port in 1024..65535"
  (( 10#$SIP_MEDIA_UDP_MAX >= 10#$SIP_MEDIA_UDP_MIN &&
     10#$SIP_MEDIA_UDP_MAX - 10#$SIP_MEDIA_UDP_MIN <= 1024 )) || \
    die "SIP media UDP range must contain between 1 and 1025 ports"

  PBX_VERSION="$(deployment_env_value "$deployment" ASTERISK_VERSION)"
  PBX_ENTITY_ID="$(deployment_env_value "$deployment" ENTITY_ID)"
  PBX_ARI_PORT="$(deployment_env_value "$deployment" ARI_PORT)"
  PBX_SIP_PORT="$(deployment_env_value "$deployment" SIP_PORT)"
  PBX_RTP_MIN="$(deployment_env_value "$deployment" RTP_MIN)"
  PBX_RTP_MAX="$(deployment_env_value "$deployment" RTP_MAX)"
  PBX_APPLICATION="$(deployment_env_value "$deployment" APPLICATION)"
  PBX_ARGUMENT="$(deployment_env_value "$deployment" ARGUMENT)"
  PBX_POLICY_ID="$(deployment_env_value "$deployment" POLICY_ID)"
  PBX_CONTEXT="$(deployment_env_value "$deployment" CONTEXT)"
  PBX_ENDPOINT="$(deployment_env_value "$deployment" ENDPOINT)"
  PBX_CONTROL_USER="$(deployment_env_value "$deployment" CONTROL_USER)"
  PBX_INSPECT_USER="$(deployment_env_value "$deployment" INSPECT_USER)"

  if (( 10#$SIP_MEDIA_UDP_MIN <= 10#$PBX_ARI_PORT && 10#$PBX_ARI_PORT <= 10#$SIP_MEDIA_UDP_MAX )) ||
     (( 10#$SIP_MEDIA_UDP_MIN <= 10#$PBX_SIP_PORT && 10#$PBX_SIP_PORT <= 10#$SIP_MEDIA_UDP_MAX )) ||
     (( 10#$SIP_MEDIA_UDP_MIN <= 10#$PBX_RTP_MAX && 10#$PBX_RTP_MIN <= 10#$SIP_MEDIA_UDP_MAX )); then
    die "SIP media UDP range must not overlap Asterisk ARI, SIP, or RTP ports"
  fi

  /bin/cp "$deployment" "$STAGE_DIR/asterisk-deployment.env"
  /bin/cp "$control" "$STAGE_DIR/asterisk-ari-control.password"
  /bin/cp "$inspect" "$STAGE_DIR/asterisk-ari-inspect.password"
  /bin/chmod 600 \
    "$STAGE_DIR/asterisk-deployment.env" \
    "$STAGE_DIR/asterisk-ari-control.password" \
    "$STAGE_DIR/asterisk-ari-inspect.password"
  for staged_file in \
    "$STAGE_DIR/asterisk-deployment.env" \
    "$STAGE_DIR/asterisk-ari-control.password" \
    "$STAGE_DIR/asterisk-ari-inspect.password"; do
    validate_private_file "$staged_file" "staged external SIP credential/config"
    [[ "$(/usr/bin/stat -f '%l' "$staged_file")" == "1" ]] || \
      die "staged external SIP credential/config must have exactly one hard link"
  done
  validate_asterisk_deployment_env_content "$STAGE_DIR/asterisk-deployment.env"
  validate_asterisk_password_content "$STAGE_DIR/asterisk-ari-control.password" "staged Asterisk ARI control password"
  validate_asterisk_password_content "$STAGE_DIR/asterisk-ari-inspect.password" "staged Asterisk ARI inspect password"
}

validate_destination_file_if_present() {
  local path="$1"
  local description="$2"
  if [[ -e "$path" ]]; then
    validate_regular_user_file "$path" "$description"
  fi
}

plan_file_action() {
  local source="$1"
  local destination="$2"
  local expected_mode="$3"
  local result_variable="$4"
  local action
  validate_destination_file_if_present "$destination" "installed runtime target"
  if [[ ! -e "$destination" ]]; then
    action="create"
  elif /usr/bin/cmp -s "$source" "$destination" && [[ "$(file_mode "$destination")" == "$expected_mode" ]]; then
    action="keep"
  else
    action="replace"
  fi
  printf -v "$result_variable" '%s' "$action"
}

choose_binary_source
choose_cloudflared_source
stage_allowlist
stage_token
stage_media_helper
stage_turn_secret
stage_vapid_private_key
stage_external_sip_configuration

plan_file_action "$STAGE_DIR/djonehub-macos.arm64-cgo" "$INSTALLED_DJONEHUB_BINARY" 700 BINARY_ACTION
plan_file_action "$STAGE_DIR/cloudflared" "$INSTALLED_CLOUDFLARED_BINARY" 700 CLOUDFLARED_ACTION
plan_file_action "$STAGE_DIR/access-allowed-emails" "$INSTALLED_ALLOWLIST_FILE" 600 ALLOWLIST_ACTION
plan_file_action "$STAGE_DIR/cloudflared-token" "$INSTALLED_TOKEN_FILE" 600 TOKEN_ACTION
if [[ "$PROFILE" == "direct-voice" ]]; then
  plan_file_action "$STAGE_DIR/DJOneHubNotifier" "$INSTALLED_MEDIA_HELPER_BINARY" 700 HELPER_ACTION
fi
if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
  plan_file_action "$STAGE_DIR/public-web-turn-secret" "$INSTALLED_TURN_SECRET_FILE" 600 TURN_SECRET_ACTION
  plan_file_action "$STAGE_DIR/public-web-vapid-private-key" "$INSTALLED_VAPID_PRIVATE_KEY_FILE" 600 VAPID_PRIVATE_KEY_ACTION
fi
if [[ "$PROFILE" == "external-sip" ]]; then
  plan_file_action "$STAGE_DIR/asterisk-deployment.env" "$INSTALLED_ASTERISK_DEPLOYMENT_ENV" 600 ASTERISK_DEPLOYMENT_ENV_ACTION
  plan_file_action "$STAGE_DIR/asterisk-ari-control.password" "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" 600 ASTERISK_CONTROL_PASSWORD_ACTION
  plan_file_action "$STAGE_DIR/asterisk-ari-inspect.password" "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" 600 ASTERISK_INSPECT_PASSWORD_ACTION
fi

LEGACY_SMS_STORE="$LEGACY_DIR/sms-store"
if [[ -d "$LEGACY_DIR/data/sms-store" ]]; then
  LEGACY_SMS_STORE="$LEGACY_DIR/data/sms-store"
fi
readonly LEGACY_SMS_STORE

preflight_sms_migration() {
  local source="$1"
  local path relative destination mode
  [[ -e "$source" ]] || return 0
  directory_is_safe_user_dir "$source" || \
    die "legacy SMS store must be a user-owned non-symlink directory without group/world writes: $source"
  [[ "$source" != "$SMS_STORE_DIR" ]] || return 0

  while IFS= read -r -d '' path; do
    relative="${path#"$source"/}"
    destination="$SMS_STORE_DIR/$relative"
    if [[ -L "$path" ]]; then
      die "legacy SMS store contains a symlink, which is not migrated: $path"
    elif [[ -d "$path" ]]; then
      directory_is_safe_user_dir "$path" || die "legacy SMS directory is not private: $path"
      if [[ -e "$destination" ]]; then
        directory_is_private "$destination" || die "installed SMS directory is not private: $destination"
      fi
    elif [[ -f "$path" ]]; then
      [[ "$(file_uid "$path")" == "$USER_UID" ]] || die "legacy SMS file has the wrong owner: $path"
      mode="$(file_mode "$path")"
      (( (8#$mode & 077) == 0 )) || die "legacy SMS file is not private: $path"
      if [[ -e "$destination" ]]; then
        [[ -f "$destination" && ! -L "$destination" ]] || die "installed SMS path conflicts with a non-file: $destination"
        [[ "$(file_uid "$destination")" == "$USER_UID" ]] || die "installed SMS file has the wrong owner: $destination"
        mode="$(file_mode "$destination")"
        (( (8#$mode & 077) == 0 )) || die "installed SMS file is not private: $destination"
        /usr/bin/cmp -s "$path" "$destination" || \
          die "SMS store conflict; refusing to overwrite different existing data: $destination"
        (( SMS_MATCH_COUNT += 1 ))
      else
        (( SMS_COPY_COUNT += 1 ))
      fi
    else
      die "legacy SMS store contains an unsupported file type: $path"
    fi
  done < <(/usr/bin/find -P "$source" -mindepth 1 -print0)
}

preflight_sms_migration "$LEGACY_SMS_STORE"

escape_sed_replacement() {
  printf '%s' "$1" | /usr/bin/sed -e 's/[\\&|]/\\&/g'
}

render_plists() {
  local binary public_host turn_host push_subject team aud allowlist logs sms cloudflared token helper turn_secret vapid_private_key push_subscriptions backend_template
  local voice_gateway_id asterisk_ari_port asterisk_application asterisk_argument asterisk_context asterisk_endpoint
  local asterisk_entity_id asterisk_version asterisk_policy_id asterisk_control_user asterisk_inspect_user
  local asterisk_control_password asterisk_inspect_password sip_recovery_store sip_media_udp_min sip_media_udp_max
  local asterisk_outgoing_endpoint sip_dial_outgoing
  binary="$(escape_sed_replacement "$INSTALLED_DJONEHUB_BINARY")"
  public_host="$(escape_sed_replacement "$PUBLIC_HOST")"
  turn_host="$(escape_sed_replacement "$TURN_HOST")"
  push_subject="$(escape_sed_replacement "https://$PUBLIC_HOST")"
  team="$(escape_sed_replacement "$TEAM_DOMAIN")"
  aud="$(escape_sed_replacement "$AUDIENCE")"
  allowlist="$(escape_sed_replacement "$INSTALLED_ALLOWLIST_FILE")"
  logs="$(escape_sed_replacement "$LOG_DIR")"
  sms="$(escape_sed_replacement "$SMS_STORE_DIR")"
  cloudflared="$(escape_sed_replacement "$INSTALLED_CLOUDFLARED_BINARY")"
  token="$(escape_sed_replacement "$INSTALLED_TOKEN_FILE")"
  helper="$(escape_sed_replacement "$INSTALLED_MEDIA_HELPER_BINARY")"
  turn_secret="$(escape_sed_replacement "$INSTALLED_TURN_SECRET_FILE")"
  vapid_private_key="$(escape_sed_replacement "$INSTALLED_VAPID_PRIVATE_KEY_FILE")"
  push_subscriptions="$(escape_sed_replacement "$PUSH_SUBSCRIPTIONS_FILE")"
  voice_gateway_id="$(escape_sed_replacement "$VOICE_GATEWAY_ID")"
  asterisk_ari_port="$(escape_sed_replacement "$PBX_ARI_PORT")"
  asterisk_application="$(escape_sed_replacement "$PBX_APPLICATION")"
  asterisk_argument="$(escape_sed_replacement "$PBX_ARGUMENT")"
  asterisk_context="$(escape_sed_replacement "$PBX_CONTEXT")"
  asterisk_endpoint="$(escape_sed_replacement "$PBX_ENDPOINT")"
  asterisk_entity_id="$(escape_sed_replacement "$PBX_ENTITY_ID")"
  asterisk_version="$(escape_sed_replacement "$PBX_VERSION")"
  asterisk_policy_id="$(escape_sed_replacement "$PBX_POLICY_ID")"
  asterisk_control_user="$(escape_sed_replacement "$PBX_CONTROL_USER")"
  asterisk_inspect_user="$(escape_sed_replacement "$PBX_INSPECT_USER")"
  asterisk_control_password="$(escape_sed_replacement "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE")"
  asterisk_inspect_password="$(escape_sed_replacement "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE")"
  sip_recovery_store="$(escape_sed_replacement "$SIP_RECOVERY_STORE_OPTION")"
  sip_media_udp_min="$(escape_sed_replacement "$SIP_MEDIA_UDP_MIN")"
  sip_media_udp_max="$(escape_sed_replacement "$SIP_MEDIA_UDP_MAX")"
  asterisk_outgoing_endpoint="$(escape_sed_replacement "$ASTERISK_OUTGOING_ENDPOINT")"
  sip_dial_outgoing="$(escape_sed_replacement "$SIP_DIAL_OUTGOING")"
  backend_template="$TEMPLATE_BACKEND"
  if [[ "$PROFILE" == "direct-voice" ]]; then
    backend_template="$TEMPLATE_DIRECT_BACKEND"
  elif [[ "$PROFILE" == "external-sip" ]]; then
    backend_template="$TEMPLATE_EXTERNAL_BACKEND"
  fi

  /usr/bin/sed \
    -e "s|__DJONEHUB_BINARY__|$binary|g" \
    -e "s|__PUBLIC_WEB_HOST__|$public_host|g" \
    -e "s|__TURN_HOST__|$turn_host|g" \
    -e "s|__PUSH_SUBJECT__|$push_subject|g" \
    -e "s|__CLOUDFLARE_TEAM_DOMAIN__|$team|g" \
    -e "s|__CLOUDFLARE_ACCESS_AUD__|$aud|g" \
    -e "s|__ALLOWED_EMAILS_FILE__|$allowlist|g" \
    -e "s|__LOG_DIR__|$logs|g" \
    -e "s|__SMS_STORE_DIR__|$sms|g" \
    -e "s|__TURN_SECRET_FILE__|$turn_secret|g" \
    -e "s|__VAPID_PRIVATE_KEY_FILE__|$vapid_private_key|g" \
    -e "s|__PUSH_SUBSCRIPTIONS_FILE__|$push_subscriptions|g" \
    -e "s|__VOICE_GATEWAY_ID__|$voice_gateway_id|g" \
    -e "s|__ASTERISK_ARI_PORT__|$asterisk_ari_port|g" \
    -e "s|__ASTERISK_APPLICATION__|$asterisk_application|g" \
    -e "s|__ASTERISK_ARGUMENT__|$asterisk_argument|g" \
    -e "s|__ASTERISK_CONTEXT__|$asterisk_context|g" \
    -e "s|__ASTERISK_ENDPOINT__|$asterisk_endpoint|g" \
    -e "s|__ASTERISK_OUTGOING_ENDPOINT__|$asterisk_outgoing_endpoint|g" \
    -e "s|__ASTERISK_ENTITY_ID__|$asterisk_entity_id|g" \
    -e "s|__ASTERISK_VERSION__|$asterisk_version|g" \
    -e "s|__ASTERISK_POLICY_ID__|$asterisk_policy_id|g" \
    -e "s|__ASTERISK_CONTROL_USER__|$asterisk_control_user|g" \
    -e "s|__ASTERISK_INSPECT_USER__|$asterisk_inspect_user|g" \
    -e "s|__ASTERISK_CONTROL_PASSWORD_FILE__|$asterisk_control_password|g" \
    -e "s|__ASTERISK_INSPECT_PASSWORD_FILE__|$asterisk_inspect_password|g" \
    -e "s|__SIP_RECOVERY_STORE__|$sip_recovery_store|g" \
    -e "s|__SIP_MEDIA_UDP_MIN__|$sip_media_udp_min|g" \
    -e "s|__SIP_MEDIA_UDP_MAX__|$sip_media_udp_max|g" \
    -e "s|__SIP_DIAL_OUTGOING__|$sip_dial_outgoing|g" \
    "$backend_template" > "$STAGE_DIR/$BACKEND_LABEL.plist"

  if [[ "$PROFILE" == "direct-voice" ]]; then
    /usr/bin/sed \
      -e "s|__MEDIA_HELPER_BINARY__|$helper|g" \
      -e "s|__LOG_DIR__|$logs|g" \
      "$TEMPLATE_HELPER" > "$STAGE_DIR/$HELPER_LABEL.plist"
  fi

  /usr/bin/sed \
    -e "s|__CLOUDFLARED_BINARY__|$cloudflared|g" \
    -e "s|__CLOUDFLARED_TOKEN_FILE__|$token|g" \
    -e "s|__LOG_DIR__|$logs|g" \
    "$TEMPLATE_TUNNEL" > "$STAGE_DIR/$TUNNEL_LABEL.plist"

  local rendered_plists=("$STAGE_DIR/$BACKEND_LABEL.plist" "$STAGE_DIR/$TUNNEL_LABEL.plist")
  if [[ "$PROFILE" == "direct-voice" ]]; then
    rendered_plists+=("$STAGE_DIR/$HELPER_LABEL.plist")
  fi
  if /usr/bin/grep -Eq '__[A-Z0-9_]+__' "${rendered_plists[@]}"; then
    die "rendered LaunchAgent still contains a template placeholder"
  fi
  if /usr/bin/grep -Fq -- "$REPO_ROOT" "${rendered_plists[@]}" || \
     /usr/bin/grep -Fq -- "$LEGACY_DIR" "${rendered_plists[@]}"; then
    die "rendered LaunchAgent still depends on the repository or legacy runtime"
  fi
  /usr/bin/plutil -lint "${rendered_plists[@]}" >/dev/null
}

render_plists

note "CHECK PASS: $PROFILE profile sources, arm64 binaries, private configuration, SMS merge and LaunchAgents are valid."
note "  profile: $PROFILE"
note "  public hostname: $PUBLIC_HOST"
if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
  note "  TURN hostname: $TURN_HOST"
fi
note "  production runtime: $RUNTIME_DIR"
note "  DJOneHub binary: would $BINARY_ACTION in private bin/"
note "  cloudflared binary: would $CLOUDFLARED_ACTION in private bin/"
note "  allowlist: would $ALLOWLIST_ACTION in private config/"
note "  Tunnel token: would $TOKEN_ACTION in private config/ (content hidden)"
if [[ "$PROFILE" == "direct-voice" ]]; then
  note "  media helper: would $HELPER_ACTION in private bin/"
fi
if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
  note "  TURN shared secret: would $TURN_SECRET_ACTION in private config/ (content hidden)"
  note "  VAPID private key: would $VAPID_PRIVATE_KEY_ACTION in private config/ (content hidden)"
  note "  Web Push subscriptions: forward data at private data/push/; never overwritten or rolled back"
fi
if [[ "$PROFILE" == "external-sip" ]]; then
  note "  Asterisk deployment.env: would $ASTERISK_DEPLOYMENT_ENV_ACTION in private config/"
  note "  Asterisk control password: would $ASTERISK_CONTROL_PASSWORD_ACTION in private config/ (content hidden)"
  note "  Asterisk inspect password: would $ASTERISK_INSPECT_PASSWORD_ACTION in private config/ (content hidden)"
  note "  external voice recovery: forward data at private data/external-voice/; never overwritten or rolled back"
  note "  QDC media helper: disabled and not installed or started"
fi
if [[ -d "$LEGACY_SMS_STORE" ]]; then
  note "  SMS migration: $SMS_COPY_COUNT missing file(s) would be copied; $SMS_MATCH_COUNT existing file(s) matched"
else
  note "  SMS migration: no legacy store found; installed data remains unchanged"
fi
note "  LaunchAgents: rendered paths use Application Support, never Documents runtime files"
note "  sleep behavior: /usr/bin/caffeinate -s follows the backend while connected to AC power"

if [[ "$MODE" == "check" ]]; then
  note "DRY RUN ONLY: no production runtime, LaunchAgent, service, power setting or SMS data was changed."
  if [[ "$PROFILE" == "direct-voice" ]]; then
    note "Run the same command with explicit 'apply' to install and start the three services."
  else
    note "Run the same command with explicit 'apply' to install and start the two services."
  fi
  exit 0
fi

ensure_mode_700_dir() {
  local path="$1"
  if [[ ! -e "$path" ]]; then
    /usr/bin/install -d -m 700 "$path"
  fi
  directory_is_mode_700 "$path" || die "private directory validation failed after creation: $path"
}

for target_dir in "$RUNTIME_DIR" "$BIN_DIR" "$CONFIG_DIR" "$DATA_DIR" "$LOG_DIR" "$SMS_STORE_DIR"; do
  ensure_mode_700_dir "$target_dir"
done
if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
  ensure_mode_700_dir "$PUSH_DATA_DIR"
  if [[ -e "$PUSH_SUBSCRIPTIONS_FILE" ]]; then
    validate_private_file "$PUSH_SUBSCRIPTIONS_FILE" "Web Push subscriptions database"
  fi
fi
if [[ "$PROFILE" == "external-sip" ]]; then
  ensure_mode_700_dir "$EXTERNAL_VOICE_DATA_DIR"
fi
if [[ ! -e "$LAUNCH_AGENTS_DIR" ]]; then
  /usr/bin/install -d -m 700 "$LAUNCH_AGENTS_DIR"
fi
directory_is_safe_user_dir "$LAUNCH_AGENTS_DIR" || \
  die "LaunchAgents directory permission validation failed: $LAUNCH_AGENTS_DIR"

readonly BACKEND_PLIST="$LAUNCH_AGENTS_DIR/$BACKEND_LABEL.plist"
readonly HELPER_PLIST="$LAUNCH_AGENTS_DIR/$HELPER_LABEL.plist"
readonly TUNNEL_PLIST="$LAUNCH_AGENTS_DIR/$TUNNEL_LABEL.plist"

plist_label() {
  /usr/libexec/PlistBuddy -c 'Print :Label' "$1" 2>/dev/null
}

for pair in "$BACKEND_PLIST:$BACKEND_LABEL" "$HELPER_PLIST:$HELPER_LABEL" "$TUNNEL_PLIST:$TUNNEL_LABEL"; do
  installed_path="${pair%%:*}"
  expected_label="${pair#*:}"
  if [[ -e "$installed_path" ]]; then
    validate_regular_user_file "$installed_path" "installed LaunchAgent"
    [[ "$(plist_label "$installed_path")" == "$expected_label" ]] || \
      die "installed plist label does not match its filename: $installed_path"
  fi
done

LAUNCHCTL_BIN="/bin/launchctl"
CURL_BIN="/usr/bin/curl"
SLEEP_BIN="/bin/sleep"
if [[ "${DJONEHUB_INSTALLER_TESTING:-0}" == "1" ]]; then
  case "$RUNTIME_DIR/" in
    "$HOME/"*) ;;
    *) die "test runtime must remain below the temporary HOME" ;;
  esac
  LAUNCHCTL_BIN="${DJONEHUB_INSTALLER_TEST_LAUNCHCTL:-$LAUNCHCTL_BIN}"
  CURL_BIN="${DJONEHUB_INSTALLER_TEST_CURL:-$CURL_BIN}"
  SLEEP_BIN="${DJONEHUB_INSTALLER_TEST_SLEEP:-$SLEEP_BIN}"
fi
for testable_tool in "$LAUNCHCTL_BIN" "$CURL_BIN" "$SLEEP_BIN"; do
  [[ "$testable_tool" == /* && -x "$testable_tool" && ! -L "$testable_tool" ]] || \
    die "installer service tool must be an absolute executable non-symlink file: $testable_tool"
done

BACKEND_WAS_LOADED=0
HELPER_WAS_LOADED=0
TUNNEL_WAS_LOADED=0
if "$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$BACKEND_LABEL" >/dev/null 2>&1; then BACKEND_WAS_LOADED=1; fi
if "$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$HELPER_LABEL" >/dev/null 2>&1; then HELPER_WAS_LOADED=1; fi
if "$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$TUNNEL_LABEL" >/dev/null 2>&1; then TUNNEL_WAS_LOADED=1; fi
(( BACKEND_WAS_LOADED == 0 )) || [[ -f "$BACKEND_PLIST" ]] || \
  die "backend label is loaded from an unknown plist; stop and inspect it manually"
(( HELPER_WAS_LOADED == 0 )) || [[ -f "$HELPER_PLIST" ]] || \
  die "media helper label is loaded from an unknown plist; stop and inspect it manually"
(( TUNNEL_WAS_LOADED == 0 )) || [[ -f "$TUNNEL_PLIST" ]] || \
  die "Tunnel label is loaded from an unknown plist; stop and inspect it manually"

BINARY_HAD_FILE=0
HELPER_HAD_FILE=0
CLOUDFLARED_HAD_FILE=0
ALLOWLIST_HAD_FILE=0
TOKEN_HAD_FILE=0
TURN_SECRET_HAD_FILE=0
VAPID_PRIVATE_KEY_HAD_FILE=0
ASTERISK_DEPLOYMENT_ENV_HAD_FILE=0
ASTERISK_CONTROL_PASSWORD_HAD_FILE=0
ASTERISK_INSPECT_PASSWORD_HAD_FILE=0
BACKEND_HAD_FILE=0
HELPER_PLIST_HAD_FILE=0
TUNNEL_HAD_FILE=0

snapshot_optional() {
  local path="$1"
  local backup_name="$2"
  local flag_variable="$3"
  if [[ -e "$path" ]]; then
    validate_regular_user_file "$path" "rollback snapshot source"
    /bin/cp -p "$path" "$STAGE_DIR/$backup_name"
    printf -v "$flag_variable" '%s' 1
  fi
}

snapshot_optional "$INSTALLED_DJONEHUB_BINARY" binary.previous BINARY_HAD_FILE
snapshot_optional "$INSTALLED_MEDIA_HELPER_BINARY" helper.previous HELPER_HAD_FILE
snapshot_optional "$INSTALLED_CLOUDFLARED_BINARY" cloudflared.previous CLOUDFLARED_HAD_FILE
snapshot_optional "$INSTALLED_ALLOWLIST_FILE" allowlist.previous ALLOWLIST_HAD_FILE
snapshot_optional "$INSTALLED_TOKEN_FILE" token.previous TOKEN_HAD_FILE
snapshot_optional "$INSTALLED_TURN_SECRET_FILE" turn-secret.previous TURN_SECRET_HAD_FILE
if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
  snapshot_optional "$INSTALLED_VAPID_PRIVATE_KEY_FILE" vapid-private-key.previous VAPID_PRIVATE_KEY_HAD_FILE
fi
if [[ "$PROFILE" == "external-sip" ]]; then
  snapshot_optional "$INSTALLED_ASTERISK_DEPLOYMENT_ENV" asterisk-deployment.previous.env ASTERISK_DEPLOYMENT_ENV_HAD_FILE
  snapshot_optional "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" asterisk-control-password.previous ASTERISK_CONTROL_PASSWORD_HAD_FILE
  snapshot_optional "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" asterisk-inspect-password.previous ASTERISK_INSPECT_PASSWORD_HAD_FILE
fi
snapshot_optional "$BACKEND_PLIST" backend.previous.plist BACKEND_HAD_FILE
snapshot_optional "$HELPER_PLIST" helper.previous.plist HELPER_PLIST_HAD_FILE
snapshot_optional "$TUNNEL_PLIST" tunnel.previous.plist TUNNEL_HAD_FILE

atomic_install() {
  local source="$1"
  local destination="$2"
  local mode="$3"
  local parent temporary
  parent="$(/usr/bin/dirname "$destination")"
  temporary="$(/usr/bin/mktemp "$parent/.djonehub-install.XXXXXX")"
  if ! /bin/cp "$source" "$temporary" || ! /bin/chmod "$mode" "$temporary" || ! /bin/mv -f "$temporary" "$destination"; then
    /bin/rm -f -- "$temporary"
    return 1
  fi
}

atomic_restore() {
  local source="$1"
  local destination="$2"
  local parent temporary
  parent="$(/usr/bin/dirname "$destination")"
  temporary="$(/usr/bin/mktemp "$parent/.djonehub-restore.XXXXXX")"
  if ! /bin/cp -p "$source" "$temporary" || ! /bin/mv -f "$temporary" "$destination"; then
    /bin/rm -f -- "$temporary"
    return 1
  fi
}

install_if_changed() {
  local source="$1"
  local destination="$2"
  local mode="$3"
  if [[ -f "$destination" ]] && /usr/bin/cmp -s "$source" "$destination" && [[ "$(file_mode "$destination")" == "$mode" ]]; then
    return 0
  fi
  atomic_install "$source" "$destination" "$mode"
}

atomic_install_new_sms_file() {
  local source="$1"
  local destination="$2"
  local parent temporary
  parent="$(/usr/bin/dirname "$destination")"
  temporary="$(/usr/bin/mktemp "$parent/.djonehub-sms.XXXXXX")"
  if ! /bin/cp "$source" "$temporary" || ! /bin/chmod 600 "$temporary"; then
    /bin/rm -f -- "$temporary"
    return 1
  fi
  if /bin/ln "$temporary" "$destination" 2>/dev/null; then
    /bin/rm -f -- "$temporary"
    return 0
  fi
  /bin/rm -f -- "$temporary"
  [[ -f "$destination" && ! -L "$destination" ]] && /usr/bin/cmp -s "$source" "$destination"
}

copy_sms_incremental() {
  local source="$1"
  local path relative destination
  [[ -d "$source" ]] || return 0
  [[ "$source" != "$SMS_STORE_DIR" ]] || return 0

  while IFS= read -r -d '' path; do
    relative="${path#"$source"/}"
    destination="$SMS_STORE_DIR/$relative"
    if [[ -d "$path" ]]; then
      if [[ ! -e "$destination" ]]; then
        /usr/bin/install -d -m 700 "$destination"
      fi
      directory_is_private "$destination" || return 1
    fi
  done < <(/usr/bin/find -P "$source" -mindepth 1 -type d -print0)

  while IFS= read -r -d '' path; do
    relative="${path#"$source"/}"
    destination="$SMS_STORE_DIR/$relative"
    if [[ -e "$destination" ]]; then
      [[ -f "$destination" && ! -L "$destination" ]] || return 1
      /usr/bin/cmp -s "$path" "$destination" || return 1
    else
      atomic_install_new_sms_file "$path" "$destination" || return 1
    fi
  done < <(/usr/bin/find -P "$source" -mindepth 1 -type f -print0)
}

label_running() {
  local label="$1"
  local state
  state="$("$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$label" 2>/dev/null)" || return 1
  printf '%s\n' "$state" | /usr/bin/grep -Eq 'state = running' || return 1
  printf '%s\n' "$state" | /usr/bin/grep -Eq 'pid = [1-9][0-9]*' || return 1
}

media_control_socket_ready() {
  [[ -S "$MEDIA_CONTROL_SOCKET" && ! -L "$MEDIA_CONTROL_SOCKET" ]] || return 1
  [[ "$(file_uid "$MEDIA_CONTROL_SOCKET")" == "$USER_UID" ]] || return 1
  [[ "$(file_mode "$MEDIA_CONTROL_SOCKET")" == "600" ]]
}

backend_ready() {
  label_running "$BACKEND_LABEL" || return 1
  local status
  status="$("$CURL_BIN" --silent --show-error --output /dev/null --write-out '%{http_code}' \
    --max-time 2 --header "Host: $PUBLIC_HOST" http://127.0.0.1:7578/ 2>/dev/null || true)"
  [[ "$status" == "401" ]] || return 1
  if [[ "$PROFILE" == "direct-voice" ]]; then
    media_control_socket_ready || return 1
  fi
}

wait_for_media_control_socket() {
  local attempt
  # A real QDC507 USB re-claim can take slightly over 30 seconds after the old
  # backend releases its vendor-specific interfaces. Keep the transaction
  # bounded, but do not roll back a healthy replacement during that normal
  # enumeration window.
  for attempt in $(/usr/bin/seq 1 60); do
    if media_control_socket_ready; then return 0; fi
    "$SLEEP_BIN" 1
  done
  return 1
}

wait_for_backend() {
  local attempt
  for attempt in $(/usr/bin/seq 1 60); do
    if backend_ready; then return 0; fi
    "$SLEEP_BIN" 1
  done
  return 1
}

wait_for_label() {
  local label="$1"
  local attempt
  for attempt in $(/usr/bin/seq 1 20); do
    if label_running "$label"; then
      "$SLEEP_BIN" 3
      label_running "$label" && return 0
    fi
    "$SLEEP_BIN" 1
  done
  return 1
}

bootstrap_label() {
  local label="$1"
  local plist="$2"
  local attempt
  # launchd can remove a job from `print` slightly before the old process and
  # its XPC bookkeeping have fully drained. On this Mac an immediate bootstrap
  # then returns EIO, while the same valid plist succeeds a moment later.
  for attempt in $(/usr/bin/seq 1 15); do
    if "$LAUNCHCTL_BIN" bootstrap "$LAUNCH_DOMAIN" "$plist" >/dev/null 2>&1; then
      return 0
    fi
    "$SLEEP_BIN" 1
  done
  "$LAUNCHCTL_BIN" bootstrap "$LAUNCH_DOMAIN" "$plist"
}

restore_or_remove() {
  local had_file="$1"
  local backup="$2"
  local destination="$3"
  if (( had_file == 1 )); then
    atomic_restore "$STAGE_DIR/$backup" "$destination"
  else
    /bin/rm -f -- "$destination"
  fi
}

rollback() {
  local rollback_failed=0
  set +e
  "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$TUNNEL_LABEL" >/dev/null 2>&1
  "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$HELPER_LABEL" >/dev/null 2>&1
  "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$BACKEND_LABEL" >/dev/null 2>&1

  restore_or_remove "$BINARY_HAD_FILE" binary.previous "$INSTALLED_DJONEHUB_BINARY" || rollback_failed=1
  restore_or_remove "$HELPER_HAD_FILE" helper.previous "$INSTALLED_MEDIA_HELPER_BINARY" || rollback_failed=1
  restore_or_remove "$CLOUDFLARED_HAD_FILE" cloudflared.previous "$INSTALLED_CLOUDFLARED_BINARY" || rollback_failed=1
  restore_or_remove "$ALLOWLIST_HAD_FILE" allowlist.previous "$INSTALLED_ALLOWLIST_FILE" || rollback_failed=1
  restore_or_remove "$TOKEN_HAD_FILE" token.previous "$INSTALLED_TOKEN_FILE" || rollback_failed=1
  restore_or_remove "$TURN_SECRET_HAD_FILE" turn-secret.previous "$INSTALLED_TURN_SECRET_FILE" || rollback_failed=1
  if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
    restore_or_remove "$VAPID_PRIVATE_KEY_HAD_FILE" vapid-private-key.previous "$INSTALLED_VAPID_PRIVATE_KEY_FILE" || rollback_failed=1
  fi
  if [[ "$PROFILE" == "external-sip" ]]; then
    restore_or_remove "$ASTERISK_DEPLOYMENT_ENV_HAD_FILE" asterisk-deployment.previous.env "$INSTALLED_ASTERISK_DEPLOYMENT_ENV" || rollback_failed=1
    restore_or_remove "$ASTERISK_CONTROL_PASSWORD_HAD_FILE" asterisk-control-password.previous "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" || rollback_failed=1
    restore_or_remove "$ASTERISK_INSPECT_PASSWORD_HAD_FILE" asterisk-inspect-password.previous "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" || rollback_failed=1
  fi
  restore_or_remove "$BACKEND_HAD_FILE" backend.previous.plist "$BACKEND_PLIST" || rollback_failed=1
  restore_or_remove "$HELPER_PLIST_HAD_FILE" helper.previous.plist "$HELPER_PLIST" || rollback_failed=1
  restore_or_remove "$TUNNEL_HAD_FILE" tunnel.previous.plist "$TUNNEL_PLIST" || rollback_failed=1

  if (( BACKEND_WAS_LOADED == 1 && BACKEND_HAD_FILE == 1 )); then
    bootstrap_label "$BACKEND_LABEL" "$BACKEND_PLIST" >/dev/null 2>&1 || rollback_failed=1
  fi
  if (( HELPER_WAS_LOADED == 1 && HELPER_PLIST_HAD_FILE == 1 )); then
    wait_for_media_control_socket >/dev/null 2>&1 || rollback_failed=1
    "$LAUNCHCTL_BIN" bootstrap "$LAUNCH_DOMAIN" "$HELPER_PLIST" >/dev/null 2>&1 || rollback_failed=1
  fi
  if (( TUNNEL_WAS_LOADED == 1 && TUNNEL_HAD_FILE == 1 )); then
    "$LAUNCHCTL_BIN" bootstrap "$LAUNCH_DOMAIN" "$TUNNEL_PLIST" >/dev/null 2>&1 || rollback_failed=1
  fi
  set -e
  if (( rollback_failed == 0 )); then
    note "Rollback complete: previous runtime files and LaunchAgents were restored; SMS stores and Web Push subscriptions were not overwritten or deleted." >&2
  else
    note "Rollback was incomplete; inspect the named runtime files and LaunchAgents. SMS stores and Web Push subscriptions were not overwritten or deleted." >&2
  fi
}

transaction() {
  set -e
  install_if_changed "$STAGE_DIR/djonehub-macos.arm64-cgo" "$INSTALLED_DJONEHUB_BINARY" 700
  install_if_changed "$STAGE_DIR/cloudflared" "$INSTALLED_CLOUDFLARED_BINARY" 700
  install_if_changed "$STAGE_DIR/access-allowed-emails" "$INSTALLED_ALLOWLIST_FILE" 600
  install_if_changed "$STAGE_DIR/cloudflared-token" "$INSTALLED_TOKEN_FILE" 600
  if [[ "$PROFILE" == "direct-voice" ]]; then
    install_if_changed "$STAGE_DIR/DJOneHubNotifier" "$INSTALLED_MEDIA_HELPER_BINARY" 700
  fi
  if [[ "$PROFILE" == "direct-voice" || "$PROFILE" == "external-sip" ]]; then
    install_if_changed "$STAGE_DIR/public-web-turn-secret" "$INSTALLED_TURN_SECRET_FILE" 600
    install_if_changed "$STAGE_DIR/public-web-vapid-private-key" "$INSTALLED_VAPID_PRIVATE_KEY_FILE" 600
  fi
  if [[ "$PROFILE" == "external-sip" ]]; then
    install_if_changed "$STAGE_DIR/asterisk-deployment.env" "$INSTALLED_ASTERISK_DEPLOYMENT_ENV" 600
    install_if_changed "$STAGE_DIR/asterisk-ari-control.password" "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" 600
    install_if_changed "$STAGE_DIR/asterisk-ari-inspect.password" "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" 600
  fi
  validate_executable "$INSTALLED_DJONEHUB_BINARY" "installed DJOneHub arm64-cgo binary" "yes"
  validate_executable "$INSTALLED_CLOUDFLARED_BINARY" "installed cloudflared binary" "yes"
  validate_private_file "$INSTALLED_ALLOWLIST_FILE" "installed email allowlist"
  validate_private_file "$INSTALLED_TOKEN_FILE" "installed Tunnel token"
  if [[ "$PROFILE" == "direct-voice" ]]; then
    validate_direct_voice_binary_contract "$INSTALLED_DJONEHUB_BINARY"
    validate_executable "$INSTALLED_MEDIA_HELPER_BINARY" "installed DJOneHubNotifier media helper" "yes"
    "$INSTALLED_MEDIA_HELPER_BINARY" --self-test >/dev/null 2>&1
    validate_private_file "$INSTALLED_TURN_SECRET_FILE" "installed TURN shared-secret"
    validate_turn_secret_file_content "$INSTALLED_TURN_SECRET_FILE"
    validate_private_file "$INSTALLED_VAPID_PRIVATE_KEY_FILE" "installed VAPID private key"
    validate_vapid_private_key_file_content "$INSTALLED_VAPID_PRIVATE_KEY_FILE"
    if [[ -e "$PUSH_SUBSCRIPTIONS_FILE" ]]; then
      validate_private_file "$PUSH_SUBSCRIPTIONS_FILE" "Web Push subscriptions database"
    fi
  elif [[ "$PROFILE" == "external-sip" ]]; then
    validate_external_sip_binary_contract "$INSTALLED_DJONEHUB_BINARY"
    validate_private_file "$INSTALLED_TURN_SECRET_FILE" "installed TURN shared-secret"
    validate_turn_secret_file_content "$INSTALLED_TURN_SECRET_FILE"
    validate_private_file "$INSTALLED_VAPID_PRIVATE_KEY_FILE" "installed VAPID private key"
    validate_vapid_private_key_file_content "$INSTALLED_VAPID_PRIVATE_KEY_FILE"
    validate_private_single_link_file "$INSTALLED_ASTERISK_DEPLOYMENT_ENV" "installed Asterisk deployment.env"
    validate_private_single_link_file "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" "installed Asterisk control password"
    validate_private_single_link_file "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" "installed Asterisk inspect password"
    validate_asterisk_deployment_env_content "$INSTALLED_ASTERISK_DEPLOYMENT_ENV"
    validate_asterisk_password_content "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" "installed Asterisk ARI control password"
    validate_asterisk_password_content "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" "installed Asterisk ARI inspect password"
    /usr/bin/cmp -s "$INSTALLED_ASTERISK_CONTROL_PASSWORD_FILE" "$INSTALLED_ASTERISK_INSPECT_PASSWORD_FILE" && \
      die "installed Asterisk control and inspect passwords must differ"
    if [[ -e "$PUSH_SUBSCRIPTIONS_FILE" ]]; then
      validate_private_file "$PUSH_SUBSCRIPTIONS_FILE" "Web Push subscriptions database"
    fi
  fi

  # Quiesce the legacy writer before taking the final migration snapshot. A
  # successful bootout is verified through launchctl state; copying a live
  # append-only store would not be a coherent migration.
  "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$TUNNEL_LABEL" >/dev/null 2>&1 || true
  "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$HELPER_LABEL" >/dev/null 2>&1 || true
  "$LAUNCHCTL_BIN" bootout "$LAUNCH_DOMAIN/$BACKEND_LABEL" >/dev/null 2>&1 || true
  ! "$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$TUNNEL_LABEL" >/dev/null 2>&1
  ! "$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$HELPER_LABEL" >/dev/null 2>&1
  ! "$LAUNCHCTL_BIN" print "$LAUNCH_DOMAIN/$BACKEND_LABEL" >/dev/null 2>&1

  # SMS migration is forward-only. Existing destination files are byte-checked,
  # never replaced. Source files are never changed or removed. Newly copied SMS
  # data intentionally survives a later service rollback.
  SMS_COPY_COUNT=0
  SMS_MATCH_COUNT=0
  preflight_sms_migration "$LEGACY_SMS_STORE"
  copy_sms_incremental "$LEGACY_SMS_STORE"

  atomic_install "$STAGE_DIR/$BACKEND_LABEL.plist" "$BACKEND_PLIST" 600
  atomic_install "$STAGE_DIR/$TUNNEL_LABEL.plist" "$TUNNEL_PLIST" 600
  if [[ "$PROFILE" == "direct-voice" ]]; then
    atomic_install "$STAGE_DIR/$HELPER_LABEL.plist" "$HELPER_PLIST" 600
    /usr/bin/plutil -lint "$BACKEND_PLIST" "$HELPER_PLIST" "$TUNNEL_PLIST" >/dev/null
  else
    /bin/rm -f -- "$HELPER_PLIST"
    /usr/bin/plutil -lint "$BACKEND_PLIST" "$TUNNEL_PLIST" >/dev/null
  fi

  bootstrap_label "$BACKEND_LABEL" "$BACKEND_PLIST"
  wait_for_backend
  if [[ "$PROFILE" == "direct-voice" ]]; then
    "$LAUNCHCTL_BIN" bootstrap "$LAUNCH_DOMAIN" "$HELPER_PLIST"
    wait_for_label "$HELPER_LABEL"
  fi
  "$LAUNCHCTL_BIN" bootstrap "$LAUNCH_DOMAIN" "$TUNNEL_PLIST"
  wait_for_label "$TUNNEL_LABEL"
  backend_ready
  if [[ "$PROFILE" == "direct-voice" ]]; then
    label_running "$HELPER_LABEL"
    media_control_socket_ready
  fi
}

set +e
( trap - EXIT HUP INT TERM; transaction )
transaction_status=$?
set -e
if (( transaction_status != 0 )); then
  note "APPLY FAILED: the installed runtime or services did not pass validation." >&2
  note "Logs, forward-copied SMS data, and Web Push subscriptions were preserved at $RUNTIME_DIR." >&2
  rollback
  exit 1
fi

if [[ "$PROFILE" == "direct-voice" ]]; then
  note "APPLY PASS: runtime files are installed under Application Support and all three LaunchAgents are healthy."
else
  note "APPLY PASS: runtime files are installed under Application Support and both LaunchAgents are healthy."
fi
note "  profile: $PROFILE"
note "  backend label: $BACKEND_LABEL"
if [[ "$PROFILE" == "direct-voice" ]]; then
  note "  media helper label: $HELPER_LABEL"
fi
note "  Tunnel label: $TUNNEL_LABEL"
note "  runtime: $RUNTIME_DIR"
note "  logs: $LOG_DIR"
note "SMS data is at $SMS_STORE_DIR; legacy and installed SMS files were never overwritten or deleted."
if [[ "$PROFILE" == "direct-voice" ]]; then
  note "Web Push subscriptions are forward data at $PUSH_SUBSCRIPTIONS_FILE; install rollback never overwrites or deletes them."
fi
