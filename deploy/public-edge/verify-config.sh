#!/usr/bin/env bash
set -euo pipefail

umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
COMPOSE="$SCRIPT_DIR/compose.yaml"
EDGE_TEMPLATE="$SCRIPT_DIR/templates/edge.env.in"
TURN_TEMPLATE="$SCRIPT_DIR/templates/turnserver.conf.in"
COMPOSE_POLICY="$SCRIPT_DIR/verify-compose-policy.py"
TOOLCHAIN_LOCK="$SCRIPT_DIR/image/toolchain.lock"
ROOT=""
DECLARED_ROOT=""
COMPOSE_ONLY=0
COMPOSE_MODE=auto
QUIET=0
IMMUTABLE_SNAPSHOT=0
CURRENT_UID=$(id -u)
CURRENT_GID=$(id -g)
TMP_WORK=""
DOCKER_CONFIG_DIR=""
COMPOSE_CMD=()
PYTHON_BIN=""

usage() {
  cat <<'EOF'
Usage:
  verify-config.sh [--compose /absolute/compose.yaml] [--compose-mode MODE] [--immutable-snapshot] [--quiet] /absolute/runtime/root
  verify-config.sh --compose-only [--compose /absolute/compose.yaml] [--compose-mode MODE] [--quiet]

--declared-root is reserved for generator/snapshot atomic staging checks.
--immutable-snapshot requires the private 0500/0400 edge runtime snapshot used
by start-edge; it never relaxes the file/tree/content allowlist.
The verifier performs only local checks. It invokes exactly Docker Compose
the platform-specific hash-locked Compose parser, but never contacts the Docker
daemon or network and never prints secret values. MODE is auto or production;
state-changing deployment always selects the exact production system plugin.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [ -n "$TMP_WORK" ]; then
    case "$TMP_WORK" in
      /tmp/dji4g-public-edge-verify.*|"${TMPDIR:-/tmp}"/dji4g-public-edge-verify.*)
        rm -rf -- "$TMP_WORK"
        ;;
    esac
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

while [ "$#" -gt 0 ]; do
  case "$1" in
    --compose)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--compose requires a value"
      COMPOSE=$2; shift 2 ;;
    --compose-only)
      COMPOSE_ONLY=1; shift ;;
    --compose-mode)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--compose-mode requires a value"
      COMPOSE_MODE=$2; shift 2 ;;
    --immutable-snapshot)
      IMMUTABLE_SNAPSHOT=1; shift ;;
    --declared-root)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--declared-root requires a value"
      DECLARED_ROOT=$2; shift 2 ;;
    --quiet)
      QUIET=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    --*)
      fail "unknown argument: $1" ;;
    *)
      [ -z "$ROOT" ] || fail "only one runtime root may be supplied"
      ROOT=$1; shift ;;
  esac
done

case "$COMPOSE_MODE" in auto|production) ;; *) fail "invalid --compose-mode" ;; esac

[ -f "$COMPOSE" ] && [ ! -L "$COMPOSE" ] || fail "compose file must be a regular, non-symlink file"
[ -f "$EDGE_TEMPLATE" ] && [ ! -L "$EDGE_TEMPLATE" ] || fail "edge template is missing or symlinked"
[ -f "$TURN_TEMPLATE" ] && [ ! -L "$TURN_TEMPLATE" ] || fail "TURN template is missing or symlinked"
[ -f "$COMPOSE_POLICY" ] && [ ! -L "$COMPOSE_POLICY" ] || fail "Compose policy checker is missing or symlinked"
[ -f "$TOOLCHAIN_LOCK" ] && [ ! -L "$TOOLCHAIN_LOCK" ] || fail "toolchain.lock is missing or symlinked"

require_line() {
  local file=$1 line=$2 label=$3 count
  count=$(grep -Fxc -- "$line" "$file" || true)
  [ "$count" -eq 1 ] || fail "$label must appear exactly once"
}

reject_pattern() {
  local file=$1 pattern=$2 label=$3
  if grep -Eiq -- "$pattern" "$file"; then
    fail "$label"
  fi
}

validate_image() {
  local label=$1 value=$2 name last
  [[ "$value" =~ ^[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}$ ]] ||
    fail "$label must be an explicit lowercase name@sha256 digest"
  name=${value%@sha256:*}
  [[ "$name" == */* ]] || fail "$label must include an explicit registry or namespace"
  [[ "$name" != */ && "$name" != *//* && "$name" != *../* && "$name" != */..* ]] ||
    fail "$label contains an ambiguous repository path"
  last=${name##*/}
  [[ "$last" != *:* ]] || fail "$label must not combine a mutable tag with the digest"
}

validate_edge_image() {
  local value=$1
  if [[ "$value" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    return 0
  fi
  validate_image "DJI4G_EDGE_IMAGE" "$value"
}

validate_edge_identity() {
  local gateway_id=$1 team_domain=$2 audience=$3
  [[ "$gateway_id" =~ ^[a-z0-9]([a-z0-9_-]{0,62}[a-z0-9])?$ ]] ||
    fail "edge gateway id is invalid"
  [[ "$team_domain" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$ ]] ||
    fail "Access team domain is invalid"
  [ "${#audience}" -le 256 ] && [[ "$audience" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] ||
    fail "Access audience is invalid"
}

validate_integer() {
  local label=$1 value=$2
  [[ "$value" =~ ^[0-9]+$ ]] || fail "$label must be an unsigned integer"
  [ "$value" -ge 1 ] 2>/dev/null || fail "$label must be at least 1"
  [ "$value" -le 2147483647 ] 2>/dev/null || fail "$label is too large"
}

validate_abs_path() {
  local label=$1 value=$2
  case "$value" in
    /*) ;;
    *) fail "$label must be absolute" ;;
  esac
  [ "${#value}" -le 240 ] || fail "$label is too long"
  case "$value" in
    *[!A-Za-z0-9_./-]*) fail "$label contains unsupported path characters" ;;
    *//*|*/./*|*/../*|*/.|*/..) fail "$label is not a clean path" ;;
  esac
}

validate_ipv4() {
  local label=$1 value=$2 a b c d part
  IFS=. read -r a b c d <<<"$value"
  [ -n "${a:-}" ] && [ -n "${b:-}" ] && [ -n "${c:-}" ] && [ -n "${d:-}" ] ||
    fail "$label must contain four IPv4 octets"
  [ "$value" = "$a.$b.$c.$d" ] || fail "$label must be canonical IPv4"
  for part in "$a" "$b" "$c" "$d"; do
    [[ "$part" =~ ^(0|[1-9][0-9]{0,2})$ ]] || fail "$label has a non-canonical octet"
    [ "$part" -le 255 ] || fail "$label has an octet above 255"
  done
  [ "$a" -ne 0 ] && [ "$a" -ne 127 ] && [ "$a" -lt 224 ] ||
    fail "$label must be a unicast address"
}

validate_public_ipv4() {
  local label=$1 value=$2 a b c d
  validate_ipv4 "$label" "$value"
  IFS=. read -r a b c d <<<"$value"
  ! { [ "$a" -eq 10 ] ||
      { [ "$a" -eq 100 ] && [ "$b" -ge 64 ] && [ "$b" -le 127 ]; } ||
      { [ "$a" -eq 169 ] && [ "$b" -eq 254 ]; } ||
      { [ "$a" -eq 172 ] && [ "$b" -ge 16 ] && [ "$b" -le 31 ]; } ||
      { [ "$a" -eq 192 ] && [ "$b" -eq 168 ]; } ||
      { [ "$a" -eq 192 ] && [ "$b" -eq 0 ] && { [ "$c" -eq 0 ] || [ "$c" -eq 2 ]; }; } ||
      { [ "$a" -eq 198 ] && { [ "$b" -eq 18 ] || [ "$b" -eq 19 ]; }; } ||
      { [ "$a" -eq 198 ] && [ "$b" -eq 51 ] && [ "$c" -eq 100 ]; } ||
      { [ "$a" -eq 203 ] && [ "$b" -eq 0 ] && [ "$c" -eq 113 ]; }; } ||
    fail "$label must be a globally routable IPv4 address"
}

stat_mode() {
  if stat -f '%Lp' / >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

stat_uid() {
  if stat -f '%u' / >/dev/null 2>&1; then
    stat -f '%u' "$1"
  else
    stat -c '%u' "$1"
  fi
}

stat_nlink() {
  if stat -f '%l' / >/dev/null 2>&1; then
    stat -f '%l' "$1"
  else
    stat -c '%h' "$1"
  fi
}

check_mode_owner() {
  local path=$1 mode=$2 label=$3
  [ "$(stat_mode "$path")" = "$mode" ] || fail "$label must have mode 0$mode"
  [ "$(stat_uid "$path")" = "$CURRENT_UID" ] || fail "$label must be owned by uid $CURRENT_UID"
}

read_env_value() {
  local file=$1 key=$2 count line
  count=$(grep -Ec "^${key}=" "$file" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once"
  line=$(grep -E "^${key}=" "$file")
  printf '%s' "${line#*=}"
}

read_conf_value() {
  local file=$1 key=$2 count line
  count=$(grep -Ec "^${key}=" "$file" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once in turnserver.conf"
  line=$(grep -E "^${key}=" "$file")
  printf '%s' "${line#*=}"
}

read_single_line_secret() {
  local label=$1 path=$2 value line_count file_bytes value_bytes last_byte
  line_count=$(awk 'END { print NR + 0 }' "$path")
  [ "$line_count" -eq 1 ] || fail "$label must contain exactly one line"
  value=$(<"$path")
  [ -n "$value" ] || fail "$label is empty"
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || fail "$label contains a line break"
  file_bytes=$(wc -c <"$path" | tr -d ' ')
  value_bytes=$(printf '%s' "$value" | wc -c | tr -d ' ')
  if [ "$file_bytes" -eq $((value_bytes + 1)) ]; then
    last_byte=$(tail -c 1 "$path" | od -An -tu1 | tr -d ' ')
    [ "$last_byte" = 10 ] || fail "$label contains a NUL or hidden byte"
  else
    [ "$file_bytes" -eq "$value_bytes" ] || fail "$label contains a NUL or hidden byte"
  fi
  printf '%s' "$value"
}

validate_allowed_emails_file() {
  local path=$1
  "$PYTHON_BIN" -I - "$path" <<'PY' || fail "Access allowed-email file is invalid"
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
try:
    data = path.read_bytes()
    text = data.decode("ascii")
except (OSError, UnicodeDecodeError):
    raise SystemExit("ERROR: Access allowed-email file is invalid")
if not data or len(data) > 8192 or "\r" in text or "\x00" in text:
    raise SystemExit("ERROR: Access allowed-email file is invalid")
if text.endswith("\n"):
    text = text[:-1]
if not text or text.endswith("\n") or "\n\n" in text:
    raise SystemExit("ERROR: Access allowed-email file is invalid")
lines = text.split("\n")
local = r"[A-Za-z0-9]+(?:[._%+\-][A-Za-z0-9]+)*"
label = r"[A-Za-z0-9](?:[A-Za-z0-9\-]{0,61}[A-Za-z0-9])?"
pattern = re.compile(local + r"@" + label + r"(?:\." + label + r")+")
if not 1 <= len(lines) <= 8 or len(set(lines)) != len(lines):
    raise SystemExit("ERROR: Access allowed-email file is invalid")
if any(len(value) > 254 or pattern.fullmatch(value) is None for value in lines):
    raise SystemExit("ERROR: Access allowed-email file is invalid")
PY
}

validate_gateway_public_key_file() {
  local path=$1
  "$PYTHON_BIN" -I - "$path" <<'PY' || fail "gateway public key must be one canonical PUBLIC KEY PEM block"
import base64
import binascii
import pathlib
import re
import sys

try:
    data = pathlib.Path(sys.argv[1]).read_bytes()
    text = data.decode("ascii")
except (OSError, UnicodeDecodeError):
    raise SystemExit(1)
if not data or len(data) > 8192 or "\r" in text or "\x00" in text:
    raise SystemExit(1)
if text.endswith("\n"):
    text = text[:-1]
lines = text.split("\n")
if len(lines) < 3 or lines[0] != "-----BEGIN PUBLIC KEY-----" or lines[-1] != "-----END PUBLIC KEY-----":
    raise SystemExit(1)
body = lines[1:-1]
if not body or any(re.fullmatch(r"[A-Za-z0-9+/]+={0,2}", line) is None or len(line) > 64 for line in body):
    raise SystemExit(1)
if any(len(line) != 64 for line in body[:-1]):
    raise SystemExit(1)
encoded = "".join(body)
try:
    decoded = base64.b64decode(encoded, validate=True)
except (binascii.Error, ValueError):
    raise SystemExit(1)
prefix = bytes.fromhex("3059301306072a8648ce3d020106082a8648ce3d03010703420004")
if (len(decoded) != len(prefix) + 64 or not decoded.startswith(prefix) or
        base64.b64encode(decoded).decode("ascii") != encoded):
    raise SystemExit(1)
PY
}

read_toolchain() {
  local key=$1 count line
  count=$(grep -Ec "^${key}=" "$TOOLCHAIN_LOCK" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once in toolchain.lock"
  line=$(grep -E "^${key}=" "$TOOLCHAIN_LOCK")
  printf '%s' "${line#*=}"
}

find_compose_parser() {
  local candidate version expected_version expected_sha parent base physical owner mode system_name machine
  system_name=$(uname -s); machine=$(uname -m)
  if [ "$COMPOSE_MODE" = production ] || { [ "$system_name" = Linux ] && { [ "$machine" = x86_64 ] || [ "$machine" = amd64 ]; }; }; then
    candidate=$(read_toolchain COMPOSE_PLUGIN_PATH)
    expected_version=$(read_toolchain COMPOSE_CLI_VERSION)
    expected_sha=$(read_toolchain COMPOSE_LINUX_AMD64_SHA256)
  elif [ "$COMPOSE_MODE" = auto ] && [ "$system_name" = Darwin ] && [ "$machine" = arm64 ]; then
    candidate=$(command -v docker-compose 2>/dev/null) || fail "hash-locked Darwin/arm64 Compose parser is required"
    expected_version=$(read_toolchain COMPOSE_STATIC_DARWIN_ARM64_VERSION)
    expected_sha=$(read_toolchain COMPOSE_STATIC_DARWIN_ARM64_SHA256)
  else
    fail "no hash-locked Compose parser is defined for this host platform"
  fi
  case "$candidate" in /*) ;; *) fail "Compose parser path must be absolute" ;; esac
  [ -f "$candidate" ] && [ ! -L "$candidate" ] && [ -x "$candidate" ] || fail "Compose parser is not an executable regular file"
  parent=$(dirname -- "$candidate");base=$(basename -- "$candidate")
  physical=$(CDPATH= cd -- "$parent" && pwd -P)/$base
  [ "$candidate" = "$physical" ] || fail "Compose parser path must be physical and canonical"
  owner=$(stat -f '%u' "$candidate" 2>/dev/null || stat -c '%u' "$candidate")
  [ "$owner" = 0 ] || [ "$owner" = "$CURRENT_UID" ] || fail "Compose parser owner is not trusted"
  mode=$(stat -f '%Lp' "$candidate" 2>/dev/null || stat -c '%a' "$candidate")
  case "$mode" in 500|555|700|755) ;; *) fail "Compose parser permissions are writable by an untrusted user" ;; esac
  [ "$(shasum -a 256 "$candidate" | awk '{print $1}')" = "$expected_sha" ] || fail "Compose parser SHA-256 differs from toolchain.lock"
  COMPOSE_CMD=("$candidate")
  version=$(env -i PATH="$PATH" DOCKER_CONFIG="$DOCKER_CONFIG_DIR" DOCKER_CONTEXT=default \
    "${COMPOSE_CMD[@]}" version --short 2>/dev/null | tr -d '\r\n') ||
    fail "selected Docker Compose parser could not report its version"
  version=${version#v}
  [ "$version" = "$expected_version" ] || fail "Docker Compose parser version differs from toolchain.lock"
  PYTHON_BIN=$(command -v python3 2>/dev/null) || fail "Python 3 is required for the normalized Compose allowlist"
}

compose_policy_check() {
  local root=$1 uid=$2 gid=$3 edge_image=$4 cloudflared_image=$5 coturn_image=$6 edge_env_file=$7
  local all_json default_json parser_error gateway_id team_domain audience
  all_json="$TMP_WORK/compose-all.json"
  default_json="$TMP_WORK/compose-default.json"
  parser_error="$TMP_WORK/compose-parser.stderr"
  gateway_id=$(read_env_value "$edge_env_file" DJI4G_EDGE_GATEWAY_ID)
  team_domain=$(read_env_value "$edge_env_file" DJI4G_ACCESS_TEAM_DOMAIN)
  audience=$(read_env_value "$edge_env_file" DJI4G_ACCESS_AUDIENCE)
  validate_edge_identity "$gateway_id" "$team_domain" "$audience"

  if ! env -i \
    PATH="$PATH" \
    DOCKER_CONFIG="$DOCKER_CONFIG_DIR" \
    DOCKER_CONTEXT=default \
    DJI4G_PUBLIC_EDGE_ROOT="$root" \
    DJI4G_EDGE_ENV_FILE="$edge_env_file" \
    DJI4G_EDGE_UID="$uid" \
    DJI4G_EDGE_GID="$gid" \
    DJI4G_EDGE_IMAGE="$edge_image" \
    DJI4G_CLOUDFLARED_IMAGE="$cloudflared_image" \
    DJI4G_COTURN_IMAGE="$coturn_image" \
    "${COMPOSE_CMD[@]}" --env-file /dev/null -f "$COMPOSE" --profile '*' \
      config --no-env-resolution --format json >"$all_json" 2>"$parser_error"; then
    fail "Docker Compose could not normalize the full-profile model; JSON config support is required"
  fi
  env -i PATH="$PATH" "$PYTHON_BIN" -I "$COMPOSE_POLICY" \
    --root "$root" \
    --uid-gid "$uid:$gid" \
    --edge-image "$edge_image" \
    --cloudflared-image "$cloudflared_image" \
    --coturn-image "$coturn_image" \
    --gateway-id "$gateway_id" \
    --access-team-domain "$team_domain" \
    --access-audience "$audience" \
    "$all_json" || fail "normalized full-profile Compose model is outside the complete allowlist"

  if ! env -i \
    PATH="$PATH" \
    DOCKER_CONFIG="$DOCKER_CONFIG_DIR" \
    DOCKER_CONTEXT=default \
    DJI4G_PUBLIC_EDGE_ROOT="$root" \
    DJI4G_EDGE_ENV_FILE="$edge_env_file" \
    DJI4G_EDGE_UID="$uid" \
    DJI4G_EDGE_GID="$gid" \
    DJI4G_EDGE_IMAGE="$edge_image" \
    DJI4G_CLOUDFLARED_IMAGE="$cloudflared_image" \
    DJI4G_COTURN_IMAGE="$coturn_image" \
    "${COMPOSE_CMD[@]}" --env-file /dev/null -f "$COMPOSE" \
      config --no-env-resolution --format json >"$default_json" 2>"$parser_error"; then
    fail "Docker Compose could not normalize the default-profile model"
  fi
  env -i PATH="$PATH" "$PYTHON_BIN" -I "$COMPOSE_POLICY" --default-off "$default_json" ||
    fail "Compose must select no services when no profile is supplied"
}

render_turn_template() {
  local output=$1 relay_ip=$2 external_ip=$3 auth_secret=$4 line
  : >"$output"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      'listening-ip=__TURN_RELAY_IP__')
        printf 'listening-ip=%s\n' "$relay_ip" >>"$output" ;;
      'relay-ip=__TURN_RELAY_IP__')
        printf 'relay-ip=%s\n' "$relay_ip" >>"$output" ;;
      'external-ip=__TURN_EXTERNAL_IP__')
        printf 'external-ip=%s\n' "$external_ip" >>"$output" ;;
      'static-auth-secret=__TURN_AUTH_SECRET__')
        printf 'static-auth-secret=%s\n' "$auth_secret" >>"$output" ;;
      *)
        printf '%s\n' "$line" >>"$output" ;;
    esac
  done <"$TURN_TEMPLATE"
}

render_edge_template() {
  local output=$1 gateway_id=$2 team_domain=$3 audience=$4 line
  : >"$output"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      'DJI4G_EDGE_GATEWAY_ID=__EDGE_GATEWAY_ID__')
        printf 'DJI4G_EDGE_GATEWAY_ID=%s\n' "$gateway_id" >>"$output" ;;
      'DJI4G_ACCESS_TEAM_DOMAIN=__ACCESS_TEAM_DOMAIN__')
        printf 'DJI4G_ACCESS_TEAM_DOMAIN=%s\n' "$team_domain" >>"$output" ;;
      'DJI4G_ACCESS_AUDIENCE=__ACCESS_AUDIENCE__')
        printf 'DJI4G_ACCESS_AUDIENCE=%s\n' "$audience" >>"$output" ;;
      *)
        printf '%s\n' "$line" >>"$output" ;;
    esac
  done <"$EDGE_TEMPLATE"
}

contains_literal() {
  local needle=$1 file line
  shift
  for file in "$@"; do
    while IFS= read -r line || [ -n "$line" ]; do
      case "$line" in
        *"$needle"*) return 0 ;;
      esac
    done <"$file"
  done
  return 1
}

# The checked-in skeleton itself is deliberately narrow and default-off.
reject_pattern "$COMPOSE" $'\t|\r' "compose must not contain tabs or carriage returns"
reject_pattern "$COMPOSE" 'docker\.sock|/run/docker|/var/run/docker' "Docker socket access is forbidden"
reject_pattern "$COMPOSE" '(^|[^A-Za-z0-9_])(/?ari)([^A-Za-z0-9_]|$)|ASTERISK_ARI' "public ARI wiring is forbidden"
reject_pattern "$COMPOSE" '7576' "port 7576 is forbidden in the public deployment"
reject_pattern "$COMPOSE" 'ss-master-dashboard|fridge|cecilia|/volume[0-9]+/|/Users/|/home/' "cross-project or workstation mounts are forbidden"
reject_pattern "$COMPOSE" '^[[:space:]]+privileged:|^[[:space:]]+pid:[[:space:]]*host' "privileged or host-pid mode is forbidden"
reject_pattern "$COMPOSE" '^[[:space:]]+build:' "runtime image builds are forbidden"
reject_pattern "$COMPOSE" '^[[:space:]]+environment:' "inline environment injection is forbidden"
reject_pattern "$COMPOSE" 'TUNNEL_TOKEN[[:space:]]*=|CLOUDFLARE_TOKEN[[:space:]]*=' "Cloudflare token environment injection is forbidden"
reject_pattern "$COMPOSE" 'include[[:space:]]*:|extends[[:space:]]*:' "external Compose include/extends is forbidden"
reject_pattern "$COMPOSE" '(^|[^0-9])(3479|444)([^0-9]|$)' "alternate TURN ports 3479/444 must not be published"
BIND_SEQUENCE_COUNT=$(awk '
  previous_previous == "        read_only: true" &&
    previous == "        bind:" &&
    $0 == "          create_host_path: false" { count++ }
  { previous_previous=previous; previous=$0 }
  END { print count+0 }
' "$COMPOSE")
[ "$BIND_SEQUENCE_COUNT" -eq 6 ] &&
  [ "$(grep -Fxc '        bind:' "$COMPOSE" || true)" -eq 6 ] &&
  [ "$(grep -Fxc '          create_host_path: false' "$COMPOSE" || true)" -eq 6 ] ||
  fail "every exact read-only bind must set create_host_path false"

TMP_BASE=${TMPDIR:-/tmp}
[ -d "$TMP_BASE" ] || fail "temporary directory base is unavailable"
TMP_WORK=$(mktemp -d "$TMP_BASE/dji4g-public-edge-verify.XXXXXX")
DOCKER_CONFIG_DIR="$TMP_WORK/docker-config"
mkdir "$DOCKER_CONFIG_DIR"
chmod 700 "$DOCKER_CONFIG_DIR"
find_compose_parser
POLICY_ROOT="$TMP_WORK/policy-root"
mkdir -p "$POLICY_ROOT/config"
render_edge_template \
  "$POLICY_ROOT/config/edge.env" \
  policy-gateway \
  policy.cloudflareaccess.com \
  policy_audience
compose_policy_check \
  "$POLICY_ROOT" \
  65532 \
  65532 \
  "ghcr.io/example/maccellular-edge@sha256:$(printf '1%.0s' {1..64})" \
  "cloudflare/cloudflared@sha256:$(printf '2%.0s' {1..64})" \
  "coturn/coturn@sha256:$(printf '3%.0s' {1..64})" \
  "$POLICY_ROOT/config/edge.env"

# Template invariants prevent user-controlled values from becoming directives.
reject_pattern "$EDGE_TEMPLATE" $'\t|\r|`|\$\(|\$\{' "edge template contains unsafe syntax"
[ "$(wc -l <"$EDGE_TEMPLATE" | tr -d ' ')" -eq 12 ] || fail "edge template line count changed"
[ "$(grep -Foc '__EDGE_GATEWAY_ID__' "$EDGE_TEMPLATE" || true)" -eq 1 ] || fail "edge gateway placeholder count changed"
[ "$(grep -Foc '__ACCESS_TEAM_DOMAIN__' "$EDGE_TEMPLATE" || true)" -eq 1 ] || fail "Access team placeholder count changed"
[ "$(grep -Foc '__ACCESS_AUDIENCE__' "$EDGE_TEMPLATE" || true)" -eq 1 ] || fail "Access audience placeholder count changed"
for line in \
  'DJI4G_EDGE_LISTEN_ADDR=0.0.0.0:8080' \
  'DJI4G_EDGE_PUBLIC_HOST=phone.example.com' \
  'DJI4G_EDGE_GATEWAY_ID=__EDGE_GATEWAY_ID__' \
  'DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE=/run/config/gateway-public-key.pem' \
  'DJI4G_ACCESS_TEAM_DOMAIN=__ACCESS_TEAM_DOMAIN__' \
  'DJI4G_ACCESS_AUDIENCE=__ACCESS_AUDIENCE__' \
  'DJI4G_ACCESS_ALLOWED_EMAILS_FILE=/run/config/access-allowed-emails' \
  'DJI4G_EDGE_TRUST_FORWARDED_IDENTITY=false' \
  'DJI4G_EDGE_ENROLLMENT_ENABLED=false' \
  'DJI4G_EDGE_MUTATIONS_ENABLED=false' \
  'DJI4G_EDGE_PUSH_ENABLED=false' \
  'DJI4G_EDGE_TURN_ISSUANCE_ENABLED=false'; do
  require_line "$EDGE_TEMPLATE" "$line" "edge fail-closed setting $line"
done

reject_pattern "$TURN_TEMPLATE" $'\t|\r|`|\$\(|\$\{' "TURN template contains unsafe shell syntax"
[ "$(wc -l <"$TURN_TEMPLATE" | tr -d ' ')" -eq 44 ] || fail "TURN template line count changed"
[ "$(grep -Foc '__TURN_RELAY_IP__' "$TURN_TEMPLATE" || true)" -eq 2 ] || fail "TURN relay placeholder count changed"
[ "$(grep -Foc '__TURN_EXTERNAL_IP__' "$TURN_TEMPLATE" || true)" -eq 1 ] || fail "TURN external placeholder count changed"
[ "$(grep -Foc '__TURN_AUTH_SECRET__' "$TURN_TEMPLATE" || true)" -eq 1 ] || fail "TURN secret placeholder count changed"
for line in \
  'realm=turn.example.com' \
  'server-name=turn.example.com' \
  'listening-port=3478' \
  'tls-listening-port=5349' \
  'min-port=49160' \
  'max-port=49167' \
  'fingerprint' \
  'use-auth-secret' \
  'secure-stun' \
  'user-quota=4' \
  'total-quota=8' \
  'max-bps=256000' \
  'bps-capacity=2000000' \
  'stale-nonce=600' \
  'max-allocate-lifetime=600' \
  'channel-lifetime=600' \
  'permission-lifetime=300' \
  'allocation-default-address-family=ipv4' \
  'cert=/run/secrets/turn-tls-cert.pem' \
  'pkey=/run/secrets/turn-tls-key.pem' \
  'userdb=/tmp/turnserver.db' \
  'pidfile=/tmp/turnserver.pid' \
  'log-file=stdout' \
  'simple-log' \
  'log-min-level=info' \
  'no-cli' \
  'no-multicast-peers' \
  'no-rfc5780' \
  'no-tcp-relay' \
  'no-dtls' \
  'denied-peer-ip=0.0.0.0-0.255.255.255' \
  'denied-peer-ip=10.0.0.0-10.255.255.255' \
  'denied-peer-ip=100.64.0.0-100.127.255.255' \
  'denied-peer-ip=127.0.0.0-127.255.255.255' \
  'denied-peer-ip=169.254.0.0-169.254.255.255' \
  'denied-peer-ip=172.16.0.0-172.31.255.255' \
  'denied-peer-ip=192.0.0.0-192.0.0.255' \
  'denied-peer-ip=192.168.0.0-192.168.255.255' \
  'denied-peer-ip=198.18.0.0-198.19.255.255' \
  'denied-peer-ip=224.0.0.0-255.255.255.255'; do
  require_line "$TURN_TEMPLATE" "$line" "fixed TURN setting $line"
done
reject_pattern "$TURN_TEMPLATE" '(^|[^0-9])(3479|444)([^0-9]|$)' "alternate TURN ports 3479/444 must not be configured"

if [ "$COMPOSE_ONLY" -eq 1 ]; then
  [ -z "$ROOT" ] || fail "runtime root is not accepted with --compose-only"
  [ -z "$DECLARED_ROOT" ] || fail "--declared-root is not accepted with --compose-only"
  [ "$IMMUTABLE_SNAPSHOT" -eq 0 ] || fail "--immutable-snapshot is not accepted with --compose-only"
  [ "$QUIET" -eq 1 ] || printf 'OK: public-edge Compose and templates are statically valid; all profiles remain off by default.\n'
  exit 0
fi

[ -n "$ROOT" ] || fail "runtime root is required"
validate_abs_path "runtime root" "$ROOT"
[ -d "$ROOT" ] && [ ! -L "$ROOT" ] || fail "runtime root must be a real directory"
PHYSICAL_ROOT=$(CDPATH= cd -- "$ROOT" && pwd -P) || fail "runtime root is inaccessible"
[ "$PHYSICAL_ROOT" = "$ROOT" ] || fail "runtime root path traverses a symlink or is not canonical"

if [ -z "$DECLARED_ROOT" ]; then
  DECLARED_ROOT=$ROOT
else
  validate_abs_path "declared root" "$DECLARED_ROOT"
fi
if [ "$IMMUTABLE_SNAPSHOT" -eq 1 ]; then
  case "$DECLARED_ROOT" in */.*.edge-snapshot.??????) ;; *) fail "immutable snapshot declared root must use the random private snapshot name" ;; esac
  DIR_MODE=500
  FILE_MODE=400
else
  DIR_MODE=700
  FILE_MODE=600
fi

[ -z "$(find "$ROOT" -type l -print -quit)" ] || fail "runtime root must not contain symlinks"
[ -z "$(find "$ROOT" -mindepth 1 ! -type d ! -type f -print -quit)" ] || fail "runtime root contains a non-file object"
[ -z "$(find "$ROOT" -mindepth 2 -type d -print -quit)" ] || fail "runtime root contains an unapproved nested directory"
[ "$(find "$ROOT" -mindepth 1 -maxdepth 1 -print | wc -l | tr -d ' ')" -eq 3 ] || fail "runtime root top-level allowlist changed"
for dir in "$ROOT" "$ROOT/config" "$ROOT/secrets"; do
  [ -d "$dir" ] && [ ! -L "$dir" ] || fail "missing runtime directory: $dir"
  check_mode_owner "$dir" "$DIR_MODE" "$dir"
done

BASE_FILES=(
  "$ROOT/compose.env"
  "$ROOT/config/edge.env"
  "$ROOT/config/gateway-public-key.pem"
  "$ROOT/config/access-allowed-emails"
  "$ROOT/secrets/cloudflare-tunnel.token"
)
for file in "${BASE_FILES[@]}"; do
  [ -f "$file" ] && [ ! -L "$file" ] || fail "missing runtime file: $file"
  check_mode_owner "$file" "$FILE_MODE" "$file"
  [ "$(stat_nlink "$file")" = 1 ] || fail "$file must have exactly one hard link"
done

[ "$(wc -l <"$ROOT/compose.env" | tr -d ' ')" -eq 8 ] || fail "compose.env must contain exactly eight entries"
ENV_ROOT=$(read_env_value "$ROOT/compose.env" DJI4G_PUBLIC_EDGE_ROOT)
ENV_EDGE_ENV_FILE=$(read_env_value "$ROOT/compose.env" DJI4G_EDGE_ENV_FILE)
ENV_PROFILES=$(read_env_value "$ROOT/compose.env" DJI4G_ENABLED_PROFILES)
ENV_UID=$(read_env_value "$ROOT/compose.env" DJI4G_EDGE_UID)
ENV_GID=$(read_env_value "$ROOT/compose.env" DJI4G_EDGE_GID)
ENV_EDGE_IMAGE=$(read_env_value "$ROOT/compose.env" DJI4G_EDGE_IMAGE)
ENV_CLOUDFLARED_IMAGE=$(read_env_value "$ROOT/compose.env" DJI4G_CLOUDFLARED_IMAGE)
ENV_COTURN_IMAGE=$(read_env_value "$ROOT/compose.env" DJI4G_COTURN_IMAGE)
LOCKED_CLOUDFLARED_IMAGE=$(read_toolchain CLOUDFLARED_IMAGE)
[ "$ENV_CLOUDFLARED_IMAGE" = "$LOCKED_CLOUDFLARED_IMAGE" ] ||
  fail "cloudflared image must equal the exact toolchain-locked audited target"
[ "$ENV_ROOT" = "$DECLARED_ROOT" ] || fail "compose.env root does not match the declared root"
[ "$ENV_EDGE_ENV_FILE" = "$DECLARED_ROOT/config/edge.env" ] || fail "compose.env edge env path does not match the declared root"
case "$ENV_PROFILES" in edge|edge,turn) ;; *) fail "DJI4G_ENABLED_PROFILES is invalid" ;; esac
validate_integer "DJI4G_EDGE_UID" "$ENV_UID"
validate_integer "DJI4G_EDGE_GID" "$ENV_GID"
[ "$ENV_UID" = "$CURRENT_UID" ] || fail "container uid must match the 0600 file owner"
[ "$ENV_GID" = "$CURRENT_GID" ] || fail "container gid must match the runtime owner group"
validate_edge_image "$ENV_EDGE_IMAGE"
validate_image "DJI4G_CLOUDFLARED_IMAGE" "$ENV_CLOUDFLARED_IMAGE"
validate_image "DJI4G_COTURN_IMAGE" "$ENV_COTURN_IMAGE"
DISABLED_COTURN_IMAGE="invalid.local/dji4g-coturn-disabled@sha256:0000000000000000000000000000000000000000000000000000000000000000"
if [ "$ENV_PROFILES" = edge ]; then
  [ "$ENV_COTURN_IMAGE" = "$DISABLED_COTURN_IMAGE" ] || fail "edge-only runtime must use the inert coturn sentinel"
  [ "$(find "$ROOT/config" -mindepth 1 -maxdepth 1 -type f -print | wc -l | tr -d ' ')" -eq 3 ] || fail "edge-only config allowlist changed"
  [ "$(find "$ROOT/secrets" -mindepth 1 -maxdepth 1 -type f -print | wc -l | tr -d ' ')" -eq 1 ] || fail "edge-only secret allowlist changed"
else
  [ "$ENV_COTURN_IMAGE" != "$DISABLED_COTURN_IMAGE" ] || fail "TURN-enabled runtime may not use the inert image sentinel"
  [ "$(find "$ROOT/config" -mindepth 1 -maxdepth 1 -type f -print | wc -l | tr -d ' ')" -eq 4 ] || fail "TURN config allowlist changed"
  [ "$(find "$ROOT/secrets" -mindepth 1 -maxdepth 1 -type f -print | wc -l | tr -d ' ')" -eq 4 ] || fail "TURN secret allowlist changed"
  for file in "$ROOT/config/turnserver.conf" "$ROOT/secrets/turn-auth-secret" \
    "$ROOT/secrets/turn-tls-cert.pem" "$ROOT/secrets/turn-tls-key.pem"; do
    [ -f "$file" ] && [ ! -L "$file" ] || fail "missing TURN runtime file: $file"
    check_mode_owner "$file" 600 "$file"
    [ "$(stat_nlink "$file")" = 1 ] || fail "$file must have exactly one hard link"
  done
fi
compose_policy_check \
  "$ENV_ROOT" \
  "$ENV_UID" \
  "$ENV_GID" \
  "$ENV_EDGE_IMAGE" \
  "$ENV_CLOUDFLARED_IMAGE" \
  "$ENV_COTURN_IMAGE" \
  "$ROOT/config/edge.env"

EDGE_GATEWAY_ID=$(read_env_value "$ROOT/config/edge.env" DJI4G_EDGE_GATEWAY_ID)
ACCESS_TEAM_DOMAIN=$(read_env_value "$ROOT/config/edge.env" DJI4G_ACCESS_TEAM_DOMAIN)
ACCESS_AUDIENCE=$(read_env_value "$ROOT/config/edge.env" DJI4G_ACCESS_AUDIENCE)
validate_edge_identity "$EDGE_GATEWAY_ID" "$ACCESS_TEAM_DOMAIN" "$ACCESS_AUDIENCE"
EXPECTED_EDGE="$TMP_WORK/edge.env"
render_edge_template "$EXPECTED_EDGE" "$EDGE_GATEWAY_ID" "$ACCESS_TEAM_DOMAIN" "$ACCESS_AUDIENCE"
cmp -s "$EXPECTED_EDGE" "$ROOT/config/edge.env" || fail "edge.env contains an extra or altered setting"
validate_allowed_emails_file "$ROOT/config/access-allowed-emails"
validate_gateway_public_key_file "$ROOT/config/gateway-public-key.pem"
command -v openssl >/dev/null 2>&1 || fail "openssl is required for offline key checks"
openssl pkey -pubin -pubcheck -in "$ROOT/config/gateway-public-key.pem" -noout -text 2>/dev/null |
  grep -Eq 'ASN1 OID: prime256v1|NIST CURVE: P-256' ||
  fail "gateway public key must be a valid P-256 PUBLIC KEY"

CLOUDFLARE_TOKEN=$(read_single_line_secret "cloudflare token" "$ROOT/secrets/cloudflare-tunnel.token")
[ "${#CLOUDFLARE_TOKEN}" -ge 32 ] && [ "${#CLOUDFLARE_TOKEN}" -le 4096 ] || fail "cloudflare token length is invalid"
[[ "$CLOUDFLARE_TOKEN" =~ ^[A-Za-z0-9._~+/=-]+$ ]] || fail "cloudflare token contains unsafe characters"
TURN_AUTH_SECRET=""
if [ "$ENV_PROFILES" = edge,turn ]; then
  TURN_AUTH_SECRET=$(read_single_line_secret "TURN auth secret" "$ROOT/secrets/turn-auth-secret")
  [ "${#TURN_AUTH_SECRET}" -ge 43 ] && [ "${#TURN_AUTH_SECRET}" -le 128 ] || fail "TURN auth secret length is invalid"
  [[ "$TURN_AUTH_SECRET" =~ ^[A-Za-z0-9_-]+$ ]] || fail "TURN auth secret must be unpadded base64url"

  TURN_RELAY_IP=$(read_conf_value "$ROOT/config/turnserver.conf" relay-ip)
  TURN_LISTEN_IP=$(read_conf_value "$ROOT/config/turnserver.conf" listening-ip)
  TURN_EXTERNAL_IP=$(read_conf_value "$ROOT/config/turnserver.conf" external-ip)
  TURN_CONFIG_SECRET=$(read_conf_value "$ROOT/config/turnserver.conf" static-auth-secret)
  validate_ipv4 "TURN relay-ip" "$TURN_RELAY_IP"
  [ "$TURN_RELAY_IP" = 172.30.247.2 ] || fail "TURN relay-ip must match the pinned coturn bridge address"
  [ "$TURN_LISTEN_IP" = "$TURN_RELAY_IP" ] || fail "TURN listening-ip and relay-ip must match"
  case "$TURN_EXTERNAL_IP" in
    */*)
      TURN_PUBLIC_IP=${TURN_EXTERNAL_IP%%/*}; TURN_EXTERNAL_RELAY=${TURN_EXTERNAL_IP#*/}
      validate_public_ipv4 "TURN external public IP" "$TURN_PUBLIC_IP"
      [ "$TURN_EXTERNAL_RELAY" = "$TURN_RELAY_IP" ] || fail "TURN external NAT mapping does not match relay-ip" ;;
    *)
      validate_public_ipv4 "TURN external-ip" "$TURN_EXTERNAL_IP"
      [ "$TURN_EXTERNAL_IP" = "$TURN_RELAY_IP" ] || fail "TURN external-ip without NAT mapping must equal relay-ip" ;;
  esac
  [ "$TURN_CONFIG_SECRET" = "$TURN_AUTH_SECRET" ] || fail "TURN auth secret injection mismatch"
  EXPECTED_TURN="$TMP_WORK/turnserver.conf"
  render_turn_template "$EXPECTED_TURN" "$TURN_RELAY_IP" "$TURN_EXTERNAL_IP" "$TURN_AUTH_SECRET"
  cmp -s "$EXPECTED_TURN" "$ROOT/config/turnserver.conf" || fail "turnserver.conf contains an extra or altered directive"
fi

LEAK_SCAN=("$ROOT/compose.env" "$ROOT/config/edge.env" "$ROOT/config/gateway-public-key.pem" "$ROOT/config/access-allowed-emails")
if [ "$ENV_PROFILES" = edge,turn ]; then
  LEAK_SCAN+=("$ROOT/config/turnserver.conf" "$ROOT/secrets/turn-auth-secret" "$ROOT/secrets/turn-tls-cert.pem" "$ROOT/secrets/turn-tls-key.pem")
fi
if contains_literal "$CLOUDFLARE_TOKEN" "${LEAK_SCAN[@]}"; then
  fail "cloudflare token leaked outside its token file"
fi
if [ "$ENV_PROFILES" = edge,turn ] && contains_literal "$TURN_AUTH_SECRET" \
  "$ROOT/compose.env" "$ROOT/config/edge.env" "$ROOT/secrets/cloudflare-tunnel.token" \
  "$ROOT/config/gateway-public-key.pem" "$ROOT/config/access-allowed-emails" \
  "$ROOT/secrets/turn-tls-cert.pem" "$ROOT/secrets/turn-tls-key.pem"; then
  fail "TURN auth secret leaked outside its allowed file and coturn directive"
fi

if [ "$ENV_PROFILES" = edge,turn ]; then
  command -v openssl >/dev/null 2>&1 || fail "openssl is required for offline certificate checks"
  openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -noout >/dev/null 2>&1 || fail "TURN certificate is invalid"
  openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -checkend 604800 -noout >/dev/null 2>&1 || fail "TURN certificate is expired or expires within seven days"
  openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -text -noout 2>/dev/null |
    grep -Eq 'DNS:turn\.example\.com([,[:space:]]|$)' || fail "TURN certificate SAN is wrong"
  openssl pkey -in "$ROOT/secrets/turn-tls-key.pem" -passin pass: -noout >/dev/null 2>&1 || fail "TURN private key is invalid or encrypted"
  CERT_PUBLIC_KEY=$(openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -pubkey -noout 2>/dev/null |
    openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256 2>/dev/null)
  KEY_PUBLIC_KEY=$(openssl pkey -in "$ROOT/secrets/turn-tls-key.pem" -passin pass: -pubout -outform DER 2>/dev/null |
    openssl dgst -sha256 2>/dev/null)
  [ -n "$CERT_PUBLIC_KEY" ] && [ "$CERT_PUBLIC_KEY" = "$KEY_PUBLIC_KEY" ] || fail "TURN certificate/key mismatch"
fi

[ "$QUIET" -eq 1 ] || printf 'OK: runtime root and default-off public-edge skeleton passed static verification.\n'
