#!/usr/bin/env bash
set -euo pipefail

umask 077
export LC_ALL=C

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
PREPARE="$SCRIPT_DIR/prepare-runtime.sh"
VERIFY="$SCRIPT_DIR/verify-runtime.sh"
CTL="$SCRIPT_DIR/turnctl.sh"
COMPOSE="$SCRIPT_DIR/compose.yaml"
TEMPLATE="$SCRIPT_DIR/turnserver.conf.in"
PASS_COUNT=0

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  printf 'ok %d - %s\n' "$PASS_COUNT" "$1"
}

fail() {
  printf 'not ok - %s\n' "$*" >&2
  exit 1
}

expect_fail() {
  local label=$1
  shift
  if "$@" >"$TMP_ROOT/expected-failure.out" 2>&1; then
    fail "$label"
  fi
  pass "$label"
}

RAW_TMP=$(mktemp -d "${TMPDIR:-/tmp}/dji4g-public-turn-test.XXXXXX")
TMP_ROOT=$(CDPATH= cd -- "$RAW_TMP" && pwd -P)
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  case "$TMP_ROOT" in
    /tmp/dji4g-public-turn-test.??????|/private/tmp/dji4g-public-turn-test.??????|\
      /private/var/folders/*/dji4g-public-turn-test.??????|/var/folders/*/dji4g-public-turn-test.??????)
      rm -rf -- "$TMP_ROOT"
      ;;
  esac
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

for script in "$PREPARE" "$VERIFY" "$CTL" "$0"; do
  bash -n "$script"
done
pass "shell scripts pass bash syntax checks"

[ "$(grep -Fxc 'name: dji4g-public-turn' "$COMPOSE")" -eq 1 ] || fail "Compose project name changed"
[ "$(grep -Fxc '    pull_policy: never' "$COMPOSE")" -eq 1 ] || fail "pull policy is not pinned to never"
[ "$(grep -Fxc '    read_only: true' "$COMPOSE")" -eq 1 ] || fail "read-only root is missing"
[ "$(grep -Fxc '      - ALL' "$COMPOSE")" -eq 1 ] || fail "capability drop is missing"
[ "$(grep -Fxc '      - NET_BIND_SERVICE' "$COMPOSE")" -eq 1 ] || fail "the one required low-port capability is missing"
[ "$(grep -Fxc '      - no-new-privileges:true' "$COMPOSE")" -eq 1 ] || fail "no-new-privileges is missing"
[ "$(grep -Fxc '      - /var/lib/coturn:rw,noexec,nosuid,nodev,size=8m,mode=1777' "$COMPOSE")" -eq 1 ] || fail "official image volume is not replaced by tmpfs"
[ "$(grep -Fxc '        - exec 3<>/dev/tcp/172.30.247.2/3478' "$COMPOSE")" -eq 1 ] || fail "bounded TCP listener health check is missing"
[ "$(grep -Fxc '        - subnet: 172.30.247.0/28' "$COMPOSE")" -eq 1 ] || fail "private bridge subnet changed"
[ "$(grep -Fxc '        ipv4_address: 172.30.247.2' "$COMPOSE")" -eq 1 ] || fail "fixed coturn bridge IP changed"
! grep -Eq '^[[:space:]]*(network_mode:[[:space:]]*host|privileged:[[:space:]]*true)' "$COMPOSE" || fail "host/privileged mode is forbidden"
pass "Compose keeps the isolated least-privilege service boundary"

[ "$(grep -Fxc '        published: "3478"' "$COMPOSE")" -eq 2 ] || fail "3478 must be published exactly for TCP and UDP"
[ "$(grep -Fxc '        published: "443"' "$COMPOSE")" -eq 1 ] || fail "443/tcp mapping changed"
for port in 49160 49161 49162 49163 49164 49165 49166 49167; do
  [ "$(grep -Fxc "        published: \"$port\"" "$COMPOSE")" -eq 1 ] || fail "relay port $port changed"
done
[ "$(grep -Ec '^[[:space:]]*published:' "$COMPOSE")" -eq 11 ] || fail "an extra host port was added"
pass "Compose publishes only the exact TURN and relay ports"

for line in \
  'realm=turn.example.com' 'server-name=turn.example.com' \
  'listening-ip=172.30.247.2' 'relay-ip=172.30.247.2' \
  'external-ip=__TURN_PUBLIC_IP__/172.30.247.2' \
  'listening-port=3478' 'tls-listening-port=5349' \
  'min-port=49160' 'max-port=49167' 'use-auth-secret' \
  'static-auth-secret=__TURN_AUTH_SECRET__' 'no-rfc5780' 'no-cli' \
  'no-multicast-peers' 'no-dtls' 'no-tcp-relay'; do
  [ "$(grep -Fxc "$line" "$TEMPLATE")" -eq 1 ] || fail "required coturn policy missing: $line"
done
! grep -Eiq '^(alt-listening-port|tls-alt-listening-port|aux-server|alternate-server)=' "$TEMPLATE" ||
  fail "alternate TURN listeners are forbidden"
pass "coturn policy pins REST auth and disables alternate listeners"

INPUT="$TMP_ROOT/input"
mkdir "$INPUT"
chmod 700 "$INPUT"
SECRET="$INPUT/turn-auth-secret"
CERT="$INPUT/turn-tls-cert.pem"
KEY="$INPUT/turn-tls-key.pem"
printf '%s' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' >"$SECRET"
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj '/CN=turn.example.com' -addext 'subjectAltName=DNS:turn.example.com' \
  -keyout "$KEY" -out "$CERT" >/dev/null 2>&1
chmod 600 "$SECRET" "$CERT" "$KEY"

DIGEST=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE="coturn/coturn@sha256:$DIGEST"
RUNTIME="$TMP_ROOT/runtime-v1"
COMMON_ARGS=(
  --runtime-root "$RUNTIME"
  --image "$IMAGE"
  --public-ip 8.8.8.8
  --auth-secret-file "$SECRET"
  --tls-cert-file "$CERT"
  --tls-key-file "$KEY"
)

"$PREPARE" "${COMMON_ARGS[@]}" >"$TMP_ROOT/dry-run.out"
[ ! -e "$RUNTIME" ] || fail "dry-run created the runtime"
grep -Fq 'No files changed.' "$TMP_ROOT/dry-run.out" || fail "dry-run summary is missing"
! grep -Fq 'AAAAAAAA' "$TMP_ROOT/dry-run.out" || fail "dry-run printed the auth secret"
pass "prepare defaults to a secret-free dry-run"

"$PREPARE" "${COMMON_ARGS[@]}" --apply >"$TMP_ROOT/apply.out"
"$VERIFY" "$RUNTIME" >"$TMP_ROOT/verify.out"
pass "valid inputs create a verified versioned runtime"

stat_mode() {
  if stat -f '%Lp' / >/dev/null 2>&1; then stat -f '%Lp' "$1"; else stat -c '%a' "$1"; fi
}
[ "$(stat_mode "$RUNTIME")" = 700 ] && [ "$(stat_mode "$RUNTIME/config")" = 700 ] &&
  [ "$(stat_mode "$RUNTIME/secrets")" = 700 ] || fail "runtime directories are not 0700"
for file in compose.env config/turnserver.conf secrets/turn-auth-secret secrets/turn-tls-cert.pem secrets/turn-tls-key.pem; do
  [ "$(stat_mode "$RUNTIME/$file")" = 600 ] || fail "$file is not 0600"
done
pass "runtime directories and private files have exact permissions"

expect_fail "prepare refuses to overwrite an existing runtime" "$PREPARE" "${COMMON_ARGS[@]}" --apply
expect_fail "prepare rejects mutable image tags" "$PREPARE" \
  --runtime-root "$TMP_ROOT/bad-image" --image coturn/coturn:latest --public-ip 8.8.8.8 \
  --auth-secret-file "$SECRET" --tls-cert-file "$CERT" --tls-key-file "$KEY"

BAD_SECRET="$INPUT/bad-secret"
printf '%s\n%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' 'alt-listening-port=3479' >"$BAD_SECRET"
chmod 600 "$BAD_SECRET"
expect_fail "prepare rejects multiline configuration injection in the secret" "$PREPARE" \
  --runtime-root "$TMP_ROOT/bad-secret-runtime" --image "$IMAGE" --public-ip 8.8.8.8 \
  --auth-secret-file "$BAD_SECRET" --tls-cert-file "$CERT" --tls-key-file "$KEY"

cp "$RUNTIME/config/turnserver.conf" "$TMP_ROOT/turnserver.good"
printf '%s\n' 'alt-listening-port=3479' >>"$RUNTIME/config/turnserver.conf"
expect_fail "runtime verification rejects alternate-listener drift" "$VERIFY" "$RUNTIME"
cp "$TMP_ROOT/turnserver.good" "$RUNTIME/config/turnserver.conf"
chmod 600 "$RUNTIME/config/turnserver.conf"
"$VERIFY" "$RUNTIME" >/dev/null

chmod 644 "$RUNTIME/secrets/turn-tls-key.pem"
expect_fail "runtime verification rejects relaxed private-key permissions" "$VERIFY" "$RUNTIME"
chmod 600 "$RUNTIME/secrets/turn-tls-key.pem"
"$VERIFY" "$RUNTIME" >/dev/null

MOCK_BIN="$TMP_ROOT/mock-bin"
MOCK_LOG="$TMP_ROOT/docker.log"
mkdir "$MOCK_BIN"
cat >"$MOCK_BIN/sudo" <<'SH'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >>"$MOCK_LOG"
joined=" $* "
case "$joined" in
  *" docker --host unix:///var/run/docker.sock compose "*" version "*) printf '%s\n' 'Docker Compose version v2.40.3' ;;
  *" docker --host unix:///var/run/docker.sock compose "*" config --quiet "*) ;;
  *" docker --host unix:///var/run/docker.sock image inspect "*) printf '%s\n' 'sha256:cached-image' ;;
  *" docker --host unix:///var/run/docker.sock compose "*" ps --all --quiet coturn "*)
    [ -z "${MOCK_EXISTING:-}" ] || printf '%s\n' 'existing-container'
    ;;
  *" docker --host unix:///var/run/docker.sock compose "*" up -d "*)
    [ -z "${MOCK_FAIL_UP:-}" ] || exit 55
    ;;
  *" docker --host unix:///var/run/docker.sock compose "*" ps --status running --services "*) printf '%s\n' coturn ;;
  *" docker --host unix:///var/run/docker.sock compose "*" ps coturn "*) printf '%s\n' 'coturn running' ;;
  *" docker --host unix:///var/run/docker.sock compose "*" down --timeout 20 "*) ;;
  *) printf 'unexpected mock sudo call: %s\n' "$*" >&2; exit 64 ;;
esac
SH
chmod 700 "$MOCK_BIN/sudo"
export MOCK_LOG

chmod 644 "$RUNTIME/secrets/turn-tls-key.pem"
PATH="$MOCK_BIN:$PATH" "$CTL" --runtime-root "$RUNTIME" stop >"$TMP_ROOT/damaged-stop.out"
chmod 600 "$RUNTIME/secrets/turn-tls-key.pem"
grep -Fq -- 'down --timeout 20' "$MOCK_LOG" || fail "damaged runtime blocked the stop path"
pass "stop remains available when private runtime validation is damaged"

: >"$MOCK_LOG"
PATH="$MOCK_BIN:$PATH" "$CTL" --runtime-root "$RUNTIME" check >"$TMP_ROOT/check.out"
PATH="$MOCK_BIN:$PATH" "$CTL" --runtime-root "$RUNTIME" status >"$TMP_ROOT/status.out"
PATH="$MOCK_BIN:$PATH" "$CTL" --runtime-root "$RUNTIME" start >"$TMP_ROOT/start.out"
PATH="$MOCK_BIN:$PATH" "$CTL" --runtime-root "$RUNTIME" stop >"$TMP_ROOT/stop.out"
grep -Fq -- '--project-name dji4g-public-turn' "$MOCK_LOG" || fail "fixed Compose project name was not used"
grep -Fq -- 'up -d --no-build --no-deps --pull never --wait --wait-timeout 35 coturn' "$MOCK_LOG" || fail "start could pull, build or skip bounded health"
grep -Fq -- 'down --timeout 20' "$MOCK_LOG" || fail "stop did not use the bounded project rollback"
pass "wrapper uses sudo -n, fixed project scope, no-pull start and bounded stop"

: >"$MOCK_LOG"
expect_fail "failed start rolls back its partial project" env PATH="$MOCK_BIN:$PATH" MOCK_LOG="$MOCK_LOG" MOCK_FAIL_UP=1 \
  "$CTL" --runtime-root "$RUNTIME" start
grep -Fq -- 'up -d --no-build --no-deps --pull never --wait --wait-timeout 35 coturn' "$MOCK_LOG" || fail "failed-start fixture did not reach up"
grep -Fq -- 'down --timeout 20' "$MOCK_LOG" || fail "failed start did not run rollback"
pass "failed start rollback is limited to the TURN Compose project"

expect_fail "wrapper rejects ambient Compose overrides" env PATH="$MOCK_BIN:$PATH" MOCK_LOG="$MOCK_LOG" \
  DJI4G_COTURN_IMAGE="$IMAGE" "$CTL" --runtime-root "$RUNTIME" status

printf '1..%d\n' "$PASS_COUNT"
