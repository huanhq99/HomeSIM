#!/usr/bin/env bash
set -euo pipefail

umask 077
export LC_ALL=C

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
COMPOSE_YAML="$SCRIPT_DIR/compose.yaml"
VERIFY="$SCRIPT_DIR/verify-runtime.sh"
ROOT=""
ACTION=""

usage() {
  cat <<'EOF'
Usage:
  turnctl.sh --runtime-root /absolute/runtime-v1 check
  turnctl.sh --runtime-root /absolute/runtime-v1 start
  turnctl.sh --runtime-root /absolute/runtime-v1 status
  turnctl.sh --runtime-root /absolute/runtime-v1 stop

The wrapper always uses `sudo -n docker compose`, the fixed project name
`dji4g-public-turn`, and pull_policy=never. It never changes DNS or firewalls.
`stop` is the rollback action: it removes only this Compose project's coturn
container and private bridge; the versioned runtime files remain intact.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --runtime-root)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--runtime-root requires a value"
      ROOT=$2
      shift 2
      ;;
    check|start|status|stop)
      [ -z "$ACTION" ] || fail "only one action may be supplied"
      ACTION=$1
      shift
      ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

[ -n "$ROOT" ] || fail "--runtime-root is required"
[ -n "$ACTION" ] || fail "an action is required"
[ "$(id -u)" -gt 0 ] && [ "$(id -g)" -gt 0 ] || fail "run as the non-root runtime owner"
case "$ROOT" in /*) ;; *) fail "--runtime-root must be absolute" ;; esac
[ -x "$VERIFY" ] || fail "verify-runtime.sh is missing or not executable"
[ -f "$COMPOSE_YAML" ] && [ ! -L "$COMPOSE_YAML" ] || fail "compose.yaml is missing or symlinked"

for name in \
  DJI4G_PUBLIC_TURN_ROOT DJI4G_COTURN_IMAGE DJI4G_TURN_UID DJI4G_TURN_GID \
  COMPOSE_PROJECT_NAME COMPOSE_FILE DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG BUILDKIT_HOST; do
  [ -z "${!name+x}" ] || fail "ambient $name is forbidden"
done

docker_cli() {
  sudo -n docker --host unix:///var/run/docker.sock "$@"
}

compose() {
  DJI4G_PUBLIC_TURN_ROOT="$ROOT" \
    sudo -n docker --host unix:///var/run/docker.sock compose \
      --project-name dji4g-public-turn \
      --project-directory "$SCRIPT_DIR" \
      --env-file "$ROOT/compose.env" \
      -f "$COMPOSE_YAML" \
      "$@"
}

read_image() {
  local line
  line=$(grep -E '^DJI4G_COTURN_IMAGE=' "$ROOT/compose.env")
  printf '%s' "${line#*=}"
}

compose_syntax_check() {
  compose version >/dev/null
  compose config --quiet
}

config_check() {
  "$VERIFY" "$ROOT" >/dev/null
  compose_syntax_check
}

cached_image_check() {
  local image
  image=$(read_image)
  docker_cli image inspect --format '{{.Id}}' "$image" >/dev/null 2>&1 ||
    fail "the exact coturn image digest is not cached; pull that digest deliberately before start"
}

case "$ACTION" in
  check)
    config_check
    cached_image_check
    printf 'TURN deployment is ready to start: %s\n' "$ROOT"
    ;;
  status)
    compose_syntax_check
    compose ps coturn
    ;;
  start)
    config_check
    cached_image_check
    EXISTING=$(compose ps --all --quiet coturn)
    [ -z "$EXISTING" ] || fail "this TURN project already has a container; inspect status or run stop before a clean start"
    if ! compose up -d --no-build --no-deps --pull never --wait --wait-timeout 35 coturn; then
      compose down --timeout 20 >/dev/null 2>&1 || true
      fail "coturn start or TCP listener health check failed; this project's partial container and bridge were rolled back"
    fi
    RUNNING=$(compose ps --status running --services)
    if ! printf '%s\n' "$RUNNING" | grep -Fxq coturn; then
      compose down --timeout 20 >/dev/null 2>&1 || true
      fail "coturn did not remain running; this project's container and bridge were rolled back"
    fi
    printf 'TURN service started from %s\n' "$ROOT"
    ;;
  stop)
    # Recovery must remain possible even after a certificate expires or a
    # private runtime file is damaged. The fixed Compose file and project name
    # still bound this action to the one coturn service.
    compose_syntax_check
    compose down --timeout 20
    printf 'TURN service stopped; runtime files retained at %s\n' "$ROOT"
    ;;
esac
