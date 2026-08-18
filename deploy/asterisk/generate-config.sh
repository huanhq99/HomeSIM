#!/bin/sh
set -eu

LC_ALL=C
export LC_ALL

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "${SCRIPT_DIR}/../.." && pwd)

usage() {
  cat <<'EOF'
Usage:
  deploy/asterisk/generate-config.sh [options]

Required deployment inputs:
  --gateway-ip IPV4       exact VoLTE-to-SIP appliance address
  --sip-bind-ip IPV4      exact private address of the Mac soft router
  --lan-cidr IPV4/PREFIX  exact LAN containing the appliance

Options:
  --output ABSOLUTE_PATH  default: repository local/asterisk (Git-ignored)
  --ari-port PORT         host-loopback ARI port (default: 18088)
  --sip-port PORT         host SIP/UDP port (default: 5060)
  --rtp-min PORT          first RTP/UDP port (default: 10000)
  --rtp-max PORT          last RTP/UDP port (default: 10031)
  --max-call-seconds N    absolute PBX call timeout (default: 900)
  --application TOKEN     Stasis application (default: maccellular_voice)
  --argument TOKEN        Stasis argument (default: incoming)
  --context TOKEN         inbound context (default: from-cellular)
  --endpoint TOKEN        exact PJSIP endpoint (default: cellular-gateway)
  --control-user TOKEN    read-write ARI user (default: maccellular_voice_control)
  --inspect-user TOKEN    read-only ARI user (default: maccellular_voice_inspect)
  --test-loopback         allow loopback only for disposable integration tests
  --apply                 atomically create the new output tree

Without --apply this command only validates and prints the plan. It never
overwrites an existing output tree and never prints generated credentials.
EOF
}

die() {
  printf 'ERROR: %s\n' "$1" >&2
  exit "${2:-64}"
}

require_value() {
  [ "$#" -ge 2 ] && [ -n "$2" ] || die "missing value for $1"
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

is_uint() {
  case "$1" in ''|*[!0-9]*) return 1 ;; esac
  return 0
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

contract_value() {
  awk -F= -v wanted="$2" '
    $1 == wanted { count++; value = substr($0, length(wanted) + 2) }
    END { if (count != 1 || value == "") exit 1; print value }
  ' "$1"
}

load_static_contracts() {
  network_contract="$SCRIPT_DIR/runtime-network.env"
  module_contract="$SCRIPT_DIR/canonical-modules.txt"
  module_template="$SCRIPT_DIR/templates/modules.conf"
  source_contract="$SCRIPT_DIR/source.lock"

  [ -f "$network_contract" ] && [ ! -L "$network_contract" ] || die "runtime network contract must be a regular file"
  awk '
    /^PBX_CONTAINER_SUBNET=[0-9.]+\/[0-9]+$/ { subnet++ ; next }
    /^PBX_CONTAINER_IPV4=[0-9.]+$/ { address++ ; next }
    { exit 1 }
    END { if (NR != 2 || subnet != 1 || address != 1) exit 1 }
  ' "$network_contract" || die "runtime network contract is malformed"
  PBX_CONTAINER_SUBNET=$(contract_value "$network_contract" PBX_CONTAINER_SUBNET) || die "fixed PBX container subnet is missing"
  PBX_CONTAINER_IPV4=$(contract_value "$network_contract" PBX_CONTAINER_IPV4) || die "fixed PBX container IPv4 is missing"

  PBX_CONTAINER_NETWORK_IP=${PBX_CONTAINER_SUBNET%/*}
  PBX_CONTAINER_PREFIX=${PBX_CONTAINER_SUBNET#*/}
  [ "$PBX_CONTAINER_NETWORK_IP" != "$PBX_CONTAINER_SUBNET" ] || die "fixed PBX container subnet lacks a prefix"
  is_ipv4 "$PBX_CONTAINER_NETWORK_IP" || die "fixed PBX container subnet address is not canonical IPv4"
  is_ipv4 "$PBX_CONTAINER_IPV4" || die "fixed PBX container address is not canonical IPv4"
  is_uint "$PBX_CONTAINER_PREFIX" && [ "$PBX_CONTAINER_PREFIX" -ge 24 ] && [ "$PBX_CONTAINER_PREFIX" -le 30 ] || \
    die "fixed PBX container prefix must be between 24 and 30"
  is_private_ipv4 "$PBX_CONTAINER_NETWORK_IP" || die "fixed PBX container subnet must be private"
  container_network_is_consistent "$PBX_CONTAINER_IPV4" "$PBX_CONTAINER_NETWORK_IP" "$PBX_CONTAINER_PREFIX" || \
    die "fixed PBX container address must be a usable member of its canonical subnet"

  [ -f "$module_contract" ] && [ ! -L "$module_contract" ] || die "canonical module manifest must be a regular file"
  awk 'NF != 1 || $0 !~ /^[a-z0-9_]+[.]so$/ { exit 1 } END { if (NR != 35) exit 1 }' "$module_contract" || \
    die "canonical module manifest must contain exactly 35 safe module names"
  sort -cu "$module_contract" >/dev/null 2>&1 || \
    die "canonical module manifest must be sorted and unique"
  [ -f "$module_template" ] && [ ! -L "$module_template" ] || die "module configuration template must be a regular file"
  configured_modules=$(awk '
    /^require = [a-z0-9_]+[.]so$/ { sub(/^require = /, ""); print; next }
    /^require[[:space:]]*=/ { exit 1 }
  ' "$module_template") || die "module configuration template contains an unsafe require directive"
  [ "$configured_modules" = "$(cat "$module_contract")" ] || \
    die "module configuration and physical image allowlists differ"

  [ -f "$source_contract" ] && [ ! -L "$source_contract" ] || die "source lock must be a regular file"
  ASTERISK_IMAGE_REF=$(contract_value "$source_contract" ASTERISK_IMAGE_REF) || die "pinned PBX image reference is missing"
  case "$ASTERISK_IMAGE_REF" in
    maccellular-asterisk@sha256:????????????????????????????????????????????????????????????????) ;;
    *) die "pinned PBX image reference is malformed" ;;
  esac
  image_digest=${ASTERISK_IMAGE_REF#maccellular-asterisk@sha256:}
  case "$image_digest" in *[!0-9a-f]*) die "pinned PBX image digest is malformed" ;; esac
}

render() {
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

OUTPUT="${REPO_ROOT}/local/asterisk"
ARI_PORT=18088
SIP_PORT=5060
RTP_MIN=10000
RTP_MAX=10031
MAX_CALL_SECONDS=900
APPLICATION=maccellular_voice
ARGUMENT=incoming
POLICY_ID=djonehub-incoming-v1
CONTEXT=from-cellular
ENDPOINT=cellular-gateway
CONTROL_USER=maccellular_voice_control
INSPECT_USER=maccellular_voice_inspect
GATEWAY_IP=
SIP_BIND_IP=
LAN_CIDR=
MODE=production
APPLY=0

command -v awk >/dev/null 2>&1 || die "awk is required" 69
command -v sort >/dev/null 2>&1 || die "sort is required" 69
command -v cat >/dev/null 2>&1 || die "cat is required" 69
load_static_contracts

while [ "$#" -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --output) require_value "$@"; OUTPUT=$2; shift 2 ;;
    --gateway-ip) require_value "$@"; GATEWAY_IP=$2; shift 2 ;;
    --sip-bind-ip) require_value "$@"; SIP_BIND_IP=$2; shift 2 ;;
    --lan-cidr) require_value "$@"; LAN_CIDR=$2; shift 2 ;;
    --ari-port) require_value "$@"; ARI_PORT=$2; shift 2 ;;
    --sip-port) require_value "$@"; SIP_PORT=$2; shift 2 ;;
    --rtp-min) require_value "$@"; RTP_MIN=$2; shift 2 ;;
    --rtp-max) require_value "$@"; RTP_MAX=$2; shift 2 ;;
    --max-call-seconds) require_value "$@"; MAX_CALL_SECONDS=$2; shift 2 ;;
    --application) require_value "$@"; APPLICATION=$2; shift 2 ;;
    --argument) require_value "$@"; ARGUMENT=$2; shift 2 ;;
    --context) require_value "$@"; CONTEXT=$2; shift 2 ;;
    --endpoint) require_value "$@"; ENDPOINT=$2; shift 2 ;;
    --control-user) require_value "$@"; CONTROL_USER=$2; shift 2 ;;
    --inspect-user) require_value "$@"; INSPECT_USER=$2; shift 2 ;;
    --test-loopback) MODE=synthetic-loopback; shift ;;
    --apply) APPLY=1; shift ;;
    *) die "unknown option: $1" ;;
  esac
done

case "$OUTPUT" in
  /*) ;;
  *) die "--output must be an absolute path" ;;
esac
case "$OUTPUT" in
  /|*/../*|*/./*|*//*|*/|*[[:space:]]*|*:*) die "--output must be a clean absolute path without whitespace or colon" ;;
esac

[ -n "$GATEWAY_IP" ] || die "--gateway-ip is required"
[ -n "$SIP_BIND_IP" ] || die "--sip-bind-ip is required"
[ -n "$LAN_CIDR" ] || die "--lan-cidr is required"
is_ipv4 "$GATEWAY_IP" || die "gateway IP must be canonical IPv4"
is_ipv4 "$SIP_BIND_IP" || die "SIP bind IP must be canonical IPv4"
[ "$GATEWAY_IP" != "$SIP_BIND_IP" ] || die "gateway and SIP bind IP must differ"

LAN_IP=${LAN_CIDR%/*}
LAN_PREFIX=${LAN_CIDR#*/}
[ "$LAN_IP" != "$LAN_CIDR" ] || die "LAN CIDR must include a prefix"
is_ipv4 "$LAN_IP" || die "LAN CIDR address must be canonical IPv4"
is_uint "$LAN_PREFIX" && [ "$LAN_PREFIX" -ge 8 ] && [ "$LAN_PREFIX" -le 30 ] || die "LAN prefix must be between 8 and 30"
lan_contract_is_consistent "$GATEWAY_IP" "$SIP_BIND_IP" "$LAN_IP" "$LAN_PREFIX" || \
  die "LAN CIDR must be a canonical network containing both non-network/non-broadcast addresses"
cidrs_do_not_overlap "$LAN_IP" "$LAN_PREFIX" "$PBX_CONTAINER_NETWORK_IP" "$PBX_CONTAINER_PREFIX" || \
  die "physical LAN CIDR must not overlap fixed PBX container subnet $PBX_CONTAINER_SUBNET"

if [ "$MODE" = production ]; then
  is_private_ipv4 "$GATEWAY_IP" || die "production gateway IP must be private"
  is_private_ipv4 "$SIP_BIND_IP" || die "production SIP bind IP must be private"
  is_private_ipv4 "$LAN_IP" || die "production LAN CIDR must be private"
else
  case "$GATEWAY_IP,$SIP_BIND_IP,$LAN_CIDR" in
    127.0.0.2,127.0.0.1,127.0.0.0/8) ;;
    *) die "--test-loopback accepts only 127.0.0.2, 127.0.0.1 and 127.0.0.0/8" ;;
  esac
fi

for pair in \
  "ARI port:$ARI_PORT" \
  "SIP port:$SIP_PORT" \
  "RTP minimum:$RTP_MIN" \
  "RTP maximum:$RTP_MAX" \
  "maximum call seconds:$MAX_CALL_SECONDS"; do
  label=${pair%%:*}
  number=${pair#*:}
  is_uint "$number" || die "$label must be an integer"
done
[ "$ARI_PORT" -ge 1024 ] && [ "$ARI_PORT" -le 65535 ] || die "ARI port out of range"
[ "$SIP_PORT" -ge 1024 ] && [ "$SIP_PORT" -le 65535 ] || die "SIP port out of range"
[ "$RTP_MIN" -ge 1024 ] && [ "$RTP_MAX" -le 65535 ] && [ "$RTP_MAX" -ge "$RTP_MIN" ] || die "RTP range is invalid"
[ $((RTP_MIN % 2)) -eq 0 ] || die "RTP minimum must be even"
[ $(((RTP_MAX - RTP_MIN + 1) % 2)) -eq 0 ] || die "RTP range must contain whole RTP/RTCP pairs"
[ $((RTP_MAX - RTP_MIN + 1)) -le 64 ] || die "RTP range may contain at most 64 ports"
[ "$ARI_PORT" -ne "$SIP_PORT" ] || die "ARI and SIP host ports must differ"
if [ "$ARI_PORT" -ge "$RTP_MIN" ] && [ "$ARI_PORT" -le "$RTP_MAX" ] || \
   [ "$SIP_PORT" -ge "$RTP_MIN" ] && [ "$SIP_PORT" -le "$RTP_MAX" ]; then
  die "ARI/SIP host ports must not overlap the RTP range"
fi
[ "$MAX_CALL_SECONDS" -ge 30 ] && [ "$MAX_CALL_SECONDS" -le 3600 ] || die "maximum call timeout must be 30..3600 seconds"

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
[ "$CONTROL_USER" != "$INSPECT_USER" ] || die "control and inspect ARI users must differ"

if [ "$APPLY" -ne 1 ]; then
  printf '%s\n' \
    "DRY RUN: no files created" \
    "output: $OUTPUT" \
    "mode: $MODE" \
    "Asterisk: 22.10.1 (official SHA-256 pinned)" \
    "ARI host publication: 127.0.0.1:$ARI_PORT" \
    "SIP host publication: $SIP_BIND_IP:$SIP_PORT/udp" \
    "RTP host publication: $SIP_BIND_IP:$RTP_MIN-$RTP_MAX/udp" \
    "gateway identify: $GATEWAY_IP" \
    "PBX container network: $PBX_CONTAINER_SUBNET" \
    "PBX container address: $PBX_CONTAINER_IPV4" \
    "PJSIP local_net: $PBX_CONTAINER_SUBNET" \
    "next: rerun this exact command with --apply"
  exit 0
fi

OPENSSL_SHA512=$(find_sha512_crypt_openssl) || \
  die "OpenSSL 3 with passwd -6 support is required for private credentials" 69
command -v awk >/dev/null 2>&1 || die "awk is required" 69
command -v mktemp >/dev/null 2>&1 || die "mktemp is required" 69

if [ -e "$OUTPUT" ] || [ -L "$OUTPUT" ]; then
  die "output already exists; move it aside and review it before generating a replacement" 73
fi

PARENT=$(dirname -- "$OUTPUT")
mkdir -p "$PARENT"
[ ! -L "$PARENT" ] || die "output parent must not be a symbolic link" 73
PARENT=$(CDPATH= cd -- "$PARENT" && pwd -P)
OUTPUT="${PARENT}/$(basename -- "$OUTPUT")"
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] || die "resolved output already exists" 73

STAGE=$(mktemp -d "${PARENT}/.asterisk.generate.XXXXXX")
cleanup() {
  if [ -n "${STAGE:-}" ]; then
    case "$STAGE" in
      "${PARENT}"/.asterisk.generate.*) rm -rf -- "$STAGE" ;;
    esac
  fi
}
trap cleanup EXIT HUP INT TERM
umask 077

mkdir -p "$STAGE/config" "$STAGE/secrets" "$STAGE/data" "$STAGE/log" "$STAGE/spool"

CONTROL_PASSWORD=$("$OPENSSL_SHA512" rand -base64 48) || die "failed to generate control credential" 70
INSPECT_PASSWORD=$("$OPENSSL_SHA512" rand -base64 48) || die "failed to generate inspect credential" 70
[ "$CONTROL_PASSWORD" != "$INSPECT_PASSWORD" ] || die "credential generator repeated output" 70
case "$CONTROL_PASSWORD$INSPECT_PASSWORD" in
  *:*) die "credential generator returned an unsafe value" 70 ;;
esac

CONTROL_HASH=$(printf '%s\n' "$CONTROL_PASSWORD" | "$OPENSSL_SHA512" passwd -6 -stdin) || die "failed to hash control credential" 70
INSPECT_HASH=$(printf '%s\n' "$INSPECT_PASSWORD" | "$OPENSSL_SHA512" passwd -6 -stdin) || die "failed to hash inspect credential" 70
case "$CONTROL_HASH" in '$6$'*) ;; *) die "openssl did not return a control SHA-512 crypt hash" 70 ;; esac
case "$INSPECT_HASH" in '$6$'*) ;; *) die "openssl did not return an inspect SHA-512 crypt hash" 70 ;; esac

ENTITY_RANDOM=$("$OPENSSL_SHA512" rand -hex 6) || die "failed to generate PBX entity ID" 70
[ "${#ENTITY_RANDOM}" -eq 12 ] || die "invalid PBX entity randomness" 70
ENTITY_ID="$(printf '%s' "$ENTITY_RANDOM" | cut -c1)2:$(printf '%s' "$ENTITY_RANDOM" | cut -c3-4):$(printf '%s' "$ENTITY_RANDOM" | cut -c5-6):$(printf '%s' "$ENTITY_RANDOM" | cut -c7-8):$(printf '%s' "$ENTITY_RANDOM" | cut -c9-10):$(printf '%s' "$ENTITY_RANDOM" | cut -c11-12)"

printf '%s\n' "$CONTROL_PASSWORD" >"$STAGE/secrets/ari-control.password"
printf '%s\n' "$INSPECT_PASSWORD" >"$STAGE/secrets/ari-inspect.password"

render "$SCRIPT_DIR/templates/asterisk.conf.in" "$STAGE/config/asterisk.conf"
render "$SCRIPT_DIR/templates/ari.conf.in" "$STAGE/config/ari.conf"
render "$SCRIPT_DIR/templates/extensions.conf.in" "$STAGE/config/extensions.conf"
render "$SCRIPT_DIR/templates/pjsip.conf.in" "$STAGE/config/pjsip.conf"
render "$SCRIPT_DIR/templates/rtp.conf.in" "$STAGE/config/rtp.conf"
cp "$SCRIPT_DIR/templates/acl.conf" "$STAGE/config/acl.conf"
cp "$SCRIPT_DIR/templates/ccss.conf" "$STAGE/config/ccss.conf"
cp "$SCRIPT_DIR/templates/cdr.conf" "$STAGE/config/cdr.conf"
cp "$SCRIPT_DIR/templates/cel.conf" "$STAGE/config/cel.conf"
cp "$SCRIPT_DIR/templates/chan_websocket.conf" "$STAGE/config/chan_websocket.conf"
cp "$SCRIPT_DIR/templates/features.conf" "$STAGE/config/features.conf"
cp "$SCRIPT_DIR/templates/http.conf" "$STAGE/config/http.conf"
cp "$SCRIPT_DIR/templates/indications.conf" "$STAGE/config/indications.conf"
cp "$SCRIPT_DIR/templates/logger.conf" "$STAGE/config/logger.conf"
cp "$SCRIPT_DIR/templates/manager.conf" "$STAGE/config/manager.conf"
cp "$SCRIPT_DIR/templates/modules.conf" "$STAGE/config/modules.conf"
cp "$SCRIPT_DIR/templates/pjproject.conf" "$STAGE/config/pjproject.conf"
cp "$SCRIPT_DIR/templates/stasis.conf" "$STAGE/config/stasis.conf"
cp "$SCRIPT_DIR/templates/udptl.conf" "$STAGE/config/udptl.conf"
cp "$SCRIPT_DIR/templates/websocket_client.conf" "$STAGE/config/websocket_client.conf"
cp "$SCRIPT_DIR/source.lock" "$STAGE/source.lock"

cat >"$STAGE/deployment.env" <<EOF
MODE=$MODE
ASTERISK_VERSION=22.10.1
ASTERISK_SHA256=0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353
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

cat >"$STAGE/compose.env" <<EOF
MACCELLULAR_PBX_ROOT=$OUTPUT
MACCELLULAR_PBX_IMAGE_REF=$ASTERISK_IMAGE_REF
MACCELLULAR_PBX_UID=$(id -u)
MACCELLULAR_PBX_GID=$(id -g)
MACCELLULAR_PBX_ARI_PORT=$ARI_PORT
MACCELLULAR_PBX_SIP_BIND_IP=$SIP_BIND_IP
MACCELLULAR_PBX_SIP_PORT=$SIP_PORT
MACCELLULAR_PBX_RTP_MIN=$RTP_MIN
MACCELLULAR_PBX_RTP_MAX=$RTP_MAX
MACCELLULAR_PBX_CONTAINER_SUBNET=$PBX_CONTAINER_SUBNET
MACCELLULAR_PBX_CONTAINER_IPV4=$PBX_CONTAINER_IPV4
EOF

find "$STAGE" -type d -exec chmod 0700 {} +
find "$STAGE" -type f -exec chmod 0600 {} +

"$SCRIPT_DIR/verify-config.sh" "$STAGE" --expected-root "$OUTPUT"
mv -- "$STAGE" "$OUTPUT"
STAGE=
CONTROL_PASSWORD=
INSPECT_PASSWORD=
CONTROL_HASH=
INSPECT_HASH=

printf '%s\n' \
  "Generated private Asterisk deployment: $OUTPUT" \
  "Control password file: $OUTPUT/secrets/ari-control.password" \
  "Read-only password file: $OUTPUT/secrets/ari-inspect.password" \
  "No container or service was started."
