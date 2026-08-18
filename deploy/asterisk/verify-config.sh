#!/bin/sh
set -eu

LC_ALL=C
export LC_ALL
umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

usage() {
  cat <<'EOF'
Usage: deploy/asterisk/verify-config.sh GENERATED_ROOT [--expected-root PATH]

Performs a static, non-networking verification. It does not contact Docker,
Asterisk, the cellular appliance, or any remote device.
EOF
}

die() {
  printf 'VERIFY FAIL: %s\n' "$1" >&2
  exit "${2:-1}"
}

[ "$#" -ge 1 ] || { usage >&2; exit 64; }
case "$1" in -h|--help) usage; exit 0 ;; esac
ROOT=$1
shift
EXPECTED_ROOT=$ROOT
if [ "$#" -gt 0 ]; then
  [ "$#" -eq 2 ] && [ "$1" = --expected-root ] || die "invalid verifier arguments" 64
  EXPECTED_ROOT=$2
fi

case "$ROOT,$EXPECTED_ROOT" in
  /*,/*) ;;
  *) die "generated and expected roots must be absolute" 64 ;;
esac
[ -d "$ROOT" ] && [ ! -L "$ROOT" ] || die "generated root must be a non-symlink directory"
EXPECTED_PARENT=$(dirname -- "$EXPECTED_ROOT")
[ -d "$EXPECTED_PARENT" ] && [ ! -L "$EXPECTED_PARENT" ] || die "expected-root parent must be a non-symlink directory"
EXPECTED_ROOT="$(CDPATH= cd -- "$EXPECTED_PARENT" && pwd -P)/$(basename -- "$EXPECTED_ROOT")"

case "$(uname -s)" in
  Darwin)
    stat_mode() { stat -f '%Lp' "$1"; }
    stat_uid() { stat -f '%u' "$1"; }
    stat_nlink() { stat -f '%l' "$1"; }
    ;;
  *)
    stat_mode() { stat -c '%a' "$1"; }
    stat_uid() { stat -c '%u' "$1"; }
    stat_nlink() { stat -c '%h' "$1"; }
    ;;
esac

CURRENT_UID=$(id -u)
for directory in "$ROOT" "$ROOT/config" "$ROOT/secrets" "$ROOT/data" "$ROOT/log" "$ROOT/spool"; do
  [ -d "$directory" ] && [ ! -L "$directory" ] || die "missing private directory: $directory"
  [ "$(stat_mode "$directory")" = 700 ] || die "directory mode is not 0700: $directory"
  [ "$(stat_uid "$directory")" = "$CURRENT_UID" ] || die "directory is not owned by current user: $directory"
done

EXPECTED_FILES='source.lock
deployment.env
compose.env
config/acl.conf
config/asterisk.conf
config/ari.conf
config/ccss.conf
config/cdr.conf
config/cel.conf
config/chan_websocket.conf
config/extensions.conf
config/features.conf
config/http.conf
config/indications.conf
config/logger.conf
config/manager.conf
config/modules.conf
config/pjproject.conf
config/pjsip.conf
config/rtp.conf
config/stasis.conf
config/udptl.conf
config/websocket_client.conf
secrets/ari-control.password
secrets/ari-inspect.password'

VERIFY_TEMP=$(mktemp -d "${TMPDIR:-/tmp}/maccellular-asterisk-verify.XXXXXX")
LIST_FILE="$VERIFY_TEMP/generated-files.txt"
cleanup() {
  case "${VERIFY_TEMP:-}" in
    "${TMPDIR:-/tmp}"/maccellular-asterisk-verify.*) /bin/rm -rf -- "$VERIFY_TEMP" ;;
  esac
}
trap cleanup EXIT HUP INT TERM
(cd "$ROOT" && find . -type f -print | sed 's#^./##' | sort) >"$LIST_FILE"
if [ "$(printf '%s\n' "$EXPECTED_FILES" | sort)" != "$(cat "$LIST_FILE")" ]; then
  die "generated tree contains missing or unexpected files"
fi
if find "$ROOT" -type l -print | grep -q .; then
  die "generated tree contains a symbolic link"
fi

while IFS= read -r relative; do
  file="$ROOT/$relative"
  [ -f "$file" ] && [ ! -L "$file" ] || die "not a regular file: $relative"
  [ "$(stat_mode "$file")" = 600 ] || die "file mode is not 0600: $relative"
  [ "$(stat_uid "$file")" = "$CURRENT_UID" ] || die "file is not owned by current user: $relative"
  [ "$(stat_nlink "$file")" = 1 ] || die "file has more than one hard link: $relative"
done <"$LIST_FILE"

env_value() {
  awk -F= -v wanted="$2" '
    $1 == wanted { count++; value = substr($0, length(wanted) + 2) }
    END { if (count != 1 || value == "") exit 1; print value }
  ' "$1"
}

ini_value() {
  awk -v wanted="[$2]" -v key="$3" '
    /^\[/ { active = ($0 == wanted) }
    active {
      line = $0
      sub(/^[[:space:]]*/, "", line)
      equals = key " = "
      arrow = key " => "
      if (index(line, equals) == 1) {
        count++
        value = substr(line, length(equals) + 1)
      } else if (index(line, arrow) == 1) {
        count++
        value = substr(line, length(arrow) + 1)
      }
    }
    END { if (count != 1) exit 1; print value }
  ' "$1"
}

assert_ini() {
  actual=$(ini_value "$1" "$2" "$3") || die "missing unique $3 in [$2]"
  [ "$actual" = "$4" ] || die "unexpected $3 in [$2]"
}

is_canonical_decimal() {
  case "$1" in
    ''|*[!0-9]*) return 1 ;;
    0|[1-9]*) return 0 ;;
    *) return 1 ;;
  esac
}

decimal_in_range() {
  value=$1
  minimum=$2
  maximum=$3
  is_canonical_decimal "$value" || return 1
  awk -v value="$value" -v minimum="$minimum" -v maximum="$maximum" '
    BEGIN { exit !(value >= minimum && value <= maximum) }
  '
}

is_ipv4() {
  printf '%s\n' "$1" | awk -F. '
    NF != 4 { exit 1 }
    {
      for (i = 1; i <= 4; i++) {
        if ($i !~ /^[0-9]+$/ || $i < 0 || $i > 255 || ($i != "0" && $i ~ /^0/)) exit 1
      }
    }
  '
}

is_private_ipv4() {
  printf '%s\n' "$1" | awk -F. '
    NF != 4 { exit 1 }
    $1 == 10 { exit 0 }
    $1 == 172 && $2 >= 16 && $2 <= 31 { exit 0 }
    $1 == 192 && $2 == 168 { exit 0 }
    $1 == 100 && $2 >= 64 && $2 <= 127 { exit 0 }
    { exit 1 }
  '
}

is_token() {
  value=$1
  [ "${#value}" -ge 1 ] && [ "${#value}" -le 64 ] || return 1
  case "$value" in
    [abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ]*) ;;
    *) return 1 ;;
  esac
  case "$value" in
    *[!A-Za-z0-9_.-]*) return 1 ;;
    *) return 0 ;;
  esac
}

is_generator_password() {
  [ "${#1}" -eq 64 ] || return 1
  case "$1" in
    *[!A-Za-z0-9+/]*) return 1 ;;
    *) return 0 ;;
  esac
}

find_sha512_crypt_openssl() {
  path_openssl=$(command -v openssl 2>/dev/null || true)
  for candidate in \
    "$path_openssl" \
    /opt/homebrew/opt/openssl@3/bin/openssl \
    /opt/homebrew/bin/openssl \
    /usr/local/opt/openssl@3/bin/openssl \
    /usr/local/bin/openssl; do
    [ -n "$candidate" ] && [ -x "$candidate" ] || continue
    if printf '%s\n' 'synthetic-probe' | "$candidate" passwd -6 -stdin >/dev/null 2>&1; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

OPENSSL_SHA512=$(find_sha512_crypt_openssl) || \
  die "OpenSSL 3 with passwd -6 support is required to verify ARI credentials" 69

verify_password_hash() {
  hash_label=$1
  plaintext=$2
  stored_hash=$3
  case "$stored_hash" in
    '$6$'*) ;;
    *) die "$hash_label ARI hash is not SHA-512 crypt" ;;
  esac
  hash_body=${stored_hash#'$6$'}
  hash_salt=${hash_body%%\$*}
  hash_digest=${hash_body#*\$}
  [ "$hash_salt" != "$hash_body" ] || die "$hash_label ARI hash has no digest separator"
  [ "${#hash_salt}" -ge 1 ] && [ "${#hash_salt}" -le 16 ] || die "$hash_label ARI hash salt length is invalid"
  [ "${#hash_digest}" -eq 86 ] || die "$hash_label ARI hash digest length is invalid"
  case "$hash_salt$hash_digest" in
    *[!A-Za-z0-9./]*) die "$hash_label ARI hash contains unsafe characters" ;;
  esac
  recomputed_hash=$(printf '%s\n' "$plaintext" | "$OPENSSL_SHA512" passwd -6 -salt "$hash_salt" -stdin) || \
    die "could not verify $hash_label ARI hash"
  [ "$recomputed_hash" = "$stored_hash" ] || die "$hash_label ARI hash does not correspond to its secret"
}

lan_contract_is_consistent() {
  awk -v gateway="$1" -v bindip="$2" -v network_ip="$3" -v prefix="$4" '
    function number(ip, parts) {
      split(ip, parts, ".")
      return (((parts[1] * 256) + parts[2]) * 256 + parts[3]) * 256 + parts[4]
    }
    BEGIN {
      block = 1
      for (i = prefix; i < 32; i++) block *= 2
      network = number(network_ip)
      canonical = int(network / block) * block
      broadcast = canonical + block - 1
      gateway_number = number(gateway)
      bind_number = number(bindip)
      if (network != canonical) exit 1
      if (gateway_number <= canonical || gateway_number >= broadcast) exit 1
      if (bind_number <= canonical || bind_number >= broadcast) exit 1
    }
  '
}

container_network_is_consistent() {
  awk -v address="$1" -v network_ip="$2" -v prefix="$3" '
    function number(ip, parts) {
      split(ip, parts, ".")
      return (((parts[1] * 256) + parts[2]) * 256 + parts[3]) * 256 + parts[4]
    }
    BEGIN {
      block = 1
      for (i = prefix; i < 32; i++) block *= 2
      network = number(network_ip)
      canonical = int(network / block) * block
      broadcast = canonical + block - 1
      address_number = number(address)
      if (network != canonical) exit 1
      if (address_number <= canonical || address_number >= broadcast) exit 1
    }
  '
}

cidrs_do_not_overlap() {
  awk -v first_ip="$1" -v first_prefix="$2" -v second_ip="$3" -v second_prefix="$4" '
    function number(ip, parts) {
      split(ip, parts, ".")
      return (((parts[1] * 256) + parts[2]) * 256 + parts[3]) * 256 + parts[4]
    }
    function block_size(prefix, size, i) {
      size = 1
      for (i = prefix; i < 32; i++) size *= 2
      return size
    }
    BEGIN {
      first_block = block_size(first_prefix)
      first_start = int(number(first_ip) / first_block) * first_block
      first_end = first_start + first_block - 1
      second_block = block_size(second_prefix)
      second_start = int(number(second_ip) / second_block) * second_block
      second_end = second_start + second_block - 1
      if (!(first_end < second_start || second_end < first_start)) exit 1
    }
  '
}

render_expected_config() {
  input=$1
  output_file=$2
  awk \
    -v entity_id="$ENTITY_ID" \
    -v control_user="$CONTROL_USER" \
    -v inspect_user="$INSPECT_USER" \
    -v control_hash="$CONTROL_HASH" \
    -v inspect_hash="$INSPECT_HASH" \
    -v context="$CONTEXT" \
    -v endpoint="$ENDPOINT" \
    -v application="$APPLICATION" \
    -v argument="$ARGUMENT" \
    -v policy_id="$POLICY_ID" \
    -v max_call_seconds="$MAX_CALL_SECONDS" \
    -v sip_bind_ip="$SIP_BIND_IP" \
    -v sip_port="$SIP_PORT" \
    -v gateway_ip="$GATEWAY_IP" \
    -v pbx_container_subnet="$PBX_CONTAINER_SUBNET" \
    -v rtp_min="$RTP_MIN" \
    -v rtp_max="$RTP_MAX" '
      {
        gsub(/@@ENTITY_ID@@/, entity_id)
        gsub(/@@CONTROL_USER@@/, control_user)
        gsub(/@@INSPECT_USER@@/, inspect_user)
        gsub(/@@CONTROL_HASH@@/, control_hash)
        gsub(/@@INSPECT_HASH@@/, inspect_hash)
        gsub(/@@CONTEXT@@/, context)
        gsub(/@@ENDPOINT@@/, endpoint)
        gsub(/@@APPLICATION@@/, application)
        gsub(/@@ARGUMENT@@/, argument)
        gsub(/@@POLICY_ID@@/, policy_id)
        gsub(/@@MAX_CALL_SECONDS@@/, max_call_seconds)
        gsub(/@@SIP_BIND_IP@@/, sip_bind_ip)
        gsub(/@@SIP_PORT@@/, sip_port)
        gsub(/@@GATEWAY_IP@@/, gateway_ip)
        gsub(/@@PBX_CONTAINER_SUBNET@@/, pbx_container_subnet)
        gsub(/@@RTP_MIN@@/, rtp_min)
        gsub(/@@RTP_MAX@@/, rtp_max)
        print
      }
    ' "$input" >"$output_file"
}

DEPLOYMENT_ENV="$ROOT/deployment.env"
COMPOSE_ENV="$ROOT/compose.env"
MODE=$(env_value "$DEPLOYMENT_ENV" MODE)
ASTERISK_VERSION=$(env_value "$DEPLOYMENT_ENV" ASTERISK_VERSION)
ASTERISK_SHA256=$(env_value "$DEPLOYMENT_ENV" ASTERISK_SHA256)
ASTERISK_IMAGE_REF=$(env_value "$DEPLOYMENT_ENV" ASTERISK_IMAGE_REF)
ENTITY_ID=$(env_value "$DEPLOYMENT_ENV" ENTITY_ID)
ARI_PORT=$(env_value "$DEPLOYMENT_ENV" ARI_PORT)
SIP_PORT=$(env_value "$DEPLOYMENT_ENV" SIP_PORT)
RTP_MIN=$(env_value "$DEPLOYMENT_ENV" RTP_MIN)
RTP_MAX=$(env_value "$DEPLOYMENT_ENV" RTP_MAX)
MAX_CALL_SECONDS=$(env_value "$DEPLOYMENT_ENV" MAX_CALL_SECONDS)
APPLICATION=$(env_value "$DEPLOYMENT_ENV" APPLICATION)
ARGUMENT=$(env_value "$DEPLOYMENT_ENV" ARGUMENT)
POLICY_ID=$(env_value "$DEPLOYMENT_ENV" POLICY_ID)
CONTEXT=$(env_value "$DEPLOYMENT_ENV" CONTEXT)
ENDPOINT=$(env_value "$DEPLOYMENT_ENV" ENDPOINT)
CONTROL_USER=$(env_value "$DEPLOYMENT_ENV" CONTROL_USER)
INSPECT_USER=$(env_value "$DEPLOYMENT_ENV" INSPECT_USER)
GATEWAY_IP=$(env_value "$DEPLOYMENT_ENV" GATEWAY_IP)
SIP_BIND_IP=$(env_value "$DEPLOYMENT_ENV" SIP_BIND_IP)
LAN_CIDR=$(env_value "$DEPLOYMENT_ENV" LAN_CIDR)
PBX_CONTAINER_SUBNET=$(env_value "$DEPLOYMENT_ENV" PBX_CONTAINER_SUBNET)
PBX_CONTAINER_IPV4=$(env_value "$DEPLOYMENT_ENV" PBX_CONTAINER_IPV4)

COMPOSE_ROOT=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_ROOT)
COMPOSE_IMAGE_REF=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_IMAGE_REF)
COMPOSE_UID=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_UID)
COMPOSE_GID=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_GID)
COMPOSE_ARI_PORT=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_ARI_PORT)
COMPOSE_SIP_BIND_IP=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_SIP_BIND_IP)
COMPOSE_SIP_PORT=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_SIP_PORT)
COMPOSE_RTP_MIN=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_RTP_MIN)
COMPOSE_RTP_MAX=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_RTP_MAX)
COMPOSE_PBX_CONTAINER_SUBNET=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_CONTAINER_SUBNET)
COMPOSE_PBX_CONTAINER_IPV4=$(env_value "$COMPOSE_ENV" MACCELLULAR_PBX_CONTAINER_IPV4)

[ "$ASTERISK_VERSION" = 22.10.1 ] || die "Asterisk version drift"
[ "$ASTERISK_SHA256" = 0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353 ] || die "Asterisk SHA-256 drift"
cmp -s "$ROOT/source.lock" "$SCRIPT_DIR/source.lock" || die "source lock drift"
LOCKED_IMAGE_REF=$(env_value "$SCRIPT_DIR/source.lock" ASTERISK_IMAGE_REF)
[ "$LOCKED_IMAGE_REF" = 'maccellular-asterisk@sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51' ] || \
  die "pinned PBX image reference drift"
[ "$ASTERISK_IMAGE_REF" = "$LOCKED_IMAGE_REF" ] || die "deployment PBX image reference drift"
[ "$COMPOSE_IMAGE_REF" = "$LOCKED_IMAGE_REF" ] || die "Compose PBX image reference drift"

case "$MODE" in production|synthetic-loopback) ;; *) die "unknown deployment mode" ;; esac
case "$ENTITY_ID" in
  [0-9a-f][26ae]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]) ;;
  *) die "entity ID is not a locally administered unicast identity" ;;
esac

for pair in \
  "application:$APPLICATION" \
  "argument:$ARGUMENT" \
  "context:$CONTEXT" \
  "endpoint:$ENDPOINT" \
  "control user:$CONTROL_USER" \
  "inspect user:$INSPECT_USER"; do
  label=${pair%%:*}
  token=${pair#*:}
  is_token "$token" || die "$label is not a safe Asterisk token"
done

for pair in \
  "ARI port:$ARI_PORT:1024:65535" \
  "SIP port:$SIP_PORT:1024:65535" \
  "RTP minimum:$RTP_MIN:1024:65535" \
  "RTP maximum:$RTP_MAX:1024:65535" \
  "maximum call seconds:$MAX_CALL_SECONDS:30:3600" \
  "Compose UID:$COMPOSE_UID:0:2147483647" \
  "Compose GID:$COMPOSE_GID:0:2147483647" \
  "Compose ARI port:$COMPOSE_ARI_PORT:1024:65535" \
  "Compose SIP port:$COMPOSE_SIP_PORT:1024:65535" \
  "Compose RTP minimum:$COMPOSE_RTP_MIN:1024:65535" \
  "Compose RTP maximum:$COMPOSE_RTP_MAX:1024:65535"; do
  label=${pair%%:*}
  remainder=${pair#*:}
  number=${remainder%%:*}
  remainder=${remainder#*:}
  minimum=${remainder%%:*}
  maximum=${remainder#*:}
  decimal_in_range "$number" "$minimum" "$maximum" || die "$label is not a canonical in-range decimal"
done

LAN_NETWORK_IP=${LAN_CIDR%/*}
LAN_PREFIX=${LAN_CIDR#*/}
[ "$LAN_NETWORK_IP" != "$LAN_CIDR" ] && [ "${LAN_PREFIX#*/}" = "$LAN_PREFIX" ] || die "LAN CIDR is malformed"
is_ipv4 "$GATEWAY_IP" || die "gateway IP is not canonical IPv4"
is_ipv4 "$SIP_BIND_IP" || die "SIP bind IP is not canonical IPv4"
is_ipv4 "$LAN_NETWORK_IP" || die "LAN network is not canonical IPv4"
decimal_in_range "$LAN_PREFIX" 8 30 || die "LAN prefix is not a canonical decimal in 8..30"

PBX_CONTAINER_NETWORK_IP=${PBX_CONTAINER_SUBNET%/*}
PBX_CONTAINER_PREFIX=${PBX_CONTAINER_SUBNET#*/}
[ "$PBX_CONTAINER_NETWORK_IP" != "$PBX_CONTAINER_SUBNET" ] && [ "${PBX_CONTAINER_PREFIX#*/}" = "$PBX_CONTAINER_PREFIX" ] || \
  die "fixed PBX container subnet is malformed"
is_ipv4 "$PBX_CONTAINER_NETWORK_IP" || die "fixed PBX container network is not canonical IPv4"
is_ipv4 "$PBX_CONTAINER_IPV4" || die "fixed PBX container address is not canonical IPv4"
decimal_in_range "$PBX_CONTAINER_PREFIX" 24 30 || die "fixed PBX container prefix is not a canonical decimal in 24..30"
container_network_is_consistent "$PBX_CONTAINER_IPV4" "$PBX_CONTAINER_NETWORK_IP" "$PBX_CONTAINER_PREFIX" || \
  die "fixed PBX container address is not a usable member of its canonical subnet"

FIXED_PBX_CONTAINER_SUBNET=$(env_value "$SCRIPT_DIR/runtime-network.env" PBX_CONTAINER_SUBNET)
FIXED_PBX_CONTAINER_IPV4=$(env_value "$SCRIPT_DIR/runtime-network.env" PBX_CONTAINER_IPV4)
[ "$PBX_CONTAINER_SUBNET" = "$FIXED_PBX_CONTAINER_SUBNET" ] || die "deployment fixed PBX container subnet drift"
[ "$PBX_CONTAINER_IPV4" = "$FIXED_PBX_CONTAINER_IPV4" ] || die "deployment fixed PBX container IPv4 drift"
[ "$COMPOSE_PBX_CONTAINER_SUBNET" = "$FIXED_PBX_CONTAINER_SUBNET" ] || die "Compose fixed PBX container subnet drift"
[ "$COMPOSE_PBX_CONTAINER_IPV4" = "$FIXED_PBX_CONTAINER_IPV4" ] || die "Compose fixed PBX container IPv4 drift"

lan_contract_is_consistent "$GATEWAY_IP" "$SIP_BIND_IP" "$LAN_NETWORK_IP" "$LAN_PREFIX" || \
  die "generated LAN contract is inconsistent"
cidrs_do_not_overlap "$LAN_NETWORK_IP" "$LAN_PREFIX" "$PBX_CONTAINER_NETWORK_IP" "$PBX_CONTAINER_PREFIX" || \
  die "physical LAN overlaps the fixed PBX container subnet"
if [ "$MODE" = production ]; then
  is_private_ipv4 "$GATEWAY_IP" || die "production gateway IP is not private"
  is_private_ipv4 "$SIP_BIND_IP" || die "production SIP bind IP is not private"
  is_private_ipv4 "$LAN_NETWORK_IP" || die "production LAN is not private"
else
  [ "$GATEWAY_IP,$SIP_BIND_IP,$LAN_CIDR" = "127.0.0.2,127.0.0.1,127.0.0.0/8" ] || \
    die "synthetic loopback identity drift"
fi

[ "$CONTROL_USER" != "$INSPECT_USER" ] || die "ARI users are not separated"
[ "$POLICY_ID" = djonehub-incoming-v1 ] || die "incoming policy ID drift"
[ $((RTP_MAX - RTP_MIN + 1)) -ge 2 ] && [ $((RTP_MAX - RTP_MIN + 1)) -le 64 ] || die "RTP range is not bounded to 2..64 ports"
[ $((RTP_MIN % 2)) -eq 0 ] && [ $(((RTP_MAX - RTP_MIN + 1) % 2)) -eq 0 ] || die "RTP range does not contain whole RTP/RTCP pairs"
[ "$MAX_CALL_SECONDS" -ge 30 ] && [ "$MAX_CALL_SECONDS" -le 3600 ] || die "absolute timeout is outside 30..3600 seconds"
[ "$ARI_PORT" -ne "$SIP_PORT" ] || die "ARI and SIP ports overlap"
if [ "$ARI_PORT" -ge "$RTP_MIN" ] && [ "$ARI_PORT" -le "$RTP_MAX" ] || \
   [ "$SIP_PORT" -ge "$RTP_MIN" ] && [ "$SIP_PORT" -le "$RTP_MAX" ]; then
  die "ARI or SIP port overlaps the RTP range"
fi

[ "$COMPOSE_ROOT" = "$EXPECTED_ROOT" ] || die "compose root does not match the intended final path"
[ "$COMPOSE_UID" = "$CURRENT_UID" ] || die "compose UID does not match generated-tree ownership"
[ "$COMPOSE_GID" = "$(id -g)" ] || die "compose GID does not match the current user"
[ "$COMPOSE_ARI_PORT" = "$ARI_PORT" ] || die "compose ARI port drift"
[ "$COMPOSE_SIP_BIND_IP" = "$SIP_BIND_IP" ] || die "compose SIP address drift"
[ "$COMPOSE_SIP_PORT" = "$SIP_PORT" ] || die "compose SIP port drift"
[ "$COMPOSE_RTP_MIN" = "$RTP_MIN" ] || die "compose RTP minimum drift"
[ "$COMPOSE_RTP_MAX" = "$RTP_MAX" ] || die "compose RTP maximum drift"

cat >"$VERIFY_TEMP/expected-deployment.env" <<EOF
MODE=$MODE
ASTERISK_VERSION=$ASTERISK_VERSION
ASTERISK_SHA256=$ASTERISK_SHA256
ASTERISK_IMAGE_REF=$ASTERISK_IMAGE_REF
ENTITY_ID=$ENTITY_ID
ARI_PORT=$ARI_PORT
SIP_PORT=$SIP_PORT
RTP_MIN=$RTP_MIN
RTP_MAX=$RTP_MAX
MAX_CALL_SECONDS=$MAX_CALL_SECONDS
APPLICATION=$APPLICATION
ARGUMENT=$ARGUMENT
POLICY_ID=$POLICY_ID
CONTEXT=$CONTEXT
ENDPOINT=$ENDPOINT
CONTROL_USER=$CONTROL_USER
INSPECT_USER=$INSPECT_USER
GATEWAY_IP=$GATEWAY_IP
SIP_BIND_IP=$SIP_BIND_IP
LAN_CIDR=$LAN_CIDR
PBX_CONTAINER_SUBNET=$PBX_CONTAINER_SUBNET
PBX_CONTAINER_IPV4=$PBX_CONTAINER_IPV4
EOF
cmp -s "$DEPLOYMENT_ENV" "$VERIFY_TEMP/expected-deployment.env" || die "deployment environment is not exact canonical output"

cat >"$VERIFY_TEMP/expected-compose.env" <<EOF
MACCELLULAR_PBX_ROOT=$COMPOSE_ROOT
MACCELLULAR_PBX_IMAGE_REF=$COMPOSE_IMAGE_REF
MACCELLULAR_PBX_UID=$COMPOSE_UID
MACCELLULAR_PBX_GID=$COMPOSE_GID
MACCELLULAR_PBX_ARI_PORT=$COMPOSE_ARI_PORT
MACCELLULAR_PBX_SIP_BIND_IP=$COMPOSE_SIP_BIND_IP
MACCELLULAR_PBX_SIP_PORT=$COMPOSE_SIP_PORT
MACCELLULAR_PBX_RTP_MIN=$COMPOSE_RTP_MIN
MACCELLULAR_PBX_RTP_MAX=$COMPOSE_RTP_MAX
MACCELLULAR_PBX_CONTAINER_SUBNET=$COMPOSE_PBX_CONTAINER_SUBNET
MACCELLULAR_PBX_CONTAINER_IPV4=$COMPOSE_PBX_CONTAINER_IPV4
EOF
cmp -s "$COMPOSE_ENV" "$VERIFY_TEMP/expected-compose.env" || die "Compose environment is not exact canonical output"

CONTROL_PASSWORD=$(sed -n '1p' "$ROOT/secrets/ari-control.password")
INSPECT_PASSWORD=$(sed -n '1p' "$ROOT/secrets/ari-inspect.password")
printf '%s\n' "$CONTROL_PASSWORD" >"$VERIFY_TEMP/expected-control.password"
printf '%s\n' "$INSPECT_PASSWORD" >"$VERIFY_TEMP/expected-inspect.password"
cmp -s "$ROOT/secrets/ari-control.password" "$VERIFY_TEMP/expected-control.password" || \
  die "control credential is not exactly one generator line"
cmp -s "$ROOT/secrets/ari-inspect.password" "$VERIFY_TEMP/expected-inspect.password" || \
  die "inspect credential is not exactly one generator line"
is_generator_password "$CONTROL_PASSWORD" || die "control credential is not an exact 48-byte-generator base64 value"
is_generator_password "$INSPECT_PASSWORD" || die "inspect credential is not an exact 48-byte-generator base64 value"
[ "$CONTROL_PASSWORD" != "$INSPECT_PASSWORD" ] || die "ARI credentials are identical"
CONTROL_HASH=$(ini_value "$ROOT/config/ari.conf" "$CONTROL_USER" password) || die "missing unique control ARI hash"
INSPECT_HASH=$(ini_value "$ROOT/config/ari.conf" "$INSPECT_USER" password) || die "missing unique inspect ARI hash"
[ "$CONTROL_HASH" != "$INSPECT_HASH" ] || die "ARI password hashes are identical"
verify_password_hash control "$CONTROL_PASSWORD" "$CONTROL_HASH"
verify_password_hash inspect "$INSPECT_PASSWORD" "$INSPECT_HASH"
if grep -R -F -f "$ROOT/secrets/ari-control.password" "$ROOT/config" "$ROOT/deployment.env" "$ROOT/compose.env" >/dev/null 2>&1 || \
   grep -R -F -f "$ROOT/secrets/ari-inspect.password" "$ROOT/config" "$ROOT/deployment.env" "$ROOT/compose.env" >/dev/null 2>&1; then
  die "plaintext ARI credential escaped the secret files"
fi

EXPECTED_CONFIG_DIR="$VERIFY_TEMP/config"
mkdir "$EXPECTED_CONFIG_DIR"
for config_name in \
  acl.conf \
  ccss.conf \
  cdr.conf \
  cel.conf \
  chan_websocket.conf \
  features.conf \
  http.conf \
  indications.conf \
  logger.conf \
  manager.conf \
  modules.conf \
  pjproject.conf \
  stasis.conf \
  udptl.conf \
  websocket_client.conf; do
  cp "$SCRIPT_DIR/templates/$config_name" "$EXPECTED_CONFIG_DIR/$config_name"
done
render_expected_config "$SCRIPT_DIR/templates/asterisk.conf.in" "$EXPECTED_CONFIG_DIR/asterisk.conf"
render_expected_config "$SCRIPT_DIR/templates/ari.conf.in" "$EXPECTED_CONFIG_DIR/ari.conf"
render_expected_config "$SCRIPT_DIR/templates/extensions.conf.in" "$EXPECTED_CONFIG_DIR/extensions.conf"
render_expected_config "$SCRIPT_DIR/templates/pjsip.conf.in" "$EXPECTED_CONFIG_DIR/pjsip.conf"
render_expected_config "$SCRIPT_DIR/templates/rtp.conf.in" "$EXPECTED_CONFIG_DIR/rtp.conf"
for config_name in \
  acl.conf asterisk.conf ari.conf ccss.conf cdr.conf cel.conf \
  chan_websocket.conf extensions.conf features.conf http.conf indications.conf \
  logger.conf manager.conf modules.conf pjproject.conf pjsip.conf rtp.conf \
  stasis.conf udptl.conf websocket_client.conf; do
  cmp -s "$ROOT/config/$config_name" "$EXPECTED_CONFIG_DIR/$config_name" || \
    die "generated configuration is not exact canonical output: $config_name"
done
CONTROL_PASSWORD=
INSPECT_PASSWORD=

assert_ini "$ROOT/config/asterisk.conf" options entityid "$ENTITY_ID"
assert_ini "$ROOT/config/asterisk.conf" options live_dangerously no
assert_ini "$ROOT/config/asterisk.conf" directories astvarlibdir /var/lib/asterisk
assert_ini "$ROOT/config/asterisk.conf" directories astdatadir /var/lib/asterisk
assert_ini "$ROOT/config/asterisk.conf" directories astdbdir /var/lib/asterisk-private
assert_ini "$ROOT/config/asterisk.conf" directories astkeydir /var/lib/asterisk-private
assert_ini "$ROOT/config/http.conf" general enabled yes
assert_ini "$ROOT/config/http.conf" general bindaddr 0.0.0.0
assert_ini "$ROOT/config/http.conf" general bindport 8088
assert_ini "$ROOT/config/http.conf" general tlsenable no
assert_ini "$ROOT/config/chan_websocket.conf" global control_message_format json

assert_ini "$ROOT/config/ari.conf" general enabled yes
assert_ini "$ROOT/config/ari.conf" general channelvars MEDIA_WEBSOCKET_CONNECTION_ID,DJONEHUB_POLICY_ID
assert_ini "$ROOT/config/ari.conf" "$CONTROL_USER" type user
assert_ini "$ROOT/config/ari.conf" "$CONTROL_USER" read_only no
assert_ini "$ROOT/config/ari.conf" "$CONTROL_USER" password_format crypt
assert_ini "$ROOT/config/ari.conf" "$INSPECT_USER" type user
assert_ini "$ROOT/config/ari.conf" "$INSPECT_USER" read_only yes
assert_ini "$ROOT/config/ari.conf" "$INSPECT_USER" password_format crypt
case "$(ini_value "$ROOT/config/ari.conf" "$CONTROL_USER" password)" in '$6$'*) ;; *) die "control ARI hash is not SHA-512 crypt" ;; esac
case "$(ini_value "$ROOT/config/ari.conf" "$INSPECT_USER" password)" in '$6$'*) ;; *) die "inspect ARI hash is not SHA-512 crypt" ;; esac

assert_ini "$ROOT/config/pjsip.conf" transport-volte-udp type transport
assert_ini "$ROOT/config/pjsip.conf" global endpoint_identifier_order ip
assert_ini "$ROOT/config/pjsip.conf" transport-volte-udp protocol udp
assert_ini "$ROOT/config/pjsip.conf" transport-volte-udp bind 0.0.0.0:5060
assert_ini "$ROOT/config/pjsip.conf" transport-volte-udp external_signaling_address "$SIP_BIND_IP"
assert_ini "$ROOT/config/pjsip.conf" transport-volte-udp external_signaling_port "$SIP_PORT"
assert_ini "$ROOT/config/pjsip.conf" transport-volte-udp external_media_address "$SIP_BIND_IP"
assert_ini "$ROOT/config/pjsip.conf" transport-volte-udp local_net "$PBX_CONTAINER_SUBNET"
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" type endpoint
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" context "$CONTEXT"
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" identify_by ip
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" disallow all
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" allow ulaw
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" direct_media no
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" allow_transfer no
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" trust_connected_line no
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT" send_connected_line no
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT-identify" type identify
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT-identify" endpoint "$ENDPOINT"
assert_ini "$ROOT/config/pjsip.conf" "$ENDPOINT-identify" match "$GATEWAY_IP"
if grep -Eq '^[[:space:]]*(auth|outbound_auth|registration)[[:space:]]*=' "$ROOT/config/pjsip.conf"; then
  die "unexpected SIP credential or outbound registration"
fi

assert_ini "$ROOT/config/rtp.conf" general rtpstart "$RTP_MIN"
assert_ini "$ROOT/config/rtp.conf" general rtpend "$RTP_MAX"
assert_ini "$ROOT/config/rtp.conf" general strictrtp yes
assert_ini "$ROOT/config/rtp.conf" general icesupport no
if grep -Eq '^[[:space:]]*(stunaddr|turnaddr)[[:space:]]*=' "$ROOT/config/rtp.conf"; then
  die "STUN or TURN is configured for the PBX RTP leg"
fi

assert_ini "$ROOT/config/ccss.conf" general enabled yes
assert_ini "$ROOT/config/cdr.conf" general enable no
assert_ini "$ROOT/config/cdr.conf" general channeldefaultenabled no
assert_ini "$ROOT/config/cel.conf" general enable no
assert_ini "$ROOT/config/manager.conf" general enabled no
assert_ini "$ROOT/config/manager.conf" general webenabled no
assert_ini "$ROOT/config/pjproject.conf" startup type startup
assert_ini "$ROOT/config/pjproject.conf" startup log_level 2
assert_ini "$ROOT/config/stasis.conf" taskpool initial_size 2
assert_ini "$ROOT/config/stasis.conf" taskpool max_size 8
assert_ini "$ROOT/config/udptl.conf" general udptlstart 4000
assert_ini "$ROOT/config/udptl.conf" general udptlend 4000

grep -Fqx 'autoload = no' "$ROOT/config/modules.conf" || die "module autoload is not disabled"
cmp -s "$ROOT/config/modules.conf" "$SCRIPT_DIR/templates/modules.conf" || die "module allowlist is not the exact canonical file"
if grep -Eq '^[[:space:]]*(load|preload|preload-require|noload)[[:space:]]*=' "$ROOT/config/modules.conf"; then
  die "module allowlist contains a non-required load directive"
fi

MODULE_MANIFEST="$SCRIPT_DIR/canonical-modules.txt"
[ -f "$MODULE_MANIFEST" ] && [ ! -L "$MODULE_MANIFEST" ] || die "canonical module manifest is not a regular file"
awk 'NF != 1 || $0 !~ /^[a-z0-9_]+[.]so$/ { exit 1 } END { if (NR != 35) exit 1 }' "$MODULE_MANIFEST" || \
  die "canonical module manifest is not exactly 35 safe names"
sort -cu "$MODULE_MANIFEST" >/dev/null 2>&1 || die "canonical module manifest is not sorted and unique"
awk '
  /^require = [a-z0-9_]+[.]so$/ { sub(/^require = /, ""); print; next }
  /^require[[:space:]]*=/ { exit 1 }
' "$SCRIPT_DIR/templates/modules.conf" >"$VERIFY_TEMP/template-modules.txt" || \
  die "module configuration template contains an unsafe require directive"
cmp -s "$MODULE_MANIFEST" "$VERIFY_TEMP/template-modules.txt" || \
  die "module configuration and physical-image manifests differ"

grep -Fqx "exten => dj1-incoming,1,Set(DJONEHUB_POLICY_ID=$POLICY_ID)" "$ROOT/config/extensions.conf" || die "incoming policy marker is missing"
POLICY_LINE=$(grep -nFx "exten => dj1-incoming,1,Set(DJONEHUB_POLICY_ID=$POLICY_ID)" "$ROOT/config/extensions.conf" | awk -F: 'NR == 1 { print $1 }')
GROUP_LINE=$(grep -nFx " same => n,Set(GROUP(dj1_external_voice)=incoming)" "$ROOT/config/extensions.conf" | awk -F: 'NR == 1 { print $1 }')
[ "$GROUP_LINE" -eq $((POLICY_LINE + 1)) ] || die "GROUP admission marker does not immediately follow the policy marker"
grep -Fqx ' same => n,GotoIf($[${GROUP_COUNT(incoming@dj1_external_voice)} > 1]?capacity)' "$ROOT/config/extensions.conf" || die "single-call GROUP_COUNT gate is missing"
grep -Fqx " same => n,Set(TIMEOUT(absolute)=$MAX_CALL_SECONDS)" "$ROOT/config/extensions.conf" || die "absolute call timeout is missing"
STASIS_LINE=$(grep -nFx " same => n,Stasis($APPLICATION,$ARGUMENT)" "$ROOT/config/extensions.conf" | awk -F: 'NR == 1 { print $1 }')
[ -n "$STASIS_LINE" ] || die "exact Stasis application/argument is missing"
NEXT_LINE=$(sed -n "$((STASIS_LINE + 1))p" "$ROOT/config/extensions.conf")
[ "$NEXT_LINE" = ' same => n,Hangup()' ] || die "Stasis is not immediately followed by fail-closed Hangup"

COMPOSE="$SCRIPT_DIR/compose.yaml"
grep -Fq 'profiles: ["explicit-pbx"]' "$COMPOSE" || die "compose service is not explicit-profile gated"
grep -Fq 'image: "${MACCELLULAR_PBX_IMAGE_REF:?missing pinned PBX image reference}"' "$COMPOSE" || die "Compose does not require the pinned PBX image reference"
grep -Fq 'pull_policy: never' "$COMPOSE" || die "Compose may pull an unverified PBX image"
grep -Fq 'restart: "no"' "$COMPOSE" || die "PBX may restart before the Adapter mutation-ABA gate is closed"
grep -Fq 'ipv4_address: "${MACCELLULAR_PBX_CONTAINER_IPV4:?missing fixed PBX container IPv4}"' "$COMPOSE" || die "Compose lost the fixed PBX container IPv4"
grep -Fq 'subnet: "${MACCELLULAR_PBX_CONTAINER_SUBNET:?missing fixed PBX container subnet}"' "$COMPOSE" || die "Compose lost the fixed PBX container subnet"
grep -Fq '127.0.0.1:${MACCELLULAR_PBX_ARI_PORT:?missing ARI port}:8088/tcp' "$COMPOSE" || die "ARI is not host-loopback-only"
grep -Fq '${MACCELLULAR_PBX_SIP_BIND_IP:?missing exact SIP bind IP}:${MACCELLULAR_PBX_SIP_PORT:?missing SIP port}:5060/udp' "$COMPOSE" || die "SIP is not exact-address published"
grep -Fq '${MACCELLULAR_PBX_SIP_BIND_IP:?missing exact SIP bind IP}:${MACCELLULAR_PBX_RTP_MIN:?missing RTP min}-${MACCELLULAR_PBX_RTP_MAX:?missing RTP max}:${MACCELLULAR_PBX_RTP_MIN:?missing RTP min}-${MACCELLULAR_PBX_RTP_MAX:?missing RTP max}/udp' "$COMPOSE" || die "RTP is not exact-address and exact-range published"
grep -Fq 'read_only: true' "$COMPOSE" || die "container root filesystem is writable"
grep -Fq 'target: /var/lib/asterisk-private' "$COMPOSE" || die "mutable PBX state would mask image-supplied ARI schemas"
grep -Fq 'cap_drop: ["ALL"]' "$COMPOSE" || die "container capabilities are not dropped"
grep -Fq 'no-new-privileges:true' "$COMPOSE" || die "no-new-privileges is missing"
if grep -Eq '(^|["[:space:]-])(0\.0\.0\.0:|[0-9]+:[0-9]+/(tcp|udp))' "$COMPOSE"; then
  die "compose contains a wildcard or address-less host publication"
fi

grep -Fq 'ARG ASTERISK_VERSION=22.10.1' "$SCRIPT_DIR/Dockerfile" || die "Dockerfile version drift"
grep -Fq 'ARG ASTERISK_SHA256=0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353' "$SCRIPT_DIR/Dockerfile" || die "Dockerfile source SHA drift"
grep -Fq 'ARG PJPROJECT_VERSION=2.17' "$SCRIPT_DIR/Dockerfile" || die "Dockerfile pjproject version drift"
grep -Fq 'ARG PJPROJECT_SHA256=04b2eb1f0f01aa0ad1945b167171843448a51aa6b7c3e806496d434f13a112b7' "$SCRIPT_DIR/Dockerfile" || die "Dockerfile pjproject SHA drift"
grep -Fqx '# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e' "$SCRIPT_DIR/Dockerfile" || die "Dockerfile frontend digest drift"
grep -Fq 'RUN --network=none ./configure' "$SCRIPT_DIR/Dockerfile" || die "Asterisk build may perform unpinned network fetches"
grep -Fq 'rm -rf /opt/asterisk-root/var/run' "$SCRIPT_DIR/Dockerfile" || die "staged /var/run would conflict with Debian runtime layout"
grep -Fq 'debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241' "$SCRIPT_DIR/Dockerfile" || die "Debian base digest is not pinned"

printf 'Asterisk deployment static verification: PASS (%s, %s)\n' "$MODE" "$ENTITY_ID"
