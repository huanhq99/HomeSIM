#!/usr/bin/env bash
set -euo pipefail

umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
TEMPLATE_DIR="$SCRIPT_DIR/templates"
VERIFY="$SCRIPT_DIR/verify-config.sh"
ATOMIC_COMMIT="$SCRIPT_DIR/image/atomic-commit.py"
CURRENT_UID=$(id -u)
CURRENT_GID=$(id -g)

OUTPUT=""
EDGE_IMAGE=""
CLOUDFLARED_IMAGE=""
COTURN_IMAGE=""
EDGE_UID="$CURRENT_UID"
EDGE_GID="$CURRENT_GID"
TURN_PUBLIC_IP=""
TURN_RELAY_IP=""
CLOUDFLARE_TOKEN_FILE=""
TURN_AUTH_SECRET_FILE=""
TURN_TLS_CERT_FILE=""
TURN_TLS_KEY_FILE=""
EDGE_GATEWAY_ID=""
GATEWAY_PUBLIC_KEY_FILE=""
ACCESS_TEAM_DOMAIN=""
ACCESS_AUDIENCE=""
ACCESS_ALLOWED_EMAILS_FILE=""
APPLY=0
TURN_BRIDGE_IP=172.30.247.2
PROFILE=edge
DISABLED_COTURN_IMAGE="invalid.local/dji4g-coturn-disabled@sha256:0000000000000000000000000000000000000000000000000000000000000000"

usage() {
  cat <<'EOF'
Usage:
  generate-config.sh \
    --output /absolute/runtime/root \
    --edge-image registry/repository@sha256:<64-lowercase-hex> \
    --cloudflared-image registry/repository@sha256:<64-lowercase-hex> \
    --coturn-image registry/repository@sha256:<64-lowercase-hex> \
    --turn-public-ip A.B.C.D \
    --turn-relay-ip 172.30.247.2 \
    --cloudflare-token-file /absolute/0600/file \
    --turn-auth-secret-file /absolute/0600/file \
    --turn-tls-cert-file /absolute/0600/file \
    --turn-tls-key-file /absolute/0600/file \
    --edge-gateway-id home-gateway-1 \
    --gateway-public-key-file /absolute/0600/p256-public-key.pem \
    --access-team-domain account.cloudflareaccess.com \
    --access-audience APPLICATION_AUD_TAG \
    --access-allowed-emails-file /absolute/0600/file \
    [--profile edge|edge,turn] \
    [--edge-uid INTEGER] [--edge-gid INTEGER] [--apply]

Dry-run is the default. --apply creates a new runtime root atomically and
refuses to overwrite an existing path. Secret values are never printed.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

need_value() {
  [ "$#" -ge 2 ] || fail "$1 requires a value"
  [ -n "$2" ] || fail "$1 requires a non-empty value"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      need_value "$@"; OUTPUT=$2; shift 2 ;;
    --edge-image)
      need_value "$@"; EDGE_IMAGE=$2; shift 2 ;;
    --cloudflared-image)
      need_value "$@"; CLOUDFLARED_IMAGE=$2; shift 2 ;;
    --coturn-image)
      need_value "$@"; COTURN_IMAGE=$2; shift 2 ;;
    --edge-uid)
      need_value "$@"; EDGE_UID=$2; shift 2 ;;
    --edge-gid)
      need_value "$@"; EDGE_GID=$2; shift 2 ;;
    --turn-public-ip)
      need_value "$@"; TURN_PUBLIC_IP=$2; shift 2 ;;
    --turn-relay-ip)
      need_value "$@"; TURN_RELAY_IP=$2; shift 2 ;;
    --cloudflare-token-file)
      need_value "$@"; CLOUDFLARE_TOKEN_FILE=$2; shift 2 ;;
    --turn-auth-secret-file)
      need_value "$@"; TURN_AUTH_SECRET_FILE=$2; shift 2 ;;
    --turn-tls-cert-file)
      need_value "$@"; TURN_TLS_CERT_FILE=$2; shift 2 ;;
    --turn-tls-key-file)
      need_value "$@"; TURN_TLS_KEY_FILE=$2; shift 2 ;;
    --edge-gateway-id)
      need_value "$@"; EDGE_GATEWAY_ID=$2; shift 2 ;;
    --gateway-public-key-file)
      need_value "$@"; GATEWAY_PUBLIC_KEY_FILE=$2; shift 2 ;;
    --access-team-domain)
      need_value "$@"; ACCESS_TEAM_DOMAIN=$2; shift 2 ;;
    --access-audience)
      need_value "$@"; ACCESS_AUDIENCE=$2; shift 2 ;;
    --access-allowed-emails-file)
      need_value "$@"; ACCESS_ALLOWED_EMAILS_FILE=$2; shift 2 ;;
    --profile)
      need_value "$@"; PROFILE=$2; shift 2 ;;
    --apply)
      APPLY=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      fail "unknown argument: $1" ;;
  esac
done

[ -n "$OUTPUT" ] || fail "--output is required"
[ -n "$EDGE_IMAGE" ] || fail "--edge-image is required"
[ -n "$CLOUDFLARED_IMAGE" ] || fail "--cloudflared-image is required"
[ -n "$CLOUDFLARE_TOKEN_FILE" ] || fail "--cloudflare-token-file is required"
[ -n "$EDGE_GATEWAY_ID" ] || fail "--edge-gateway-id is required"
[ -n "$GATEWAY_PUBLIC_KEY_FILE" ] || fail "--gateway-public-key-file is required"
[ -n "$ACCESS_TEAM_DOMAIN" ] || fail "--access-team-domain is required"
[ -n "$ACCESS_AUDIENCE" ] || fail "--access-audience is required"
[ -n "$ACCESS_ALLOWED_EMAILS_FILE" ] || fail "--access-allowed-emails-file is required"
case "$PROFILE" in
  edge)
    for value in "$COTURN_IMAGE" "$TURN_PUBLIC_IP" "$TURN_RELAY_IP" \
      "$TURN_AUTH_SECRET_FILE" "$TURN_TLS_CERT_FILE" "$TURN_TLS_KEY_FILE"; do
      [ -z "$value" ] || fail "TURN inputs are forbidden for an edge-only runtime; use --profile edge,turn"
    done
    COTURN_IMAGE=$DISABLED_COTURN_IMAGE
    ;;
  edge,turn)
    [ -n "$COTURN_IMAGE" ] || fail "--coturn-image is required for edge,turn"
    [ -n "$TURN_PUBLIC_IP" ] || fail "--turn-public-ip is required for edge,turn"
    [ -n "$TURN_RELAY_IP" ] || fail "--turn-relay-ip is required for edge,turn"
    [ -n "$TURN_AUTH_SECRET_FILE" ] || fail "--turn-auth-secret-file is required for edge,turn"
    [ -n "$TURN_TLS_CERT_FILE" ] || fail "--turn-tls-cert-file is required for edge,turn"
    [ -n "$TURN_TLS_KEY_FILE" ] || fail "--turn-tls-key-file is required for edge,turn"
    ;;
  *) fail "--profile must be exactly edge or edge,turn" ;;
esac

validate_integer() {
  local label=$1 value=$2
  [[ "$value" =~ ^[0-9]+$ ]] || fail "$label must be an unsigned integer"
  [ "$value" -ge 1 ] 2>/dev/null || fail "$label must be at least 1"
  [ "$value" -le 2147483647 ] 2>/dev/null || fail "$label is too large"
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
  validate_image "--edge-image" "$value"
}

validate_edge_identity() {
  [[ "$EDGE_GATEWAY_ID" =~ ^[a-z0-9]([a-z0-9_-]{0,62}[a-z0-9])?$ ]] ||
    fail "--edge-gateway-id is invalid"
  [[ "$ACCESS_TEAM_DOMAIN" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$ ]] ||
    fail "--access-team-domain must be one Cloudflare Access team host"
  [ "${#ACCESS_AUDIENCE}" -le 256 ] &&
    [[ "$ACCESS_AUDIENCE" =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] ||
    fail "--access-audience is invalid"
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

physical_file_path() {
  local path=$1 parent base physical
  parent=$(dirname -- "$path")
  base=$(basename -- "$path")
  physical=$(CDPATH= cd -- "$parent" 2>/dev/null && pwd -P) || return 1
  if [ "$physical" = / ]; then
    printf '/%s\n' "$base"
  else
    printf '%s/%s\n' "$physical" "$base"
  fi
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

validate_secret_file() {
  local label=$1 path=$2 physical
  validate_abs_path "$label" "$path"
  [ -f "$path" ] || fail "$label is not a regular file"
  [ ! -L "$path" ] || fail "$label must not be a symlink"
  physical=$(physical_file_path "$path") || fail "$label parent is inaccessible"
  [ "$physical" = "$path" ] || fail "$label path traverses a symlink or is not canonical"
  [ "$(stat_mode "$path")" = 600 ] || fail "$label must have mode 0600"
  [ "$(stat_uid "$path")" = "$CURRENT_UID" ] || fail "$label must be owned by uid $CURRENT_UID"
  [ "$(stat_nlink "$path")" = 1 ] || fail "$label must have exactly one hard link"
}

validate_allowed_emails_file() {
  local path=$1 python_bin
  python_bin=$(command -v python3 2>/dev/null) || fail "Python 3 is required"
  "$python_bin" -I - "$path" <<'PY' || fail "Access allowed-email file is invalid"
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
  local path=$1 python_bin
  python_bin=$(command -v python3 2>/dev/null) || fail "Python 3 is required"
  "$python_bin" -I - "$path" <<'PY' || fail "gateway public key must be one canonical PUBLIC KEY PEM block"
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
  local value=$1 a b c d
  validate_ipv4 "--turn-public-ip" "$value"
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
    fail "--turn-public-ip must be a globally routable IPv4 address"
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
  done <"$TEMPLATE_DIR/turnserver.conf.in"
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
  done <"$TEMPLATE_DIR/edge.env.in"
}

validate_integer "--edge-uid" "$EDGE_UID"
validate_integer "--edge-gid" "$EDGE_GID"
[ "$EDGE_UID" = "$CURRENT_UID" ] || fail "--edge-uid must match the owner of the 0600 runtime files ($CURRENT_UID)"
[ "$EDGE_GID" = "$CURRENT_GID" ] || fail "--edge-gid must match the runtime owner group ($CURRENT_GID)"
validate_edge_image "$EDGE_IMAGE"
validate_image "--cloudflared-image" "$CLOUDFLARED_IMAGE"
validate_image "--coturn-image" "$COTURN_IMAGE"
validate_edge_identity
if [ "$PROFILE" = edge,turn ]; then
  validate_public_ipv4 "$TURN_PUBLIC_IP"
  validate_ipv4 "--turn-relay-ip" "$TURN_RELAY_IP"
  [ "$TURN_RELAY_IP" = "$TURN_BRIDGE_IP" ] ||
    fail "--turn-relay-ip must match the pinned coturn bridge address $TURN_BRIDGE_IP"
fi
validate_abs_path "--output" "$OUTPUT"

for item in "cloudflare token:$CLOUDFLARE_TOKEN_FILE"; do
  validate_secret_file "${item%%:*}" "${item#*:}"
done
if [ "$PROFILE" = edge,turn ]; then
  for item in \
    "TURN auth secret:$TURN_AUTH_SECRET_FILE" \
    "TURN TLS certificate:$TURN_TLS_CERT_FILE" \
    "TURN TLS private key:$TURN_TLS_KEY_FILE"; do
    validate_secret_file "${item%%:*}" "${item#*:}"
  done
fi
validate_secret_file "gateway public key" "$GATEWAY_PUBLIC_KEY_FILE"
validate_secret_file "Access allowed-email file" "$ACCESS_ALLOWED_EMAILS_FILE"
validate_allowed_emails_file "$ACCESS_ALLOWED_EMAILS_FILE"
validate_gateway_public_key_file "$GATEWAY_PUBLIC_KEY_FILE"

command -v openssl >/dev/null 2>&1 || fail "openssl is required for offline key and certificate checks"
openssl pkey -pubin -pubcheck -in "$GATEWAY_PUBLIC_KEY_FILE" -noout -text 2>/dev/null |
  grep -Eq 'ASN1 OID: prime256v1|NIST CURVE: P-256' ||
  fail "gateway public key must be a valid P-256 PUBLIC KEY"

CLOUDFLARE_TOKEN=$(read_single_line_secret "cloudflare token" "$CLOUDFLARE_TOKEN_FILE")
[ "${#CLOUDFLARE_TOKEN}" -ge 32 ] && [ "${#CLOUDFLARE_TOKEN}" -le 4096 ] ||
  fail "cloudflare token length must be 32..4096 bytes"
[[ "$CLOUDFLARE_TOKEN" =~ ^[A-Za-z0-9._~+/=-]+$ ]] ||
  fail "cloudflare token contains unsafe characters"

TURN_AUTH_SECRET=""
if [ "$PROFILE" = edge,turn ]; then
  TURN_AUTH_SECRET=$(read_single_line_secret "TURN auth secret" "$TURN_AUTH_SECRET_FILE")
  [ "${#TURN_AUTH_SECRET}" -ge 43 ] && [ "${#TURN_AUTH_SECRET}" -le 128 ] ||
    fail "TURN auth secret length must be 43..128 bytes"
  [[ "$TURN_AUTH_SECRET" =~ ^[A-Za-z0-9_-]+$ ]] ||
    fail "TURN auth secret must be unpadded base64url"

  openssl x509 -in "$TURN_TLS_CERT_FILE" -noout >/dev/null 2>&1 ||
    fail "TURN TLS certificate is not valid PEM X.509"
  openssl x509 -in "$TURN_TLS_CERT_FILE" -checkend 604800 -noout >/dev/null 2>&1 ||
    fail "TURN TLS certificate is expired or expires within seven days"
  openssl x509 -in "$TURN_TLS_CERT_FILE" -text -noout 2>/dev/null |
    grep -Eq 'DNS:turn\.example\.com([,[:space:]]|$)' ||
    fail "TURN TLS certificate SAN must include turn.example.com"
  openssl pkey -in "$TURN_TLS_KEY_FILE" -passin pass: -noout >/dev/null 2>&1 ||
    fail "TURN TLS private key must be valid, unencrypted PEM"
  CERT_PUBLIC_KEY=$(openssl x509 -in "$TURN_TLS_CERT_FILE" -pubkey -noout 2>/dev/null |
    openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256 2>/dev/null)
  KEY_PUBLIC_KEY=$(openssl pkey -in "$TURN_TLS_KEY_FILE" -passin pass: -pubout -outform DER 2>/dev/null |
    openssl dgst -sha256 2>/dev/null)
  [ -n "$CERT_PUBLIC_KEY" ] && [ "$CERT_PUBLIC_KEY" = "$KEY_PUBLIC_KEY" ] ||
    fail "TURN TLS certificate and private key do not match"
fi

[ -x "$VERIFY" ] || fail "static verifier is missing or not executable: $VERIFY"
[ -x "$ATOMIC_COMMIT" ] || fail "atomic output helper is missing or not executable: $ATOMIC_COMMIT"
"$VERIFY" --compose-only --quiet

OUTPUT_PARENT=$(dirname -- "$OUTPUT")
OUTPUT_BASE=$(basename -- "$OUTPUT")
[ "$OUTPUT_BASE" != . ] && [ "$OUTPUT_BASE" != .. ] && [ -n "$OUTPUT_BASE" ] ||
  fail "--output must name a child directory"
[ -d "$OUTPUT_PARENT" ] || fail "--output parent does not exist"
[ ! -L "$OUTPUT_PARENT" ] || fail "--output parent must not be a symlink"
PHYSICAL_PARENT=$(CDPATH= cd -- "$OUTPUT_PARENT" && pwd -P) || fail "--output parent is inaccessible"
[ "$PHYSICAL_PARENT" = "$OUTPUT_PARENT" ] || fail "--output parent path traverses a symlink or is not canonical"
[ "$(stat_uid "$OUTPUT_PARENT")" = "$CURRENT_UID" ] || fail "--output parent must be owned by uid $CURRENT_UID"
PARENT_MODE=$(stat_mode "$OUTPUT_PARENT")
[[ "$PARENT_MODE" =~ ^[0-7]{3}$ ]] || fail "--output parent has an unsupported permission mode"
[ "${PARENT_MODE:0:1}" = 7 ] || fail "--output parent must grant its owner rwx permissions"
case "${PARENT_MODE:1:1}${PARENT_MODE:2:1}" in
  00|01|04|05|10|11|14|15|40|41|44|45|50|51|54|55) ;;
  *) fail "--output parent must not be group- or world-writable" ;;
esac
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] || fail "--output already exists; refusing to overwrite"

TURN_EXTERNAL_IP=""
if [ "$PROFILE" = edge,turn ]; then
  if [ "$TURN_PUBLIC_IP" = "$TURN_RELAY_IP" ]; then TURN_EXTERNAL_IP=$TURN_PUBLIC_IP
  else TURN_EXTERNAL_IP="$TURN_PUBLIC_IP/$TURN_RELAY_IP"; fi
fi

printf 'mode=%s\n' "$([ "$APPLY" -eq 1 ] && printf apply || printf dry-run)"
printf 'output=%s\n' "$OUTPUT"
printf 'services_default=off\n'
printf 'profiles=%s\n' "$PROFILE"
if [ "$PROFILE" = edge,turn ]; then
  printf 'turn_listener=tcp+udp:3478,tls:443\n'
  printf 'turn_relay_udp=49160-49167\n'
fi
printf 'secrets=validated-not-printed\n'

[ "$APPLY" -eq 1 ] || exit 0

STAGE=""
OUTPUT_COMMIT_ATTEMPT=0
OUTPUT_COMMITTED=0
OUTPUT_STAGE_ID=""
cleanup() {
  local current
  if [ -n "$STAGE" ]; then
    case "$STAGE" in
      "$OUTPUT_PARENT"/.dji4g-public-edge.*)
        rm -rf -- "$STAGE"
        ;;
    esac
  fi
  if [ "$OUTPUT_COMMIT_ATTEMPT" -eq 1 ] && [ "$OUTPUT_COMMITTED" -eq 0 ] && \
      [ -n "$OUTPUT_STAGE_ID" ] && [ -d "$OUTPUT" ] && [ ! -L "$OUTPUT" ]; then
    current=$(stat -f '%d:%i' "$OUTPUT" 2>/dev/null || stat -c '%d:%i' "$OUTPUT" 2>/dev/null || true)
    if [ "$current" = "$OUTPUT_STAGE_ID" ]; then
      rm -rf -- "$OUTPUT"
    fi
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

STAGE=$(mktemp -d "$OUTPUT_PARENT/.dji4g-public-edge.XXXXXX")
chmod 700 "$STAGE"
mkdir "$STAGE/config" "$STAGE/secrets"
chmod 700 "$STAGE/config" "$STAGE/secrets"

printf '%s\n' \
  "DJI4G_PUBLIC_EDGE_ROOT=$OUTPUT" \
  "DJI4G_EDGE_ENV_FILE=$OUTPUT/config/edge.env" \
  "DJI4G_ENABLED_PROFILES=$PROFILE" \
  "DJI4G_EDGE_UID=$EDGE_UID" \
  "DJI4G_EDGE_GID=$EDGE_GID" \
  "DJI4G_EDGE_IMAGE=$EDGE_IMAGE" \
  "DJI4G_CLOUDFLARED_IMAGE=$CLOUDFLARED_IMAGE" \
  "DJI4G_COTURN_IMAGE=$COTURN_IMAGE" \
  >"$STAGE/compose.env"

render_edge_template "$STAGE/config/edge.env" "$EDGE_GATEWAY_ID" "$ACCESS_TEAM_DOMAIN" "$ACCESS_AUDIENCE"
grep -q '__[A-Z0-9_][A-Z0-9_]*__' "$STAGE/config/edge.env" && fail "an unresolved edge placeholder remains"

printf '%s' "$CLOUDFLARE_TOKEN" >"$STAGE/secrets/cloudflare-tunnel.token"
cp "$GATEWAY_PUBLIC_KEY_FILE" "$STAGE/config/gateway-public-key.pem"
cp "$ACCESS_ALLOWED_EMAILS_FILE" "$STAGE/config/access-allowed-emails"
chmod 600 \
  "$STAGE/compose.env" \
  "$STAGE/config/edge.env" \
  "$STAGE/config/gateway-public-key.pem" \
  "$STAGE/config/access-allowed-emails" \
  "$STAGE/secrets/cloudflare-tunnel.token"
if [ "$PROFILE" = edge,turn ]; then
  render_turn_template "$STAGE/config/turnserver.conf" "$TURN_RELAY_IP" "$TURN_EXTERNAL_IP" "$TURN_AUTH_SECRET"
  grep -q '__[A-Z0-9_][A-Z0-9_]*__' "$STAGE/config/turnserver.conf" && fail "an unresolved TURN placeholder remains"
  printf '%s' "$TURN_AUTH_SECRET" >"$STAGE/secrets/turn-auth-secret"
  cp "$TURN_TLS_CERT_FILE" "$STAGE/secrets/turn-tls-cert.pem"
  cp "$TURN_TLS_KEY_FILE" "$STAGE/secrets/turn-tls-key.pem"
  chmod 600 "$STAGE/config/turnserver.conf" "$STAGE/secrets/turn-auth-secret" \
    "$STAGE/secrets/turn-tls-cert.pem" "$STAGE/secrets/turn-tls-key.pem"
fi

"$VERIFY" --declared-root "$OUTPUT" --quiet "$STAGE"
OUTPUT_STAGE_ID=$(stat -f '%d:%i' "$STAGE" 2>/dev/null || stat -c '%d:%i' "$STAGE")
OUTPUT_COMMIT_ATTEMPT=1
COMMITTED_ID=$(env -i PATH="$PATH" python3 -I "$ATOMIC_COMMIT" --source "$STAGE" --target "$OUTPUT")
[ "$COMMITTED_ID" = "$OUTPUT_STAGE_ID" ] || fail "atomic runtime commit returned a different inode"
STAGE=""
"$VERIFY" --quiet "$OUTPUT"
trap '' HUP INT TERM
OUTPUT_COMMITTED=1
printf 'created=%s\n' "$OUTPUT"
