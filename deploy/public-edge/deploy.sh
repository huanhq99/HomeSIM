#!/usr/bin/env bash
set -euo pipefail

umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd -P)
COMPOSE="$SCRIPT_DIR/compose.yaml"
VERIFY="$SCRIPT_DIR/verify-config.sh"
STATE_VERIFY="$SCRIPT_DIR/verify-runtime-state.py"
IMAGE_VERIFY="$SCRIPT_DIR/image/verify-evidence.py"
SOURCE_VERIFY="$SCRIPT_DIR/image/verify-source.py"
ACTION_VERIFY="$SCRIPT_DIR/verify-action-evidence.py"
ATOMIC_COMMIT="$SCRIPT_DIR/image/atomic-commit.py"
TRUSTED_SOURCE_LOCK="$SCRIPT_DIR/image/source.lock"
TRUSTED_TOOLCHAIN_LOCK="$SCRIPT_DIR/image/toolchain.lock"
ROOT=""
ACTION=""
EDGE_EVIDENCE=""
STATE_EVIDENCE=""
EVIDENCE_STAGE=""
DOCKER_CONFIG_DIR=""
DOCKER_TEMP_CONFIG_DIR=""
DOCKER_CMD=()
DOCKER_USES_SUDO=0
COMPOSE_CMD=()
MUTATED=0
FAILURE_HANDLING=0
COMPOSE_ENV_FILE=""
STATE_RUNTIME_ROOT=""
RUNTIME_SNAPSHOT=""
RUNTIME_SNAPSHOT_STAGE=""
RUNTIME_SNAPSHOT_STAGE_ID=""
RUNTIME_SNAPSHOT_COMMIT_ATTEMPT=0
RUNTIME_SNAPSHOT_RETAIN=0
STOP_RUNTIME_SNAPSHOT=""
STOP_RUNTIME_SNAPSHOT_ID=""
STOP_RUNTIME_SNAPSHOT_AMBIGUOUS=0

usage() {
  cat <<'EOF'
Usage:
  deploy.sh --runtime-root /absolute/runtime/root check
  deploy.sh --runtime-root /absolute/runtime/root \
    --edge-evidence /absolute/image-evidence \
    --state-evidence /absolute/new/action-evidence start-edge
  deploy.sh --runtime-root /absolute/runtime/root \
    --state-evidence /absolute/new/action-evidence stop-edge

Run this script as the non-root runtime owner. If that user lacks docker.sock
access, only daemon/Compose calls use the already-authorized `sudo -n docker`;
never sudo this whole script and never add the user to the docker group.
start-edge requires fresh image evidence, rejects project orphans, waits for
health, and rolls back only edge+cloudflared on failure. stop-edge never touches
coturn. Every state-changing action writes a bounded evidence directory.
The start-turn token is intentionally accepted only to return a fixed error
before evidence staging or daemon access in this edge-only first release.
EOF
}

fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
remove_owned_runtime_snapshot() {
  local target=$1 expected_id=$2 current relative path
  [ -n "$target" ] && [ -n "$expected_id" ] && [ -d "$target" ] && [ ! -L "$target" ] || return 1
  current=$(stat -f '%d:%i' "$target" 2>/dev/null || stat -c '%d:%i' "$target" 2>/dev/null || true)
  [ "$current" = "$expected_id" ] || return 1
  for relative in \
    compose.env config config/edge.env config/gateway-public-key.pem \
    config/access-allowed-emails secrets secrets/cloudflare-tunnel.token; do
    [ ! -L "$target/$relative" ] || return 1
  done
  while IFS= read -r -d '' path; do
    relative=${path#"$target/"}
    case "$relative" in
      compose.env|config|config/edge.env|config/gateway-public-key.pem|\
        config/access-allowed-emails|secrets|secrets/cloudflare-tunnel.token) ;;
      *) return 1 ;;
    esac
  done < <(find "$target" -mindepth 1 -print0)
  chmod 700 "$target" "$target/config" "$target/secrets" 2>/dev/null || return 1
  chmod 600 \
    "$target/compose.env" "$target/config/edge.env" \
    "$target/config/gateway-public-key.pem" "$target/config/access-allowed-emails" \
    "$target/secrets/cloudflare-tunnel.token" 2>/dev/null || return 1
  rm -f -- \
    "$target/compose.env" "$target/config/edge.env" \
    "$target/config/gateway-public-key.pem" "$target/config/access-allowed-emails" \
    "$target/secrets/cloudflare-tunnel.token" || return 1
  rmdir "$target/config" "$target/secrets" "$target"
}
cleanup() {
  cleanup_status=$?
  local status=$cleanup_status
  trap - EXIT HUP INT TERM
  set +e
  if [ -n "$EVIDENCE_STAGE" ]; then
    if [ "$MUTATED" -eq 1 ]; then
      if [ "$FAILURE_HANDLING" -eq 0 ]; then
        handle_mutation_failure "unexpected command failure after the first container mutation (exit $status)"
      fi
    else
      case "$EVIDENCE_STAGE" in */.dji4g-edge-action.*) rm -rf -- "$EVIDENCE_STAGE" ;; esac
    fi
  fi
  if [ -n "$DOCKER_TEMP_CONFIG_DIR" ]; then
    case "$DOCKER_TEMP_CONFIG_DIR" in
      /tmp/dji4g-edge-docker-config.*) rm -rf -- "$DOCKER_TEMP_CONFIG_DIR" ;;
    esac
  fi
  if [ "$RUNTIME_SNAPSHOT_RETAIN" -eq 0 ]; then
    if [ "$RUNTIME_SNAPSHOT_COMMIT_ATTEMPT" -eq 1 ] && [ -n "$RUNTIME_SNAPSHOT" ]; then
      remove_owned_runtime_snapshot "$RUNTIME_SNAPSHOT" "$RUNTIME_SNAPSHOT_STAGE_ID" >/dev/null 2>&1 || true
    fi
    if [ -n "$RUNTIME_SNAPSHOT_STAGE" ]; then
      case "$RUNTIME_SNAPSHOT_STAGE" in
        */.*.edge-stage.??????)
          remove_owned_runtime_snapshot "$RUNTIME_SNAPSHOT_STAGE" "$RUNTIME_SNAPSHOT_STAGE_ID" >/dev/null 2>&1 || true
          ;;
      esac
    fi
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

while [ "$#" -gt 0 ]; do
  case "$1" in
    --runtime-root) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--runtime-root requires a value"; ROOT=$2; shift 2 ;;
    --edge-evidence) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--edge-evidence requires a value"; EDGE_EVIDENCE=$2; shift 2 ;;
    --state-evidence) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--state-evidence requires a value"; STATE_EVIDENCE=$2; shift 2 ;;
    check|start-edge|stop-edge|start-turn) [ -z "$ACTION" ] || fail "only one action may be supplied"; ACTION=$1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

[ -n "$ROOT" ] || fail "--runtime-root is required"
[ -n "$ACTION" ] || fail "an action is required"
[ "$ACTION" != start-turn ] ||
  fail "start-turn is not supported by this edge-only release; no TURN mutation was attempted"
case "$ROOT" in /*) ;; *) fail "--runtime-root must be absolute" ;; esac
[ "$(id -u)" -gt 0 ] && [ "$(id -g)" -gt 0 ] || fail "deploy.sh must run as the non-root runtime owner"
for name in DJI4G_PUBLIC_EDGE_ROOT DJI4G_EDGE_ENV_FILE DJI4G_ENABLED_PROFILES \
  DJI4G_EDGE_UID DJI4G_EDGE_GID DJI4G_EDGE_IMAGE DJI4G_CLOUDFLARED_IMAGE DJI4G_COTURN_IMAGE; do
  [ -z "${!name+x}" ] || fail "ambient DJI4G Compose variables are forbidden"
done

[ -x "$VERIFY" ] || fail "runtime verifier is missing or not executable"
[ -x "$STATE_VERIFY" ] || fail "state verifier is missing or not executable"
[ -x "$SOURCE_VERIFY" ] || fail "source verifier is missing or not executable"
[ -x "$ACTION_VERIFY" ] || fail "action evidence verifier is missing or not executable"
[ -x "$ATOMIC_COMMIT" ] || fail "atomic output helper is missing or not executable"
PYTHON_BIN=$(command -v python3 2>/dev/null) || fail "Python 3 is required"

validate_root_docker_config() {
  local trusted_path trusted_mode
  [ -d "$DOCKER_ROOT_CONFIG_DIR" ] && [ ! -L "$DOCKER_ROOT_CONFIG_DIR" ] ||
    fail "root-owned Docker CLI config prerequisite is missing"
  [ "$(CDPATH= cd -- "$DOCKER_ROOT_CONFIG_DIR" && pwd -P)" = "$DOCKER_ROOT_CONFIG_DIR" ] ||
    fail "root Docker config path must be physical"
  [ -f "$DOCKER_ROOT_CONFIG_DIR/config.json" ] && [ ! -L "$DOCKER_ROOT_CONFIG_DIR/config.json" ] ||
    fail "root Docker config.json prerequisite is missing or symlinked"
  [ "$(stat -f '%Lp' "$DOCKER_ROOT_CONFIG_DIR/config.json" 2>/dev/null || stat -c '%a' "$DOCKER_ROOT_CONFIG_DIR/config.json")" = 644 ] ||
    fail "root Docker config.json must be root-owned mode 0644"
  [ "$(stat -f '%l' "$DOCKER_ROOT_CONFIG_DIR/config.json" 2>/dev/null || stat -c '%h' "$DOCKER_ROOT_CONFIG_DIR/config.json")" = 1 ] ||
    fail "root Docker config.json must have exactly one hard link"
  trusted_path="$DOCKER_ROOT_CONFIG_DIR/config.json"
  while :; do
    [ "$(stat -f '%u' "$trusted_path" 2>/dev/null || stat -c '%u' "$trusted_path")" = 0 ] ||
      fail "root Docker config path chain must be root-owned"
    trusted_mode=$(stat -f '%Lp' "$trusted_path" 2>/dev/null || stat -c '%a' "$trusted_path")
    [ $((8#$trusted_mode & 8#022)) -eq 0 ] || fail "root Docker config path chain is writable"
    [ "$trusted_path" != / ] || break
    trusted_path=$(dirname -- "$trusted_path")
  done
  [ "$(shasum -a 256 "$DOCKER_ROOT_CONFIG_DIR/config.json" | awk '{print $1}')" = "$DOCKER_ROOT_CONFIG_SHA256" ] ||
    fail "root Docker config differs from toolchain.lock"
}

select_docker_daemon() {
  local docker_bin sudo_bin
  docker_bin=$(command -v docker 2>/dev/null || true)
  if [ -n "$docker_bin" ] && env -i PATH="$PATH" "$docker_bin" \
      --config "$DOCKER_CONFIG_DIR" --context default info >/dev/null 2>&1; then
    DOCKER_CMD=("$docker_bin")
    return
  fi
  sudo_bin=$(command -v sudo 2>/dev/null || true)
  if [ -n "$sudo_bin" ]; then
    validate_root_docker_config
    if ! env -i PATH="$PATH" "$sudo_bin" -n docker --config "$DOCKER_ROOT_CONFIG_DIR" \
        --context default info >/dev/null 2>&1; then
      fail "Docker daemon access requires direct access or existing sudo -n docker"
    fi
    DOCKER_CONFIG_DIR=$DOCKER_ROOT_CONFIG_DIR
    DOCKER_CMD=("$sudo_bin" -n docker)
    DOCKER_USES_SUDO=1
    return
  fi
  fail "Docker daemon access requires direct access or existing sudo -n docker"
}

docker_clean() {
  # Every daemon and Compose call uses one environment. In particular, ambient
  # DOCKER_HOST/DOCKER_CONTEXT/TLS/config and COMPOSE_* values cannot split
  # verification, mutation, evidence collection, or rollback across daemons.
  env -i PATH="$PATH" "${DOCKER_CMD[@]}" \
    --config "$DOCKER_CONFIG_DIR" --context default "$@"
}

if [ "$ACTION" = check ]; then
  "$VERIFY" --compose-mode auto --quiet "$ROOT"
  printf 'OK: runtime and hash-locked normalized Compose policy verified; all services remain profile-off by default.\n'
  exit 0
fi

[ -d "$ROOT" ] && [ ! -L "$ROOT" ] || fail "runtime root must be a real directory"
ROOT_PHYSICAL=$(CDPATH= cd -- "$ROOT" && pwd -P)
[ "$ROOT" = "$ROOT_PHYSICAL" ] || fail "runtime root must be a physical canonical path"
[ -n "$STATE_EVIDENCE" ] || fail "--state-evidence is required for state-changing actions"
case "$STATE_EVIDENCE" in /*) ;; *) fail "--state-evidence must be absolute" ;; esac
[ ! -e "$STATE_EVIDENCE" ] && [ ! -L "$STATE_EVIDENCE" ] || fail "state evidence output already exists or is a symlink"
EVIDENCE_PARENT=$(dirname -- "$STATE_EVIDENCE")
EVIDENCE_BASENAME=$(basename -- "$STATE_EVIDENCE")
case "$EVIDENCE_BASENAME" in ""|.|..|*[!A-Za-z0-9_.-]*) fail "state evidence basename is invalid" ;; esac
[ -d "$EVIDENCE_PARENT" ] && [ ! -L "$EVIDENCE_PARENT" ] || fail "state evidence parent must exist"
EVIDENCE_PARENT_PHYSICAL=$(CDPATH= cd -- "$EVIDENCE_PARENT" && pwd -P)
[ "$EVIDENCE_PARENT" = "$EVIDENCE_PARENT_PHYSICAL" ] && \
  [ "$STATE_EVIDENCE" = "$EVIDENCE_PARENT_PHYSICAL/$EVIDENCE_BASENAME" ] ||
  fail "state evidence path and parent must be physical canonical paths"
EVIDENCE_PARENT=$EVIDENCE_PARENT_PHYSICAL
[ "$(stat -f '%u' "$EVIDENCE_PARENT" 2>/dev/null || stat -c '%u' "$EVIDENCE_PARENT")" = "$(id -u)" ] ||
  fail "state evidence parent must be owned by the runtime user"
[ "$(stat -f '%Lp' "$EVIDENCE_PARENT" 2>/dev/null || stat -c '%a' "$EVIDENCE_PARENT")" = 700 ] ||
  fail "state evidence parent must have mode 0700"
case "$EVIDENCE_PARENT/" in "$ROOT_PHYSICAL/"*) fail "state evidence parent may not be inside runtime root" ;; esac
case "$ROOT_PHYSICAL/" in "$EVIDENCE_PARENT/"*) fail "runtime root may not be inside state evidence parent" ;; esac
"$PYTHON_BIN" -I "$SOURCE_VERIFY" --repo-root "$REPO_ROOT" --lock "$TRUSTED_SOURCE_LOCK" >/dev/null ||
  fail "deployed policy/runtime scripts do not match the trusted committed source lock"
EVIDENCE_STAGE=$(mktemp -d "$EVIDENCE_PARENT/.dji4g-edge-action.XXXXXX")
[ "$(stat -f '%Lp' "$EVIDENCE_STAGE" 2>/dev/null || stat -c '%a' "$EVIDENCE_STAGE")" = 700 ] ||
  fail "temporary action evidence directory is not mode 0700"
DOCKER_TEMP_CONFIG_DIR=$(mktemp -d /tmp/dji4g-edge-docker-config.XXXXXX)
chmod 700 "$DOCKER_TEMP_CONFIG_DIR"
DOCKER_CONFIG_DIR=$DOCKER_TEMP_CONFIG_DIR

read_toolchain_value() {
  local key=$1 count line
  count=$(grep -Ec "^${key}=" "$TRUSTED_TOOLCHAIN_LOCK" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once in trusted toolchain.lock"
  line=$(grep -E "^${key}=" "$TRUSTED_TOOLCHAIN_LOCK")
  printf '%s' "${line#*=}"
}
IMAGE_STORE_DRIVER=$(read_toolchain_value IMAGE_STORE_DRIVER)
DOCKER_ROOT_CONFIG_DIR=$(read_toolchain_value DOCKER_ROOT_CONFIG_DIR)
DOCKER_ROOT_CONFIG_SHA256=$(read_toolchain_value DOCKER_ROOT_CONFIG_SHA256)
DOCKER_SERVER_VERSION=$(read_toolchain_value DOCKER_SERVER_VERSION)
DOCKER_CLIENT_VERSION=$(read_toolchain_value DOCKER_CLIENT_VERSION)
TARGET_PLATFORM=$(read_toolchain_value TARGET_PLATFORM)
COMPOSE_CLI_VERSION=$(read_toolchain_value COMPOSE_CLI_VERSION)
COMPOSE_PACKAGE_NAME=$(read_toolchain_value COMPOSE_PACKAGE_NAME)
COMPOSE_PACKAGE_VERSION=$(read_toolchain_value COMPOSE_PACKAGE_VERSION)
COMPOSE_PLUGIN_PATH=$(read_toolchain_value COMPOSE_PLUGIN_PATH)
COMPOSE_SHA256=$(read_toolchain_value COMPOSE_LINUX_AMD64_SHA256)
[ -f "$COMPOSE_PLUGIN_PATH" ] && [ ! -L "$COMPOSE_PLUGIN_PATH" ] && [ -x "$COMPOSE_PLUGIN_PATH" ] ||
  fail "locked production Compose plugin is missing, symlinked, or not executable"
[ "$(shasum -a 256 "$COMPOSE_PLUGIN_PATH" | awk '{print $1}')" = "$COMPOSE_SHA256" ] ||
  fail "production Compose plugin SHA-256 differs from toolchain.lock"
DPKG_QUERY_BIN=$(command -v dpkg-query 2>/dev/null) || fail "dpkg-query is required to bind the production Compose package"
[ "$(env -i PATH="$PATH" "$DPKG_QUERY_BIN" -W -f='${Version}' "$COMPOSE_PACKAGE_NAME" 2>/dev/null)" = "$COMPOSE_PACKAGE_VERSION" ] ||
  fail "production Compose package version differs from toolchain.lock"
PLUGIN_DIR=$(dirname -- "$COMPOSE_PLUGIN_PATH")
PLUGIN_BASE=$(basename -- "$COMPOSE_PLUGIN_PATH")
[ "$PLUGIN_BASE" = docker-compose ] || fail "production Compose plugin basename must be docker-compose"
[ "$(CDPATH= cd -- "$PLUGIN_DIR" && pwd -P)/$PLUGIN_BASE" = "$COMPOSE_PLUGIN_PATH" ] ||
  fail "production Compose plugin path must be physical and canonical"
trusted_path=$COMPOSE_PLUGIN_PATH
while :; do
  [ "$(stat -f '%u' "$trusted_path" 2>/dev/null || stat -c '%u' "$trusted_path")" = 0 ] ||
    fail "production Compose plugin path chain must be root-owned"
  trusted_mode=$(stat -f '%Lp' "$trusted_path" 2>/dev/null || stat -c '%a' "$trusted_path")
  [ $((8#$trusted_mode & 8#022)) -eq 0 ] || fail "production Compose plugin path chain is group/world writable"
  [ "$trusted_path" != / ] || break
  trusted_path=$(dirname -- "$trusted_path")
done
printf '{"cliPluginsExtraDirs":["%s"]}\n' "$PLUGIN_DIR" >"$DOCKER_CONFIG_DIR/config.json"
chmod 600 "$DOCKER_CONFIG_DIR/config.json"

select_docker_daemon
if [ "$DOCKER_USES_SUDO" -eq 1 ]; then
  for candidate in \
    "$DOCKER_ROOT_CONFIG_DIR/cli-plugins/docker-compose" \
    /usr/local/lib/docker/cli-plugins/docker-compose \
    /usr/local/libexec/docker/cli-plugins/docker-compose \
    /usr/lib/docker/cli-plugins/docker-compose; do
    [ ! -e "$candidate" ] && [ ! -L "$candidate" ] ||
      fail "a higher-priority Compose plugin shadows the hash-locked system plugin: $candidate"
  done
fi
[ "$(docker_clean version --format '{{.Server.Version}}')" = "$DOCKER_SERVER_VERSION" ] ||
  fail "Docker server version differs from the audited toolchain"
[ "$(docker_clean version --format '{{.Client.Version}}')" = "$DOCKER_CLIENT_VERSION" ] ||
  fail "Docker client version differs from the audited toolchain"
[ "$(docker_clean info --format '{{.OSType}}/{{.Architecture}}')" = "$TARGET_PLATFORM" ] ||
  fail "Docker daemon platform differs from the audited toolchain"
[ "$(docker_clean info --format '{{json .DriverStatus}}')" = \
    "[[\"driver-type\",\"$IMAGE_STORE_DRIVER\"]]" ] ||
  fail "Docker image store must match the audited containerd contract"
[ "$(docker_clean info --format '{{json .SecurityOptions}}')" = \
    '["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]' ] ||
  fail "Docker daemon security options differ from the audited AppArmor/seccomp/cgroupns contract"
"$VERIFY" --compose-mode production --quiet "$ROOT"
[ "$(docker_clean compose version --short | sed 's/^v//')" = "$COMPOSE_CLI_VERSION" ] ||
  fail "Docker Compose runtime version differs from toolchain.lock"
COMPOSE_CMD=(compose)

read_env_value() {
  local file=$1 key=$2 line
  line=$(grep -E "^${key}=" "$file")
  printf '%s' "${line#*=}"
}
load_runtime_values() {
  local file=$1
  ENV_ROOT=$(read_env_value "$file" DJI4G_PUBLIC_EDGE_ROOT)
  ENV_EDGE_ENV_FILE=$(read_env_value "$file" DJI4G_EDGE_ENV_FILE)
  ENV_PROFILES=$(read_env_value "$file" DJI4G_ENABLED_PROFILES)
  ENV_UID=$(read_env_value "$file" DJI4G_EDGE_UID)
  ENV_GID=$(read_env_value "$file" DJI4G_EDGE_GID)
  ENV_EDGE_IMAGE=$(read_env_value "$file" DJI4G_EDGE_IMAGE)
  ENV_CLOUDFLARED_IMAGE=$(read_env_value "$file" DJI4G_CLOUDFLARED_IMAGE)
  ENV_COTURN_IMAGE=$(read_env_value "$file" DJI4G_COTURN_IMAGE)
}
COMPOSE_ENV_FILE="$ROOT/compose.env"
STATE_RUNTIME_ROOT=$ROOT
load_runtime_values "$COMPOSE_ENV_FILE"

compose_clean() {
  # Use the already verified file rather than environment forwarding. sudo's
  # env_reset may strip DJI4G_* variables, while an explicit --env-file keeps
  # direct Docker and `sudo -n docker` behavior identical and auditable.
  docker_clean "${COMPOSE_CMD[@]}" --env-file "$COMPOSE_ENV_FILE" -f "$COMPOSE" "$@"
}

prepare_runtime_snapshot() {
  local parent current_id committed_id basename nonce candidate candidate_file existing=0
  parent=$(dirname -- "$ROOT_PHYSICAL")
  [ -d "$parent" ] && [ ! -L "$parent" ] && \
    [ "$(CDPATH= cd -- "$parent" && pwd -P)" = "$parent" ] ||
    fail "runtime snapshot parent must be a physical directory"
  [ "$(stat -f '%u' "$parent" 2>/dev/null || stat -c '%u' "$parent")" = "$(id -u)" ] && \
    [ "$(stat -f '%Lp' "$parent" 2>/dev/null || stat -c '%a' "$parent")" = 700 ] ||
    fail "runtime snapshot parent must be runtime-user-owned mode 0700"
  basename=$(basename -- "$ROOT_PHYSICAL")
  candidate_file="$EVIDENCE_STAGE/runtime-snapshot-candidates.tmp"
  [ ! -e "$candidate_file" ] && [ ! -L "$candidate_file" ] ||
    fail "runtime snapshot candidate stage already exists"
  find "$parent" -mindepth 1 -maxdepth 1 \
    -name ".$basename.edge-snapshot.??????" -print0 >"$candidate_file" ||
    fail "could not enumerate existing runtime snapshots"
  while IFS= read -r -d '' candidate; do
    existing=$((existing + 1))
  done <"$candidate_file"
  rm -f -- "$candidate_file" || fail "could not remove runtime snapshot candidate stage"
  [ "$existing" -eq 0 ] ||
    fail "an existing runtime snapshot must be removed by stop-edge before another start"
  RUNTIME_SNAPSHOT_STAGE=$(mktemp -d "$parent/.$basename.edge-stage.XXXXXX")
  nonce=${RUNTIME_SNAPSHOT_STAGE##*.}
  RUNTIME_SNAPSHOT="$parent/.$basename.edge-snapshot.$nonce"
  [ "${#RUNTIME_SNAPSHOT}" -le 240 ] || fail "runtime snapshot path is too long"
  chmod 700 "$RUNTIME_SNAPSHOT_STAGE"
  RUNTIME_SNAPSHOT_STAGE_ID=$(stat -f '%d:%i' "$RUNTIME_SNAPSHOT_STAGE" 2>/dev/null || stat -c '%d:%i' "$RUNTIME_SNAPSHOT_STAGE")
  "$PYTHON_BIN" -I "$ACTION_VERIFY" snapshot --source-root "$ROOT_PHYSICAL" \
    --declared-root "$RUNTIME_SNAPSHOT" --output "$RUNTIME_SNAPSHOT_STAGE" ||
    fail "could not create the private immutable runtime snapshot"
  "$VERIFY" --compose-mode production --immutable-snapshot \
    --declared-root "$RUNTIME_SNAPSHOT" --quiet "$RUNTIME_SNAPSHOT_STAGE" ||
    fail "staged immutable runtime snapshot failed verification"
  RUNTIME_SNAPSHOT_COMMIT_ATTEMPT=1
  committed_id=$(env -i PATH="$PATH" "$PYTHON_BIN" -I "$ATOMIC_COMMIT" \
    --source "$RUNTIME_SNAPSHOT_STAGE" --target "$RUNTIME_SNAPSHOT") ||
    fail "immutable runtime snapshot atomic commit failed"
  [ "$committed_id" = "$RUNTIME_SNAPSHOT_STAGE_ID" ] ||
    fail "immutable runtime snapshot commit returned a different inode"
  current_id=$(stat -f '%d:%i' "$RUNTIME_SNAPSHOT" 2>/dev/null || stat -c '%d:%i' "$RUNTIME_SNAPSHOT")
  [ "$current_id" = "$RUNTIME_SNAPSHOT_STAGE_ID" ] ||
    fail "immutable runtime snapshot target inode changed after commit"
  RUNTIME_SNAPSHOT_STAGE=""
  "$VERIFY" --compose-mode production --immutable-snapshot --quiet "$RUNTIME_SNAPSHOT" ||
    fail "committed immutable runtime snapshot failed verification"
  COMPOSE_ENV_FILE="$RUNTIME_SNAPSHOT/compose.env"
  STATE_RUNTIME_ROOT=$RUNTIME_SNAPSHOT
  load_runtime_values "$COMPOSE_ENV_FILE"
}

discover_stop_runtime_snapshot() {
  local parent basename candidate candidate_file count=0
  STOP_RUNTIME_SNAPSHOT=""
  STOP_RUNTIME_SNAPSHOT_ID=""
  STOP_RUNTIME_SNAPSHOT_AMBIGUOUS=0
  parent=$(dirname -- "$ROOT_PHYSICAL")
  basename=$(basename -- "$ROOT_PHYSICAL")
  candidate_file="$EVIDENCE_STAGE/runtime-snapshot-candidates.tmp"
  if [ -e "$candidate_file" ] || [ -L "$candidate_file" ] || ! \
      find "$parent" -mindepth 1 -maxdepth 1 \
        -name ".$basename.edge-snapshot.??????" -print0 >"$candidate_file"; then
    rm -f -- "$candidate_file" >/dev/null 2>&1 || true
    STOP_RUNTIME_SNAPSHOT_AMBIGUOUS=1
    return 0
  fi
  while IFS= read -r -d '' candidate; do
    count=$((count + 1))
    if [ "$count" -eq 1 ]; then STOP_RUNTIME_SNAPSHOT=$candidate; fi
  done <"$candidate_file"
  if ! rm -f -- "$candidate_file"; then
    STOP_RUNTIME_SNAPSHOT=""
    STOP_RUNTIME_SNAPSHOT_AMBIGUOUS=1
    return 0
  fi
  if [ "$count" -gt 1 ]; then
    STOP_RUNTIME_SNAPSHOT=""
    STOP_RUNTIME_SNAPSHOT_AMBIGUOUS=1
  elif [ "$count" -eq 1 ] && [ -d "$STOP_RUNTIME_SNAPSHOT" ] && \
      [ ! -L "$STOP_RUNTIME_SNAPSHOT" ]; then
    STOP_RUNTIME_SNAPSHOT_ID=$(stat -f '%d:%i' "$STOP_RUNTIME_SNAPSHOT" 2>/dev/null || \
      stat -c '%d:%i' "$STOP_RUNTIME_SNAPSHOT" 2>/dev/null || true)
  fi
}

collect_state() {
  local output=$1 ids base control_name turn_name project_names
  local containers_file control_file turn_file
  base=${output%.json}
  containers_file="$base.containers.tmp"
  control_file="$base.control-network.tmp"
  turn_file="$base.turn-network.tmp"
  control_name=dji4g-public-edge_public-edge-control
  turn_name=dji4g-public-edge_public-edge-turn
  ids=$(docker_clean ps -aq --filter label=com.docker.compose.project=dji4g-public-edge) || return 1
  if [ -z "$ids" ]; then
    printf '[]\n' >"$containers_file" || return 1
  else
    for id in $ids; do
      if ! [[ "$id" =~ ^[0-9a-f]{12,64}$ ]]; then
        printf 'ERROR: Docker returned an invalid container ID\n' >&2
        return 1
      fi
    done
    # IDs are validated lowercase hex words above.
    # shellcheck disable=SC2086
    docker_clean inspect $ids >"$containers_file" || return 1
  fi
  inspect_network_or_null() {
    local name=$1 target=$2 matches network_id returned_name
    matches=$(docker_clean network ls --filter "name=^${name}$" --format '{{.ID}} {{.Name}}') || return 1
    if [ -z "$matches" ]; then printf 'null\n' >"$target"; return; fi
    [ "$(printf '%s\n' "$matches" | awk 'NF {count++} END {print count+0}')" -eq 1 ] || return 1
    read -r network_id returned_name <<EOF
$matches
EOF
    [[ "$network_id" =~ ^[0-9a-f]{12,64}$ ]] && [ "$returned_name" = "$name" ] || return 1
    docker_clean network inspect "$network_id" >"$target" || return 1
  }
  inspect_network_or_null "$control_name" "$control_file" || return 1
  inspect_network_or_null "$turn_name" "$turn_file" || return 1
  project_names=$(docker_clean network ls \
    --filter label=com.docker.compose.project=dji4g-public-edge --format '{{.Name}}') || return 1
  if printf '%s\n' "$project_names" | awk 'NF' | grep -Evq \
      '^(dji4g-public-edge_public-edge-control|dji4g-public-edge_public-edge-turn)$'; then
    printf 'ERROR: unapproved project network exists\n' >&2
    return 1
  fi
  if ! "$PYTHON_BIN" -I - "$containers_file" "$control_file" "$turn_file" "$project_names" >"$output" <<'PY'
import json,pathlib,sys
def load(path):return json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
def network(path):
    value=load(path)
    if value is None:return None
    if not isinstance(value,list) or len(value)!=1 or not isinstance(value[0],dict):raise SystemExit(1)
    return value[0]
names=sorted(line for line in sys.argv[4].splitlines() if line)
print(json.dumps({"containers":load(sys.argv[1]),"networks":{"control":network(sys.argv[2]),"turn":network(sys.argv[3])},"project_network_names":names},sort_keys=True,separators=(",",":")))
PY
  then
    return 1
  fi
  for target in "$containers_file" "$control_file" "$turn_file"; do
    rm -f "$target" || return 1
  done
}

verify_state() {
  local phase=$1 input=$2 output=$3 baseline=${4:-}
  if [ -n "$baseline" ]; then
    "$PYTHON_BIN" -I "$STATE_VERIFY" --phase "$phase" \
      --edge-image "$ENV_EDGE_IMAGE" --cloudflared-image "$ENV_CLOUDFLARED_IMAGE" \
      --coturn-image "$ENV_COTURN_IMAGE" --uid-gid "$ENV_UID:$ENV_GID" \
      --runtime-root "$STATE_RUNTIME_ROOT" --compose-file "$COMPOSE" \
      --baseline "$baseline" "$input" >"$output"
  else
    "$PYTHON_BIN" -I "$STATE_VERIFY" --phase "$phase" \
      --edge-image "$ENV_EDGE_IMAGE" --cloudflared-image "$ENV_CLOUDFLARED_IMAGE" \
      --coturn-image "$ENV_COTURN_IMAGE" --uid-gid "$ENV_UID:$ENV_GID" \
      --runtime-root "$STATE_RUNTIME_ROOT" --compose-file "$COMPOSE" \
      "$input" >"$output"
  fi
}

cleanup_control_network() {
  local mode=$1 checkpoint=$2 summary=$3 phase network_id
  case "$mode" in
    stop) phase=pre-control-cleanup-stop ;;
    rollback) phase=pre-control-cleanup-rollback ;;
    *) return 1 ;;
  esac
  collect_state "$checkpoint" || return 1
  verify_state "$phase" "$checkpoint" "$summary" "$EVIDENCE_STAGE/pre-inspect.json" || return 1
  network_id=$("$PYTHON_BIN" -I - "$summary" <<'PY'
import json,pathlib,re,sys
value=json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
identifier=value.get("remove_control_network_id")
if identifier is None: print("")
elif isinstance(identifier,str) and re.fullmatch(r"[0-9a-f]{64}",identifier): print(identifier)
else: raise SystemExit(1)
PY
  ) || return 1
  if [ -n "$network_id" ]; then
    docker_clean network rm "$network_id" >/dev/null || return 1
  fi
}

verify_local_image() {
  local ref=$1 image_id platform digests
  image_id=$(docker_clean image inspect --format '{{.Id}}' "$ref" 2>/dev/null) ||
    fail "pinned image is not present locally: $ref"
  platform=$(docker_clean image inspect --format '{{.Os}}/{{.Architecture}}' "$ref")
  [ "$platform" = linux/amd64 ] || fail "image is not linux/amd64: $ref"
  if [[ "$ref" =~ ^sha256: ]]; then
    [ "$ref" = "$image_id" ] || fail "local image ID mismatch"
  else
    digests=$(docker_clean image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref")
    printf '%s\n' "$digests" | grep -Fqx "$ref" || fail "repository digest is not locally proven: $ref"
    [ "$image_id" = "sha256:${ref##*@sha256:}" ] ||
      fail "repository digest does not equal the Docker29 containerd target ID: $ref"
  fi
}

redact_action_evidence() {
  "$PYTHON_BIN" -I - "$STATE_RUNTIME_ROOT/secrets/cloudflare-tunnel.token" \
    "$ROOT/secrets/turn-auth-secret" "$ROOT/secrets/turn-tls-key.pem" \
    "$EVIDENCE_STAGE" <<'PY'
import pathlib, sys
cloud=pathlib.Path(sys.argv[1]).read_bytes().rstrip(b"\n")
turn_path=pathlib.Path(sys.argv[2]);key_path=pathlib.Path(sys.argv[3]);root=pathlib.Path(sys.argv[4])
if not cloud:
    raise SystemExit("ERROR: Cloudflare token file is empty")
secrets=[(cloud,b"[REDACTED_CLOUDFLARE_TOKEN]")]
if turn_path.exists():
    turn=turn_path.read_bytes().rstrip(b"\n")
    if not turn: raise SystemExit("ERROR: TURN auth secret file is empty")
    secrets.append((turn,b"[REDACTED_TURN_AUTH_SECRET]"))
if key_path.exists():
    key=key_path.read_bytes()
    if not key: raise SystemExit("ERROR: TURN TLS private key file is empty")
    secrets.append((key,b"[REDACTED_TURN_TLS_PRIVATE_KEY]"))
    for line in key.splitlines():
        if len(line) >= 16 and b"PRIVATE KEY-----" not in line:
            secrets.append((line,b"[REDACTED_TURN_TLS_PRIVATE_KEY_DATA]"))
fragments=set()
for secret,_ in secrets:
    if len(secret)>=16:
        fragments.update(secret[index:index+16] for index in range(len(secret)-15))
for path in root.iterdir():
    if path.is_file() and not path.is_symlink():
        data=path.read_bytes()
        for secret,replacement in secrets:
            if secret in data:data=data.replace(secret,replacement)
        if b"-----BEGIN " in data and b"PRIVATE KEY-----" in data:
            raise SystemExit("ERROR: action evidence contains a private-key marker")
        if any(fragment in data for fragment in fragments):
            raise SystemExit("ERROR: action evidence contains a 16-byte secret fragment")
        path.write_bytes(data)
for path in root.iterdir():
    if path.is_file() and not path.is_symlink():
        data=path.read_bytes()
        if any(secret in data for secret,_ in secrets) or any(fragment in data for fragment in fragments):
            raise SystemExit("ERROR: action evidence still contains secret bytes/fragments")
PY
}

replace_with_safe_failure_evidence() {
  # Redaction failure means no collected Docker/log byte is publishable. Erase
  # every staged regular artifact and rebuild only constant local status text.
  "$PYTHON_BIN" -I - "$EVIDENCE_STAGE" <<'PY'
import hashlib,json,os,pathlib,re,stat,sys
root=pathlib.Path(sys.argv[1])
anchor_path=root/"runtime-inputs.pre.json"
try:
    raw=anchor_path.read_text(encoding="ascii")
    anchor=json.loads(raw)
except (OSError,UnicodeError,json.JSONDecodeError):
    raise SystemExit("ERROR: unsafe runtime input anchor cannot be preserved")
expected={"compose.env","config/edge.env","config/gateway-public-key.pem","config/access-allowed-emails"}
if (raw!=json.dumps(anchor,sort_keys=True,separators=(",",":"))+"\n" or
    not isinstance(anchor,dict) or set(anchor)!=expected or
    any(not isinstance(item,str) or re.fullmatch(r"[0-9a-f]{64}",item) is None for item in anchor.values())):
    raise SystemExit("ERROR: unsafe runtime input anchor cannot be preserved")
recorded={
    "runtime-input-compose.env":"compose.env",
    "runtime-input-edge.env":"config/edge.env",
    "runtime-input-gateway-public-key.pem":"config/gateway-public-key.pem",
    "runtime-input-access-allowed-emails":"config/access-allowed-emails",
}
for path in root.iterdir():
    value=path.lstat()
    if stat.S_ISREG(value.st_mode):
        if path.name == "runtime-inputs.pre.json":
            continue
        if path.name in recorded:
            logical=recorded[path.name]
            if hashlib.sha256(path.read_bytes()).hexdigest()!=anchor[logical]:
                raise SystemExit("ERROR: recorded runtime input cannot be safely preserved")
            continue
        with path.open("wb") as handle:
            handle.truncate(0)
        path.unlink()
    elif stat.S_ISLNK(value.st_mode):
        path.unlink()
    else:
        raise SystemExit("ERROR: unsafe evidence reset found a non-file artifact")
(root/"failure-detail.txt").write_text(
    "evidence redaction failed; all collected external bytes were erased\n",
    encoding="ascii",
)
PY
}

record_failure_detail() {
  local detail=$1
  if ! printf '%s\n' "$detail" >>"$EVIDENCE_STAGE/failure-detail.txt"; then
    printf 'ERROR: additionally failed to write evidence detail: %s\n' "$detail" >&2
  fi
}

secure_action_evidence() {
  local artifact failed=0
  chmod 700 "$EVIDENCE_STAGE" || failed=1
  for artifact in "$EVIDENCE_STAGE"/*; do
    [ -e "$artifact" ] || continue
    if [ -L "$artifact" ] || [ ! -f "$artifact" ]; then
      printf 'ERROR: action evidence contains a non-regular artifact: %s\n' "$artifact" >&2
      failed=1
    elif ! chmod 600 "$artifact"; then
      failed=1
    fi
  done
  [ "$failed" -eq 0 ]
}

create_action_evidence() {
  rm -f -- "$EVIDENCE_STAGE/action.json" || return 1
  if [ "$ACTION" = start-edge ]; then
    "$PYTHON_BIN" -I "$ACTION_VERIFY" create --evidence "$EVIDENCE_STAGE" \
      --runtime-root "$STATE_RUNTIME_ROOT" --configured-runtime-root "$ROOT_PHYSICAL" \
      --uid-gid "$ENV_UID:$ENV_GID" \
      --edge-image "$ENV_EDGE_IMAGE" --cloudflared-image "$ENV_CLOUDFLARED_IMAGE" \
      --runtime-input-anchor "$EVIDENCE_STAGE/runtime-inputs.pre.json" \
      --edge-evidence "$EDGE_EVIDENCE" \
      --source-lock "$TRUSTED_SOURCE_LOCK" --toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK" \
      --compose "$COMPOSE" || return 1
    "$PYTHON_BIN" -I "$ACTION_VERIFY" verify --evidence "$EVIDENCE_STAGE" \
      --source-lock "$TRUSTED_SOURCE_LOCK" --toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK" \
      --compose "$COMPOSE" --repo-root "$REPO_ROOT" \
      --edge-evidence "$EDGE_EVIDENCE" >/dev/null
  else
    "$PYTHON_BIN" -I "$ACTION_VERIFY" create --evidence "$EVIDENCE_STAGE" \
      --runtime-root "$STATE_RUNTIME_ROOT" --configured-runtime-root "$ROOT_PHYSICAL" \
      --uid-gid "$ENV_UID:$ENV_GID" \
      --edge-image "$ENV_EDGE_IMAGE" --cloudflared-image "$ENV_CLOUDFLARED_IMAGE" \
      --runtime-input-anchor "$EVIDENCE_STAGE/runtime-inputs.pre.json" \
      --source-lock "$TRUSTED_SOURCE_LOCK" --toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK" \
      --compose "$COMPOSE" || return 1
    "$PYTHON_BIN" -I "$ACTION_VERIFY" verify --evidence "$EVIDENCE_STAGE" \
      --source-lock "$TRUSTED_SOURCE_LOCK" --toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK" \
      --compose "$COMPOSE" --repo-root "$REPO_ROOT" >/dev/null
  fi
}

anchor_runtime_inputs() {
  [ ! -e "$EVIDENCE_STAGE/runtime-inputs.pre.json" ] && \
    [ ! -L "$EVIDENCE_STAGE/runtime-inputs.pre.json" ] || return 1
  "$PYTHON_BIN" -I "$ACTION_VERIFY" anchor --runtime-root "$STATE_RUNTIME_ROOT" \
    --output "$EVIDENCE_STAGE/runtime-inputs.pre.json"
}

attempt_edge_rollback() {
  local failed=0
  if ! compose_clean --profile edge stop --timeout 20 edge cloudflared >/dev/null 2>&1; then
    record_failure_detail "rollback stop command failed"
    failed=1
  fi
  if ! compose_clean --profile edge rm -f -s -v edge cloudflared >/dev/null 2>&1; then
    record_failure_detail "rollback remove command failed"
    failed=1
  fi
  if ! cleanup_control_network rollback "$EVIDENCE_STAGE/rollback-pre-network-remove.json" \
      "$EVIDENCE_STAGE/rollback-pre-network-remove-summary.json"; then
    record_failure_detail "rollback exact control-network cleanup failed"
    failed=1
  fi
  if ! collect_state "$EVIDENCE_STAGE/rollback-inspect.json"; then
    record_failure_detail "rollback state collection failed"
    failed=1
  elif ! verify_state post-rollback-edge "$EVIDENCE_STAGE/rollback-inspect.json" \
      "$EVIDENCE_STAGE/rollback-summary.json" "$EVIDENCE_STAGE/pre-inspect.json"; then
    record_failure_detail "rollback state/coturn verification failed"
    failed=1
  fi
  [ "$failed" -eq 0 ]
}

write_failure_result() {
  local reason=$1 collection=$2 rollback=$3 evidence=$4 coturn=$5
  printf 'status=failed\naction=%s\nreason=%s\ncollection_status=%s\nrollback_status=%s\nevidence_status=%s\ncoturn_status=%s\n' \
    "$ACTION" "$reason" "$collection" "$rollback" "$evidence" "$coturn" \
    >"$EVIDENCE_STAGE/result.txt"
}

handle_mutation_failure() {
  local reason=$1 category=${2:-operation} collection=complete rollback=not-applicable evidence=complete coturn=not-applicable safe_to_publish=1
  local preserved=$EVIDENCE_STAGE rollback_cleanup_safe=0
  FAILURE_HANDLING=1
  set +e
  if [ "$category" = evidence ]; then collection=incomplete; evidence=incomplete; fi
  record_failure_detail "failure after mutation: $reason"

  case "$ACTION" in
    start-edge)
      coturn=not-proven
      if ! compose_clean --profile edge logs --no-color --tail 200 edge cloudflared \
          >"$EVIDENCE_STAGE/failure-startup.log" 2>&1; then
        record_failure_detail "startup log collection failed"
        collection=incomplete
      fi
      if attempt_edge_rollback; then
        rollback=completed
        rollback_cleanup_safe=1
        coturn=exact-pre-action-fingerprint-verified
      else
        rollback=incomplete
        collection=incomplete
      fi
      ;;
    stop-edge)
      coturn=not-proven
      if ! collect_state "$EVIDENCE_STAGE/failure-inspect.json"; then
        record_failure_detail "post-failure state collection failed; current container facts are incomplete"
        collection=incomplete
      elif ! verify_state pre "$EVIDENCE_STAGE/failure-inspect.json" \
          "$EVIDENCE_STAGE/failure-summary.json"; then
        record_failure_detail "post-failure state verification failed; current container facts require inspection"
        collection=incomplete
      else
        coturn=current-facts-collected-but-unchanged-not-proven
      fi
      ;;
  esac

  if ! redact_action_evidence; then
    if replace_with_safe_failure_evidence; then
      collection=incomplete
      record_failure_detail "evidence redaction failed; collected external bytes were erased"
      if [ "$rollback" = completed ]; then
        rollback=not-proven-after-evidence-erasure
        coturn=not-proven
      fi
    else
      collection=incomplete
      printf 'ERROR: evidence redaction and safe reset both failed; private stage was not published\n' >&2
      safe_to_publish=0
    fi
    evidence=incomplete
  fi
  if [ "$collection" = incomplete ]; then evidence=incomplete; fi
  if ! write_failure_result "$reason" "$collection" "$rollback" "$evidence" "$coturn"; then
    printf 'ERROR: could not write result.txt in %s\n' "$EVIDENCE_STAGE" >&2
    evidence=incomplete
  fi
  if ! create_action_evidence; then
    evidence=incomplete
    record_failure_detail "action evidence manifest creation/verification failed"
    if ! write_failure_result "$reason" "$collection" "$rollback" "$evidence" "$coturn"; then
      printf 'ERROR: could not rewrite result after action-manifest failure\n' >&2
    fi
    safe_to_publish=0
  fi
  if [ "$safe_to_publish" -eq 1 ] && ! secure_action_evidence; then
    evidence=incomplete
    record_failure_detail "evidence permission finalization failed"
    if ! write_failure_result "$reason" "$collection" "$rollback" "$evidence" "$coturn"; then
      printf 'ERROR: retry writing incomplete result failed\n' >&2
    fi
    if ! create_action_evidence; then
      printf 'ERROR: retry creating action manifest failed\n' >&2
    fi
    if ! secure_action_evidence; then
      printf 'ERROR: retry securing incomplete evidence failed\n' >&2
    fi
  fi
  if [ "$safe_to_publish" -eq 0 ]; then
    chmod 700 "$EVIDENCE_STAGE" >/dev/null 2>&1 || true
    preserved=$EVIDENCE_STAGE
  else
    trap '' HUP INT TERM
  fi
  if [ "$safe_to_publish" -eq 1 ] && \
      env -i PATH="$PATH" "$PYTHON_BIN" -I "$ATOMIC_COMMIT" \
        --source "$EVIDENCE_STAGE" --target "$STATE_EVIDENCE" >/dev/null; then
    EVIDENCE_STAGE=""
    preserved=$STATE_EVIDENCE
  elif [ "$safe_to_publish" -eq 1 ]; then
    evidence=incomplete
    record_failure_detail "evidence final move failed; temporary directory is the only preserved evidence"
    if ! write_failure_result "$reason" "$collection" "$rollback" "$evidence" "$coturn"; then
      printf 'ERROR: could not record final move failure in result.txt\n' >&2
    fi
    if ! create_action_evidence >/dev/null 2>&1; then
      printf 'ERROR: could not rebuild action manifest after final move failure\n' >&2
    fi
    if ! chmod 700 "$EVIDENCE_STAGE"; then
      printf 'ERROR: could not restore mode 0700 on temporary evidence directory\n' >&2
    fi
    preserved=$EVIDENCE_STAGE
  fi
  printf 'ERROR: %s; rollback_status=%s; evidence_status=%s; evidence=%s\n' \
    "$reason" "$rollback" "$evidence" "$preserved" >&2
  if [ "$rollback_cleanup_safe" -eq 1 ]; then
    RUNTIME_SNAPSHOT_RETAIN=0
  fi
}

abort_after_mutation() {
  handle_mutation_failure "$1" "${2:-operation}"
  exit 1
}

PRE_PHASE=pre
case "$ACTION" in
  start-edge) PRE_PHASE=pre-start-edge ;;
  stop-edge) PRE_PHASE=pre-stop-edge ;;
esac
collect_state "$EVIDENCE_STAGE/pre-inspect.json" || fail "pre-action state collection failed"
verify_state "$PRE_PHASE" "$EVIDENCE_STAGE/pre-inspect.json" "$EVIDENCE_STAGE/pre-summary.json" ||
  fail "pre-action state/orphan verification failed"
if [ "$ACTION" = stop-edge ]; then discover_stop_runtime_snapshot; fi

case "$ACTION" in
  start-edge)
    [ "$ENV_PROFILES" = edge ] || fail "first public start-edge requires an edge-only runtime root"
    [ -n "$EDGE_EVIDENCE" ] || fail "--edge-evidence is required for start-edge"
    case "$EDGE_EVIDENCE" in /*) ;; *) fail "--edge-evidence must be absolute" ;; esac
    prepare_runtime_snapshot
    [ "$ENV_PROFILES" = edge ] || fail "immutable runtime snapshot is not edge-only"
    "$PYTHON_BIN" -I "$IMAGE_VERIFY" --evidence "$EDGE_EVIDENCE" --image "$ENV_EDGE_IMAGE" \
      --repo-root "$REPO_ROOT" --trusted-source-lock "$TRUSTED_SOURCE_LOCK" \
      --trusted-toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK"
    verify_local_image "$ENV_EDGE_IMAGE"
    verify_local_image "$ENV_CLOUDFLARED_IMAGE"
    anchor_runtime_inputs || fail "could not anchor runtime identity inputs before start-edge mutation"
    RUNTIME_SNAPSHOT_RETAIN=1
    MUTATED=1
    if ! compose_clean --profile edge up -d --no-build --pull never --wait --wait-timeout 60 edge cloudflared; then
      abort_after_mutation "start-edge Compose up/wait failed"
    fi
    collect_state "$EVIDENCE_STAGE/post-inspect.json" || abort_after_mutation "post-start state collection failed" evidence
    verify_state post-edge "$EVIDENCE_STAGE/post-inspect.json" "$EVIDENCE_STAGE/post-summary.json" \
      "$EVIDENCE_STAGE/pre-inspect.json" || abort_after_mutation "post-start state/health/coturn verification failed" evidence
    compose_clean --profile edge logs --no-color --tail 200 edge cloudflared \
      >"$EVIDENCE_STAGE/startup.log" 2>&1 || abort_after_mutation "post-start log collection failed" evidence
    printf 'status=completed\naction=start-edge\nevidence_status=complete\nrollback_status=not-required\ncoturn_status=exact-pre-action-fingerprint-verified\n' \
      >"$EVIDENCE_STAGE/result.txt" || abort_after_mutation "success result evidence write failed" evidence
    ;;
  stop-edge)
    anchor_runtime_inputs || fail "could not anchor runtime identity inputs before stop-edge mutation"
    MUTATED=1
    if ! compose_clean --profile edge stop --timeout 20 edge cloudflared; then
      abort_after_mutation "stop-edge could not stop the selected services; no removal claim"
    fi
    if ! compose_clean --profile edge rm -f -s -v edge cloudflared; then
      abort_after_mutation "stop-edge could not remove every selected container; cleanup is incomplete"
    fi
    cleanup_control_network stop "$EVIDENCE_STAGE/pre-network-remove.json" \
      "$EVIDENCE_STAGE/pre-network-remove-summary.json" || \
      abort_after_mutation "stop-edge could not prove/remove the exact control network" evidence
    collect_state "$EVIDENCE_STAGE/post-inspect.json" || abort_after_mutation "post-stop state collection failed" evidence
    verify_state post-stop-edge "$EVIDENCE_STAGE/post-inspect.json" "$EVIDENCE_STAGE/post-summary.json" \
      "$EVIDENCE_STAGE/pre-inspect.json" || abort_after_mutation "post-stop state/coturn verification failed" evidence
    [ "$STOP_RUNTIME_SNAPSHOT_AMBIGUOUS" -eq 0 ] ||
      abort_after_mutation "stop-edge removed its containers/network but found ambiguous runtime snapshots" evidence
    if [ -n "$STOP_RUNTIME_SNAPSHOT" ]; then
      [ -n "$STOP_RUNTIME_SNAPSHOT_ID" ] && \
        remove_owned_runtime_snapshot "$STOP_RUNTIME_SNAPSHOT" "$STOP_RUNTIME_SNAPSHOT_ID" ||
        abort_after_mutation "stop-edge removed its containers/network but could not remove the exact runtime snapshot" evidence
      STOP_RUNTIME_SNAPSHOT=""
      STOP_RUNTIME_SNAPSHOT_ID=""
    fi
    discover_stop_runtime_snapshot
    [ "$STOP_RUNTIME_SNAPSHOT_AMBIGUOUS" -eq 0 ] && [ -z "$STOP_RUNTIME_SNAPSHOT" ] ||
      abort_after_mutation "stop-edge found an unremoved or concurrently-created runtime snapshot" evidence
    printf 'status=completed\naction=stop-edge\nevidence_status=complete\nrollback_status=not-applicable\ncoturn_status=exact-pre-action-fingerprint-verified\n' \
      >"$EVIDENCE_STAGE/result.txt" || abort_after_mutation "success result evidence write failed" evidence
    ;;
esac

redact_action_evidence || abort_after_mutation "success evidence redaction failed" evidence
create_action_evidence || abort_after_mutation "success action evidence manifest failed" evidence
secure_action_evidence || abort_after_mutation "success evidence permission finalization failed" evidence
trap '' HUP INT TERM
env -i PATH="$PATH" "$PYTHON_BIN" -I "$ATOMIC_COMMIT" \
  --source "$EVIDENCE_STAGE" --target "$STATE_EVIDENCE" >/dev/null || \
  abort_after_mutation "success evidence atomic commit failed" evidence
EVIDENCE_STAGE=""
MUTATED=0
printf 'OK: %s completed; evidence=%s\n' "$ACTION" "$STATE_EVIDENCE"
