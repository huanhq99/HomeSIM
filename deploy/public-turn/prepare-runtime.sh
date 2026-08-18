#!/usr/bin/env bash
set -euo pipefail

umask 077
export LC_ALL=C

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
TEMPLATE="$SCRIPT_DIR/turnserver.conf.in"
VERIFY="$SCRIPT_DIR/verify-runtime.sh"
CURRENT_UID=$(id -u)
CURRENT_GID=$(id -g)

OUTPUT=""
IMAGE=""
PUBLIC_IP=""
AUTH_SECRET_FILE=""
TLS_CERT_FILE=""
TLS_KEY_FILE=""
APPLY=0
OUTPUT_CREATED=0
OUTPUT_ID=""

usage() {
  cat <<'EOF'
Usage:
  prepare-runtime.sh \
    --runtime-root /absolute/new/runtime-v1 \
    --image coturn/coturn@sha256:<64-lowercase-hex> \
    --public-ip A.B.C.D \
    --auth-secret-file /absolute/0600/base64url-secret \
    --tls-cert-file /absolute/0600/fullchain.pem \
    --tls-key-file /absolute/0600/privkey.pem \
    [--apply]

Dry-run is the default. --apply creates one new private runtime root and
refuses to overwrite an existing path. Secret values are never printed.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

stat_id() {
  stat -f '%d:%i' "$1" 2>/dev/null || stat -c '%d:%i' "$1"
}

cleanup_created_output() {
  [ "$OUTPUT_CREATED" -eq 1 ] || return 0
  [ -d "$OUTPUT" ] && [ ! -L "$OUTPUT" ] || return 0
  [ "$(stat_id "$OUTPUT" 2>/dev/null || true)" = "$OUTPUT_ID" ] || return 0
  set +e
  rm -f -- \
    "$OUTPUT/compose.env" "$OUTPUT/config/turnserver.conf" \
    "$OUTPUT/secrets/turn-auth-secret" "$OUTPUT/secrets/turn-tls-cert.pem" \
    "$OUTPUT/secrets/turn-tls-key.pem"
  rmdir "$OUTPUT/config" "$OUTPUT/secrets" "$OUTPUT" 2>/dev/null
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  cleanup_created_output
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

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

need_value() {
  [ "$#" -ge 2 ] && [ -n "$2" ] || fail "$1 requires a value"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --runtime-root) need_value "$@"; OUTPUT=$2; shift 2 ;;
    --image) need_value "$@"; IMAGE=$2; shift 2 ;;
    --public-ip) need_value "$@"; PUBLIC_IP=$2; shift 2 ;;
    --auth-secret-file) need_value "$@"; AUTH_SECRET_FILE=$2; shift 2 ;;
    --tls-cert-file) need_value "$@"; TLS_CERT_FILE=$2; shift 2 ;;
    --tls-key-file) need_value "$@"; TLS_KEY_FILE=$2; shift 2 ;;
    --apply) APPLY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

validate_abs_path() {
  local label=$1 value=$2
  case "$value" in /*) ;; *) fail "$label must be absolute" ;; esac
  [ "${#value}" -le 240 ] || fail "$label is too long"
  case "$value" in
    *[!A-Za-z0-9_./-]*) fail "$label contains unsupported characters" ;;
    *//*|*/./*|*/../*|*/.|*/..) fail "$label is not canonical" ;;
  esac
}

validate_image() {
  local value=$1 name last
  [[ "$value" =~ ^[a-z0-9][a-z0-9./_-]*@sha256:[0-9a-f]{64}$ ]] ||
    fail "--image must be an exact lowercase name@sha256 digest without a tag"
  name=${value%@sha256:*}
  [[ "$name" == */* && "$name" != */ && "$name" != *//* && "$name" != *../* && "$name" != */..* ]] ||
    fail "--image repository path is ambiguous"
  last=${name##*/}
  [[ "$last" != *:* ]] || fail "--image must not combine a mutable tag with its digest"
}

validate_ipv4() {
  local label=$1 value=$2 a b c d part
  IFS=. read -r a b c d <<EOF
$value
EOF
  [ -n "${a:-}" ] && [ -n "${b:-}" ] && [ -n "${c:-}" ] && [ -n "${d:-}" ] ||
    fail "$label must contain four IPv4 octets"
  [ "$value" = "$a.$b.$c.$d" ] || fail "$label must be canonical IPv4"
  for part in "$a" "$b" "$c" "$d"; do
    [[ "$part" =~ ^(0|[1-9][0-9]{0,2})$ ]] || fail "$label contains a non-canonical octet"
    [ "$part" -le 255 ] || fail "$label contains an octet above 255"
  done
  [ "$a" -ne 0 ] && [ "$a" -ne 127 ] && [ "$a" -lt 224 ] || fail "$label must be unicast IPv4"
}

validate_public_ipv4() {
  local label=$1 value=$2 a b c d
  validate_ipv4 "$label" "$value"
  IFS=. read -r a b c d <<EOF
$value
EOF
  ! { [ "$a" -eq 10 ] ||
      { [ "$a" -eq 100 ] && [ "$b" -ge 64 ] && [ "$b" -le 127 ]; } ||
      { [ "$a" -eq 169 ] && [ "$b" -eq 254 ]; } ||
      { [ "$a" -eq 172 ] && [ "$b" -ge 16 ] && [ "$b" -le 31 ]; } ||
      { [ "$a" -eq 192 ] && [ "$b" -eq 168 ]; } ||
      { [ "$a" -eq 192 ] && [ "$b" -eq 0 ] && { [ "$c" -eq 0 ] || [ "$c" -eq 2 ]; }; } ||
      { [ "$a" -eq 198 ] && { [ "$b" -eq 18 ] || [ "$b" -eq 19 ]; }; } ||
      { [ "$a" -eq 198 ] && [ "$b" -eq 51 ] && [ "$c" -eq 100 ]; } ||
      { [ "$a" -eq 203 ] && [ "$b" -eq 0 ] && [ "$c" -eq 113 ]; }; } ||
    fail "$label must be globally routable IPv4"
}

validate_private_input() {
  local label=$1 path=$2 parent base physical
  validate_abs_path "$label" "$path"
  [ -f "$path" ] && [ ! -L "$path" ] || fail "$label must be a regular non-symlink file"
  parent=$(dirname -- "$path")
  base=$(basename -- "$path")
  physical=$(CDPATH= cd -- "$parent" 2>/dev/null && pwd -P) || fail "$label parent is inaccessible"
  if [ "$physical" = / ]; then physical="/$base"; else physical="$physical/$base"; fi
  [ "$physical" = "$path" ] || fail "$label must not traverse symlinks"
  [ "$(stat_mode "$path")" = 600 ] || fail "$label must have mode 0600"
  [ "$(stat_uid "$path")" = "$CURRENT_UID" ] || fail "$label must be owned by uid $CURRENT_UID"
  [ "$(stat_nlink "$path")" = 1 ] || fail "$label must have exactly one hard link"
}

read_single_line_secret() {
  local path=$1 value file_bytes value_bytes last_byte
  value=$(<"$path")
  [ -n "$value" ] || fail "TURN auth secret is empty"
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || fail "TURN auth secret contains a line break"
  file_bytes=$(wc -c <"$path" | tr -d ' ')
  value_bytes=$(printf '%s' "$value" | wc -c | tr -d ' ')
  if [ "$file_bytes" -eq $((value_bytes + 1)) ]; then
    last_byte=$(tail -c 1 "$path" | od -An -tu1 | tr -d ' ')
    [ "$last_byte" = 10 ] || fail "TURN auth secret contains a hidden byte"
  else
    [ "$file_bytes" -eq "$value_bytes" ] || fail "TURN auth secret contains a hidden byte"
  fi
  printf '%s' "$value"
}

render_config() {
  local output=$1 public_ip=$2 auth_secret=$3 line
  : >"$output"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      'external-ip=__TURN_PUBLIC_IP__/172.30.247.2')
        printf 'external-ip=%s/172.30.247.2\n' "$public_ip" >>"$output"
        ;;
      'static-auth-secret=__TURN_AUTH_SECRET__')
        printf 'static-auth-secret=%s\n' "$auth_secret" >>"$output"
        ;;
      *) printf '%s\n' "$line" >>"$output" ;;
    esac
  done <"$TEMPLATE"
}

[ "$CURRENT_UID" -gt 0 ] && [ "$CURRENT_GID" -gt 0 ] || fail "run as the non-root runtime owner"
[ -n "$OUTPUT" ] || fail "--runtime-root is required"
[ -n "$IMAGE" ] || fail "--image is required"
[ -n "$PUBLIC_IP" ] || fail "--public-ip is required"
[ -n "$AUTH_SECRET_FILE" ] || fail "--auth-secret-file is required"
[ -n "$TLS_CERT_FILE" ] || fail "--tls-cert-file is required"
[ -n "$TLS_KEY_FILE" ] || fail "--tls-key-file is required"
[ -f "$TEMPLATE" ] && [ ! -L "$TEMPLATE" ] || fail "turnserver template is missing or symlinked"
[ -x "$VERIFY" ] || fail "verify-runtime.sh is missing or not executable"

validate_abs_path "runtime root" "$OUTPUT"
validate_image "$IMAGE"
validate_public_ipv4 "TURN public IP" "$PUBLIC_IP"
validate_private_input "TURN auth secret file" "$AUTH_SECRET_FILE"
validate_private_input "TURN TLS certificate file" "$TLS_CERT_FILE"
validate_private_input "TURN TLS private key file" "$TLS_KEY_FILE"
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] || fail "runtime root already exists; choose a new versioned path"

PARENT=$(dirname -- "$OUTPUT")
[ -d "$PARENT" ] && [ ! -L "$PARENT" ] || fail "runtime parent must be a real directory"
[ "$(CDPATH= cd -- "$PARENT" && pwd -P)" = "$PARENT" ] || fail "runtime parent must be physical"
[ -w "$PARENT" ] || fail "runtime parent is not writable by the runtime owner"

AUTH_SECRET=$(read_single_line_secret "$AUTH_SECRET_FILE")
[ "${#AUTH_SECRET}" -ge 43 ] && [ "${#AUTH_SECRET}" -le 128 ] || fail "TURN auth secret length must be 43..128 bytes"
[[ "$AUTH_SECRET" =~ ^[A-Za-z0-9_-]+$ ]] || fail "TURN auth secret must be unpadded base64url"

command -v openssl >/dev/null 2>&1 || fail "openssl is required"
openssl x509 -in "$TLS_CERT_FILE" -noout >/dev/null 2>&1 || fail "TURN certificate is not valid PEM X.509"
openssl x509 -in "$TLS_CERT_FILE" -checkend 604800 -noout >/dev/null 2>&1 ||
  fail "TURN certificate is expired or expires within seven days"
openssl x509 -in "$TLS_CERT_FILE" -ext subjectAltName -noout 2>/dev/null |
  tr ',' '\n' | grep -Eq '^[[:space:]]*DNS:turn\.example\.com[[:space:]]*$' ||
  fail "TURN certificate SAN must include turn.example.com"
openssl pkey -in "$TLS_KEY_FILE" -passin pass: -noout >/dev/null 2>&1 ||
  fail "TURN private key must be valid unencrypted PEM"
CERT_KEY=$(openssl x509 -in "$TLS_CERT_FILE" -pubkey -noout 2>/dev/null |
  openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256 2>/dev/null)
PRIVATE_KEY=$(openssl pkey -in "$TLS_KEY_FILE" -passin pass: -pubout -outform DER 2>/dev/null |
  openssl dgst -sha256 2>/dev/null)
[ -n "$CERT_KEY" ] && [ "$CERT_KEY" = "$PRIVATE_KEY" ] || fail "TURN certificate and private key do not match"

if [ "$APPLY" -eq 0 ]; then
  printf '%s\n' \
    "DRY RUN: would create $OUTPUT" \
    "  service: turn.example.com on 3478/udp, 3478/tcp and 443/tcp" \
    "  relay: UDP 49160-49167 via bridge 172.30.247.0/28" \
    "  image: $IMAGE" \
    "  owner: $CURRENT_UID:$CURRENT_GID" \
    "No files changed. Re-run with --apply to create it."
  exit 0
fi

mkdir -- "$OUTPUT"
OUTPUT_CREATED=1
OUTPUT_ID=$(stat_id "$OUTPUT")
chmod 700 "$OUTPUT"
mkdir "$OUTPUT/config" "$OUTPUT/secrets"
chmod 700 "$OUTPUT/config" "$OUTPUT/secrets"

printf 'DJI4G_COTURN_IMAGE=%s\nDJI4G_TURN_UID=%s\nDJI4G_TURN_GID=%s\n' \
  "$IMAGE" "$CURRENT_UID" "$CURRENT_GID" >"$OUTPUT/compose.env"
render_config "$OUTPUT/config/turnserver.conf" "$PUBLIC_IP" "$AUTH_SECRET"
printf '%s' "$AUTH_SECRET" >"$OUTPUT/secrets/turn-auth-secret"
cp "$TLS_CERT_FILE" "$OUTPUT/secrets/turn-tls-cert.pem"
cp "$TLS_KEY_FILE" "$OUTPUT/secrets/turn-tls-key.pem"
chmod 600 \
  "$OUTPUT/compose.env" "$OUTPUT/config/turnserver.conf" \
  "$OUTPUT/secrets/turn-auth-secret" "$OUTPUT/secrets/turn-tls-cert.pem" \
  "$OUTPUT/secrets/turn-tls-key.pem"

"$VERIFY" "$OUTPUT" >/dev/null
OUTPUT_CREATED=0
printf 'Created verified TURN runtime: %s (secret redacted)\n' "$OUTPUT"
