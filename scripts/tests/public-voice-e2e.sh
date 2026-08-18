#!/bin/sh
set -eu

PATH=/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin
export PATH

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DEPLOY_DIR="$ROOT_DIR/deploy/asterisk"
MODE=${1:---static}
GO_BIN=${MACCELLULAR_PUBLIC_VOICE_GO_BIN:-/usr/local/bin/go}
DOCKER_BIN=/opt/homebrew/bin/docker
COMPOSE_BIN=/opt/homebrew/bin/docker-compose
BUILDX_BIN=/opt/homebrew/bin/docker-buildx
IMAGE_TAG=maccellular-asterisk:22.10.1-local
TEST_PARENT="$ROOT_DIR/local/asterisk-integration-tests"
TEST_ROOT=
OVERLAY_FILE=
COMPOSE_PROJECT=
GENERATED_ROOT=
E2E_IMAGE_REF=
LIVE_STARTED=0
ENDPOINT_CONTAINER=

usage() {
  cat <<'EOF'
Usage:
  scripts/tests/public-voice-e2e.sh --static
  MACCELLULAR_PUBLIC_VOICE_E2E=1 scripts/tests/public-voice-e2e.sh --live

--static compiles the complete test overlay and runs only hermetic SIP/SDP/RTP
contract checks. --live starts one disposable loopback Asterisk 22 project,
sends a real synthetic SIP INVITE, drives the external voice v2 HTTP
Offer/Answer/End flow, verifies distinct PCMU patterns in both directions, and
requires the final ARI channel and bridge sets to be empty.

The live mode never skips. Missing Docker, Compose, Buildx, Go, a usable image,
or any signaling/media/cleanup proof exits non-zero. It installs no software
and leaves no long-running service.
EOF
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

missing() {
  printf 'MISSING DEPENDENCY: %s\n' "$1" >&2
  exit 69
}

need_file() {
  [ -x "$1" ] || missing "$1"
}

compose() {
  MACCELLULAR_PBX_IMAGE_REF="$E2E_IMAGE_REF" "$COMPOSE_BIN" \
    --project-name "$COMPOSE_PROJECT" \
    --env-file "$GENERATED_ROOT/compose.env" \
    -f "$DEPLOY_DIR/compose.yaml" \
    --profile explicit-pbx "$@"
}

make_overlay() {
  main_target="$ROOT_DIR/cmd/djonehub-macos/public_voice_e2e_overlay_test.go"
  voice_target="$ROOT_DIR/internal/remotevoice/public_voice_e2e_overlay.go"
  [ ! -e "$main_target" ] || fail "main overlay target already exists"
  [ ! -e "$voice_target" ] || fail "remotevoice overlay target already exists"
  OVERLAY_FILE=$(mktemp "${TMPDIR:-/tmp}/public-voice-e2e-overlay.XXXXXX")
  printf '{"Replace":{"%s":"%s","%s":"%s"}}\n' \
    "$main_target" "$ROOT_DIR/integration/asterisk22/public_voice_e2e_test.go.overlay" \
    "$voice_target" "$ROOT_DIR/integration/asterisk22/remotevoice_public_voice_e2e.go.overlay" \
    >"$OVERLAY_FILE"
}

cleanup() {
  original_status=$?
  trap - EXIT HUP INT TERM
  cleanup_ok=1
  if [ "$LIVE_STARTED" -eq 1 ]; then
    if [ -n "$ENDPOINT_CONTAINER" ]; then
      "$DOCKER_BIN" logs "$ENDPOINT_CONTAINER" >"$TEST_ROOT/synthetic-endpoint.log" 2>&1 || true
      "$DOCKER_BIN" rm -f "$ENDPOINT_CONTAINER" >"$TEST_ROOT/synthetic-endpoint-cleanup.log" 2>&1 || cleanup_ok=0
      [ -z "$("$DOCKER_BIN" ps -aq --filter "name=^/${ENDPOINT_CONTAINER}$" 2>/dev/null || true)" ] || cleanup_ok=0
    fi
    compose logs --no-color asterisk >"$TEST_ROOT/asterisk-container.log" 2>&1 || true
    compose down --timeout 5 --remove-orphans >"$TEST_ROOT/cleanup.log" 2>&1 || cleanup_ok=0
    remaining_containers=$($DOCKER_BIN ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" 2>/dev/null || true)
    remaining_networks=$($DOCKER_BIN network ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" 2>/dev/null || true)
    [ -z "$remaining_containers" ] || cleanup_ok=0
    [ -z "$remaining_networks" ] || cleanup_ok=0
  fi
  if [ -n "$OVERLAY_FILE" ]; then
    case "$OVERLAY_FILE" in
      "${TMPDIR:-/tmp}"/public-voice-e2e-overlay.*) /bin/rm -f -- "$OVERLAY_FILE" || cleanup_ok=0 ;;
      *) cleanup_ok=0 ;;
    esac
  fi
  if [ -n "$TEST_ROOT" ] && [ "$original_status" -eq 0 ] && [ "$cleanup_ok" -eq 1 ]; then
    case "$TEST_ROOT" in
      "$TEST_PARENT"/public-voice-e2e.*) /bin/rm -rf -- "$TEST_ROOT" || cleanup_ok=0 ;;
      *) cleanup_ok=0 ;;
    esac
  elif [ -n "$TEST_ROOT" ] && [ -d "$TEST_ROOT" ]; then
    chmod 0700 "$TEST_ROOT" || true
    printf 'FAILURE EVIDENCE: preserved test root: %s\n' "$TEST_ROOT" >&2
  fi
  if [ "$original_status" -eq 0 ] && [ "$cleanup_ok" -ne 1 ]; then
    original_status=1
  fi
  exit "$original_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

go_arch() {
  case $(uname -m) in
    arm64|aarch64) printf '%s\n' arm64 ;;
    x86_64|amd64) printf '%s\n' amd64 ;;
    *) fail "unsupported Go test architecture" ;;
  esac
}

run_go_test() {
  test_name=$1
  shift
  GOARCH=$(go_arch) CGO_ENABLED=1 "$GO_BIN" test \
    -mod=readonly \
    -overlay="$OVERLAY_FILE" \
    -tags=public_voice_e2e \
    -run "$test_name" \
    -count=1 \
    -timeout=2m \
    "$@" \
    ./cmd/djonehub-macos
}

run_static() {
  need_file "$GO_BIN"
  make_overlay
  run_go_test '^TestPublicVoiceE2EHarnessContract$'
  printf '%s\n' 'Public voice E2E static harness: PASS'
}

read_env_value() {
  key=$1
  awk -F= -v wanted="$key" '
    $1 == wanted { count++; value = substr($0, index($0, "=") + 1) }
    END { if (count != 1 || value == "") exit 1; print value }
  ' "$GENERATED_ROOT/deployment.env"
}

resolve_local_image() {
  if ! "$DOCKER_BIN" image inspect "$IMAGE_TAG" >/dev/null 2>&1; then
    "$BUILDX_BIN" build \
      --load \
      --provenance=false \
      --tag "$IMAGE_TAG" \
      --file "$DEPLOY_DIR/Dockerfile" \
      "$DEPLOY_DIR"
  fi
  image_id=$($DOCKER_BIN image inspect --format '{{.Id}}' "$IMAGE_TAG") || fail "cannot inspect local Asterisk image"
  E2E_IMAGE_REF=$($DOCKER_BIN image inspect --format '{{index .RepoDigests 0}}' "$IMAGE_TAG") || fail "local Asterisk image has no RepoDigest"
  case "$image_id:$E2E_IMAGE_REF" in
    sha256:????????????????????????????????????????????????????????????????:maccellular-asterisk@sha256:????????????????????????????????????????????????????????????????) ;;
    *) fail "local Asterisk image does not have an exact content ID and RepoDigest" ;;
  esac
  printf 'E2E Asterisk local image: %s (%s)\n' "$E2E_IMAGE_REF" "$image_id"
}

run_live() {
  [ "${MACCELLULAR_PUBLIC_VOICE_E2E:-}" = 1 ] || {
    printf '%s\n' 'LIVE TEST NOT AUTHORIZED: set MACCELLULAR_PUBLIC_VOICE_E2E=1 explicitly.' >&2
    exit 64
  }
  need_file "$GO_BIN"
  need_file "$DOCKER_BIN"
  need_file "$COMPOSE_BIN"
  need_file "$BUILDX_BIN"
  "$DOCKER_BIN" info >/dev/null 2>&1 || missing "reachable Docker daemon/context"
  "$COMPOSE_BIN" version >/dev/null 2>&1 || missing "standalone Docker Compose"
  "$BUILDX_BIN" version >/dev/null 2>&1 || missing "standalone Docker Buildx"

  [ ! -e "$TEST_PARENT" ] || [ -d "$TEST_PARENT" ] || fail "test parent is not a directory"
  mkdir -p "$TEST_PARENT"
  [ ! -L "$TEST_PARENT" ] || fail "test parent must not be a symlink"
  chmod 0700 "$TEST_PARENT"
  TEST_ROOT=$(mktemp -d "$TEST_PARENT/public-voice-e2e.XXXXXX")
  GENERATED_ROOT="$TEST_ROOT/generated"
  COMPOSE_PROJECT="maccellular-public-voice-e2e-$PPID-$$"
  make_overlay
  resolve_local_image

  "$DEPLOY_DIR/generate-config.sh" \
    --output "$GENERATED_ROOT" \
    --gateway-ip 127.0.0.2 \
    --sip-bind-ip 127.0.0.1 \
    --lan-cidr 127.0.0.0/8 \
    --ari-port 28088 \
    --sip-port 25060 \
    --rtp-min 26000 \
    --rtp-max 26031 \
    --test-loopback \
    --apply >"$TEST_ROOT/generate.log"
  "$DEPLOY_DIR/verify-config.sh" "$GENERATED_ROOT" >"$TEST_ROOT/verify.log"
  # Colima does not forward host-originated UDP into this loopback publication.
  # The production output is verified unchanged above, then this disposable
  # copy is narrowed to one synthetic peer on the same isolated Docker bridge.
  sed \
    -e 's/^external_signaling_address = 127\.0\.0\.1$/external_signaling_address = 172.31.255.250/' \
    -e 's/^external_media_address = 127\.0\.0\.1$/external_media_address = 172.31.255.250/' \
    -e 's/^match = 127\.0\.0\.2$/match = 172.31.255.251/' \
    "$GENERATED_ROOT/config/pjsip.conf" >"$GENERATED_ROOT/config/pjsip.conf.e2e"
  mv -- "$GENERATED_ROOT/config/pjsip.conf.e2e" "$GENERATED_ROOT/config/pjsip.conf"
  chmod 0600 "$GENERATED_ROOT/config/pjsip.conf"
  grep -Fqx 'external_signaling_address = 172.31.255.250' "$GENERATED_ROOT/config/pjsip.conf" || fail "E2E signaling address was not applied"
  grep -Fqx 'external_media_address = 172.31.255.250' "$GENERATED_ROOT/config/pjsip.conf" || fail "E2E media address was not applied"
  grep -Fqx 'match = 172.31.255.251' "$GENERATED_ROOT/config/pjsip.conf" || fail "E2E synthetic peer match was not applied"
  GOOS=linux GOARCH=arm64 CGO_ENABLED=0 "$GO_BIN" build \
    -mod=readonly \
    -o "$TEST_ROOT/synthetic-endpoint" \
    ./integration/asterisk22/synthetic_endpoint
  chmod 0555 "$TEST_ROOT/synthetic-endpoint"
  compose config >"$TEST_ROOT/compose-resolved.yaml"
  grep -Fq "image: $E2E_IMAGE_REF" "$TEST_ROOT/compose-resolved.yaml" || fail "resolved Compose lost the exact local image digest"
  grep -Fq 'host_ip: 127.0.0.1' "$TEST_ROOT/compose-resolved.yaml" || fail "resolved Compose lost loopback-only publication"

  LIVE_STARTED=1
  compose up -d --no-build
  container_id=$(compose ps -q asterisk)
  [ -n "$container_id" ] || fail "Asterisk container was not created"
  attempts=0
  health=
  while [ "$attempts" -lt 60 ]; do
    health=$($DOCKER_BIN inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)
    [ "$health" = healthy ] && break
    [ "$health" != unhealthy ] || fail "Asterisk container became unhealthy"
    attempts=$((attempts + 1))
    sleep 2
  done
  [ "$health" = healthy ] || fail "Asterisk did not become healthy within 120 seconds"

  ENDPOINT_CONTAINER="${COMPOSE_PROJECT}-synthetic-endpoint"
  "$DOCKER_BIN" run -d \
    --name "$ENDPOINT_CONTAINER" \
    --label "io.maccellular.public-voice-e2e=$COMPOSE_PROJECT" \
    --network "${COMPOSE_PROJECT}_pbx-runtime" \
    --ip 172.31.255.251 \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges:true \
    --user "$(id -u):$(id -g)" \
    -p 127.0.0.1:29090:18080/tcp \
    -v "$TEST_ROOT/synthetic-endpoint:/synthetic-endpoint:ro" \
    --entrypoint /synthetic-endpoint \
    "$E2E_IMAGE_REF" >"$TEST_ROOT/synthetic-endpoint-container-id"
  attempts=0
  endpoint_ready=
  while [ "$attempts" -lt 50 ]; do
    if /usr/bin/curl --fail --silent --show-error --max-time 2 \
      http://127.0.0.1:29090/status >"$TEST_ROOT/synthetic-endpoint-status.json" 2>/dev/null; then
      endpoint_ready=1
      break
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  [ "$endpoint_ready" = 1 ] || fail "synthetic SIP/RTP endpoint did not become ready within 50 seconds"

  version_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'core show version')
  printf '%s\n' "$version_output" | grep -Fq 'Asterisk 22.10.1' || fail "runtime Asterisk version is not 22.10.1"
  compose exec -T asterisk sh -c \
    "find /usr/lib/asterisk/modules -mindepth 1 -maxdepth 1 -printf '%f\\n' | LC_ALL=C sort" \
    >"$TEST_ROOT/runtime-modules.txt"
  cmp -s "$DEPLOY_DIR/canonical-modules.txt" "$TEST_ROOT/runtime-modules.txt" || fail "runtime module directory differs from the canonical manifest"

  CONTROL_USER=$(read_env_value CONTROL_USER)
  EXPECTED_VERSION=$(read_env_value ASTERISK_VERSION)
  EXPECTED_ENTITY=$(read_env_value ENTITY_ID)
  EXPECTED_APPLICATION=$(read_env_value APPLICATION)
  MACCELLULAR_PUBLIC_VOICE_E2E=1 \
  MACCELLULAR_ASTERISK_INTEGRATION_URL=http://127.0.0.1:28088/ari \
  MACCELLULAR_ASTERISK_INTEGRATION_USERNAME="$CONTROL_USER" \
  MACCELLULAR_ASTERISK_INTEGRATION_PASSWORD_FILE="$GENERATED_ROOT/secrets/ari-control.password" \
  MACCELLULAR_ASTERISK_INTEGRATION_VERSION="$EXPECTED_VERSION" \
  MACCELLULAR_ASTERISK_INTEGRATION_ENTITY_ID="$EXPECTED_ENTITY" \
  MACCELLULAR_ASTERISK_INTEGRATION_APPLICATION="$EXPECTED_APPLICATION" \
  MACCELLULAR_SYNTHETIC_ENDPOINT_URL=http://127.0.0.1:29090 \
    run_go_test '^TestPublicVoiceE2EAsterisk22SyntheticCall$' -v

  remaining_channels=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'core show channels count')
  printf '%s\n' "$remaining_channels" | grep -Fq '0 active channels' || fail "Asterisk retained an active channel after E2E cleanup"
  printf '%s\n' 'Public voice E2E live acceptance: PASS'
}

case "$MODE" in
  --static) run_static ;;
  --live) run_live ;;
  -h|--help) usage ;;
  *) usage >&2; exit 64 ;;
esac
