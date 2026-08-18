#!/usr/bin/env bash
set -euo pipefail

umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../../.." && pwd -P)
SOURCE_LOCK="$SCRIPT_DIR/source.lock"
TOOLCHAIN_LOCK="$SCRIPT_DIR/toolchain.lock"
VERIFY_SOURCE="$SCRIPT_DIR/verify-source.py"
VERIFY_BUILD="$SCRIPT_DIR/verify-build-bundle.py"
SANITIZE_ARCHIVE="$SCRIPT_DIR/sanitize-image-archive.py"
ATOMIC_COMMIT="$SCRIPT_DIR/atomic-commit.py"
OUTPUT=""
REPOSITORY=""
BUILDX_BIN=""
APPLY=0
TMP_CONTEXT=""
TMP_OUTPUT=""
DOCKER_CONFIG_DIR=""
DOCKER_TEMP_CONFIG_DIR=""
DOCKER_CMD=()
DOCKER_USES_SUDO=0
OWNED_TAG_A_ID=""
OWNED_TAG_B_ID=""
ATTEMPT_TAG_A=0
ATTEMPT_TAG_B=0
RETAINED_TAG_COMMITTED=0
METADATA_FILE_A=""
METADATA_FILE_B=""
OUTPUT_COMMIT_ATTEMPT=0
OUTPUT_COMMITTED=0
OUTPUT_STAGE_ID=""

usage() {
  cat <<'EOF'
Usage:
  build-image.sh --output /absolute/new/build-dir \
    --repository registry.example/owner/djonehub-edge \
    [--buildx-bin /absolute/pinned/docker-buildx] [--apply]

Dry-run is the default: it verifies every locked source input and prints the
planned immutable build without contacting Docker. --apply requires the exact
locked Docker Engine on native linux/amd64, builds twice without cache, refuses
non-identical image IDs, and writes an air-gap archive plus exact local image
ID. --apply also requires the separately acquired Buildx binary whose version
and linux-amd64 SHA-256 are locked in toolchain.lock. Nothing is downloaded or
installed automatically. It does not push, deploy, or write credentials.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  local cleanup_tag current
  if [ -z "$OWNED_TAG_B_ID" ] && [ "$ATTEMPT_TAG_B" -eq 1 ]; then
    current=$(discover_tag_target "$TAG_B" "$METADATA_FILE_B" 2>/dev/null || true)
    [[ "$current" =~ ^sha256:[0-9a-f]{64}$ ]] && OWNED_TAG_B_ID=$current
  fi
  if [ -z "$OWNED_TAG_A_ID" ] && [ "$ATTEMPT_TAG_A" -eq 1 ]; then
    current=$(discover_tag_target "$TAG_A" "$METADATA_FILE_A" 2>/dev/null || true)
    [[ "$current" =~ ^sha256:[0-9a-f]{64}$ ]] && OWNED_TAG_A_ID=$current
  fi
  if [ -n "$OWNED_TAG_B_ID" ]; then
    cleanup_tag=$TAG_B
    current=$(docker_clean image inspect --format '{{.Id}}' "$cleanup_tag" 2>/dev/null || true)
    if [ "$current" = "$OWNED_TAG_B_ID" ]; then
      docker_clean image rm "$cleanup_tag" >/dev/null 2>&1 || true
    fi
  fi
  if [ -n "$OWNED_TAG_A_ID" ] && [ "$RETAINED_TAG_COMMITTED" -eq 0 ]; then
    cleanup_tag=$TAG_A
    current=$(docker_clean image inspect --format '{{.Id}}' "$cleanup_tag" 2>/dev/null || true)
    if [ "$current" = "$OWNED_TAG_A_ID" ]; then
      docker_clean image rm "$cleanup_tag" >/dev/null 2>&1 || true
    fi
  fi
  if [ -n "$TMP_CONTEXT" ]; then
    case "$TMP_CONTEXT" in
      /tmp/dji4g-edge-context.*|"${TMPDIR:-/tmp}"/dji4g-edge-context.*)
        rm -rf -- "$TMP_CONTEXT"
        ;;
    esac
  fi
  if [ -n "$TMP_OUTPUT" ]; then
    case "$TMP_OUTPUT" in
      */.dji4g-edge-build.*)
        rm -rf -- "$TMP_OUTPUT"
        ;;
    esac
  fi
  if [ "$OUTPUT_COMMIT_ATTEMPT" -eq 1 ] && [ "$OUTPUT_COMMITTED" -eq 0 ] && \
      [ -n "$OUTPUT_STAGE_ID" ] && [ -d "$OUTPUT" ] && [ ! -L "$OUTPUT" ]; then
    current=$(stat -f '%d:%i' "$OUTPUT" 2>/dev/null || stat -c '%d:%i' "$OUTPUT" 2>/dev/null || true)
    if [ "$current" = "$OUTPUT_STAGE_ID" ]; then
      rm -rf -- "$OUTPUT"
    fi
  fi
  if [ -n "$DOCKER_CONFIG_DIR" ]; then
    case "$DOCKER_TEMP_CONFIG_DIR" in
      /tmp/dji4g-edge-docker-config.*) rm -rf -- "$DOCKER_TEMP_CONFIG_DIR" ;;
    esac
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker_clean() {
  # Global flags pin the local default daemon and an empty private CLI config.
  # env -i alone is insufficient because Docker can rediscover ~/.docker via
  # the passwd database and consume persistent currentContext/proxy settings.
  env -i PATH="$PATH" "${DOCKER_CMD[@]}" \
    --config "$DOCKER_CONFIG_DIR" --context default "$@"
}

verify_exact_tool_image() {
  local ref=$1 expected_id platform digests
  [[ "$ref" == *@sha256:* ]] || fail "tool image reference is not digest-pinned: $ref"
  expected_id="sha256:${ref##*@sha256:}"
  platform=$(docker_clean image inspect --format '{{.Os}}/{{.Architecture}}' "$ref" 2>/dev/null) ||
    fail "locked tool image is not present locally: $ref"
  [ "$platform" = "$TARGET_PLATFORM" ] || fail "tool image platform differs from toolchain.lock: $ref"
  [ "$(docker_clean image inspect --format '{{.Id}}' "$ref")" = "$expected_id" ] ||
    fail "tool image target ID differs from its locked digest: $ref"
  digests=$(docker_clean image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$ref")
  printf '%s\n' "$digests" | grep -Fqx "$ref" || fail "tool image RepoDigest is not locally proven: $ref"
}

metadata_target() {
  env -i PATH="$PATH" python3 -I - "$1" <<'PY'
import json,re,sys
def no_duplicates(pairs):
    result={}
    for key,value in pairs:
        if key in result: raise ValueError("duplicate key")
        result[key]=value
    return result
try:
    with open(sys.argv[1],encoding="utf-8") as handle:value=json.load(handle,object_pairs_hook=no_duplicates)
except (OSError,UnicodeError,json.JSONDecodeError,ValueError):raise SystemExit(1)
if not isinstance(value,dict):raise SystemExit(1)
target=value.get("containerimage.digest");config=value.get("containerimage.config.digest")
pattern=re.compile(r"sha256:[0-9a-f]{64}")
if not isinstance(target,str) or pattern.fullmatch(target) is None:raise SystemExit(1)
if not isinstance(config,str) or pattern.fullmatch(config) is None or config==target:raise SystemExit(1)
print(target)
PY
}

discover_tag_target() {
  # Buildx may commit the daemon tag before its metadata response reaches this
  # shell. Recover that acknowledgement gap only for the unpredictable,
  # preflight-absent tag whose complete image labels equal the locked build.
  local tag=$1 metadata=$2 target current recovery
  if [ -f "$metadata" ]; then
    target=$(metadata_target "$metadata" 2>/dev/null || true)
    if [[ "$target" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      current=$(docker_clean image inspect --format '{{.Id}}' "$tag" 2>/dev/null || true)
      if [ "$current" = "$target" ]; then
        printf '%s\n' "$target"
        return 0
      fi
    fi
  fi
  recovery="$TMP_CONTEXT/tag-recovery-inspect.json"
  docker_clean image inspect "$tag" >"$recovery" 2>/dev/null || return 1
  env -i PATH="$PATH" python3 -I - "$recovery" \
    "$SOURCE_REVISION" "$SOURCE_CREATED" "$SOURCE_DIGEST" \
    "$SOURCE_LOCK_SHA256" "$TOOLCHAIN_LOCK_SHA256" <<'PY'
import json,re,sys
try:
    with open(sys.argv[1],encoding="utf-8") as handle:value=json.load(handle)
except (OSError,json.JSONDecodeError,UnicodeError):
    raise SystemExit(1)
if not isinstance(value,list) or len(value)!=1 or not isinstance(value[0],dict):
    raise SystemExit(1)
image=value[0];identifier=image.get("Id");config=image.get("Config")
if not isinstance(identifier,str) or re.fullmatch(r"sha256:[0-9a-f]{64}",identifier) is None:
    raise SystemExit(1)
expected={
    "org.opencontainers.image.title":"DJOneHub public edge",
    "org.opencontainers.image.description":"Fail-closed read-only public control edge",
    "org.opencontainers.image.source":"https://github.com/example/maccellular",
    "org.opencontainers.image.revision":sys.argv[2],
    "org.opencontainers.image.created":sys.argv[3],
    "io.maccellular.source-tree-sha256":sys.argv[4],
    "io.maccellular.source-lock-sha256":sys.argv[5],
    "io.maccellular.toolchain-lock-sha256":sys.argv[6],
    "io.maccellular.runtime":"scratch-no-shell",
    "io.maccellular.surface":"public-read-only",
}
if image.get("Os")!="linux" or image.get("Architecture")!="amd64" or not isinstance(config,dict) or config.get("Labels")!=expected:
    raise SystemExit(1)
print(identifier)
PY
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--output requires a value"
      OUTPUT=$2; shift 2 ;;
    --repository)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--repository requires a value"
      REPOSITORY=$2; shift 2 ;;
    --buildx-bin)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--buildx-bin requires a value"
      BUILDX_BIN=$2; shift 2 ;;
    --apply)
      APPLY=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      fail "unknown argument: $1" ;;
  esac
done

[ -n "$OUTPUT" ] || fail "--output is required"
[ -n "$REPOSITORY" ] || fail "--repository is required"
case "$OUTPUT" in
  /*) ;;
  *) fail "--output must be absolute" ;;
esac
case "$OUTPUT" in
  *[!A-Za-z0-9_./-]*|*//*|*/./*|*/../*|*/.|*/..)
    fail "--output must be a clean path" ;;
esac
[[ "$REPOSITORY" =~ ^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+$ ]] ||
  fail "--repository must be a lowercase registry/namespace/repository without a tag or digest"
[ ! -e "$OUTPUT" ] || fail "output already exists"
[ -f "$SOURCE_LOCK" ] && [ ! -L "$SOURCE_LOCK" ] || fail "source.lock is missing or symlinked"
[ -f "$TOOLCHAIN_LOCK" ] && [ ! -L "$TOOLCHAIN_LOCK" ] || fail "toolchain.lock is missing or symlinked"
[ -x "$VERIFY_SOURCE" ] || fail "verify-source.py is missing or not executable"
[ -x "$VERIFY_BUILD" ] || fail "verify-build-bundle.py is missing or not executable"
[ -x "$SANITIZE_ARCHIVE" ] || fail "sanitize-image-archive.py is missing or not executable"
[ -x "$ATOMIC_COMMIT" ] || fail "atomic-commit.py is missing or not executable"

read_lock() {
  local file=$1 key=$2 count line
  count=$(grep -Ec "^${key}=" "$file" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once in $(basename -- "$file")"
  line=$(grep -E "^${key}=" "$file")
  printf '%s' "${line#*=}"
}

SOURCE_DIGEST=$(read_lock "$SOURCE_LOCK" SOURCE_TREE_SHA256)
SOURCE_EPOCH=$(read_lock "$SOURCE_LOCK" SOURCE_DATE_EPOCH)
SOURCE_REVISION=$(read_lock "$SOURCE_LOCK" SOURCE_REVISION)
SOURCE_CREATED=$(read_lock "$SOURCE_LOCK" SOURCE_CREATED_RFC3339)
SOURCE_LOCK_SHA256=$(shasum -a 256 "$SOURCE_LOCK" | awk '{print $1}')
TOOLCHAIN_LOCK_SHA256=$(shasum -a 256 "$TOOLCHAIN_LOCK" | awk '{print $1}')
TARGET_PLATFORM=$(read_lock "$TOOLCHAIN_LOCK" TARGET_PLATFORM)
DOCKER_VERSION=$(read_lock "$TOOLCHAIN_LOCK" DOCKER_SERVER_VERSION)
DOCKER_CLIENT_VERSION=$(read_lock "$TOOLCHAIN_LOCK" DOCKER_CLIENT_VERSION)
IMAGE_STORE_DRIVER=$(read_lock "$TOOLCHAIN_LOCK" IMAGE_STORE_DRIVER)
DOCKER_ROOT_CONFIG_DIR=$(read_lock "$TOOLCHAIN_LOCK" DOCKER_ROOT_CONFIG_DIR)
DOCKER_ROOT_CONFIG_SHA256=$(read_lock "$TOOLCHAIN_LOCK" DOCKER_ROOT_CONFIG_SHA256)
BUILDX_VERSION=$(read_lock "$TOOLCHAIN_LOCK" BUILDX_VERSION)
BUILDX_PLUGIN_PATH=$(read_lock "$TOOLCHAIN_LOCK" BUILDX_PLUGIN_PATH)
BUILDX_SHA256=$(read_lock "$TOOLCHAIN_LOCK" BUILDX_LINUX_AMD64_SHA256)
BUILDER_IMAGE=$(read_lock "$TOOLCHAIN_LOCK" GO_BUILDER_IMAGE)
BUILDER_TARGET=$(read_lock "$TOOLCHAIN_LOCK" GO_BUILDER_TARGET_DIGEST)
FRONTEND_IMAGE=$(read_lock "$TOOLCHAIN_LOCK" DOCKERFILE_FRONTEND)
GO_VERSION=$(read_lock "$TOOLCHAIN_LOCK" GO_VERSION)
GO_PROXY=$(read_lock "$TOOLCHAIN_LOCK" GOPROXY)
GO_SUMDB=$(read_lock "$TOOLCHAIN_LOCK" GOSUMDB)
[ "$TARGET_PLATFORM" = linux/amd64 ] || fail "locked target platform is not linux/amd64"
[ "$(head -n 1 "$SCRIPT_DIR/Dockerfile")" = "# syntax=$FRONTEND_IMAGE" ] ||
  fail "Dockerfile frontend does not equal toolchain.lock"
grep -Fq "GOPROXY=$GO_PROXY" "$SCRIPT_DIR/Dockerfile" || fail "Dockerfile GOPROXY differs from toolchain.lock"
grep -Fq "GOSUMDB=$GO_SUMDB" "$SCRIPT_DIR/Dockerfile" || fail "Dockerfile GOSUMDB differs from toolchain.lock"

python3 -I "$VERIFY_SOURCE" \
  --repo-root "$REPO_ROOT" --lock "$SOURCE_LOCK" --print >/dev/null

SHORT_DIGEST=${SOURCE_DIGEST:0:16}

if [ "$APPLY" -eq 0 ]; then
  printf 'DRY-RUN: locked source %s will be built twice for linux/amd64.\n' "$SOURCE_DIGEST"
  printf 'DRY-RUN: no Docker, registry, service or filesystem state was changed.\n'
  exit 0
fi

[ -n "$BUILDX_BIN" ] || fail "--buildx-bin is required with --apply"
[ "$BUILDX_BIN" = "$BUILDX_PLUGIN_PATH" ] ||
  fail "--buildx-bin must equal the exact path locked in toolchain.lock"
case "$BUILDX_BIN" in /*) ;; *) fail "--buildx-bin must be absolute" ;; esac
[ -f "$BUILDX_BIN" ] && [ ! -L "$BUILDX_BIN" ] && [ -x "$BUILDX_BIN" ] ||
  fail "Buildx must be an executable regular file, not a symlink"
BUILDX_PARENT=$(dirname -- "$BUILDX_BIN")
BUILDX_BASE=$(basename -- "$BUILDX_BIN")
[ -d "$BUILDX_PARENT" ] && [ ! -L "$BUILDX_PARENT" ] || fail "Buildx parent directory is invalid"
BUILDX_PARENT_PHYSICAL=$(CDPATH= cd -- "$BUILDX_PARENT" && pwd -P)
[ "$BUILDX_BIN" = "$BUILDX_PARENT_PHYSICAL/$BUILDX_BASE" ] ||
  fail "--buildx-bin must be a physical canonical path"
BUILDX_OWNER=$(stat -f '%u' "$BUILDX_BIN" 2>/dev/null || stat -c '%u' "$BUILDX_BIN")
[ "$BUILDX_OWNER" = 0 ] || [ "$BUILDX_OWNER" = "$(id -u)" ] ||
  fail "Buildx must be owned by root or the current user"
BUILDX_MODE=$(stat -f '%Lp' "$BUILDX_BIN" 2>/dev/null || stat -c '%a' "$BUILDX_BIN")
case "$BUILDX_MODE" in 500|555|700|755) ;; *) fail "Buildx permissions must not be group/world writable" ;; esac
[ "$(shasum -a 256 "$BUILDX_BIN" | awk '{print $1}')" = "$BUILDX_SHA256" ] ||
  fail "Buildx linux-amd64 SHA-256 differs from toolchain.lock"

PARENT=$(dirname -- "$OUTPUT")
[ -d "$PARENT" ] && [ ! -L "$PARENT" ] || fail "output parent must be an existing directory"
[ "$(CDPATH= cd -- "$PARENT" && pwd -P)/$(basename -- "$OUTPUT")" = "$OUTPUT" ] ||
  fail "output path/parent must be physical and canonical"
[ "$(stat -f '%u' "$PARENT" 2>/dev/null || stat -c '%u' "$PARENT")" = "$(id -u)" ] ||
  fail "output parent must be owned by the current user"
MODE=$(stat -f '%Lp' "$PARENT" 2>/dev/null || stat -c '%a' "$PARENT")
case "$MODE" in
  700|710|750) ;;
  *) fail "output parent must not be group/world writable and must be private" ;;
esac

DOCKER_TEMP_CONFIG_DIR=$(mktemp -d /tmp/dji4g-edge-docker-config.XXXXXX)
chmod 700 "$DOCKER_TEMP_CONFIG_DIR"
printf '{}\n' >"$DOCKER_TEMP_CONFIG_DIR/config.json"
chmod 600 "$DOCKER_TEMP_CONFIG_DIR/config.json"
DOCKER_CONFIG_DIR=$DOCKER_TEMP_CONFIG_DIR
DOCKER_BIN=$(command -v docker 2>/dev/null || true)
SUDO_BIN=$(command -v sudo 2>/dev/null || true)
if [ -n "$DOCKER_BIN" ] && env -i PATH="$PATH" "$DOCKER_BIN" \
    --config "$DOCKER_CONFIG_DIR" --context default info >/dev/null 2>&1; then
  DOCKER_CMD=("$DOCKER_BIN")
elif [ -n "$SUDO_BIN" ]; then
  [ -d "$DOCKER_ROOT_CONFIG_DIR" ] && [ ! -L "$DOCKER_ROOT_CONFIG_DIR" ] ||
    fail "root-owned Docker CLI config prerequisite is missing"
  ROOT_CONFIG_PHYSICAL=$(CDPATH= cd -- "$DOCKER_ROOT_CONFIG_DIR" && pwd -P)
  [ "$ROOT_CONFIG_PHYSICAL" = "$DOCKER_ROOT_CONFIG_DIR" ] || fail "root Docker config path must be physical"
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
    fail "root Docker config.json differs from toolchain.lock"
  if env -i PATH="$PATH" "$SUDO_BIN" -n docker --config "$DOCKER_ROOT_CONFIG_DIR" \
      --context default info >/dev/null 2>&1; then
    DOCKER_CONFIG_DIR=$DOCKER_ROOT_CONFIG_DIR
  else
    fail "Docker daemon access is required (directly or through existing sudo -n docker)"
  fi
  DOCKER_CMD=("$SUDO_BIN" -n docker)
  DOCKER_USES_SUDO=1
else
  fail "Docker daemon access is required (directly or through existing sudo -n docker)"
fi

ACTUAL_DOCKER_VERSION=$(docker_clean version --format '{{.Server.Version}}')
[ "$ACTUAL_DOCKER_VERSION" = "$DOCKER_VERSION" ] ||
  fail "Docker server must be exactly $DOCKER_VERSION"
ACTUAL_DOCKER_CLIENT_VERSION=$(docker_clean version --format '{{.Client.Version}}')
[ "$ACTUAL_DOCKER_CLIENT_VERSION" = "$DOCKER_CLIENT_VERSION" ] ||
  fail "Docker client must be exactly $DOCKER_CLIENT_VERSION"
ACTUAL_PLATFORM=$(docker_clean info --format '{{.OSType}}/{{.Architecture}}')
[ "$ACTUAL_PLATFORM" = "$TARGET_PLATFORM" ] ||
  fail "Docker server must be native $TARGET_PLATFORM"
[ "$(docker_clean info --format '{{json .DriverStatus}}')" = \
    "[[\"driver-type\",\"$IMAGE_STORE_DRIVER\"]]" ] ||
  fail "Docker image store must be exactly $IMAGE_STORE_DRIVER"
[ "$(docker_clean info --format '{{json .SecurityOptions}}')" = \
    '["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]' ] ||
  fail "Docker daemon security options differ from the audited AppArmor/seccomp/cgroupns contract"

if [ "$DOCKER_USES_SUDO" -eq 1 ]; then
  # The privileged Docker CLI must never exec a plugin from a user-writable
  # directory.  The target VPS therefore requires a separately audited,
  # root-installed system plugin whose complete parent chain is root-owned and
  # not group/world writable.
  env -i PATH="$PATH" python3 -I - "$BUILDX_BIN" <<'PY' ||
import os, pathlib, stat, sys
path=pathlib.Path(sys.argv[1])
for candidate in (path, *path.parents):
    value=os.lstat(candidate)
    if stat.S_ISLNK(value.st_mode) or value.st_uid != 0 or value.st_mode & 0o022:
        raise SystemExit(1)
PY
    fail "sudo Docker requires a root-owned, non-writable system Buildx path chain"
  for candidate in \
    "$DOCKER_ROOT_CONFIG_DIR/cli-plugins/docker-buildx" \
    /usr/local/lib/docker/cli-plugins/docker-buildx \
    /usr/local/libexec/docker/cli-plugins/docker-buildx \
    /usr/lib/docker/cli-plugins/docker-buildx; do
    [ ! -e "$candidate" ] && [ ! -L "$candidate" ] ||
      fail "a higher-priority Buildx plugin shadows the hash-locked system plugin: $candidate"
  done
else
  mkdir "$DOCKER_CONFIG_DIR/cli-plugins"
  chmod 700 "$DOCKER_CONFIG_DIR/cli-plugins"
  cp "$BUILDX_BIN" "$DOCKER_CONFIG_DIR/cli-plugins/docker-buildx"
  chmod 500 "$DOCKER_CONFIG_DIR/cli-plugins/docker-buildx"
  [ "$(shasum -a 256 "$DOCKER_CONFIG_DIR/cli-plugins/docker-buildx" | awk '{print $1}')" = "$BUILDX_SHA256" ] ||
    fail "controlled Buildx copy changed"
fi
BUILDX_VERSION_OUTPUT=$(docker_clean buildx version)
case "$BUILDX_VERSION_OUTPUT" in
  "github.com/docker/buildx v$BUILDX_VERSION "*) ;;
  *) fail "Buildx runtime version differs from toolchain.lock" ;;
esac
BUILDX_INSPECT=$(docker_clean buildx inspect default)
[ "$(printf '%s\n' "$BUILDX_INSPECT" | grep -Ec '^Driver:[[:space:]]+docker$')" -eq 1 ] ||
  fail "Buildx default builder must use the local Docker driver"

BUILDER_INSPECT=$(docker_clean image inspect \
  --format '{{.Os}}/{{.Architecture}} {{.Config.Env}}' "$BUILDER_IMAGE" 2>/dev/null) ||
  fail "locked Go builder image is not present locally; pull it by digest in a separately audited step"
case "$BUILDER_INSPECT" in
  "linux/amd64 "*"GOLANG_VERSION=$GO_VERSION"*) ;;
  *) fail "locked Go builder image platform/version does not match toolchain.lock" ;;
esac
[[ "$BUILDER_IMAGE" == *@"$BUILDER_TARGET" ]] ||
  fail "locked Go builder reference and target digest disagree"
[ "$(docker_clean image inspect --format '{{.Id}}' "$BUILDER_IMAGE")" = "$BUILDER_TARGET" ] ||
  fail "locked Go builder target digest does not match the containerd image ID"
verify_exact_tool_image "$BUILDER_IMAGE"
verify_exact_tool_image "$FRONTEND_IMAGE"
TAG_NONCE=$(env -i PATH="$PATH" python3 -I -c 'import secrets;print(secrets.token_hex(16))')
[[ "$TAG_NONCE" =~ ^[0-9a-f]{32}$ ]] || fail "could not create an unpredictable staging-tag nonce"
TAG_A="${REPOSITORY}:dji4g-build-${SHORT_DIGEST}-${TAG_NONCE}-a"
TAG_B="${REPOSITORY}:dji4g-build-${SHORT_DIGEST}-${TAG_NONCE}-b"
if docker_clean image inspect "$TAG_A" >/dev/null 2>&1; then
  fail "refusing to overwrite pre-existing build tag: $TAG_A"
fi
if docker_clean image inspect "$TAG_B" >/dev/null 2>&1; then
  fail "refusing to overwrite pre-existing build tag: $TAG_B"
fi

TMP_BASE=${TMPDIR:-/tmp}
TMP_CONTEXT=$(mktemp -d "$TMP_BASE/dji4g-edge-context.XXXXXX")
METADATA_FILE_A="$TMP_CONTEXT/image-a.metadata.json"
METADATA_FILE_B="$TMP_CONTEXT/image-b.metadata.json"
python3 -I "$VERIFY_SOURCE" \
  --repo-root "$REPO_ROOT" --lock "$SOURCE_LOCK" --copy-to "$TMP_CONTEXT"

BUILD_ARGS=(
  buildx
  build
  --builder default
  --load
  --provenance=false
  --sbom=false
  --pull=false
  --no-cache
  --platform linux/amd64
  --build-arg "GO_BUILDER_IMAGE=$BUILDER_IMAGE"
  --build-arg "SOURCE_DATE_EPOCH=$SOURCE_EPOCH"
  --build-arg "SOURCE_REVISION=$SOURCE_REVISION"
  --build-arg "SOURCE_CREATED_RFC3339=$SOURCE_CREATED"
  --build-arg "SOURCE_TREE_SHA256=$SOURCE_DIGEST"
  --build-arg "SOURCE_LOCK_SHA256=$SOURCE_LOCK_SHA256"
  --build-arg "TOOLCHAIN_LOCK_SHA256=$TOOLCHAIN_LOCK_SHA256"
)
ATTEMPT_TAG_A=1
docker_clean "${BUILD_ARGS[@]}" --metadata-file "$METADATA_FILE_A" -t "$TAG_A" "$TMP_CONTEXT"
IMAGE_ID_A=$(docker_clean image inspect --format '{{.Id}}' "$TAG_A")
[[ "$IMAGE_ID_A" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "Docker returned an invalid first image ID"
OWNED_TAG_A_ID=$IMAGE_ID_A
[ "$(metadata_target "$METADATA_FILE_A")" = "$IMAGE_ID_A" ] || fail "first Buildx target metadata differs from the created tag"
ATTEMPT_TAG_B=1
docker_clean "${BUILD_ARGS[@]}" --metadata-file "$METADATA_FILE_B" -t "$TAG_B" "$TMP_CONTEXT"
IMAGE_ID_B=$(docker_clean image inspect --format '{{.Id}}' "$TAG_B")
[[ "$IMAGE_ID_B" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "Docker returned an invalid second image ID"
OWNED_TAG_B_ID=$IMAGE_ID_B
[ "$(metadata_target "$METADATA_FILE_B")" = "$IMAGE_ID_B" ] || fail "second Buildx target metadata differs from the created tag"
[ "$IMAGE_ID_A" = "$IMAGE_ID_B" ] ||
  fail "two clean builds produced different image IDs"

TMP_OUTPUT=$(mktemp -d "$PARENT/.dji4g-edge-build.XXXXXX")
docker_clean image inspect "$IMAGE_ID_A" >"$TMP_OUTPUT/image-inspect.json"
# Export the content-addressed target, never either tag name. The verifier
# also rejects every OCI/Docker compatibility naming field so a later air-gap
# load cannot overwrite an unrelated local tag.
docker_clean image save "$IMAGE_ID_A" >"$TMP_OUTPUT/image-raw.tar"
python3 -I "$SANITIZE_ARCHIVE" --input "$TMP_OUTPUT/image-raw.tar" \
  --output "$TMP_OUTPUT/image.tar" --epoch "$SOURCE_EPOCH"
rm -f "$TMP_OUTPUT/image-raw.tar"
shasum -a 256 "$TMP_OUTPUT/image.tar" | awk '{print $1 "  image.tar"}' >"$TMP_OUTPUT/image.tar.sha256"
printf '%s\n' "$IMAGE_ID_A" >"$TMP_OUTPUT/edge-image-reference.txt"
printf '%s\n' "$TAG_A" >"$TMP_OUTPUT/local-image-retention-reference.txt"
cp "$SOURCE_LOCK" "$TMP_OUTPUT/source.lock"
cp "$TOOLCHAIN_LOCK" "$TMP_OUTPUT/toolchain.lock"
python3 -I - "$TMP_OUTPUT" "$IMAGE_ID_A" "$TAG_A" "$TARGET_PLATFORM" "$SOURCE_REVISION" \
  "$SOURCE_CREATED" "$SOURCE_DIGEST" "$SOURCE_LOCK_SHA256" "$TOOLCHAIN_LOCK_SHA256" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
manifest = {
    "format": 2,
    "image_id": sys.argv[2],
    "local_retention_reference": sys.argv[3],
    "target_platform": sys.argv[4],
    "source_revision": sys.argv[5],
    "source_created_rfc3339": sys.argv[6],
    "source_tree_sha256": sys.argv[7],
    "source_lock_sha256": sys.argv[8],
    "toolchain_lock_sha256": sys.argv[9],
    "reproducible_image_id": True,
    "artifacts": {},
}
for name in ("edge-image-reference.txt", "local-image-retention-reference.txt", "image-inspect.json", "image.tar", "image.tar.sha256", "source.lock", "toolchain.lock"):
    manifest["artifacts"][name] = hashlib.sha256((root / name).read_bytes()).hexdigest()
(root / "build.json").write_text(
    json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n",
    encoding="ascii",
)
PY
chmod 600 "$TMP_OUTPUT"/*
python3 -I "$VERIFY_BUILD" --bundle "$TMP_OUTPUT" --repo-root "$REPO_ROOT" \
  --source-lock "$SOURCE_LOCK" --toolchain-lock "$TOOLCHAIN_LOCK" >/dev/null
CURRENT_TAG_B_ID=$(docker_clean image inspect --format '{{.Id}}' "$TAG_B" 2>/dev/null) ||
  fail "temporary second build tag disappeared before cleanup"
[ "$CURRENT_TAG_B_ID" = "$OWNED_TAG_B_ID" ] || fail "temporary second build tag ownership changed"
docker_clean image rm "$TAG_B" >/dev/null || fail "could not remove the owned temporary second build tag"
OWNED_TAG_B_ID=""
ATTEMPT_TAG_B=0
CURRENT_TAG_A_ID=$(docker_clean image inspect --format '{{.Id}}' "$TAG_A" 2>/dev/null) ||
  fail "local retention reference disappeared before output commit"
[ "$CURRENT_TAG_A_ID" = "$OWNED_TAG_A_ID" ] || fail "local retention reference ownership changed"
OUTPUT_STAGE_ID=$(stat -f '%d:%i' "$TMP_OUTPUT" 2>/dev/null || stat -c '%d:%i' "$TMP_OUTPUT")
OUTPUT_COMMIT_ATTEMPT=1
COMMITTED_ID=$(env -i PATH="$PATH" python3 -I "$ATOMIC_COMMIT" --source "$TMP_OUTPUT" --target "$OUTPUT")
[ "$COMMITTED_ID" = "$OUTPUT_STAGE_ID" ] || fail "atomic output commit returned a different inode"
TMP_OUTPUT=""
trap '' HUP INT TERM
OUTPUT_COMMITTED=1
RETAINED_TAG_COMMITTED=1
printf 'OK: reproducible linux/amd64 build created at %s\n' "$OUTPUT" || true
printf 'OK: exact local Compose reference is %s\n' "$IMAGE_ID_A" || true
printf 'OK: keep local retention reference until the exact image is no longer needed: %s\n' "$TAG_A" || true
printf 'NEXT: run audit-image.sh; this build alone is not deployment approval.\n' || true
exit 0
