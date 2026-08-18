#!/usr/bin/env bash
set -euo pipefail

umask 077
export LC_ALL=C

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
TEMPLATE="$SCRIPT_DIR/turnserver.conf.in"
ROOT=${1:-}
CURRENT_UID=$(id -u)
CURRENT_GID=$(id -g)
EXPECTED=""

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ -n "$EXPECTED" ]; then
    case "$EXPECTED" in
      "${TMPDIR:-/tmp}"/dji4g-public-turn.verify.??????|/tmp/dji4g-public-turn.verify.??????)
        rm -f -- "$EXPECTED"
        ;;
    esac
  fi
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
    fail "coturn image must be an exact lowercase name@sha256 digest without a tag"
  name=${value%@sha256:*}
  [[ "$name" == */* && "$name" != */ && "$name" != *//* && "$name" != *../* && "$name" != */..* ]] ||
    fail "coturn image repository path is ambiguous"
  last=${name##*/}
  [[ "$last" != *:* ]] || fail "coturn image must not combine a mutable tag with its digest"
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

check_private_dir() {
  local path=$1 label=$2
  [ -d "$path" ] && [ ! -L "$path" ] || fail "$label must be a real directory"
  [ "$(stat_mode "$path")" = 700 ] || fail "$label must have mode 0700"
  [ "$(stat_uid "$path")" = "$CURRENT_UID" ] || fail "$label must be owned by uid $CURRENT_UID"
}

check_private_file() {
  local path=$1 label=$2
  [ -f "$path" ] && [ ! -L "$path" ] || fail "$label must be a real file"
  [ "$(stat_mode "$path")" = 600 ] || fail "$label must have mode 0600"
  [ "$(stat_uid "$path")" = "$CURRENT_UID" ] || fail "$label must be owned by uid $CURRENT_UID"
}

read_env() {
  local key=$1 count line
  count=$(grep -Ec "^${key}=" "$ROOT/compose.env" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once"
  line=$(grep -E "^${key}=" "$ROOT/compose.env")
  printf '%s' "${line#*=}"
}

read_conf() {
  local key=$1 count line
  count=$(grep -Ec "^${key}=" "$ROOT/config/turnserver.conf" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once in turnserver.conf"
  line=$(grep -E "^${key}=" "$ROOT/config/turnserver.conf")
  printf '%s' "${line#*=}"
}

render_expected() {
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

[ -n "$ROOT" ] || fail "usage: verify-runtime.sh /absolute/runtime/root"
validate_abs_path "runtime root" "$ROOT"
[ -f "$TEMPLATE" ] && [ ! -L "$TEMPLATE" ] || fail "turnserver template is missing or symlinked"
[ "$(CDPATH= cd -- "$ROOT" 2>/dev/null && pwd -P)" = "$ROOT" ] || fail "runtime root must be physical"

check_private_dir "$ROOT" "runtime root"
check_private_dir "$ROOT/config" "config directory"
check_private_dir "$ROOT/secrets" "secrets directory"

for relative in \
  compose.env config/turnserver.conf secrets/turn-auth-secret \
  secrets/turn-tls-cert.pem secrets/turn-tls-key.pem; do
  check_private_file "$ROOT/$relative" "$relative"
done

while IFS= read -r path; do
  relative=${path#"$ROOT/"}
  case "$relative" in
    config|secrets|compose.env|config/turnserver.conf|secrets/turn-auth-secret|\
      secrets/turn-tls-cert.pem|secrets/turn-tls-key.pem) ;;
    *) fail "unexpected runtime path: $relative" ;;
  esac
done < <(find "$ROOT" -mindepth 1 -maxdepth 2 -print)

[ "$(awk 'END { print NR + 0 }' "$ROOT/compose.env")" -eq 3 ] || fail "compose.env must contain exactly three lines"
IMAGE=$(read_env DJI4G_COTURN_IMAGE)
RUNTIME_UID=$(read_env DJI4G_TURN_UID)
RUNTIME_GID=$(read_env DJI4G_TURN_GID)
validate_image "$IMAGE"
[[ "$RUNTIME_UID" =~ ^[0-9]+$ ]] && [ "$RUNTIME_UID" -gt 0 ] || fail "runtime uid is invalid"
[[ "$RUNTIME_GID" =~ ^[0-9]+$ ]] && [ "$RUNTIME_GID" -gt 0 ] || fail "runtime gid is invalid"
[ "$RUNTIME_UID" = "$CURRENT_UID" ] && [ "$RUNTIME_GID" = "$CURRENT_GID" ] ||
  fail "runtime uid/gid must match the invoking non-root owner"

SECRET=$(<"$ROOT/secrets/turn-auth-secret")
SECRET_BYTES=$(wc -c <"$ROOT/secrets/turn-auth-secret" | tr -d ' ')
[ "$SECRET_BYTES" -eq "${#SECRET}" ] || fail "TURN auth secret must be one line without a trailing newline"
[ "${#SECRET}" -ge 43 ] && [ "${#SECRET}" -le 128 ] || fail "TURN auth secret length must be 43..128 bytes"
[[ "$SECRET" =~ ^[A-Za-z0-9_-]+$ ]] || fail "TURN auth secret must be unpadded base64url"

MAPPING=$(read_conf external-ip)
case "$MAPPING" in
  */172.30.247.2) PUBLIC_IP=${MAPPING%/172.30.247.2} ;;
  *) fail "external-ip must map the public IPv4 to 172.30.247.2" ;;
esac
validate_public_ipv4 "TURN public IP" "$PUBLIC_IP"
[ "$(read_conf static-auth-secret)" = "$SECRET" ] || fail "TURN auth secret does not match turnserver.conf"

EXPECTED=$(mktemp "${TMPDIR:-/tmp}/dji4g-public-turn.verify.XXXXXX")
chmod 600 "$EXPECTED"
render_expected "$EXPECTED" "$PUBLIC_IP" "$SECRET"
cmp -s "$EXPECTED" "$ROOT/config/turnserver.conf" || fail "turnserver.conf differs from the exact checked-in policy"

command -v openssl >/dev/null 2>&1 || fail "openssl is required"
openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -noout >/dev/null 2>&1 || fail "TURN certificate is not valid PEM X.509"
openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -checkend 604800 -noout >/dev/null 2>&1 ||
  fail "TURN certificate is expired or expires within seven days"
openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -ext subjectAltName -noout 2>/dev/null |
  tr ',' '\n' | grep -Eq '^[[:space:]]*DNS:turn\.example\.com[[:space:]]*$' ||
  fail "TURN certificate SAN must include turn.example.com"
openssl pkey -in "$ROOT/secrets/turn-tls-key.pem" -passin pass: -noout >/dev/null 2>&1 ||
  fail "TURN private key must be valid unencrypted PEM"
CERT_KEY=$(openssl x509 -in "$ROOT/secrets/turn-tls-cert.pem" -pubkey -noout 2>/dev/null |
  openssl pkey -pubin -outform DER 2>/dev/null | openssl dgst -sha256 2>/dev/null)
PRIVATE_KEY=$(openssl pkey -in "$ROOT/secrets/turn-tls-key.pem" -passin pass: -pubout -outform DER 2>/dev/null |
  openssl dgst -sha256 2>/dev/null)
[ -n "$CERT_KEY" ] && [ "$CERT_KEY" = "$PRIVATE_KEY" ] || fail "TURN certificate and private key do not match"

printf 'TURN runtime verified: %s (secret redacted)\n' "$ROOT"
