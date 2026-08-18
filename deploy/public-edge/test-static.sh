#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
GENERATE="$SCRIPT_DIR/generate-config.sh"
VERIFY="$SCRIPT_DIR/verify-config.sh"
DEPLOY="$SCRIPT_DIR/deploy.sh"
PASS_COUNT=0
TMP_ROOT=""

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  printf 'ok %d - %s\n' "$PASS_COUNT" "$1"
}

cleanup() {
  if [ -n "$TMP_ROOT" ]; then
    case "$TMP_ROOT" in
      */dji4g-public-edge-test.*)
        chmod -R u+rwX "$TMP_ROOT" 2>/dev/null || true
        rm -rf -- "$TMP_ROOT"
        ;;
    esac
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

expect_fail() {
  local label=$1
  shift
  if "$@" >/dev/null 2>&1; then
    fail "$label unexpectedly succeeded"
  fi
  pass "$label"
}

command -v openssl >/dev/null 2>&1 || fail "openssl is required"

TMP_BASE=${TMPDIR:-/tmp}
TMP_CREATED=$(mktemp -d "$TMP_BASE/dji4g-public-edge-test.XXXXXX")
TMP_ROOT=$(CDPATH= cd -- "$TMP_CREATED" && pwd -P)
INPUT="$TMP_ROOT/input"
mkdir "$INPUT"
chmod 700 "$INPUT"

OPENSSL_CONFIG="$INPUT/openssl.cnf"
printf '%s\n' \
  '[req]' \
  'distinguished_name=dn' \
  'x509_extensions=extensions' \
  'prompt=no' \
  '[dn]' \
  'CN=turn.example.com' \
  '[extensions]' \
  'subjectAltName=DNS:turn.example.com' \
  'basicConstraints=critical,CA:FALSE' \
  'keyUsage=critical,digitalSignature,keyEncipherment' \
  'extendedKeyUsage=serverAuth' \
  >"$OPENSSL_CONFIG"

CERT="$INPUT/turn-cert.pem"
KEY="$INPUT/turn-key.pem"
OTHER_KEY="$INPUT/other-key.pem"
GATEWAY_PRIVATE_KEY="$INPUT/gateway-private-key.pem"
GATEWAY_PUBLIC_KEY="$INPUT/gateway-public-key.pem"
BAD_GATEWAY_PUBLIC_KEY="$INPUT/bad-gateway-public-key.pem"
COMPRESSED_GATEWAY_PUBLIC_KEY="$INPUT/compressed-gateway-public-key.pem"
TRAILING_GATEWAY_PUBLIC_KEY="$INPUT/trailing-gateway-public-key.pem"
OVERSIZE_GATEWAY_PUBLIC_KEY="$INPUT/oversize-gateway-public-key.pem"
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 30 \
  -config "$OPENSSL_CONFIG" -keyout "$KEY" -out "$CERT" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$OTHER_KEY" >/dev/null 2>&1
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
  -out "$GATEWAY_PRIVATE_KEY" >/dev/null 2>&1
openssl pkey -in "$GATEWAY_PRIVATE_KEY" -pubout -out "$GATEWAY_PUBLIC_KEY" >/dev/null 2>&1
openssl ec -in "$GATEWAY_PRIVATE_KEY" -pubout -conv_form compressed \
  -out "$COMPRESSED_GATEWAY_PUBLIC_KEY" >/dev/null 2>&1
openssl pkey -in "$OTHER_KEY" -pubout -out "$BAD_GATEWAY_PUBLIC_KEY" >/dev/null 2>&1
cp "$GATEWAY_PUBLIC_KEY" "$TRAILING_GATEWAY_PUBLIC_KEY"
printf '%s\n' 'unapproved-trailing-data' >>"$TRAILING_GATEWAY_PUBLIC_KEY"
cp "$GATEWAY_PUBLIC_KEY" "$OVERSIZE_GATEWAY_PUBLIC_KEY"
printf '%9000s' '' >>"$OVERSIZE_GATEWAY_PUBLIC_KEY"

TOKEN="$INPUT/cloudflare.token"
AUTH="$INPUT/turn-auth.secret"
BAD_MODE_TOKEN="$INPUT/cloudflare-bad-mode.token"
BAD_NUL_TOKEN="$INPUT/cloudflare-nul.token"
BAD_LINES_AUTH="$INPUT/turn-auth-lines.secret"
TOKEN_LINK="$INPUT/cloudflare-link.token"
ALLOWED_EMAILS="$INPUT/access-allowed-emails"
BAD_EMAILS="$INPUT/access-bad-emails"
BAD_SYNTAX_EMAILS="$INPUT/access-bad-syntax-emails"
BAD_MODE_EMAILS="$INPUT/access-bad-mode-emails"
printf '%s\n' 'eyJhIjoiZXhhbXBsZSIsInQiOiJzdGF0aWMtdGVzdC10b2tlbiJ9.abc_DEF-123=' >"$TOKEN"
printf '%s\n' 'MDEyMzQ1Njc4OWFiY2RlZkFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFla' >"$AUTH"
printf '%s\n' 'owner@example.com' >"$ALLOWED_EMAILS"
printf '%s\n%s\n' 'owner@example.com' 'owner@example.com' >"$BAD_EMAILS"
printf '%s\n' '.owner@example.com' >"$BAD_SYNTAX_EMAILS"
cp "$ALLOWED_EMAILS" "$BAD_MODE_EMAILS"
cp "$TOKEN" "$BAD_MODE_TOKEN"
printf 'eyJhIjoiZXhhbXBsZSIsInQiOiJzdGF0aWMtdGVzdC10b2tlbiJ9\0suffix\n' >"$BAD_NUL_TOKEN"
printf '%s\n%s\n' \
  'MDEyMzQ1Njc4OWFiY2RlZkFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFla' \
  'log-file=/tmp/injected' >"$BAD_LINES_AUTH"
ln -s "$TOKEN" "$TOKEN_LINK"
chmod 600 \
  "$CERT" "$KEY" "$OTHER_KEY" \
  "$GATEWAY_PRIVATE_KEY" "$GATEWAY_PUBLIC_KEY" "$BAD_GATEWAY_PUBLIC_KEY" \
  "$COMPRESSED_GATEWAY_PUBLIC_KEY" \
  "$TRAILING_GATEWAY_PUBLIC_KEY" "$OVERSIZE_GATEWAY_PUBLIC_KEY" \
  "$TOKEN" "$AUTH" "$BAD_NUL_TOKEN" "$BAD_LINES_AUTH" \
  "$ALLOWED_EMAILS" "$BAD_EMAILS" "$BAD_SYNTAX_EMAILS"
chmod 644 "$BAD_MODE_TOKEN" "$BAD_MODE_EMAILS"

DIGEST_ONE=1111111111111111111111111111111111111111111111111111111111111111
DIGEST_TWO=2222222222222222222222222222222222222222222222222222222222222222
DIGEST_THREE=3333333333333333333333333333333333333333333333333333333333333333
EDGE_IMAGE="ghcr.io/example/maccellular-edge@sha256:$DIGEST_ONE"
CLOUDFLARED_IMAGE="cloudflare/cloudflared@sha256:e39ee8da81ad5e05d77f38d2f51c60ca51bf2a8450ac3abab50c17fdb91d91bf"
COTURN_IMAGE="coturn/coturn@sha256:$DIGEST_THREE"
EDGE_GATEWAY_ID=home-gateway-1
ACCESS_TEAM_DOMAIN=unit-test.cloudflareaccess.com
ACCESS_AUDIENCE=unit_test_audience

invoke_generate_explicit() {
  local output=$1 edge_image=$2 token_file=$3 auth_file=$4 key_file=$5
  local gateway_id=$6 gateway_key=$7 team_domain=$8 audience=$9 emails_file=${10}
  shift 10
  "$GENERATE" \
    --output "$output" \
    --edge-image "$edge_image" \
    --cloudflared-image "$CLOUDFLARED_IMAGE" \
    --coturn-image "$COTURN_IMAGE" \
    --turn-public-ip 8.8.8.8 \
    --turn-relay-ip 172.30.247.2 \
    --cloudflare-token-file "$token_file" \
    --turn-auth-secret-file "$auth_file" \
    --turn-tls-cert-file "$CERT" \
    --turn-tls-key-file "$key_file" \
    --edge-gateway-id "$gateway_id" \
    --gateway-public-key-file "$gateway_key" \
    --access-team-domain "$team_domain" \
    --access-audience "$audience" \
    --access-allowed-emails-file "$emails_file" \
    --profile edge,turn \
    "$@"
}

invoke_generate_edge() {
  local output=$1 edge_image=$2 cloudflared_image=$3 token_file=$4
  "$GENERATE" \
    --output "$output" \
    --edge-image "$edge_image" \
    --cloudflared-image "$cloudflared_image" \
    --cloudflare-token-file "$token_file" \
    --edge-gateway-id "$EDGE_GATEWAY_ID" \
    --gateway-public-key-file "$GATEWAY_PUBLIC_KEY" \
    --access-team-domain "$ACCESS_TEAM_DOMAIN" \
    --access-audience "$ACCESS_AUDIENCE" \
    --access-allowed-emails-file "$ALLOWED_EMAILS" \
    --profile edge \
    "${@:5}"
}

invoke_generate() {
  local output=$1 edge_image=$2 token_file=$3 auth_file=$4 key_file=$5
  shift 5
  invoke_generate_explicit \
    "$output" "$edge_image" "$token_file" "$auth_file" "$key_file" \
    "$EDGE_GATEWAY_ID" "$GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$ALLOWED_EMAILS" "$@"
}

"$VERIFY" --compose-only --quiet
pass "checked-in Compose and templates pass the static policy"

BAD_COMPOSE_BIN="$TMP_ROOT/bad-compose-bin"
mkdir "$BAD_COMPOSE_BIN"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'while [ "${1:-}" = --config ] || [ "${1:-}" = --context ]; do shift 2; done' \
  'if [ "${1:-}" = compose ] && [ "${2:-}" = version ]; then printf "%s\\n" 2.39.0; exit 0; fi' \
  'exit 64' >"$BAD_COMPOSE_BIN/docker"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'if [ "${1:-}" = version ]; then printf "%s\\n" 2.39.0; exit 0; fi' \
  'exit 64' >"$BAD_COMPOSE_BIN/docker-compose"
chmod 700 "$BAD_COMPOSE_BIN/docker" "$BAD_COMPOSE_BIN/docker-compose"
expect_fail "wrong-version Compose parser is rejected before normalization" \
  env PATH="$BAD_COMPOSE_BIN:$PATH" "$VERIFY" --compose-mode direct-docker --compose-only --quiet

BYPASS_COMPOSE="$TMP_ROOT/compose-bypass.yaml"
awk '
  /^networks:$/ {
    print "  bypass: {image: alpine:latest, privileged: true, ports: [\"18081:18081\"]}"
  }
  { print }
' "$SCRIPT_DIR/compose.yaml" >"$BYPASS_COMPOSE"
expect_fail "flow-style bypass service is rejected after real Compose expansion" \
  "$VERIFY" --compose-only --compose "$BYPASS_COMPOSE" --quiet

ALT_PORT_COMPOSE="$TMP_ROOT/compose-alternate-port.yaml"
sed 's/published: "443"/published: "444"/' "$SCRIPT_DIR/compose.yaml" >"$ALT_PORT_COMPOSE"
expect_fail "alternate TURN port 444 is rejected" \
  "$VERIFY" --compose-only --compose "$ALT_PORT_COMPOSE" --quiet

ALT_STUN_COMPOSE="$TMP_ROOT/compose-alternate-stun-port.yaml"
sed 's/published: "3478"/published: "3479"/' "$SCRIPT_DIR/compose.yaml" >"$ALT_STUN_COMPOSE"
expect_fail "alternate TURN port 3479 is rejected" \
  "$VERIFY" --compose-only --compose "$ALT_STUN_COMPOSE" --quiet

AUTO_CREATE_BIND_COMPOSE="$TMP_ROOT/compose-auto-create-bind.yaml"
sed '/^[[:space:]]*bind:$/ { N; d; }' "$SCRIPT_DIR/compose.yaml" >"$AUTO_CREATE_BIND_COMPOSE"
expect_fail "bind mounts without create_host_path false are rejected before Compose mutation" \
  "$VERIFY" --compose-only --compose "$AUTO_CREATE_BIND_COMPOSE" --quiet

VALID_ROOT="$TMP_ROOT/runtime-valid"
invoke_generate "$VALID_ROOT" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" --apply >/dev/null
"$VERIFY" --quiet "$VALID_ROOT"
pass "valid inputs generate and verify atomically"

grep -Fqx 'no-rfc5780' "$VALID_ROOT/config/turnserver.conf" ||
  fail "generated TURN config did not disable RFC5780 alternate listeners"
if grep -Eq '(^|[^0-9])(3479|444)([^0-9]|$)' "$VALID_ROOT/config/turnserver.conf"; then
  fail "generated TURN config contains an alternate listener port"
fi
pass "generated TURN config pins no-rfc5780 and contains no 3479/444 listener"

"$DEPLOY" --runtime-root "$VALID_ROOT" check >/dev/null
pass "deployment wrapper re-verifies the canonical runtime without daemon access"
expect_fail "deployment check reaches the pinned Compose parser version gate" \
  env PATH="$BAD_COMPOSE_BIN:$PATH" "$DEPLOY" --runtime-root "$VALID_ROOT" check

LOCAL_EDGE_IMAGE="sha256:$(printf '4%.0s' {1..64})"
EDGE_RUNTIME_PARENT="$TMP_ROOT/runtime-parent"
mkdir "$EDGE_RUNTIME_PARENT"
chmod 700 "$EDGE_RUNTIME_PARENT"
EDGE_ONLY_ROOT="$EDGE_RUNTIME_PARENT/runtime-edge-only"
invoke_generate_edge "$EDGE_ONLY_ROOT" "$LOCAL_EDGE_IMAGE" "$CLOUDFLARED_IMAGE" "$TOKEN" --apply >/dev/null
"$VERIFY" --quiet "$EDGE_ONLY_ROOT"
"$DEPLOY" --runtime-root "$EDGE_ONLY_ROOT" check >/dev/null
[ ! -e "$EDGE_ONLY_ROOT/config/turnserver.conf" ] &&
  [ ! -e "$EDGE_ONLY_ROOT/secrets/turn-auth-secret" ] &&
  [ ! -e "$EDGE_ONLY_ROOT/secrets/turn-tls-cert.pem" ] ||
  fail "edge-only runtime contains TURN inputs"
pass "edge-only runtime accepts exact local image ID and contains no TURN material"

GENERATOR_SIGNAL_DIR="$TMP_ROOT/generator-signal-fixture"
mkdir "$GENERATOR_SIGNAL_DIR" "$GENERATOR_SIGNAL_DIR/templates" "$GENERATOR_SIGNAL_DIR/image"
cp "$GENERATE" "$GENERATOR_SIGNAL_DIR/generate-config.sh"
cp "$SCRIPT_DIR/image/atomic-commit.py" "$GENERATOR_SIGNAL_DIR/image/atomic-commit.py"
cp "$SCRIPT_DIR/templates/edge.env.in" "$SCRIPT_DIR/templates/turnserver.conf.in" \
  "$GENERATOR_SIGNAL_DIR/templates/"
cat >"$GENERATOR_SIGNAL_DIR/verify-config.sh" <<'SH'
#!/usr/bin/env bash
set -eu
fixture_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
count=0
[ ! -f "$fixture_dir/verify-count" ] || count=$(tr -d '\n' <"$fixture_dir/verify-count")
count=$((count + 1));printf '%s\n' "$count" >"$fixture_dir/verify-count"
if [ "$count" -eq 2 ]; then
  : >"$fixture_dir/stage-verification-started"
  sleep 1
fi
SH
chmod 700 "$GENERATOR_SIGNAL_DIR/generate-config.sh" "$GENERATOR_SIGNAL_DIR/verify-config.sh" \
  "$GENERATOR_SIGNAL_DIR/image/atomic-commit.py"
GENERATOR_SIGNAL_OUTPUT="$TMP_ROOT/runtime-generator-signal"
set +e
"$GENERATOR_SIGNAL_DIR/generate-config.sh" \
  --output "$GENERATOR_SIGNAL_OUTPUT" --edge-image "$LOCAL_EDGE_IMAGE" \
  --cloudflared-image "$CLOUDFLARED_IMAGE" --cloudflare-token-file "$TOKEN" \
  --edge-gateway-id "$EDGE_GATEWAY_ID" --gateway-public-key-file "$GATEWAY_PUBLIC_KEY" \
  --access-team-domain "$ACCESS_TEAM_DOMAIN" --access-audience "$ACCESS_AUDIENCE" \
  --access-allowed-emails-file "$ALLOWED_EMAILS" --profile edge --apply \
  >"$TMP_ROOT/generator-signal.log" 2>&1 &
GENERATOR_SIGNAL_PID=$!
set -e
for _ in $(seq 1 100); do
  [ -f "$GENERATOR_SIGNAL_DIR/stage-verification-started" ] && break
  sleep 0.02
done
[ -f "$GENERATOR_SIGNAL_DIR/stage-verification-started" ] || fail "generator signal fixture never reached staged verification"
kill -TERM "$GENERATOR_SIGNAL_PID"
set +e;wait "$GENERATOR_SIGNAL_PID";GENERATOR_SIGNAL_STATUS=$?;set -e
[ "$GENERATOR_SIGNAL_STATUS" -eq 143 ] || fail "generator did not preserve SIGTERM status 143"
[ ! -e "$GENERATOR_SIGNAL_OUTPUT" ] || fail "cancelled generator atomically published a runtime"
if find "$TMP_ROOT" -maxdepth 1 -name '.dji4g-public-edge.*' -print -quit | grep -q .; then
  fail "cancelled generator left its staged runtime behind"
fi
[ "$(tr -d '\n' <"$GENERATOR_SIGNAL_DIR/verify-count")" = 2 ] || fail "cancelled generator continued beyond staged verification"
pass "SIGTERM cancels generation before atomic runtime publication and removes the stage"

rm -f "$GENERATOR_SIGNAL_DIR/verify-count" "$GENERATOR_SIGNAL_DIR/stage-verification-started"
GENERATOR_RACE_OUTPUT="$TMP_ROOT/runtime-generator-race"
set +e
"$GENERATOR_SIGNAL_DIR/generate-config.sh" \
  --output "$GENERATOR_RACE_OUTPUT" --edge-image "$LOCAL_EDGE_IMAGE" \
  --cloudflared-image "$CLOUDFLARED_IMAGE" --cloudflare-token-file "$TOKEN" \
  --edge-gateway-id "$EDGE_GATEWAY_ID" --gateway-public-key-file "$GATEWAY_PUBLIC_KEY" \
  --access-team-domain "$ACCESS_TEAM_DOMAIN" --access-audience "$ACCESS_AUDIENCE" \
  --access-allowed-emails-file "$ALLOWED_EMAILS" --profile edge --apply \
  >"$TMP_ROOT/generator-race.log" 2>&1 &
GENERATOR_RACE_PID=$!
set -e
for _ in $(seq 1 100); do
  [ -f "$GENERATOR_SIGNAL_DIR/stage-verification-started" ] && break
  sleep 0.02
done
[ -f "$GENERATOR_SIGNAL_DIR/stage-verification-started" ] || fail "generator race fixture never reached staged verification"
mkdir "$GENERATOR_RACE_OUTPUT"
set +e;wait "$GENERATOR_RACE_PID";GENERATOR_RACE_STATUS=$?;set -e
[ "$GENERATOR_RACE_STATUS" -ne 0 ] || fail "generator overwrote a concurrently-created output"
[ -d "$GENERATOR_RACE_OUTPUT" ] && [ -z "$(find "$GENERATOR_RACE_OUTPUT" -mindepth 1 -print -quit)" ] ||
  fail "generator nested or moved its sensitive stage into the concurrent output"
if find "$TMP_ROOT" -maxdepth 1 -name '.dji4g-public-edge.*' -print -quit | grep -q .; then
  fail "failed generator no-replace commit left its staged runtime behind"
fi
pass "runtime generation atomically rejects a concurrently-created output path"

mkdir "$EDGE_ONLY_ROOT/action-evidence-parent"
chmod 700 "$EDGE_ONLY_ROOT/action-evidence-parent"
expect_fail "state evidence inside the runtime root is rejected before daemon access" \
  "$DEPLOY" --runtime-root "$EDGE_ONLY_ROOT" \
    --state-evidence "$EDGE_ONLY_ROOT/action-evidence-parent/action-1" stop-edge
rmdir "$EDGE_ONLY_ROOT/action-evidence-parent"
expect_fail "state evidence parent containing the runtime root is rejected" \
  "$DEPLOY" --runtime-root "$EDGE_ONLY_ROOT" --state-evidence "$TMP_ROOT/action-2" stop-edge

NONPRIVATE_EVIDENCE_PARENT="$TMP_ROOT/nonprivate-evidence"
mkdir "$NONPRIVATE_EVIDENCE_PARENT"
chmod 755 "$NONPRIVATE_EVIDENCE_PARENT"
expect_fail "non-private state evidence parent is rejected" \
  "$DEPLOY" --runtime-root "$EDGE_ONLY_ROOT" \
    --state-evidence "$NONPRIVATE_EVIDENCE_PARENT/action-3" stop-edge

ACTION_PARENT="$TMP_ROOT/action-evidence"
FAKE_BIN="$TMP_ROOT/fake-bin"
SYNTHETIC_IMAGE_EVIDENCE="$TMP_ROOT/synthetic-image-evidence"
mkdir "$ACTION_PARENT" "$FAKE_BIN" "$SYNTHETIC_IMAGE_EVIDENCE"
chmod 700 "$ACTION_PARENT" "$FAKE_BIN" "$SYNTHETIC_IMAGE_EVIDENCE"
REAL_PYTHON=$(command -v python3)
# Fault harness only: isolate the post-mutation state machine after the real
# verifier has already been covered by image/test-static.sh. deploy.sh itself
# has no test/bypass switch and always names the checked-in verifier.
if ORIGINAL_DOCKER=$(command -v docker 2>/dev/null) && "$ORIGINAL_DOCKER" compose version >/dev/null 2>&1; then
  printf '%s\n' "$ORIGINAL_DOCKER" >"$FAKE_BIN/compose-parser"
  printf '%s\n' docker-plugin >"$FAKE_BIN/compose-parser-kind"
else
  ORIGINAL_COMPOSE=$(command -v docker-compose)
  printf '%s\n' "$ORIGINAL_COMPOSE" >"$FAKE_BIN/compose-parser"
  printf '%s\n' standalone >"$FAKE_BIN/compose-parser-kind"
fi
printf '%s\n' '#!/usr/bin/env bash' \
  'case " $* " in' \
  '  *"/image/verify-evidence.py"*) exit 0 ;;' \
  '  *"/image/verify-source.py"*) exit 0 ;;' \
  'esac' \
  "exec \"$REAL_PYTHON\" \"\$@\"" >"$FAKE_BIN/python3"
FAKE_DOCKER_BODY='#!/usr/bin/env bash
set -eu
fake_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
via_sudo=0
if [ "${1:-}" = __sudo__ ]; then via_sudo=1; shift; fi
if [ "${1:-}" != --config ] || [ ! -d "${2:-}" ] || [ "${3:-}" != --context ] || [ "${4:-}" != default ]; then exit 77; fi
config_dir=$2;shift 4
[ -f "$config_dir/config.json" ] || exit 78
if [ "$via_sudo" -eq 1 ]; then
  [ "$(cat "$config_dir/config.json")" = "{}" ] || exit 78
else
  grep -Fq cliPluginsExtraDirs "$config_dir/config.json" || exit 78
fi
printf "%s:%s\\n" "$via_sudo" "$*" >>"$fake_dir/daemon-calls"
edge=$(tr -d "\\n" <"$fake_dir/edge-image")
cloud=$(tr -d "\\n" <"$fake_dir/cloud-image")
if [ "${1:-}" = compose ] && [ "${2:-}" = version ]; then
  if [ "$via_sudo" -eq 1 ]; then printf "%s\\n" "2.40.3"; else printf "%s\\n" "9.9.9"; fi
  exit 0
fi
for name in DOCKER_HOST DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH COMPOSE_FILE COMPOSE_PROJECT_NAME; do
  if [ -n "${!name+x}" ]; then printf "%s\\n" "$name" >>"$fake_dir/ambient-leak"; exit 78; fi
done
if [ -n "${DOCKER_CONTEXT+x}" ] && [ "$DOCKER_CONTEXT" != default ]; then printf "%s\\n" DOCKER_CONTEXT >>"$fake_dir/ambient-leak"; exit 78; fi
if [ -n "${DOCKER_CONFIG+x}" ] && [ "$DOCKER_CONFIG" != "$config_dir" ]; then printf "%s\\n" DOCKER_CONFIG >>"$fake_dir/ambient-leak"; exit 78; fi
if [ "${1:-}" = info ]; then
  [ "$via_sudo" -eq 1 ] || exit 1
  case " $* " in
    *"{{json .DriverStatus}}"*) printf "%s\\n" "[[\"driver-type\",\"io.containerd.snapshotter.v1\"]]" ;;
    *"{{json .SecurityOptions}}"*) printf "%s\\n" "[\"name=apparmor\",\"name=seccomp,profile=builtin\",\"name=cgroupns\"]" ;;
    *"{{.OSType}}/{{.Architecture}}"*) printf "%s\\n" "linux/amd64" ;;
  esac
  exit 0
fi
if [ "$via_sudo" -ne 1 ]; then printf "%s\\n" "$*" >>"$fake_dir/direct-daemon-use"; exit 79; fi
if [ "${1:-}" = version ]; then printf "%s\\n" "29.1.3"; exit 0; fi
if [ "${1:-}" = compose ] && [[ " $* " = *" config --no-env-resolution --format json "* ]]; then
  parser=$(tr -d "\\n" <"$fake_dir/compose-parser");kind=$(tr -d "\\n" <"$fake_dir/compose-parser-kind");shift
  if [ "$kind" = docker-plugin ]; then exec "$parser" --config "$config_dir" --context default compose "$@"; else exec "$parser" "$@"; fi
fi
if [ "${1:-}" = ps ]; then
  count=0
  [ ! -f "$fake_dir/ps-count" ] || count=$(tr -d "\\n" <"$fake_dir/ps-count")
  count=$((count + 1)); printf "%s\\n" "$count" >"$fake_dir/ps-count"
  mode=$(tr -d "\\n" <"$fake_dir/ps-mode")
  if { { [ "$mode" = fail-once ] || [ "$mode" = aba-fail-once ]; } && [ "$count" -eq 2 ]; } || \
      { [ "$mode" = fail-after-pre ] && [ "$count" -gt 1 ]; }; then exit 9; fi
  exit 0
fi
if [ "${1:-}" = network ] && [ "${2:-}" = ls ]; then exit 0; fi
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
  ref=${!#}
  case " $* " in
    *"{{.Id}}"*) if [ "$ref" = "$edge" ]; then printf "%s\\n" "$edge"; else printf "sha256:%s\\n" "${ref##*@sha256:}"; fi ;;
    *"{{.Os}}/{{.Architecture}}"*) printf "%s\\n" "linux/amd64" ;;
    *"{{range .RepoDigests}}"*) printf "%s\\n" "$ref" ;;
    *) exit 64 ;;
  esac
  exit 0
fi
if [ "${1:-}" = compose ]; then
  printf "%s\\n" "$*" >>"$fake_dir/compose-calls"
  mode=$(tr -d "\\n" <"$fake_dir/ps-mode")
  case " $* " in
    *" logs "*)
      if [ "$mode" = redact-fragment ]; then
        printf "%s\\n" "truncated token follows:"
        head -c 20 "$fake_dir/cloudflare-token";printf "\\n-----BEGIN PRIVATE KEY-----\\npartial-key-material\\n"
      else
        printf "%s\\n" "synthetic bounded log"
      fi
      exit 0 ;;
    *" up -d "*)
      if [ "$mode" = aba-fail-once ]; then
        env_file="";want_env=0
        for item in "$@";do
          if [ "$want_env" -eq 1 ];then env_file=$item;want_env=0
          elif [ "$item" = --env-file ];then want_env=1
          fi
        done
        original_root=$(tr -d "\\n" <"$fake_dir/original-runtime-root")
        snapshot_root=${env_file%/compose.env}
        [ -n "$env_file" ] && [ "$snapshot_root" != "$original_root" ] || exit 81
        grep -Fqx "DJI4G_PUBLIC_EDGE_ROOT=$snapshot_root" "$env_file" || exit 82
        grep -Fqx "DJI4G_EDGE_ENV_FILE=$snapshot_root/config/edge.env" "$env_file" || exit 83
        before=$(shasum -a 256 "$snapshot_root/config/access-allowed-emails" | cut -d " " -f 1)
        cp "$original_root/config/access-allowed-emails" "$fake_dir/original-allowlist-a"
        printf "%s\\n" "aba-b@example.invalid" >"$original_root/config/.access-allowed-emails.b"
        chmod 600 "$original_root/config/.access-allowed-emails.b"
        mv "$original_root/config/.access-allowed-emails.b" "$original_root/config/access-allowed-emails"
        cp "$fake_dir/original-allowlist-a" "$original_root/config/.access-allowed-emails.a"
        chmod 600 "$original_root/config/.access-allowed-emails.a"
        mv "$original_root/config/.access-allowed-emails.a" "$original_root/config/access-allowed-emails"
        after=$(shasum -a 256 "$snapshot_root/config/access-allowed-emails" | cut -d " " -f 1)
        [ "$before" = "$after" ] || exit 84
        printf "%s\\n" "$snapshot_root" >"$fake_dir/aba-snapshot-consumed"
      fi
      [ "$mode" != redact-fragment ] || exit 8
      exit 0 ;;
    *" stop "*|*" rm -f -s -v "*) exit 0 ;;
  esac
fi
exit 64
'
printf '%s' "$FAKE_DOCKER_BODY" >"$FAKE_BIN/docker"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -eu' \
  'fake_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)' \
  '[ "${1:-}" = -n ] && [ "${2:-}" = docker ] || exit 64' \
  'shift 2' \
  'exec "$fake_dir/docker" __sudo__ "$@"' \
  >"$FAKE_BIN/sudo"
printf '%s\n' "$LOCAL_EDGE_IMAGE" >"$FAKE_BIN/edge-image"
printf '%s\n' "$CLOUDFLARED_IMAGE" >"$FAKE_BIN/cloud-image"
cp "$AUTH" "$FAKE_BIN/turn-secret"
cp "$TOKEN" "$FAKE_BIN/cloudflare-token"
cp "$KEY" "$FAKE_BIN/turn-key"
chmod 700 "$FAKE_BIN/docker" "$FAKE_BIN/python3" "$FAKE_BIN/sudo"
chmod 600 "$FAKE_BIN/turn-secret" "$FAKE_BIN/cloudflare-token" "$FAKE_BIN/turn-key"
POISON_HOME="$TMP_ROOT/poison-home"; mkdir -p "$POISON_HOME/.docker"
printf '%s\n' '{"currentContext":"attacker","proxies":{"default":{"httpProxy":"http://secret@attacker.invalid"}}}' \
  >"$POISON_HOME/.docker/config.json"

# Fault-only copied deployment tree. Production deploy.sh has no provenance
# bypass and will correctly reject mutations until Commit B supplies
# image/source.lock. The copied tree lets this suite exercise the later
# mutation/rollback/evidence machine without writing a lock into the workspace.
FAULT_REPO="$TMP_ROOT/fault-repo"
mkdir -p "$FAULT_REPO/deploy"
cp -R "$SCRIPT_DIR" "$FAULT_REPO/deploy/public-edge"
FAULT_PUBLIC="$FAULT_REPO/deploy/public-edge"
DEPLOY_FAULT="$FAULT_PUBLIC/deploy.sh"
printf '%s\n' 'FORMAT=1' >"$FAULT_PUBLIC/image/source.lock"
printf '%s\n' '{}' >"$SYNTHETIC_IMAGE_EVIDENCE/evidence.json"
# Fault-tree only: action evidence now replays source/image verification in a
# child Python process. Keep those two earlier gates isolated here; their real
# implementations are exhaustively covered by image/test-static.sh.
printf '%s\n' '#!/usr/bin/env python3' 'raise SystemExit(0)' \
  >"$FAULT_PUBLIC/image/verify-source.py"
printf '%s\n' '#!/usr/bin/env python3' 'raise SystemExit(0)' \
  >"$FAULT_PUBLIC/image/verify-evidence.py"
chmod 700 "$FAULT_PUBLIC/image/verify-source.py" "$FAULT_PUBLIC/image/verify-evidence.py"
PRODUCTION_COMPOSE=$(command -v docker-compose)
PRODUCTION_COMPOSE=$(CDPATH= cd -- "$(dirname -- "$PRODUCTION_COMPOSE")" && pwd -P)/$(basename -- "$PRODUCTION_COMPOSE")
PRODUCTION_COMPOSE_SHA=$(shasum -a 256 "$PRODUCTION_COMPOSE" | awk '{print $1}')
FAULT_ROOT_CONFIG="$TMP_ROOT/root-docker-config"
mkdir "$FAULT_ROOT_CONFIG";chmod 700 "$FAULT_ROOT_CONFIG"
cp "$SCRIPT_DIR/image/docker-cli-root-config.json" "$FAULT_ROOT_CONFIG/config.json"
chmod 644 "$FAULT_ROOT_CONFIG/config.json"
sed -i.bak \
  -e "s|^DOCKER_ROOT_CONFIG_DIR=.*|DOCKER_ROOT_CONFIG_DIR=$FAULT_ROOT_CONFIG|" \
  -e "s|^COMPOSE_PLUGIN_PATH=.*|COMPOSE_PLUGIN_PATH=$PRODUCTION_COMPOSE|" \
  -e "s|^COMPOSE_LINUX_AMD64_SHA256=.*|COMPOSE_LINUX_AMD64_SHA256=$PRODUCTION_COMPOSE_SHA|" \
  "$FAULT_PUBLIC/image/toolchain.lock"
rm "$FAULT_PUBLIC/image/toolchain.lock.bak"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s" "2.40.3+ds1-0ubuntu1~24.04.1"' >"$FAKE_BIN/dpkg-query"
chmod 700 "$FAKE_BIN/dpkg-query"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - \
  "$PRODUCTION_COMPOSE" "$FAULT_ROOT_CONFIG/config.json" "$FAKE_BIN/trusted-tool-paths" <<'PY'
import pathlib,sys
values=set()
for raw in sys.argv[1:3]:
    path=pathlib.Path(raw)
    values.update(str(value) for value in (path,*path.parents))
pathlib.Path(sys.argv[3]).write_text("\n".join(sorted(values))+"\n",encoding="utf-8")
PY
cat >"$FAKE_BIN/stat" <<'SH'
#!/usr/bin/env bash
set -eu
fake_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
target=${!#}
if grep -Fqx "$target" "$fake_dir/trusted-tool-paths";then
  case " $* " in
    *"%u"*) printf '%s\n' 0 ;;
    *"%Lp"*) case "$target" in */config.json) cat "$fake_dir/root-config-mode" ;; *) printf '%s\n' 755 ;; esac ;;
    *) exec /usr/bin/stat "$@" ;;
  esac
else
  exec /usr/bin/stat "$@"
fi
SH
chmod 700 "$FAKE_BIN/stat"
printf '%s\n' 644 >"$FAKE_BIN/root-config-mode"

mkdir "$FAULT_ROOT_CONFIG/cli-plugins"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "Docker Compose version v2.40.3"' \
  >"$FAULT_ROOT_CONFIG/cli-plugins/docker-compose"
chmod 700 "$FAULT_ROOT_CONFIG/cli-plugins/docker-compose"
SHADOW_EVIDENCE="$ACTION_PARENT/root-config-plugin-shadow"
expect_fail "sudo Docker refuses a root-config Compose plugin shadow before policy or mutation" \
  env PATH="$FAKE_BIN:$PATH" "$DEPLOY_FAULT" --runtime-root "$EDGE_ONLY_ROOT" \
    --state-evidence "$SHADOW_EVIDENCE" stop-edge
[ ! -e "$SHADOW_EVIDENCE" ] || fail "root-config plugin shadow produced action evidence"
if [ -f "$FAKE_BIN/compose-calls" ] && [ -s "$FAKE_BIN/compose-calls" ]; then
  fail "root-config plugin shadow reached Compose mutation"
fi
rm -rf "$FAULT_ROOT_CONFIG/cli-plugins"

printf '%s\n' 600 >"$FAKE_BIN/root-config-mode"
ROOT_CONFIG_MODE_EVIDENCE="$ACTION_PARENT/root-config-mode-0600"
expect_fail "unreadable root-only Docker config mode is rejected before daemon mutation" \
  env PATH="$FAKE_BIN:$PATH" "$DEPLOY_FAULT" --runtime-root "$EDGE_ONLY_ROOT" \
    --state-evidence "$ROOT_CONFIG_MODE_EVIDENCE" stop-edge
[ ! -e "$ROOT_CONFIG_MODE_EVIDENCE" ] || fail "wrong root config mode published action evidence"
printf '%s\n' 644 >"$FAKE_BIN/root-config-mode"
pass "sudo deployment accepts only the readable root-owned 0644 canonical Docker config"

printf '%s\n' "$EDGE_ONLY_ROOT" >"$FAKE_BIN/original-runtime-root"
printf '%s\n' aba-fail-once >"$FAKE_BIN/ps-mode"
ACTION_EVIDENCE_1="$ACTION_PARENT/start-edge-collection-failed"
if env HOME="$POISON_HOME" DOCKER_HOST=tcp://attacker.invalid:2376 DOCKER_CONTEXT=attacker \
    DOCKER_TLS=1 DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH=/attacker/certs \
    DOCKER_CONFIG=/attacker/docker-config COMPOSE_FILE=/attacker/compose.yaml \
    COMPOSE_PROJECT_NAME=attacker PATH="$FAKE_BIN:$PATH" \
    "$DEPLOY_FAULT" --runtime-root "$EDGE_ONLY_ROOT" \
    --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" --state-evidence "$ACTION_EVIDENCE_1" \
    start-edge >"$TMP_ROOT/fault-start-1.log" 2>&1; then
  sed -n '1,160p' "$TMP_ROOT/fault-start-1.log" >&2
  [ ! -f "$FAKE_BIN/compose-calls" ] || sed -n '1,160p' "$FAKE_BIN/compose-calls" >&2
  [ ! -f "$FAKE_BIN/ps-count" ] || sed -n '1,20p' "$FAKE_BIN/ps-count" >&2
  fail "post-mutation collection fault unexpectedly succeeded"
fi
if [ ! -d "$ACTION_EVIDENCE_1" ]; then
  sed -n '1,160p' "$TMP_ROOT/fault-start-1.log" >&2
  fail "post-mutation failure evidence was deleted"
fi
[ "$(stat -f '%Lp' "$ACTION_EVIDENCE_1" 2>/dev/null || stat -c '%a' "$ACTION_EVIDENCE_1")" = 700 ] ||
  fail "failure evidence directory is not mode 0700"
[ "$(stat -f '%Lp' "$ACTION_EVIDENCE_1/result.txt" 2>/dev/null || stat -c '%a' "$ACTION_EVIDENCE_1/result.txt")" = 600 ] ||
  fail "failure result is not mode 0600"
grep -Fqx 'status=failed' "$ACTION_EVIDENCE_1/result.txt" || fail "failure result status is absent"
grep -Fqx 'collection_status=incomplete' "$ACTION_EVIDENCE_1/result.txt" || fail "collection failure is not explicit"
grep -Fqx 'rollback_status=completed' "$ACTION_EVIDENCE_1/result.txt" || fail "verified rollback completion is absent"
grep -Fq ' stop --timeout 20 edge cloudflared' "$FAKE_BIN/compose-calls" || fail "edge rollback stop was not attempted"
grep -Fq ' rm -f -s -v edge cloudflared' "$FAKE_BIN/compose-calls" || fail "edge rollback removal was not attempted"
[ -f "$ACTION_EVIDENCE_1/runtime-inputs.pre.json" ] && \
  [ ! -L "$ACTION_EVIDENCE_1/runtime-inputs.pre.json" ] ||
  fail "pre-mutation runtime input anchor was not preserved through rollback"
[ "$(stat -f '%Lp' "$ACTION_EVIDENCE_1/runtime-inputs.pre.json" 2>/dev/null || stat -c '%a' "$ACTION_EVIDENCE_1/runtime-inputs.pre.json")" = 600 ] ||
  fail "pre-mutation runtime input anchor is not mode 0600"
pass "post-mutation evidence fault preserves the pre-anchor and completes verified edge rollback"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" verify \
  --evidence "$ACTION_EVIDENCE_1" --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" \
  --source-lock "$FAULT_PUBLIC/image/source.lock" \
  --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml" \
  --repo-root "$FAULT_REPO" >/dev/null
EDGE_SNAPSHOT_ROOT=$(PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$ACTION_EVIDENCE_1/action.json" <<'PY'
import json,pathlib,sys
print(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="ascii"))["runtime_root"])
PY
)
case "$EDGE_SNAPSHOT_ROOT" in "$EDGE_RUNTIME_PARENT/.runtime-edge-only.edge-snapshot."??????) ;; *)
  fail "start-edge action did not bind a random sibling snapshot path" ;;
esac
[ ! -e "$EDGE_SNAPSHOT_ROOT" ] && [ ! -L "$EDGE_SNAPSHOT_ROOT" ] ||
  fail "verified rollback did not remove its immutable runtime snapshot"
[ "$(tr -d '\n' <"$FAKE_BIN/aba-snapshot-consumed")" = "$EDGE_SNAPSHOT_ROOT" ] ||
  fail "Compose did not consume the action-bound immutable snapshot"
cmp "$EDGE_ONLY_ROOT/config/access-allowed-emails" "$FAKE_BIN/original-allowlist-a" >/dev/null ||
  fail "the A-to-B-to-A fixture did not restore the configured runtime input"
grep -Fq " --env-file $EDGE_SNAPSHOT_ROOT/compose.env " "$FAKE_BIN/compose-calls" ||
  fail "actual Compose mutation did not use the immutable snapshot env-file"
pass "original runtime A-to-B-to-A replacement cannot change the snapshot consumed by Compose"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - \
  "$ACTION_EVIDENCE_1/action.json" "$ACTION_EVIDENCE_1/runtime-inputs.pre.json" \
  "$EDGE_ONLY_ROOT" "$EDGE_SNAPSHOT_ROOT" "$ACTION_EVIDENCE_1" "$TOKEN" <<'PY'
import hashlib,json,pathlib,sys
value=json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="ascii"))
anchor=json.loads(pathlib.Path(sys.argv[2]).read_text(encoding="ascii"))
expected={"compose.env","config/edge.env","config/gateway-public-key.pem","config/access-allowed-emails"}
if set(value.get("runtime_inputs_sha256",{}))!=expected:raise SystemExit("runtime input digest set is incomplete")
if value["runtime_inputs_sha256"]!=anchor:raise SystemExit("manifest does not bind the pre-mutation anchor")
if value.get("configured_runtime_root")!=sys.argv[3] or value.get("runtime_root")!=sys.argv[4]:raise SystemExit("manifest does not bind configured/snapshot roots")
root=pathlib.Path(sys.argv[5])
files={
 "compose.env":"runtime-input-compose.env",
 "config/edge.env":"runtime-input-edge.env",
 "config/gateway-public-key.pem":"runtime-input-gateway-public-key.pem",
 "config/access-allowed-emails":"runtime-input-access-allowed-emails",
}
for logical,name in files.items():
 data=(root/name).read_bytes()
 if hashlib.sha256(data).hexdigest()!=anchor[logical]:raise SystemExit("recorded runtime bytes differ from anchor")
token=pathlib.Path(sys.argv[6]).read_bytes().rstrip(b"\n")
if any(token in path.read_bytes() for path in root.iterdir() if path.is_file()):
 raise SystemExit("Cloudflare token leaked into action evidence")
PY
# Reconstruct the exact recorded, non-secret snapshot bytes to exercise live
# snapshot-tamper rejection below. Production rollback removed its own snapshot;
# the action evidence remains independently replayable from these private copies.
mkdir "$EDGE_SNAPSHOT_ROOT" "$EDGE_SNAPSHOT_ROOT/config" "$EDGE_SNAPSHOT_ROOT/secrets"
cp "$ACTION_EVIDENCE_1/runtime-input-compose.env" "$EDGE_SNAPSHOT_ROOT/compose.env"
cp "$ACTION_EVIDENCE_1/runtime-input-edge.env" "$EDGE_SNAPSHOT_ROOT/config/edge.env"
cp "$ACTION_EVIDENCE_1/runtime-input-gateway-public-key.pem" \
  "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem"
cp "$ACTION_EVIDENCE_1/runtime-input-access-allowed-emails" \
  "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails"
cp "$TOKEN" "$EDGE_SNAPSHOT_ROOT/secrets/cloudflare-tunnel.token"
chmod 400 "$EDGE_SNAPSHOT_ROOT/compose.env" "$EDGE_SNAPSHOT_ROOT/config/edge.env" \
  "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem" \
  "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails" \
  "$EDGE_SNAPSHOT_ROOT/secrets/cloudflare-tunnel.token"
chmod 500 "$EDGE_SNAPSHOT_ROOT/config" "$EDGE_SNAPSHOT_ROOT/secrets" "$EDGE_SNAPSHOT_ROOT"
"$FAULT_PUBLIC/verify-config.sh" --compose-mode production --immutable-snapshot --declared-root "$EDGE_SNAPSHOT_ROOT" \
  --quiet "$EDGE_SNAPSHOT_ROOT"
ABA_RENDERED="$TMP_ROOT/aba-snapshot-compose.json"
"$PRODUCTION_COMPOSE" --env-file "$EDGE_SNAPSHOT_ROOT/compose.env" \
  -f "$FAULT_PUBLIC/compose.yaml" --profile edge config --format json >"$ABA_RENDERED"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$ABA_RENDERED" "$EDGE_SNAPSHOT_ROOT" "$EDGE_ONLY_ROOT" <<'PY'
import json,pathlib,sys
model=json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
snapshot,configured=sys.argv[2:4]
expected={
 "edge":{snapshot+"/config/gateway-public-key.pem",snapshot+"/config/access-allowed-emails"},
 "cloudflared":{snapshot+"/secrets/cloudflare-tunnel.token"},
}
for service,sources in expected.items():
 volumes=model["services"][service].get("volumes",[])
 actual={item.get("source") for item in volumes if item.get("type")=="bind"}
 if actual!=sources:raise SystemExit(f"{service} bind sources do not equal immutable snapshot")
 if any(source.startswith(configured+"/") for source in actual):
  raise SystemExit("configured runtime path remained in a Compose bind")
PY
pass "normalized edge/cloudflared bind sources point only at the immutable runtime snapshot"
verify_action_1() {
  PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" verify \
    --evidence "$ACTION_EVIDENCE_1" --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" \
    --source-lock "$FAULT_PUBLIC/image/source.lock" \
    --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml" \
    --repo-root "$FAULT_REPO"
}
snapshot_unlock() {
  chmod 700 "$EDGE_SNAPSHOT_ROOT" "$EDGE_SNAPSHOT_ROOT/config" "$EDGE_SNAPSHOT_ROOT/secrets"
  chmod 600 "$EDGE_SNAPSHOT_ROOT/compose.env" "$EDGE_SNAPSHOT_ROOT/config/edge.env" \
    "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem" \
    "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails" \
    "$EDGE_SNAPSHOT_ROOT/secrets/cloudflare-tunnel.token"
}
snapshot_lock() {
  chmod 400 "$EDGE_SNAPSHOT_ROOT/compose.env" "$EDGE_SNAPSHOT_ROOT/config/edge.env" \
    "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem" \
    "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails" \
    "$EDGE_SNAPSHOT_ROOT/secrets/cloudflare-tunnel.token"
  chmod 500 "$EDGE_SNAPSHOT_ROOT/config" "$EDGE_SNAPSHOT_ROOT/secrets" "$EDGE_SNAPSHOT_ROOT"
}
RUNTIME_INPUT_BACKUP="$TMP_ROOT/action-runtime-input-backup";mkdir "$RUNTIME_INPUT_BACKUP"
cp "$EDGE_SNAPSHOT_ROOT/compose.env" "$RUNTIME_INPUT_BACKUP/compose.env"
cp "$EDGE_SNAPSHOT_ROOT/config/edge.env" "$RUNTIME_INPUT_BACKUP/edge.env"
cp "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem" "$RUNTIME_INPUT_BACKUP/gateway-public-key.pem"
cp "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails" "$RUNTIME_INPUT_BACKUP/access-allowed-emails"
snapshot_unlock
sed 's/^DJI4G_COTURN_IMAGE=.*/DJI4G_COTURN_IMAGE=invalid.local\/dji4g-coturn-disabled@sha256:1111111111111111111111111111111111111111111111111111111111111111/' \
  "$RUNTIME_INPUT_BACKUP/compose.env" >"$EDGE_SNAPSHOT_ROOT/compose.env"
snapshot_lock
expect_fail "action evidence rejects a valid-looking compose.env replacement" verify_action_1
snapshot_unlock;cp "$RUNTIME_INPUT_BACKUP/compose.env" "$EDGE_SNAPSHOT_ROOT/compose.env";snapshot_lock
snapshot_unlock
sed 's/^DJI4G_EDGE_GATEWAY_ID=.*/DJI4G_EDGE_GATEWAY_ID=replaced-gateway/' \
  "$RUNTIME_INPUT_BACKUP/edge.env" >"$EDGE_SNAPSHOT_ROOT/config/edge.env"
snapshot_lock
expect_fail "action evidence rejects a valid-looking edge.env replacement" verify_action_1
snapshot_unlock;cp "$RUNTIME_INPUT_BACKUP/edge.env" "$EDGE_SNAPSHOT_ROOT/config/edge.env";snapshot_lock
ALT_ACTION_PRIVATE_KEY="$TMP_ROOT/action-alternate-key.pem"
ALT_ACTION_PUBLIC_KEY="$TMP_ROOT/action-alternate-public.pem"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$ALT_ACTION_PRIVATE_KEY" >/dev/null 2>&1
openssl pkey -in "$ALT_ACTION_PRIVATE_KEY" -pubout -out "$ALT_ACTION_PUBLIC_KEY" >/dev/null 2>&1
snapshot_unlock;cp "$ALT_ACTION_PUBLIC_KEY" "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem";snapshot_lock
expect_fail "action evidence rejects a different valid P-256 gateway public key" verify_action_1
snapshot_unlock;cp "$RUNTIME_INPUT_BACKUP/gateway-public-key.pem" "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem";snapshot_lock
snapshot_unlock;printf '%s\n' 'attacker@example.invalid' >"$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails";snapshot_lock
expect_fail "action evidence rejects a different valid Access allowlist" verify_action_1
snapshot_unlock;cp "$RUNTIME_INPUT_BACKUP/access-allowed-emails" "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails";snapshot_lock
verify_action_1 >/dev/null || fail "restored runtime identity inputs no longer verify"
pass "action evidence independently binds and rechecks all four non-secret runtime identity inputs"

# Model the exact post-health/pre-manifest race: the pre-mutation anchor was
# already emitted, the container health gate has completed, and a valid runtime
# input is atomically replaced before action.json creation. The manifest must
# not silently record the replacement as the bytes used by the running edge.
PRE_ANCHOR_DRIFT_ACTION="$ACTION_PARENT/pre-anchor-post-health-drift"
cp -R "$ACTION_EVIDENCE_1" "$PRE_ANCHOR_DRIFT_ACTION"
rm "$PRE_ANCHOR_DRIFT_ACTION/action.json"
printf '%s\n' health-complete >"$TMP_ROOT/pre-anchor-health-checkpoint"
snapshot_unlock
DRIFT_ALLOWED_TMP="$EDGE_SNAPSHOT_ROOT/config/.access-allowed-emails.drift"
printf '%s\n' 'post-health-replacement@example.invalid' >"$DRIFT_ALLOWED_TMP"
chmod 600 "$DRIFT_ALLOWED_TMP"
mv "$DRIFT_ALLOWED_TMP" "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails"
snapshot_lock
set +e
PRE_ANCHOR_DRIFT_OUTPUT=$(PYTHONDONTWRITEBYTECODE=1 python3 -B -I \
  "$FAULT_PUBLIC/verify-action-evidence.py" create \
  --evidence "$PRE_ANCHOR_DRIFT_ACTION" --runtime-root "$EDGE_SNAPSHOT_ROOT" \
  --configured-runtime-root "$EDGE_ONLY_ROOT" \
  --uid-gid "$(id -u):$(id -g)" --edge-image "$LOCAL_EDGE_IMAGE" \
  --cloudflared-image "$CLOUDFLARED_IMAGE" --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" \
  --runtime-input-anchor "$PRE_ANCHOR_DRIFT_ACTION/runtime-inputs.pre.json" \
  --source-lock "$FAULT_PUBLIC/image/source.lock" \
  --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml" 2>&1)
PRE_ANCHOR_DRIFT_STATUS=$?
set -e
[ "$PRE_ANCHOR_DRIFT_STATUS" -ne 0 ] || fail "post-health runtime replacement minted action evidence"
printf '%s\n' "$PRE_ANCHOR_DRIFT_OUTPUT" | grep -Fq \
  'runtime identity/authorization inputs changed after the pre-mutation anchor' ||
  fail "post-health runtime replacement did not reach the pre/post anchor gate"
[ ! -e "$PRE_ANCHOR_DRIFT_ACTION/action.json" ] ||
  fail "post-health runtime replacement left a completed-looking action manifest"
snapshot_unlock
RESTORE_ALLOWED_TMP="$EDGE_SNAPSHOT_ROOT/config/.access-allowed-emails.restore"
cp "$RUNTIME_INPUT_BACKUP/access-allowed-emails" "$RESTORE_ALLOWED_TMP"
chmod 600 "$RESTORE_ALLOWED_TMP"
mv "$RESTORE_ALLOWED_TMP" "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails"
snapshot_lock
verify_action_1 >/dev/null || fail "runtime input restore after pre-anchor race no longer verifies"
pass "post-health runtime replacement cannot diverge from the pre-mutation input anchor"

SELF_REHASHED_ANCHOR_ACTION="$ACTION_PARENT/self-rehashed-pre-anchor"
cp -R "$ACTION_EVIDENCE_1" "$SELF_REHASHED_ANCHOR_ACTION"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - \
  "$SELF_REHASHED_ANCHOR_ACTION/runtime-inputs.pre.json" \
  "$SELF_REHASHED_ANCHOR_ACTION/action.json" <<'PY'
import hashlib,json,pathlib,sys
anchor_path=pathlib.Path(sys.argv[1]);manifest_path=pathlib.Path(sys.argv[2])
anchor=json.loads(anchor_path.read_text(encoding="ascii"))
anchor["config/access-allowed-emails"]="f"*64
anchor_path.write_text(json.dumps(anchor,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
manifest=json.loads(manifest_path.read_text(encoding="ascii"))
manifest["artifacts"][anchor_path.name]=hashlib.sha256(anchor_path.read_bytes()).hexdigest()
manifest_path.write_text(json.dumps(manifest,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
PY
expect_fail "self-rehashed pre-mutation anchor cannot diverge from the action identity" \
  python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" verify \
    --evidence "$SELF_REHASHED_ANCHOR_ACTION" --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" \
    --source-lock "$FAULT_PUBLIC/image/source.lock" \
    --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml" \
    --repo-root "$FAULT_REPO"

FORGED_ROLLBACK_ACTION="$ACTION_PARENT/forged-rollback-state";cp -R "$ACTION_EVIDENCE_1" "$FORGED_ROLLBACK_ACTION"
rm "$FORGED_ROLLBACK_ACTION/action.json"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$FORGED_ROLLBACK_ACTION/rollback-inspect.json" <<'PY'
import json,pathlib,sys
path=pathlib.Path(sys.argv[1]);value=json.loads(path.read_text());value["unverified"]="forged"
path.write_text(json.dumps(value,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
PY
expect_fail "self-rehashed rollback completion must replay the runtime-state verifier" \
  python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" create \
    --evidence "$FORGED_ROLLBACK_ACTION" --runtime-root "$EDGE_SNAPSHOT_ROOT" \
    --configured-runtime-root "$EDGE_ONLY_ROOT" \
    --uid-gid "$(id -u):$(id -g)" --edge-image "$LOCAL_EDGE_IMAGE" \
    --cloudflared-image "$CLOUDFLARED_IMAGE" --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" \
    --runtime-input-anchor "$FORGED_ROLLBACK_ACTION/runtime-inputs.pre.json" \
    --source-lock "$FAULT_PUBLIC/image/source.lock" \
    --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml"
[ ! -e "$FORGED_ROLLBACK_ACTION/action.json" ] ||
  fail "forged rollback state minted a replacement action manifest"
MINIMAL_COMPLETED_ACTION="$ACTION_PARENT/forged-minimal-completed"
mkdir "$MINIMAL_COMPLETED_ACTION"
chmod 700 "$MINIMAL_COMPLETED_ACTION"
printf '%s\n' \
  'status=completed' 'action=start-edge' 'evidence_status=complete' \
  'rollback_status=not-required' 'coturn_status=exact-pre-action-fingerprint-verified' \
  >"$MINIMAL_COMPLETED_ACTION/result.txt"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" anchor \
  --runtime-root "$EDGE_SNAPSHOT_ROOT" \
  --output "$MINIMAL_COMPLETED_ACTION/runtime-inputs.pre.json"
expect_fail "completed action evidence cannot omit pre/post state, summaries, and startup log" \
  python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" create \
    --evidence "$MINIMAL_COMPLETED_ACTION" --runtime-root "$EDGE_SNAPSHOT_ROOT" \
    --configured-runtime-root "$EDGE_ONLY_ROOT" \
    --uid-gid "$(id -u):$(id -g)" --edge-image "$LOCAL_EDGE_IMAGE" \
    --cloudflared-image "$CLOUDFLARED_IMAGE" --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" \
    --runtime-input-anchor "$MINIMAL_COMPLETED_ACTION/runtime-inputs.pre.json" \
    --source-lock "$FAULT_PUBLIC/image/source.lock" \
    --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml"
[ ! -e "$MINIMAL_COMPLETED_ACTION/action.json" ] ||
  fail "minimal completed evidence minted an action manifest"
TAMPERED_ACTION="$ACTION_PARENT/action-external-anchor-tamper";cp -R "$ACTION_EVIDENCE_1" "$TAMPERED_ACTION"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$TAMPERED_ACTION/action.json" <<'PY'
import json,pathlib,sys
path=pathlib.Path(sys.argv[1]);value=json.loads(path.read_text());value["source_lock_sha256"]="f"*64
path.write_text(json.dumps(value,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
PY
expect_fail "action manifest provenance must equal external source/toolchain/Compose anchors" \
  python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" verify \
    --evidence "$TAMPERED_ACTION" --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" \
    --source-lock "$FAULT_PUBLIC/image/source.lock" \
    --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml" \
    --repo-root "$FAULT_REPO"
[ ! -s "$FAKE_BIN/ambient-leak" ] || fail "a Docker/Compose daemon call inherited ambient connection context"
pass "daemon and Compose calls ignore ambient plus persistent currentContext/proxy configuration"
[ ! -s "$FAKE_BIN/direct-daemon-use" ] || fail "policy or mutation bypassed the selected sudo Docker command"
if grep -Evq '^(0:info|1:)' "$FAKE_BIN/daemon-calls"; then
  fail "unexpected direct/sudo parser or daemon split"
fi
pass "sudo daemon fallback binds Compose policy normalization and mutation to its 2.40.3 parser"

# The reconstructed fixture is no longer a running mount source. Remove its
# exact allowlisted tree and prove the action remains replayable from the
# recorded non-secret bytes after the production snapshot lifecycle ends.
snapshot_unlock
rm -f -- "$EDGE_SNAPSHOT_ROOT/compose.env" "$EDGE_SNAPSHOT_ROOT/config/edge.env" \
  "$EDGE_SNAPSHOT_ROOT/config/gateway-public-key.pem" \
  "$EDGE_SNAPSHOT_ROOT/config/access-allowed-emails" \
  "$EDGE_SNAPSHOT_ROOT/secrets/cloudflare-tunnel.token"
rmdir "$EDGE_SNAPSHOT_ROOT/config" "$EDGE_SNAPSHOT_ROOT/secrets" "$EDGE_SNAPSHOT_ROOT"
verify_action_1 >/dev/null || fail "action evidence could not replay after its verified snapshot cleanup"
pass "action evidence retains exact non-secret runtime bytes after snapshot cleanup without recording the token"

rm -f "$FAKE_BIN/ps-count" "$FAKE_BIN/compose-calls"
printf '%s\n' fail-after-pre >"$FAKE_BIN/ps-mode"
ACTION_EVIDENCE_2="$ACTION_PARENT/start-edge-rollback-unproven"
if PATH="$FAKE_BIN:$PATH" "$DEPLOY_FAULT" --runtime-root "$EDGE_ONLY_ROOT" \
    --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" --state-evidence "$ACTION_EVIDENCE_2" \
    start-edge >"$TMP_ROOT/fault-start-2.log" 2>&1; then
  fail "rollback evidence fault unexpectedly succeeded"
fi
[ -d "$ACTION_EVIDENCE_2" ] || fail "incomplete rollback evidence was deleted"
grep -Fqx 'rollback_status=incomplete' "$ACTION_EVIDENCE_2/result.txt" || fail "unproven rollback was misreported"
if grep -Fqx 'rollback_status=completed' "$ACTION_EVIDENCE_2/result.txt"; then fail "incomplete rollback was called completed"; fi
grep -Fqx 'coturn_status=not-proven' "$ACTION_EVIDENCE_2/result.txt" || fail "unproven coturn state is not explicit"
pass "rollback collection failure is preserved and never described as completed"

EDGE_SNAPSHOT_2=$(PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$ACTION_EVIDENCE_2/action.json" <<'PY'
import json,pathlib,sys
print(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="ascii"))["runtime_root"])
PY
)
[ -d "$EDGE_SNAPSHOT_2" ] || fail "unproven rollback incorrectly removed the possible live mount snapshot"
rm -f "$FAKE_BIN/ps-count"
printf '%s\n' normal >"$FAKE_BIN/ps-mode"
RESTART_WITH_RETAINED="$ACTION_PARENT/start-edge-retained-snapshot-rejected"
COMPOSE_CALLS_BEFORE=$(wc -l <"$FAKE_BIN/compose-calls" | tr -d ' ')
set +e
RETAINED_OUTPUT=$(env PATH="$FAKE_BIN:$PATH" "$DEPLOY_FAULT" --runtime-root "$EDGE_ONLY_ROOT" \
  --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" --state-evidence "$RESTART_WITH_RETAINED" \
  start-edge 2>&1)
RETAINED_STATUS=$?
set -e
[ "$RETAINED_STATUS" -ne 0 ] || fail "retained-snapshot start unexpectedly succeeded"
printf '%s\n' "$RETAINED_OUTPUT" | grep -Fq \
  'an existing runtime snapshot must be removed by stop-edge before another start' ||
  fail "retained-snapshot refusal did not reach the snapshot ownership gate"
[ ! -e "$RESTART_WITH_RETAINED" ] || fail "retained-snapshot refusal published action evidence"
[ "$(wc -l <"$FAKE_BIN/compose-calls" | tr -d ' ')" = "$COMPOSE_CALLS_BEFORE" ] ||
  fail "retained-snapshot refusal reached another Compose mutation"
pass "start-edge refuses an unproven retained snapshot before another Compose mutation"
rm -f "$FAKE_BIN/ps-count" "$FAKE_BIN/compose-calls"
printf '%s\n' normal >"$FAKE_BIN/ps-mode"
STOP_SNAPSHOT_EVIDENCE="$ACTION_PARENT/stop-edge-cleans-snapshot"
PATH="$FAKE_BIN:$PATH" "$DEPLOY_FAULT" --runtime-root "$EDGE_ONLY_ROOT" \
  --state-evidence "$STOP_SNAPSHOT_EVIDENCE" stop-edge >/dev/null ||
  fail "stop-edge could not remove the exact retained runtime snapshot after container/network cleanup"
[ ! -e "$EDGE_SNAPSHOT_2" ] && [ ! -L "$EDGE_SNAPSHOT_2" ] ||
  fail "completed stop-edge left its exact runtime snapshot behind"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$FAULT_PUBLIC/verify-action-evidence.py" verify \
  --evidence "$STOP_SNAPSHOT_EVIDENCE" --source-lock "$FAULT_PUBLIC/image/source.lock" \
  --toolchain-lock "$FAULT_PUBLIC/image/toolchain.lock" --compose "$FAULT_PUBLIC/compose.yaml" \
  --repo-root "$FAULT_REPO" >/dev/null
pass "stop-edge removes only the exact retained snapshot after proving containers and network absent"

rm -f "$FAKE_BIN/ps-count" "$FAKE_BIN/compose-calls"
printf '%s\n' redact-fragment >"$FAKE_BIN/ps-mode"
ACTION_EVIDENCE_REDACT="$ACTION_PARENT/start-edge-redaction-reset"
if PATH="$FAKE_BIN:$PATH" "$DEPLOY_FAULT" --runtime-root "$EDGE_ONLY_ROOT" \
    --edge-evidence "$SYNTHETIC_IMAGE_EVIDENCE" --state-evidence "$ACTION_EVIDENCE_REDACT" \
    start-edge >"$TMP_ROOT/fault-start-redaction.log" 2>&1; then
  fail "redaction-fault start-edge unexpectedly succeeded"
fi
[ -d "$ACTION_EVIDENCE_REDACT" ] || fail "safe redaction-reset evidence was not preserved"
grep -Fqx 'rollback_status=not-proven-after-evidence-erasure' "$ACTION_EVIDENCE_REDACT/result.txt" ||
  fail "redaction reset incorrectly claims independently verified rollback completion"
grep -Fqx 'evidence_status=incomplete' "$ACTION_EVIDENCE_REDACT/result.txt" ||
  fail "redaction reset was not marked incomplete"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$TOKEN" "$ACTION_EVIDENCE_REDACT" <<'PY'
import pathlib,sys
secret=pathlib.Path(sys.argv[1]).read_bytes().rstrip(b"\n")
root=pathlib.Path(sys.argv[2])
fragments={secret[i:i+16] for i in range(len(secret)-15)}
for evidence in root.iterdir():
    if evidence.is_file():
        data=evidence.read_bytes()
        if any(fragment in data for fragment in fragments) or b"PRIVATE KEY-----" in data:
            raise SystemExit("secret fragment/private-key marker survived safe evidence reset")
if (root/"failure-startup.log").exists():
    raise SystemExit("raw collected log survived redaction failure")
PY
pass "truncated token and private-key marker force safe log erasure and downgrade the rollback claim"

START_TURN_EVIDENCE="$ACTION_PARENT/start-turn-must-not-exist"
DAEMON_CALLS_BEFORE=$(wc -l <"$FAKE_BIN/daemon-calls" | tr -d ' ')
expect_fail "start-turn is hard-disabled before daemon access or evidence staging" \
  env PATH="$FAKE_BIN:$PATH" "$DEPLOY" --runtime-root "$VALID_ROOT" \
    --state-evidence "$START_TURN_EVIDENCE" start-turn
[ ! -e "$START_TURN_EVIDENCE" ] || fail "hard-disabled start-turn created evidence state"
[ "$(wc -l <"$FAKE_BIN/daemon-calls" | tr -d ' ')" = "$DAEMON_CALLS_BEFORE" ] ||
  fail "hard-disabled start-turn contacted the Docker daemon"

expect_fail "edge-only mode rejects supplied TURN inputs" \
  "$GENERATE" --output "$TMP_ROOT/runtime-edge-mixed" --edge-image "$LOCAL_EDGE_IMAGE" \
    --cloudflared-image "$CLOUDFLARED_IMAGE" --coturn-image "$COTURN_IMAGE" \
    --cloudflare-token-file "$TOKEN" --edge-gateway-id "$EDGE_GATEWAY_ID" \
    --gateway-public-key-file "$GATEWAY_PUBLIC_KEY" --access-team-domain "$ACCESS_TEAM_DOMAIN" \
    --access-audience "$ACCESS_AUDIENCE" --access-allowed-emails-file "$ALLOWED_EMAILS" --profile edge

expect_fail "cloudflared exact local image ID is rejected" \
  invoke_generate_edge "$TMP_ROOT/runtime-cf-local-id" "$LOCAL_EDGE_IMAGE" "$LOCAL_EDGE_IMAGE" "$TOKEN"

expect_fail "ambient Compose image override is rejected by the deployment wrapper" \
  env DJI4G_EDGE_IMAGE=alpine:latest "$DEPLOY" --runtime-root "$VALID_ROOT" check

expect_fail "ambient edge env path override is rejected by the deployment wrapper" \
  env DJI4G_EDGE_ENV_FILE=/tmp/attacker.env "$DEPLOY" --runtime-root "$VALID_ROOT" check

expect_fail "ambient enabled-profile override is rejected by the deployment wrapper" \
  env DJI4G_ENABLED_PROFILES=edge "$DEPLOY" --runtime-root "$VALID_ROOT" check

DRY_ROOT="$TMP_ROOT/runtime-dry"
invoke_generate "$DRY_ROOT" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" >/dev/null
[ ! -e "$DRY_ROOT" ] || fail "dry-run created a runtime root"
pass "dry-run performs no write"

expect_fail "mutable or unpinned edge image is rejected" \
  invoke_generate "$TMP_ROOT/runtime-tag" 'ghcr.io/example/maccellular-edge:latest' "$TOKEN" "$AUTH" "$KEY"

expect_fail "missing edge image is rejected" \
  invoke_generate "$TMP_ROOT/runtime-missing-image" '' "$TOKEN" "$AUTH" "$KEY"

expect_fail "non-0600 token file is rejected" \
  invoke_generate "$TMP_ROOT/runtime-mode" "$EDGE_IMAGE" "$BAD_MODE_TOKEN" "$AUTH" "$KEY"

expect_fail "multi-line configuration injection is rejected" \
  invoke_generate "$TMP_ROOT/runtime-injection" "$EDGE_IMAGE" "$TOKEN" "$BAD_LINES_AUTH" "$KEY"

expect_fail "NUL-bearing token is rejected" \
  invoke_generate "$TMP_ROOT/runtime-nul" "$EDGE_IMAGE" "$BAD_NUL_TOKEN" "$AUTH" "$KEY"

expect_fail "symlinked secret input is rejected" \
  invoke_generate "$TMP_ROOT/runtime-link" "$EDGE_IMAGE" "$TOKEN_LINK" "$AUTH" "$KEY"

expect_fail "relative output path is rejected" \
  invoke_generate 'runtime-relative' "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY"

expect_fail "non-integer uid is rejected" \
  invoke_generate "$TMP_ROOT/runtime-uid" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" --edge-uid 1.5

expect_fail "certificate and private-key mismatch is rejected" \
  invoke_generate "$TMP_ROOT/runtime-key-mismatch" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$OTHER_KEY"

expect_fail "non-P-256 gateway public key is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-bad-gateway-key" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$BAD_GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$ALLOWED_EMAILS"

expect_fail "compressed P-256 gateway public key is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-compressed-gateway-key" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$COMPRESSED_GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$ALLOWED_EMAILS"

expect_fail "gateway public key with trailing data is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-trailing-gateway-key" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$TRAILING_GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$ALLOWED_EMAILS"

expect_fail "oversize gateway public key is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-oversize-gateway-key" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$OVERSIZE_GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$ALLOWED_EMAILS"

expect_fail "invalid gateway identity is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-bad-gateway-id" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    'Bad;gateway' "$GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$ALLOWED_EMAILS"

expect_fail "invalid Access team domain is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-bad-team" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$GATEWAY_PUBLIC_KEY" 'https://unit-test.cloudflareaccess.com' \
    "$ACCESS_AUDIENCE" "$ALLOWED_EMAILS"

expect_fail "invalid Access audience is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-bad-audience" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    'invalid audience' "$ALLOWED_EMAILS"

expect_fail "duplicate Access allowed emails are rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-bad-emails" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$BAD_EMAILS"

expect_fail "malformed Access allowed email is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-bad-email-syntax" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$BAD_SYNTAX_EMAILS"

expect_fail "non-0600 Access allowed-email file is rejected" \
  invoke_generate_explicit \
    "$TMP_ROOT/runtime-bad-email-mode" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" \
    "$EDGE_GATEWAY_ID" "$GATEWAY_PUBLIC_KEY" "$ACCESS_TEAM_DOMAIN" \
    "$ACCESS_AUDIENCE" "$BAD_MODE_EMAILS"

printf '%s\n' "DJI4G_EDGE_IMAGE=$EDGE_IMAGE" >>"$VALID_ROOT/compose.env"
expect_fail "duplicate generated image setting is rejected" "$VERIFY" --quiet "$VALID_ROOT"

TURN_TAMPER_ROOT="$TMP_ROOT/runtime-turn-tamper"
invoke_generate "$TURN_TAMPER_ROOT" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" --apply >/dev/null
printf '%s\n' 'min-port=1' >>"$TURN_TAMPER_ROOT/config/turnserver.conf"
expect_fail "TURN relay-range tampering is rejected" "$VERIFY" --quiet "$TURN_TAMPER_ROOT"

EDGE_TAMPER_ROOT="$TMP_ROOT/runtime-edge-tamper"
invoke_generate "$EDGE_TAMPER_ROOT" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" --apply >/dev/null
printf '%s\n' 'DJI4G_EDGE_MUTATIONS_ENABLED=true' >>"$EDGE_TAMPER_ROOT/config/edge.env"
expect_fail "edge mutation enablement tampering is rejected" "$VERIFY" --quiet "$EDGE_TAMPER_ROOT"

EXTRA_ROOT="$TMP_ROOT/runtime-extra"
invoke_generate "$EXTRA_ROOT" "$EDGE_IMAGE" "$TOKEN" "$AUTH" "$KEY" --apply >/dev/null
printf '%s\n' 'unexpected' >"$EXTRA_ROOT/config/extra.conf"
chmod 600 "$EXTRA_ROOT/config/extra.conf"
expect_fail "unexpected generated files are rejected" "$VERIFY" --quiet "$EXTRA_ROOT"

if grep -Fq 's|__TURN_AUTH_SECRET__' "$GENERATE" "$VERIFY"; then
  fail "TURN secret is still interpolated through sed argv"
fi
if grep -Fq 'grep -Fq -e "$CLOUDFLARE_TOKEN"' "$GENERATE" "$VERIFY"; then
  fail "Cloudflare token is still passed through grep argv"
fi
pass "secret values are not passed through sed/grep process argv"

printf 'PASS: %d local static checks; Compose config parsing used no Docker daemon or network operation.\n' "$PASS_COUNT"
