#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
DEPLOY_DIR="${ROOT_DIR}/deploy/asterisk"
MODE=${1:---static}
TEST_ROOT=
TEST_BASE=
LIVE_STARTED=0
PASSED=0
COMPOSE_STYLE=
BUILDX_STYLE=
PBX_IMAGE=maccellular-asterisk:22.10.1-local
PBX_IMAGE_REF=maccellular-asterisk@sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51
PBX_IMAGE_ID=sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51
GO_BIN=${MACCELLULAR_ASTERISK22_GO_BIN:-/usr/local/bin/go}

usage() {
  cat <<'EOF'
Usage:
  scripts/tests/asterisk22-integration.sh --static
  MACCELLULAR_ASTERISK22_LIVE=1 scripts/tests/asterisk22-integration.sh --live

--static never contacts Docker or the network. --live builds and starts one
disposable, loopback-only Compose project and requires an explicit opt-in.
Missing dependencies are failures, never skips.
EOF
}

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  exit 1
}

pass() {
  printf 'PASS: %s\n' "$1"
  PASSED=$((PASSED + 1))
}

need() {
  command -v "$1" >/dev/null 2>&1 || {
    printf 'MISSING DEPENDENCY: %s\n' "$1" >&2
    exit 69
  }
}

compose() {
  if [ "$COMPOSE_STYLE" = plugin ]; then
    docker compose \
      --project-name "$COMPOSE_PROJECT" \
      --env-file "$GENERATED_ROOT/compose.env" \
      -f "$DEPLOY_DIR/compose.yaml" \
      --profile explicit-pbx "$@"
  else
    docker-compose \
      --project-name "$COMPOSE_PROJECT" \
      --env-file "$GENERATED_ROOT/compose.env" \
      -f "$DEPLOY_DIR/compose.yaml" \
      --profile explicit-pbx "$@"
  fi
}

build_image() {
  if [ "$BUILDX_STYLE" = plugin ]; then
    docker buildx build \
      --load \
      --provenance=false \
      --tag "$PBX_IMAGE" \
      --file "$DEPLOY_DIR/Dockerfile" \
      "$DEPLOY_DIR"
  else
    docker-buildx build \
      --load \
      --provenance=false \
      --tag "$PBX_IMAGE" \
      --file "$DEPLOY_DIR/Dockerfile" \
      "$DEPLOY_DIR"
  fi
}

cleanup() {
  original_status=$?
  trap - EXIT HUP INT TERM
  cleanup_ok=1
  if [ "$LIVE_STARTED" -eq 1 ]; then
    cleanup_log="$TEST_ROOT/cleanup.log"
    compose logs --no-color asterisk >"$TEST_ROOT/asterisk-container.log" 2>&1 || true
    compose down --timeout 5 --remove-orphans >"$cleanup_log" 2>&1 || \
      printf 'CLEANUP WARN: Compose down failed; applying exact project-label fallback.\n' >>"$cleanup_log"

    if remaining_containers=$(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" 2>>"$cleanup_log"); then
      for container in $remaining_containers; do
        docker rm -f "$container" >>"$cleanup_log" 2>&1 || cleanup_ok=0
      done
    else
      cleanup_ok=0
    fi
    if remaining_networks=$(docker network ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" 2>>"$cleanup_log"); then
      for network in $remaining_networks; do
        docker network rm "$network" >>"$cleanup_log" 2>&1 || cleanup_ok=0
      done
    else
      cleanup_ok=0
    fi

    if ! remaining_containers=$(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" 2>>"$cleanup_log"); then
      printf 'CLEANUP FAIL: Docker could not verify project containers.\n' >>"$cleanup_log"
      cleanup_ok=0
    elif [ -n "$remaining_containers" ]; then
      printf 'CLEANUP FAIL: project containers remain: %s\n' "$remaining_containers" >>"$cleanup_log"
      cleanup_ok=0
    fi
    if ! remaining_networks=$(docker network ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" 2>>"$cleanup_log"); then
      printf 'CLEANUP FAIL: Docker could not verify project networks.\n' >>"$cleanup_log"
      cleanup_ok=0
    elif [ -n "$remaining_networks" ]; then
      printf 'CLEANUP FAIL: project networks remain: %s\n' "$remaining_networks" >>"$cleanup_log"
      cleanup_ok=0
    fi
  fi

  preserve_root=0
  if [ "$original_status" -ne 0 ] || [ "$cleanup_ok" -ne 1 ]; then
    preserve_root=1
  fi
  if [ "$preserve_root" -eq 0 ] && [ -n "${TEST_ROOT:-}" ]; then
    case "$TEST_ROOT" in
      "${TEST_BASE}"/maccellular-asterisk22-test.*) /bin/rm -rf -- "$TEST_ROOT" ;;
    esac
  elif [ -n "${TEST_ROOT:-}" ] && [ -d "$TEST_ROOT" ]; then
    chmod 0700 "$TEST_ROOT" || cleanup_ok=0
    printf 'FAILURE EVIDENCE: preserved 0700 test root: %s\n' "$TEST_ROOT" >&2
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

expect_fail() {
  name=$1
  shift
  if "$@" >"$TEST_ROOT/expected-failure.stdout" 2>"$TEST_ROOT/expected-failure.stderr"; then
    fail "$name unexpectedly passed"
  fi
  pass "$name"
}

make_test_root() {
  umask 077
  TEST_BASE=${1:-${TMPDIR:-/tmp}}
  TEST_BASE=${TEST_BASE%/}
  TEST_ROOT=$(mktemp -d "${TEST_BASE}/maccellular-asterisk22-test.XXXXXX")
}

make_live_test_root() {
  live_parent="$ROOT_DIR/local/asterisk-integration-tests"
  if [ ! -e "$live_parent" ] && [ ! -L "$live_parent" ]; then
    mkdir -p "$live_parent"
  fi
  [ -d "$live_parent" ] && [ ! -L "$live_parent" ] || \
    fail "live test parent must be a non-symlink directory"
  chmod 0700 "$live_parent"
  live_parent=$(CDPATH= cd -- "$live_parent" && pwd -P)
  case "$live_parent" in
    "$ROOT_DIR"/local/asterisk-integration-tests) ;;
    *) fail "live test parent escaped the repository-local ignored tree" ;;
  esac
  make_test_root "$live_parent"
}

generate_synthetic() {
  destination=$1
  "$DEPLOY_DIR/generate-config.sh" \
    --output "$destination" \
    --gateway-ip 127.0.0.2 \
    --sip-bind-ip 127.0.0.1 \
    --lan-cidr 127.0.0.0/8 \
    --ari-port 28088 \
    --sip-port 25060 \
    --rtp-min 26000 \
    --rtp-max 26031 \
    --test-loopback \
    --apply
}

tamper() {
  source_root=$1
  destination=$2
  file=$3
  expression=$4
  cp -R "$source_root" "$destination"
  sed "$expression" "$destination/$file" >"$destination/$file.new"
  mv -- "$destination/$file.new" "$destination/$file"
  chmod 0600 "$destination/$file"
}

append_tamper() {
  source_root=$1
  destination=$2
  file=$3
  content=$4
  cp -R "$source_root" "$destination"
  printf '%s\n' "$content" >>"$destination/$file"
  chmod 0600 "$destination/$file"
}

run_static() {
  for dependency in awk cat chmod cmp cp curl cut find grep id mkdir mktemp mv sed sort stat tr wc; do
    need "$dependency"
  done
  make_test_root
  GENERATED_ROOT="$TEST_ROOT/generated"

  "$DEPLOY_DIR/generate-config.sh" \
    --output "$GENERATED_ROOT" \
    --gateway-ip 127.0.0.2 \
    --sip-bind-ip 127.0.0.1 \
    --lan-cidr 127.0.0.0/8 \
    --test-loopback >"$TEST_ROOT/dry-run.txt"
  [ ! -e "$GENERATED_ROOT" ] || fail "dry-run created an output tree"
  grep -Fq 'DRY RUN: no files created' "$TEST_ROOT/dry-run.txt" || fail "dry-run did not identify itself"
  pass "generator is dry-run by default"

  generate_synthetic "$GENERATED_ROOT" >"$TEST_ROOT/generate.txt"
  "$DEPLOY_DIR/verify-config.sh" "$GENERATED_ROOT" >"$TEST_ROOT/verify.txt"
  grep -Fq 'Asterisk deployment static verification: PASS' "$TEST_ROOT/verify.txt" || fail "verifier did not report PASS"
  grep -Fqx "ASTERISK_IMAGE_REF=$PBX_IMAGE_REF" "$DEPLOY_DIR/source.lock" || fail "integration image reference differs from source.lock"
  [ "sha256:${PBX_IMAGE_REF#maccellular-asterisk@sha256:}" = "$PBX_IMAGE_ID" ] || fail "integration image ID differs from its exact name@digest reference"
  pass "fresh private configuration passes the complete static contract"

  mkdir "$TEST_ROOT/malicious-home"
  chmod 0700 "$TEST_ROOT/malicious-home"
  printf '%s\n' 'write-out = "CURLRC_WAS_LOADED"' >"$TEST_ROOT/malicious-home/.curlrc"
  printf '%s\n' 'curl -q ignores ambient user configuration' >"$TEST_ROOT/curl-input.txt"
  CURL_HOME="$TEST_ROOT/malicious-home" HOME="$TEST_ROOT/malicious-home" \
    curl -q --silent --show-error --output "$TEST_ROOT/curl-output.txt" \
      "file://$TEST_ROOT/curl-input.txt" >"$TEST_ROOT/curl-stdout.txt"
  [ ! -s "$TEST_ROOT/curl-stdout.txt" ] || fail "curl loaded the malicious user curlrc"
  cmp -s "$TEST_ROOT/curl-input.txt" "$TEST_ROOT/curl-output.txt" || fail "curl local regression transfer drifted"
  pass "curl -q as the first argument ignores a malicious HOME curlrc"

  INJECTION_MARKER="$TEST_ROOT/deployment-command-substitution-ran"
  INJECTION_VALUE='$(touch '"$INJECTION_MARKER"')'
  cp -R "$GENERATED_ROOT" "$TEST_ROOT/deployment-numeric-injection"
  awk -F= -v malicious="$INJECTION_VALUE" '
    $1 == "RTP_MIN" { print "RTP_MIN=" malicious; next }
    { print }
  ' "$TEST_ROOT/deployment-numeric-injection/deployment.env" >"$TEST_ROOT/deployment-numeric-injection/deployment.env.new"
  mv -- "$TEST_ROOT/deployment-numeric-injection/deployment.env.new" "$TEST_ROOT/deployment-numeric-injection/deployment.env"
  chmod 0600 "$TEST_ROOT/deployment-numeric-injection/deployment.env"
  expect_fail "verifier rejects deployment arithmetic command substitution" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/deployment-numeric-injection" --expected-root "$GENERATED_ROOT"
  [ ! -e "$INJECTION_MARKER" ] || fail "verifier executed a deployment arithmetic payload"

  INJECTION_MARKER="$TEST_ROOT/compose-command-substitution-ran"
  INJECTION_VALUE='$(touch '"$INJECTION_MARKER"')'
  cp -R "$GENERATED_ROOT" "$TEST_ROOT/compose-numeric-injection"
  awk -F= -v malicious="$INJECTION_VALUE" '
    $1 == "MACCELLULAR_PBX_UID" { print "MACCELLULAR_PBX_UID=" malicious; next }
    { print }
  ' "$TEST_ROOT/compose-numeric-injection/compose.env" >"$TEST_ROOT/compose-numeric-injection/compose.env.new"
  mv -- "$TEST_ROOT/compose-numeric-injection/compose.env.new" "$TEST_ROOT/compose-numeric-injection/compose.env"
  chmod 0600 "$TEST_ROOT/compose-numeric-injection/compose.env"
  expect_fail "verifier rejects Compose numeric command substitution" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/compose-numeric-injection" --expected-root "$GENERATED_ROOT"
  [ ! -e "$INJECTION_MARKER" ] || fail "verifier executed a Compose arithmetic payload"

  expect_fail "generator rejects a non-canonical LAN network" \
    "$DEPLOY_DIR/generate-config.sh" \
      --output "$TEST_ROOT/rejected-network" \
      --gateway-ip 192.168.50.20 \
      --sip-bind-ip 192.168.50.10 \
      --lan-cidr 192.168.50.1/24

  expect_fail "generator rejects an address outside the declared LAN" \
    "$DEPLOY_DIR/generate-config.sh" \
      --output "$TEST_ROOT/rejected-member" \
      --gateway-ip 192.168.51.20 \
      --sip-bind-ip 192.168.50.10 \
      --lan-cidr 192.168.50.0/24

  expect_fail "generator rejects punctuation in an Asterisk token" \
    "$DEPLOY_DIR/generate-config.sh" \
      --output "$TEST_ROOT/rejected-token" \
      --gateway-ip 127.0.0.2 \
      --sip-bind-ip 127.0.0.1 \
      --lan-cidr 127.0.0.0/8 \
      --application 'ab;injected' \
      --test-loopback

  BAD_MULTILINE_TOKEN=$(printf 'ab\ninjected')
  expect_fail "generator rejects a multiline Asterisk token" \
    "$DEPLOY_DIR/generate-config.sh" \
      --output "$TEST_ROOT/rejected-multiline-token" \
      --gateway-ip 127.0.0.2 \
      --sip-bind-ip 127.0.0.1 \
      --lan-cidr 127.0.0.0/8 \
      --application "$BAD_MULTILINE_TOKEN" \
      --test-loopback

  tamper "$GENERATED_ROOT" "$TEST_ROOT/direct-media" config/pjsip.conf \
    's/direct_media = no/direct_media = yes/'
  expect_fail "verifier rejects direct media" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/direct-media" --expected-root "$GENERATED_ROOT"

  tamper "$GENERATED_ROOT" "$TEST_ROOT/policy" config/extensions.conf \
    's/DJONEHUB_POLICY_ID=djonehub-incoming-v1/DJONEHUB_POLICY_ID=wrong-policy/'
  expect_fail "verifier rejects policy identity drift" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/policy" --expected-root "$GENERATED_ROOT"

  tamper "$GENERATED_ROOT" "$TEST_ROOT/orphan" config/extensions.conf \
    '/ same => n,Hangup()/d'
  expect_fail "verifier rejects a missing post-Stasis Hangup" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/orphan" --expected-root "$GENERATED_ROOT"

  tamper "$GENERATED_ROOT" "$TEST_ROOT/module" config/modules.conf \
    '/require = chan_websocket.so/d'
  expect_fail "verifier rejects a non-fatal required module" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/module" --expected-root "$GENERATED_ROOT"

  tamper "$GENERATED_ROOT" "$TEST_ROOT/module-spacing" config/modules.conf \
    's/require = app_stasis.so/require=app_system.so/'
  expect_fail "verifier rejects noncanonical module syntax" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/module-spacing" --expected-root "$GENERATED_ROOT"

  cp -R "$GENERATED_ROOT" "$TEST_ROOT/weak-secret"
  chmod 0644 "$TEST_ROOT/weak-secret/secrets/ari-control.password"
  expect_fail "verifier rejects a weak secret mode" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/weak-secret" --expected-root "$GENERATED_ROOT"

  cp -R "$GENERATED_ROOT" "$TEST_ROOT/unsafe-secret"
  printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' >"$TEST_ROOT/unsafe-secret/secrets/ari-control.password"
  chmod 0600 "$TEST_ROOT/unsafe-secret/secrets/ari-control.password"
  expect_fail "verifier rejects a non-generator base64 secret" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/unsafe-secret" --expected-root "$GENERATED_ROOT"

  cp -R "$GENERATED_ROOT" "$TEST_ROOT/hash-mismatch"
  printf '%064d\n' 0 >"$TEST_ROOT/hash-mismatch/secrets/ari-control.password"
  chmod 0600 "$TEST_ROOT/hash-mismatch/secrets/ari-control.password"
  expect_fail "verifier rejects a secret whose SHA-512 crypt hash does not match" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/hash-mismatch" --expected-root "$GENERATED_ROOT"

  append_tamper "$GENERATED_ROOT" "$TEST_ROOT/extra-ari-user" config/ari.conf \
    "$(printf '\n[unexpected_user]\ntype = user\nread_only = yes\npassword_format = crypt\npassword = %s' '$6$unsafe$......................................................................................')"
  expect_fail "verifier rejects an extra ARI user" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/extra-ari-user" --expected-root "$GENERATED_ROOT"

  append_tamper "$GENERATED_ROOT" "$TEST_ROOT/extra-pjsip-endpoint" config/pjsip.conf \
    "$(printf '\n[unexpected-endpoint]\ntype = endpoint\ncontext = from-cellular')"
  expect_fail "verifier rejects an extra PJSIP endpoint" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/extra-pjsip-endpoint" --expected-root "$GENERATED_ROOT"

  append_tamper "$GENERATED_ROOT" "$TEST_ROOT/extra-dialplan" config/extensions.conf \
    "$(printf '\nexten => 999,1,Answer()')"
  expect_fail "verifier rejects an extra dialplan route" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/extra-dialplan" --expected-root "$GENERATED_ROOT"

  append_tamper "$GENERATED_ROOT" "$TEST_ROOT/extra-logger" config/logger.conf \
    "$(printf '\nfull = debug,verbose,notice,warning,error,security')"
  expect_fail "verifier rejects an extra logger sink" \
    "$DEPLOY_DIR/verify-config.sh" "$TEST_ROOT/extra-logger" --expected-root "$GENERATED_ROOT"

  printf 'Asterisk 22 static contract tests passed: %s\n' "$PASSED"
}

run_live() {
  [ "${MACCELLULAR_ASTERISK22_LIVE:-}" = 1 ] || {
    printf 'LIVE TEST NOT AUTHORIZED: set MACCELLULAR_ASTERISK22_LIVE=1 explicitly.\n' >&2
    exit 64
  }
  for dependency in awk cmp curl cut diff docker grep mktemp sed sort tr wc; do need "$dependency"; done
  [ -x "$GO_BIN" ] || {
    printf 'MISSING DEPENDENCY: %s\n' "$GO_BIN" >&2
    exit 69
  }
  if docker compose version >/dev/null 2>&1; then
    COMPOSE_STYLE=plugin
  elif command -v docker-compose >/dev/null 2>&1 && docker-compose version >/dev/null 2>&1; then
    COMPOSE_STYLE=standalone
  else
    printf 'MISSING DEPENDENCY: Docker Compose v2 plugin or standalone binary\n' >&2
    exit 69
  fi
  if docker buildx version >/dev/null 2>&1; then
    BUILDX_STYLE=plugin
  elif command -v docker-buildx >/dev/null 2>&1 && docker-buildx version >/dev/null 2>&1; then
    BUILDX_STYLE=standalone
  else
    printf 'MISSING DEPENDENCY: Docker Buildx plugin or standalone binary\n' >&2
    exit 69
  fi
  docker info >/dev/null 2>&1 || {
    printf 'MISSING DEPENDENCY: reachable Docker daemon/context\n' >&2
    exit 69
  }

  make_live_test_root
  GENERATED_ROOT="$TEST_ROOT/generated"
  COMPOSE_PROJECT="maccellular-asterisk22-$PPID-$$"
  generate_synthetic "$GENERATED_ROOT" >"$TEST_ROOT/generate.txt"
  compose config >"$TEST_ROOT/compose-resolved.yaml"
  grep -Fq 'host_ip: 127.0.0.1' "$TEST_ROOT/compose-resolved.yaml" || fail "resolved Compose lost explicit host addresses"
  grep -Fq "image: $PBX_IMAGE_REF" "$TEST_ROOT/compose-resolved.yaml" || fail "resolved Compose lost the exact pinned PBX image reference"
  grep -Fq 'pull_policy: never' "$TEST_ROOT/compose-resolved.yaml" || fail "resolved Compose may pull an unverified PBX image"
  LIVE_STARTED=1
  build_image
  built_image_id=$(docker image inspect --format '{{.Id}}' "$PBX_IMAGE" 2>/dev/null) || fail "could not inspect the newly built PBX image"
  [ "$built_image_id" = "$PBX_IMAGE_ID" ] || fail "newly built PBX image content digest differs from the pinned image"
  pinned_image_id=$(docker image inspect --format '{{.Id}}' "$PBX_IMAGE_REF" 2>/dev/null) || fail "pinned name@digest PBX image reference is not locally resolvable"
  [ "$pinned_image_id" = "$PBX_IMAGE_ID" ] || fail "pinned name@digest resolves to the wrong local image"
  pass "newly built PBX image exactly matches the pinned content digest"
  compose up -d --no-build

  container_id=$(compose ps -q asterisk)
  [ -n "$container_id" ] || fail "Compose did not create the Asterisk container"
  container_image_id=$(docker inspect --format '{{.Image}}' "$container_id") || fail "could not inspect the PBX container image"
  container_image_ref=$(docker inspect --format '{{.Config.Image}}' "$container_id") || fail "could not inspect the PBX container image reference"
  [ "$container_image_id" = "$PBX_IMAGE_ID" ] || fail "PBX container does not use the pinned image content"
  [ "$container_image_ref" = "$PBX_IMAGE_REF" ] || fail "PBX container was not created from the exact pinned name@digest reference"
  attempts=0
  while [ "$attempts" -lt 60 ]; do
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id" 2>/dev/null || true)
    [ "$health" = healthy ] && break
    [ "$health" != unhealthy ] || fail "Asterisk container became unhealthy"
    attempts=$((attempts + 1))
    sleep 2
  done
  [ "${health:-}" = healthy ] || fail "Asterisk did not become healthy within 120 seconds"
  pass "Asterisk container reaches healthy state"

  version_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'core show version')
  printf '%s\n' "$version_output" | grep -Fq 'Asterisk 22.10.1' || fail "runtime version is not 22.10.1"
  pass "runtime version is pinned"

  cp "$DEPLOY_DIR/canonical-modules.txt" "$TEST_ROOT/expected-modules.txt"
  compose exec -T asterisk sh -c \
    "find /usr/lib/asterisk/modules -mindepth 1 -maxdepth 1 -printf '%f\\n' | LC_ALL=C sort" \
    >"$TEST_ROOT/image-module-directory.txt"
  cmp -s "$TEST_ROOT/expected-modules.txt" "$TEST_ROOT/image-module-directory.txt" || \
    fail "physical image module directory differs from the exact canonical 35-module manifest"
  pass "physical image module directory exactly matches the canonical 35-module manifest"

  runtime_modules=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'module show')
  printf '%s\n' "$runtime_modules" | awk '$1 ~ /^[a-z0-9_]+\.so$/ { print $1 }' | sort >"$TEST_ROOT/actual-modules.txt"
  cmp -s "$TEST_ROOT/expected-modules.txt" "$TEST_ROOT/actual-modules.txt" || fail "runtime dynamic-module set differs from the exact 35-module allowlist"
  printf '%s\n' "$runtime_modules" | grep -Fq '51 modules loaded' || fail "runtime built-in module closure drifted"
  pass "runtime module closure exactly matches 35 dynamic and 16 built-in modules"

  for module in \
    app_originate.so app_system.so \
    res_pjsip_endpoint_identifier_anonymous.so \
    res_pjsip_endpoint_identifier_user.so \
    res_pjsip_outbound_registration.so; do
    module_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx "module show like $module")
    if printf '%s\n' "$module_output" | grep -Fq "$module"; then
      fail "unapproved runtime module is loaded: $module"
    fi
  done
  pass "high-risk and non-contract modules start unloaded"

  timing_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'timing test')
  printf '%s\n' "$timing_output" | grep -Fq "Using the 'timerfd' timing module" || fail "chan_websocket has no pinned timerfd timing provider"
  printf '%s\n' "$timing_output" | grep -Fq 'we got 50 timer ticks' || fail "timerfd timing smoke did not deliver 50 ticks"
  pass "timerfd media timing delivers the expected 50 ticks"

  manager_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'manager show settings')
  printf '%s\n' "$manager_output" | grep -Eq 'Manager \(AMI\):[[:space:]]+No' || fail "AMI is enabled"
  printf '%s\n' "$manager_output" | grep -Eq 'Web Manager \(AMI/HTTP\):[[:space:]]+No' || fail "AMI over HTTP is enabled"
  pass "AMI and AMI-over-HTTP remain disabled"

  endpoint_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'pjsip show endpoint cellular-gateway')
  printf '%s\n' "$endpoint_output" | grep -Eq 'allow[[:space:]]+:[[:space:]]+\(ulaw\)' || fail "PJSIP endpoint codec is not exact PCMU"
  printf '%s\n' "$endpoint_output" | grep -Eq 'allow_transfer[[:space:]]+:[[:space:]]+false' || fail "PJSIP transfers are enabled"
  printf '%s\n' "$endpoint_output" | grep -Eq 'direct_media[[:space:]]+:[[:space:]]+false' || fail "PJSIP direct media is enabled"
  printf '%s\n' "$endpoint_output" | grep -Eq 'identify_by[[:space:]]+:[[:space:]]+ip' || fail "PJSIP endpoint is not IP-identified"
  printf '%s\n' "$endpoint_output" | grep -Eq 'send_connected_line[[:space:]]+:[[:space:]]+no' || fail "connected-line signaling is enabled"
  printf '%s\n' "$endpoint_output" | grep -Eq 'trust_connected_line[[:space:]]+:[[:space:]]+no' || fail "connected-line input is trusted"
  pass "PJSIP endpoint is IP-only, PCMU-only, non-transfer and non-direct-media"

  dialplan_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx 'dialplan show from-cellular')
  printf '%s\n' "$dialplan_output" | grep -Fq 'Stasis(maccellular_voice,incoming)' || fail "runtime dialplan lacks exact Stasis route"
  printf '%s\n' "$dialplan_output" | grep -Fq 'Hangup()' || fail "runtime dialplan lacks orphan Hangup continuation"
  pass "runtime dialplan contains the bounded Stasis route"

  docker inspect --format '{{range $p,$bindings := .NetworkSettings.Ports}}{{range $bindings}}{{println $p .HostIp .HostPort}}{{end}}{{end}}' "$container_id" | awk 'NF' | sort >"$TEST_ROOT/actual-bindings.txt"
  awk 'BEGIN {
    print "5060/udp 127.0.0.1 25060"
    print "8088/tcp 127.0.0.1 28088"
    for (port = 26000; port <= 26031; port++) {
      print port "/udp 127.0.0.1 " port
    }
  }' | sort >"$TEST_ROOT/expected-bindings.txt"
  if ! cmp -s "$TEST_ROOT/expected-bindings.txt" "$TEST_ROOT/actual-bindings.txt"; then
    diff -u "$TEST_ROOT/expected-bindings.txt" "$TEST_ROOT/actual-bindings.txt" >&2 || true
    fail "runtime port publication differs from the exact loopback-only contract"
  fi
  pass "all 34 runtime port bindings are exact host loopback bindings"

  security_output=$(docker inspect --format 'readonly={{.HostConfig.ReadonlyRootfs}} privileged={{.HostConfig.Privileged}} user={{.Config.User}} capdrop={{json .HostConfig.CapDrop}} security={{json .HostConfig.SecurityOpt}} restart={{.HostConfig.RestartPolicy.Name}} init={{.HostConfig.Init}}' "$container_id")
  expected_user=$(awk -F= '$1 == "MACCELLULAR_PBX_UID" { uid = $2 } $1 == "MACCELLULAR_PBX_GID" { gid = $2 } END { print uid ":" gid }' "$GENERATED_ROOT/compose.env")
  printf '%s\n' "$security_output" | grep -Fq "readonly=true privileged=false user=$expected_user capdrop=[\"ALL\"] security=[\"no-new-privileges:true\"] restart=no init=true" || fail "runtime container hardening or fail-closed restart policy drifted"
  pass "container is read-only, unprivileged, capability-free, no-new-privileges and restart-disabled"

  CONTROL_USER=$(awk -F= '$1 == "CONTROL_USER" { count++; value = $2 } END { if (count != 1 || value == "") exit 1; print value }' "$GENERATED_ROOT/deployment.env")
  INSPECT_USER=$(awk -F= '$1 == "INSPECT_USER" { count++; value = $2 } END { if (count != 1 || value == "") exit 1; print value }' "$GENERATED_ROOT/deployment.env")
  CONTROL_PASSWORD=$(sed -n '1p' "$GENERATED_ROOT/secrets/ari-control.password")
  INSPECT_PASSWORD=$(sed -n '1p' "$GENERATED_ROOT/secrets/ari-inspect.password")
  {
    printf '%s\n' 'silent' 'show-error' 'max-time = 5' 'connect-timeout = 2' 'proto = "=http"' 'noproxy = "*"' 'proxy = ""'
    printf 'user = "%s:%s"\n' "$CONTROL_USER" "$CONTROL_PASSWORD"
  } >"$TEST_ROOT/control-curl.conf"
  {
    printf '%s\n' 'silent' 'show-error' 'max-time = 5' 'connect-timeout = 2' 'proto = "=http"' 'noproxy = "*"' 'proxy = ""'
    printf 'user = "%s:%s"\n' "$INSPECT_USER" "$INSPECT_PASSWORD"
  } >"$TEST_ROOT/inspect-curl.conf"
  CONTROL_PASSWORD=
  INSPECT_PASSWORD=

  control_status=$(curl -q --config "$TEST_ROOT/control-curl.conf" --output "$TEST_ROOT/control-info.json" --write-out '%{http_code}' http://127.0.0.1:28088/ari/asterisk/info)
  inspect_status=$(curl -q --config "$TEST_ROOT/inspect-curl.conf" --output "$TEST_ROOT/inspect-info.json" --write-out '%{http_code}' http://127.0.0.1:28088/ari/asterisk/info)
  [ "$control_status" = 200 ] || fail "control ARI user cannot read PBX identity"
  [ "$inspect_status" = 200 ] || fail "recovery ARI user cannot read PBX identity"
  cmp -s "$TEST_ROOT/control-info.json" "$TEST_ROOT/inspect-info.json" || fail "control and recovery ARI users did not reach the same PBX identity"
  EXPECTED_VERSION=$(awk -F= '$1 == "ASTERISK_VERSION" { count++; value = $2 } END { if (count != 1 || value == "") exit 1; print value }' "$GENERATED_ROOT/deployment.env")
  EXPECTED_ENTITY=$(awk -F= '$1 == "ENTITY_ID" { count++; value = $2 } END { if (count != 1 || value == "") exit 1; print value }' "$GENERATED_ROOT/deployment.env")
  ACTUAL_VERSION=$(sed -nE 's/.*"version"[[:space:]]*:[[:space:]]*"([^"[:space:]]+)".*/\1/p' "$TEST_ROOT/control-info.json")
  ACTUAL_ENTITY=$(sed -nE 's/.*"entity_id"[[:space:]]*:[[:space:]]*"([^"[:space:]]+)".*/\1/p' "$TEST_ROOT/control-info.json")
  [ "$ACTUAL_VERSION" = "$EXPECTED_VERSION" ] || fail "ARI runtime version differs from the generated deployment identity"
  [ "$ACTUAL_ENTITY" = "$EXPECTED_ENTITY" ] || fail "ARI runtime entity differs from the generated deployment identity"

  channels_status=$(curl -q --config "$TEST_ROOT/inspect-curl.conf" --output "$TEST_ROOT/channels.json" --write-out '%{http_code}' http://127.0.0.1:28088/ari/channels)
  [ "$channels_status" = 200 ] || fail "recovery ARI user cannot inspect channels"
  [ "$(tr -d '[:space:]' <"$TEST_ROOT/channels.json")" = '[]' ] || fail "disposable PBX did not start with an empty channel set"

  inspect_post_status=$(curl -q --config "$TEST_ROOT/inspect-curl.conf" --request POST --header 'Content-Type: application/json' --data '{}' --output "$TEST_ROOT/inspect-post.json" --write-out '%{http_code}' 'http://127.0.0.1:28088/ari/events/user/djonehub_read_only_probe?application=maccellular_voice')
  [ "$inspect_post_status" = 403 ] || fail "recovery ARI user is not enforced read-only"
  pass "control and recovery ARI identities are separated and recovery POST is forbidden"

  EXPECTED_APPLICATION=$(awk -F= '$1 == "APPLICATION" { count++; value = $2 } END { if (count != 1 || value == "") exit 1; print value }' "$GENERATED_ROOT/deployment.env")
  MACCELLULAR_ASTERISK_INTEGRATION_URL=http://127.0.0.1:28088/ari \
    MACCELLULAR_ASTERISK_INTEGRATION_USERNAME="$CONTROL_USER" \
    MACCELLULAR_ASTERISK_INTEGRATION_PASSWORD_FILE="$GENERATED_ROOT/secrets/ari-control.password" \
    MACCELLULAR_ASTERISK_INTEGRATION_VERSION="$EXPECTED_VERSION" \
    MACCELLULAR_ASTERISK_INTEGRATION_ENTITY_ID="$EXPECTED_ENTITY" \
    MACCELLULAR_ASTERISK_INTEGRATION_APPLICATION="$EXPECTED_APPLICATION" \
    "$GO_BIN" test -mod=readonly -tags=asterisk_integration -run '^TestAsterisk22RESTOverEventWebSocketLive$' \
      -count=1 -timeout=2m ./internal/sipgateway/asterisk >"$TEST_ROOT/ari-rest-ws-test.log"
  pass "real Asterisk RESTRequest/RESTResponse stays on the verified event WebSocket"

  docker logs "$container_id" >"$TEST_ROOT/runtime.log" 2>&1
  KNOWN_STASIS_DIAGNOSTIC="Could not find option 'minimum_size' with type 'threadpool' in module 'stasis'"
  [ "$(grep -Fc "$KNOWN_STASIS_DIAGNOSTIC" "$TEST_ROOT/runtime.log")" -eq 1 ] || fail "pinned Asterisk Stasis registration diagnostic changed"
  grep -Fv "$KNOWN_STASIS_DIAGNOSTIC" "$TEST_ROOT/runtime.log" >"$TEST_ROOT/runtime-filtered.log"
  if grep -Eq 'ERROR|WARNING|Could not find option|Unable to load|required load declined' "$TEST_ROOT/runtime-filtered.log"; then
    grep -En 'ERROR|WARNING|Could not find option|Unable to load|required load declined' "$TEST_ROOT/runtime-filtered.log" >&2 || true
    fail "runtime log contains a configuration or module error"
  fi
  grep -Fq 'Asterisk Ready.' "$TEST_ROOT/runtime.log" || fail "runtime log lacks the ready marker"
  pass "runtime reaches ready state with only the exact pinned upstream Stasis registration diagnostic"

  # Run negative load probes only after accepting the clean startup log: the
  # expected file-not-found diagnostics from these probes are not startup
  # configuration failures.
  for module in \
    app_originate.so app_system.so \
    res_pjsip_endpoint_identifier_anonymous.so \
    res_pjsip_endpoint_identifier_user.so \
    res_pjsip_outbound_registration.so; do
    load_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx "module load $module" 2>&1 || true)
    printf '%s\n' "$load_output" | grep -Eq 'Unable to load module|Error loading module|No such (file|module)' || \
      fail "unapproved module load did not fail explicitly: $module"
    module_output=$(compose exec -T asterisk /usr/sbin/asterisk -rx "module show like $module")
    if printf '%s\n' "$module_output" | grep -Fq "$module"; then
      fail "unapproved module became loaded after the negative probe: $module"
    fi
  done
  pass "unapproved module load attempts fail and remain unloaded"

  printf 'Asterisk 22 live integration smoke passed: %s\n' "$PASSED"
}

case "$MODE" in
  --static) run_static ;;
  --live) run_live ;;
  -h|--help) usage ;;
  *) usage >&2; exit 64 ;;
esac
