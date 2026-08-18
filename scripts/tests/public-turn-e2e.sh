#!/usr/bin/env bash
set -euo pipefail

export LC_ALL=C
umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd -P)
SECRET_FILE=""
SECRET_STDIN=0
CONNECT_ADDRESS=""
TURN_TRANSPORT="udp"
TURN_TRANSPORT_SET=0
STUN_CONTROLS=()
STUN_CONTROL_COUNT=0

usage() {
  cat <<'EOF'
Usage:
  scripts/tests/public-turn-e2e.sh
  MACCELLULAR_PUBLIC_TURN_E2E=1 scripts/tests/public-turn-e2e.sh \
    --secret-file /absolute/mode-0600/turn-auth-secret \
    [--connect-address PUBLIC_TURN_IPV4] \
    [--transport udp|tcp|tls]
  MACCELLULAR_PUBLIC_TURN_E2E=1 scripts/tests/public-turn-e2e.sh \
    --secret-stdin \
    [--connect-address PUBLIC_TURN_IPV4] \
    [--transport udp|tcp|tls]
  MACCELLULAR_PUBLIC_TURN_E2E=1 scripts/tests/public-turn-e2e.sh \
    --stun-control 74.125.250.129:19302 \
    --stun-control 18.141.157.136:3478

Without MACCELLULAR_PUBLIC_TURN_E2E=1, only hermetic tests run and no public TURN
traffic is sent. Live transport tls verifies turn.example.com as the TLS
ServerName and connects on TCP 443. Secret content is read by the probe from
the file or stdin and is never placed in argv or printed. The stdin form is
intended for an encrypted SSH pipe.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$1" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --secret-stdin)
      [ -z "$SECRET_FILE" ] && [ "$SECRET_STDIN" -eq 0 ] || fail "--secret-stdin may not be combined or repeated"
      SECRET_STDIN=1
      shift
      ;;
    --secret-file)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--secret-file requires a value"
      [ -z "$SECRET_FILE" ] && [ "$SECRET_STDIN" -eq 0 ] || fail "--secret-file may not be combined or repeated"
      SECRET_FILE=$2
      shift 2
      ;;
    --stun-control)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--stun-control requires a value"
	  [ "$STUN_CONTROL_COUNT" -lt 4 ] || fail "at most four --stun-control values are allowed"
      STUN_CONTROLS+=("$2")
	  STUN_CONTROL_COUNT=$((STUN_CONTROL_COUNT + 1))
      shift 2
      ;;
    --connect-address)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--connect-address requires a value"
      [ -z "$CONNECT_ADDRESS" ] || fail "--connect-address may be supplied only once"
      CONNECT_ADDRESS=$2
      shift 2
      ;;
    --transport)
      [ "$#" -ge 2 ] || fail "--transport requires udp, tcp, or tls"
	  [ "$2" = "udp" ] || [ "$2" = "tcp" ] || [ "$2" = "tls" ] || fail "--transport requires udp, tcp, or tls"
      [ "$TURN_TRANSPORT_SET" -eq 0 ] || fail "--transport may be supplied only once"
      TURN_TRANSPORT=$2
      TURN_TRANSPORT_SET=1
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument"
      ;;
  esac
done

command -v go >/dev/null 2>&1 || fail "Go is required"

cd "$REPO_ROOT"
export GOTOOLCHAIN=local
export GOPROXY=off
go test -mod=readonly -count=1 ./integration/publicturn

case "${MACCELLULAR_PUBLIC_TURN_E2E:-}" in
  ""|0)
    [ -z "$SECRET_FILE$CONNECT_ADDRESS" ] && [ "$SECRET_STDIN" -eq 0 ] && [ "$TURN_TRANSPORT_SET" -eq 0 ] && [ "$STUN_CONTROL_COUNT" -eq 0 ] ||
      fail "refusing live input without MACCELLULAR_PUBLIC_TURN_E2E=1"
    MACCELLULAR_PUBLIC_TURN_E2E=0 go run -mod=readonly ./integration/publicturn
    ;;
  1)
    [ -n "$SECRET_FILE" ] || [ "$SECRET_STDIN" -eq 1 ] || [ "$STUN_CONTROL_COUNT" -gt 0 ] ||
      fail "live mode requires a secret source and/or --stun-control"
    [ -z "$CONNECT_ADDRESS" ] || [ -n "$SECRET_FILE" ] || [ "$SECRET_STDIN" -eq 1 ] ||
      fail "--connect-address requires a secret source"
    LIVE_ARGS=()
    if [ -n "$SECRET_FILE" ]; then
      LIVE_ARGS+=(--secret-file "$SECRET_FILE")
    fi
    if [ "$SECRET_STDIN" -eq 1 ]; then
      LIVE_ARGS+=(--secret-stdin)
    fi
    if [ -n "$CONNECT_ADDRESS" ]; then
      LIVE_ARGS+=(--connect-address "$CONNECT_ADDRESS")
    fi
    LIVE_ARGS+=(--transport "$TURN_TRANSPORT")
    if [ "$STUN_CONTROL_COUNT" -gt 0 ]; then
      for endpoint in "${STUN_CONTROLS[@]}"; do
        LIVE_ARGS+=(--stun-control "$endpoint")
      done
    fi
    MACCELLULAR_PUBLIC_TURN_E2E=1 go run -mod=readonly ./integration/publicturn "${LIVE_ARGS[@]}"
    ;;
  *)
    fail "MACCELLULAR_PUBLIC_TURN_E2E must be exactly 1 for a live probe"
    ;;
esac
