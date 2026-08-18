#!/usr/bin/env bash
set -euo pipefail

umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../../.." && pwd -P)
VERIFY_EVIDENCE="$SCRIPT_DIR/verify-evidence.py"
VERIFY_BUILD="$SCRIPT_DIR/verify-build-bundle.py"
INSPECT_ROOTFS="$SCRIPT_DIR/inspect-rootfs.py"
VERIFY_RUNTIME="$SCRIPT_DIR/../verify-config.sh"
TRUSTED_SOURCE_LOCK="$SCRIPT_DIR/source.lock"
TRUSTED_TOOLCHAIN_LOCK="$SCRIPT_DIR/toolchain.lock"
ATOMIC_COMMIT="$SCRIPT_DIR/atomic-commit.py"
BUILD_DIR=""
RUNTIME_ROOT=""
OUTPUT=""
APPLY=0
TMP_OUTPUT=""
AUDIT_CONTAINER_ID=""
AUDIT_CONTAINER_NAME=""
AUDIT_CONTAINER_EXPECTED_IMAGE=""
AUDIT_CONTAINER_ATTEMPT=0
ROOTFS_CONTAINER_ID=""
ROOTFS_CONTAINER_NAME=""
ROOTFS_CONTAINER_EXPECTED_IMAGE=""
ROOTFS_CONTAINER_ATTEMPT=0
DOCKER_CONFIG_DIR=""
DOCKER_TEMP_CONFIG_DIR=""
DOCKER_CMD=()
SCANNER_CONTAINER_ID=""
SCANNER_CONTAINER_NAME=""
SCANNER_CONTAINER_ROLE=""
SCANNER_CONTAINER_EXPECTED_IMAGE=""
SCANNER_CONTAINER_ATTEMPT=0
AUDIT_NONCE=""
OUTPUT_COMMIT_ATTEMPT=0
OUTPUT_COMMITTED=0
OUTPUT_STAGE_ID=""

usage() {
  cat <<'EOF'
Usage:
  audit-image.sh --build-dir /absolute/build-dir \
    --runtime-root /absolute/verified-edge-runtime \
    --output /absolute/new/evidence-dir [--apply]

Dry-run verifies inputs without Docker. --apply requires the pinned Syft and
Grype images to already exist locally, updates the Grype database, produces an
SPDX SBOM, fails on every High/Critical finding, inspects the scratch rootfs,
and starts the exact image as the generated non-root UID with a read-only root.
No image is pushed and no Compose service is started.
EOF
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [[ "$AUDIT_CONTAINER_ID" =~ ^[0-9a-f]{64}$ ]]; then
    docker_clean container rm -f "$AUDIT_CONTAINER_ID" >/dev/null 2>&1 ||
      cleanup_named_container "$AUDIT_CONTAINER_NAME" startup "$AUDIT_CONTAINER_EXPECTED_IMAGE"
  elif [ "$AUDIT_CONTAINER_ATTEMPT" -eq 1 ]; then
    cleanup_named_container "$AUDIT_CONTAINER_NAME" startup "$AUDIT_CONTAINER_EXPECTED_IMAGE"
  fi
  if [[ "$ROOTFS_CONTAINER_ID" =~ ^[0-9a-f]{64}$ ]]; then
    docker_clean container rm -f "$ROOTFS_CONTAINER_ID" >/dev/null 2>&1 ||
      cleanup_named_container "$ROOTFS_CONTAINER_NAME" rootfs "$ROOTFS_CONTAINER_EXPECTED_IMAGE"
  elif [ "$ROOTFS_CONTAINER_ATTEMPT" -eq 1 ]; then
    cleanup_named_container "$ROOTFS_CONTAINER_NAME" rootfs "$ROOTFS_CONTAINER_EXPECTED_IMAGE"
  fi
  if [[ "$SCANNER_CONTAINER_ID" =~ ^[0-9a-f]{64}$ ]]; then
    docker_clean container rm -f "$SCANNER_CONTAINER_ID" >/dev/null 2>&1 ||
      cleanup_named_container "$SCANNER_CONTAINER_NAME" "$SCANNER_CONTAINER_ROLE" "$SCANNER_CONTAINER_EXPECTED_IMAGE"
  elif [ "$SCANNER_CONTAINER_ATTEMPT" -eq 1 ]; then
    cleanup_named_container "$SCANNER_CONTAINER_NAME" "$SCANNER_CONTAINER_ROLE" "$SCANNER_CONTAINER_EXPECTED_IMAGE"
  fi
  if [ -n "$TMP_OUTPUT" ]; then
    case "$TMP_OUTPUT" in
      */.dji4g-edge-audit.*)
        rm -rf -- "$TMP_OUTPUT"
        ;;
    esac
  fi
  if [ "$OUTPUT_COMMIT_ATTEMPT" -eq 1 ] && [ "$OUTPUT_COMMITTED" -eq 0 ] && \
      [ -n "$OUTPUT_STAGE_ID" ] && [ -d "$OUTPUT" ] && [ ! -L "$OUTPUT" ]; then
    current=$(stat -f '%d:%i' "$OUTPUT" 2>/dev/null || stat -c '%d:%i' "$OUTPUT" 2>/dev/null || true)
    [ "$current" != "$OUTPUT_STAGE_ID" ] || rm -rf -- "$OUTPUT"
  fi
  if [ -n "$DOCKER_TEMP_CONFIG_DIR" ]; then
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
  env -i PATH="$PATH" "${DOCKER_CMD[@]}" \
    --config "$DOCKER_CONFIG_DIR" --context default "$@"
}

cleanup_named_container() {
  local name=$1 role=$2 expected=$3 inspect_file recovered
  [ -n "$name" ] && [ -n "$role" ] && [[ "$expected" =~ ^sha256:[0-9a-f]{64}$ ]] || return 0
  [ -n "$DOCKER_TEMP_CONFIG_DIR" ] || return 0
  inspect_file="$DOCKER_TEMP_CONFIG_DIR/cleanup-${role}.json"
  docker_clean container inspect "$name" >"$inspect_file" 2>/dev/null || return 0
  recovered=$(env -i PATH="$PATH" python3 -I - "$inspect_file" "$name" "$AUDIT_NONCE" "$role" "$expected" <<'PY'
import json,re,sys
try:
    with open(sys.argv[1],encoding="utf-8") as handle:value=json.load(handle)
except (OSError,UnicodeError,json.JSONDecodeError):
    raise SystemExit(1)
if not isinstance(value,list) or len(value)!=1 or not isinstance(value[0],dict):raise SystemExit(1)
container=value[0];config=container.get("Config");labels=config.get("Labels") if isinstance(config,dict) else None
identifier=container.get("Id")
if (
    not isinstance(identifier,str) or re.fullmatch(r"[0-9a-f]{64}",identifier) is None
    or container.get("Name")!="/"+sys.argv[2]
    or container.get("Image")!=sys.argv[5]
    or not isinstance(labels,dict)
    or labels.get("io.maccellular.audit-nonce")!=sys.argv[3]
    or labels.get("io.maccellular.audit-role")!=sys.argv[4]
):raise SystemExit(1)
print(identifier)
PY
) || return 0
  [[ "$recovered" =~ ^[0-9a-f]{64}$ ]] || return 0
  docker_clean container rm -f "$recovered" >/dev/null 2>&1 || true
}

create_scanner() {
  # Bash defers a caught signal until the foreground command substitution has
  # completed; assign directly into the cleanup-owned variable so there is no
  # returned-ID-to-registration window.
  local name=$1 role=$2 expected=$3
  shift 3
  SCANNER_CONTAINER_NAME=$name
  SCANNER_CONTAINER_ROLE=$role
  SCANNER_CONTAINER_EXPECTED_IMAGE=$expected
  SCANNER_CONTAINER_ATTEMPT=1
  if ! SCANNER_CONTAINER_ID=$(docker_clean container create \
      --name "$name" \
      --label "io.maccellular.audit-nonce=$AUDIT_NONCE" \
      --label "io.maccellular.audit-role=$role" "$@"); then return 1; fi
  [[ "$SCANNER_CONTAINER_ID" =~ ^[0-9a-f]{64}$ ]]
}

remove_scanner() {
  docker_clean container rm -f "$SCANNER_CONTAINER_ID" >/dev/null || return 1
  SCANNER_CONTAINER_ID=""
  SCANNER_CONTAINER_NAME=""
  SCANNER_CONTAINER_ROLE=""
  SCANNER_CONTAINER_EXPECTED_IMAGE=""
  SCANNER_CONTAINER_ATTEMPT=0
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --build-dir)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--build-dir requires a value"
      BUILD_DIR=$2; shift 2 ;;
    --runtime-root)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--runtime-root requires a value"
      RUNTIME_ROOT=$2; shift 2 ;;
    --output)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--output requires a value"
      OUTPUT=$2; shift 2 ;;
    --apply)
      APPLY=1; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

for value in "$BUILD_DIR" "$RUNTIME_ROOT" "$OUTPUT"; do
  case "$value" in /*) ;; *) fail "all paths must be absolute" ;; esac
done
[ -d "$BUILD_DIR" ] && [ ! -L "$BUILD_DIR" ] || fail "build directory is invalid"
[ -d "$RUNTIME_ROOT" ] && [ ! -L "$RUNTIME_ROOT" ] || fail "runtime root is invalid"
[ ! -e "$OUTPUT" ] || fail "output already exists"
[ -x "$ATOMIC_COMMIT" ] || fail "atomic output helper is missing or not executable"
for value in "$BUILD_DIR" "$RUNTIME_ROOT"; do
  [ "$(CDPATH= cd -- "$value" && pwd -P)" = "$value" ] || fail "input paths must be physical and canonical"
done
OUTPUT_PARENT=$(dirname -- "$OUTPUT");OUTPUT_BASE=$(basename -- "$OUTPUT")
case "$OUTPUT_BASE" in ""|.|..|*[!A-Za-z0-9_.-]*) fail "output basename is invalid" ;; esac
[ -d "$OUTPUT_PARENT" ] && [ ! -L "$OUTPUT_PARENT" ] || fail "output parent must exist"
[ "$(CDPATH= cd -- "$OUTPUT_PARENT" && pwd -P)/$OUTPUT_BASE" = "$OUTPUT" ] || fail "output path must be physical and canonical"
[ -x "$VERIFY_RUNTIME" ] || fail "runtime verifier is missing or not executable"
"$VERIFY_RUNTIME" --quiet "$RUNTIME_ROOT"

python3 -I "$VERIFY_BUILD" --bundle "$BUILD_DIR" --repo-root "$REPO_ROOT" \
  --source-lock "$TRUSTED_SOURCE_LOCK" --toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK" >/dev/null

read_lock() {
  local file=$1 key=$2 count line
  count=$(grep -Ec "^${key}=" "$file" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once"
  line=$(grep -E "^${key}=" "$file")
  printf '%s' "${line#*=}"
}

IMAGE_ID=$(tr -d '\n' <"$BUILD_DIR/edge-image-reference.txt")
[[ "$IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "build image reference is not an exact local image ID"
LOCAL_RETENTION_REFERENCE=$(tr -d '\n' <"$BUILD_DIR/local-image-retention-reference.txt")
SOURCE_DIGEST=$(read_lock "$BUILD_DIR/source.lock" SOURCE_TREE_SHA256)
SYFT_IMAGE=$(read_lock "$BUILD_DIR/toolchain.lock" SYFT_IMAGE)
GRYPE_IMAGE=$(read_lock "$BUILD_DIR/toolchain.lock" GRYPE_IMAGE)
DOCKER_VERSION=$(read_lock "$BUILD_DIR/toolchain.lock" DOCKER_SERVER_VERSION)
DOCKER_CLIENT_VERSION=$(read_lock "$BUILD_DIR/toolchain.lock" DOCKER_CLIENT_VERSION)
TARGET_PLATFORM=$(read_lock "$BUILD_DIR/toolchain.lock" TARGET_PLATFORM)
IMAGE_STORE_DRIVER=$(read_lock "$BUILD_DIR/toolchain.lock" IMAGE_STORE_DRIVER)
UID_VALUE=$(read_lock "$RUNTIME_ROOT/compose.env" DJI4G_EDGE_UID)
GID_VALUE=$(read_lock "$RUNTIME_ROOT/compose.env" DJI4G_EDGE_GID)
RUNTIME_IMAGE=$(read_lock "$RUNTIME_ROOT/compose.env" DJI4G_EDGE_IMAGE)
ENABLED_PROFILES=$(read_lock "$RUNTIME_ROOT/compose.env" DJI4G_ENABLED_PROFILES)
[ "$RUNTIME_IMAGE" = "$IMAGE_ID" ] || fail "runtime root does not select the audited exact local image ID"
[ "$ENABLED_PROFILES" = edge ] || fail "image audit requires an edge-only runtime root"
[ "$UID_VALUE" = "$(id -u)" ] && [ "$GID_VALUE" = "$(id -g)" ] ||
  fail "runtime UID:GID must match the non-root audit user"
[ "$UID_VALUE" -gt 0 ] && [ "$GID_VALUE" -gt 0 ] || fail "root runtime identity is forbidden"

if [ "$APPLY" -eq 0 ]; then
  printf 'DRY-RUN: build %s and edge-only runtime inputs passed local checks.\n' "$IMAGE_ID"
  printf 'DRY-RUN: SBOM, CVE, rootfs and startup evidence were not executed.\n'
  exit 0
fi

PARENT=$OUTPUT_PARENT
[ -d "$PARENT" ] && [ ! -L "$PARENT" ] || fail "output parent must exist"
[ "$(stat -f '%u' "$PARENT" 2>/dev/null || stat -c '%u' "$PARENT")" = "$(id -u)" ] ||
  fail "output parent must be owned by the current user"
PARENT_MODE=$(stat -f '%Lp' "$PARENT" 2>/dev/null || stat -c '%a' "$PARENT")
case "$PARENT_MODE" in 700|710|750) ;; *) fail "output parent must be private" ;; esac

TMP_OUTPUT=$(mktemp -d "$PARENT/.dji4g-edge-audit.XXXXXX")
cp "$BUILD_DIR/build.json" "$BUILD_DIR/edge-image-reference.txt" \
  "$BUILD_DIR/local-image-retention-reference.txt" \
  "$BUILD_DIR/image-inspect.json" "$BUILD_DIR/image.tar" "$BUILD_DIR/image.tar.sha256" \
  "$BUILD_DIR/source.lock" "$BUILD_DIR/toolchain.lock" "$TMP_OUTPUT/"

# Snapshot every runtime byte that influences the audit before any daemon call.
# The private shadow includes the token only so the normal runtime verifier can
# validate the exact snapshot; the token is then deleted and never becomes an
# evidence artifact or digest. Containers consume only the retained non-secret
# snapshot files, never the concurrently mutable runtime root.
RUNTIME_VERIFY_SNAPSHOT="$TMP_OUTPUT/.runtime-input-snapshot"
mkdir "$RUNTIME_VERIFY_SNAPSHOT" "$RUNTIME_VERIFY_SNAPSHOT/config" "$RUNTIME_VERIFY_SNAPSHOT/secrets"
chmod 700 "$RUNTIME_VERIFY_SNAPSHOT" "$RUNTIME_VERIFY_SNAPSHOT/config" "$RUNTIME_VERIFY_SNAPSHOT/secrets"
env -i PATH="$PATH" python3 -I - "$RUNTIME_ROOT" "$RUNTIME_VERIFY_SNAPSHOT" <<'PY'
import os,pathlib,stat,sys
source=pathlib.Path(sys.argv[1]);target=pathlib.Path(sys.argv[2])
items=(
 ("compose.env","compose.env",65536),
 ("config/edge.env","config/edge.env",65536),
 ("config/gateway-public-key.pem","config/gateway-public-key.pem",65536),
 ("config/access-allowed-emails","config/access-allowed-emails",65536),
 ("secrets/cloudflare-tunnel.token","secrets/cloudflare-tunnel.token",4096),
)
flags=os.O_RDONLY|getattr(os,"O_NOFOLLOW",0)
for source_name,target_name,maximum in items:
    descriptor=os.open(source/source_name,flags)
    try:
        info=os.fstat(descriptor)
        if (
            not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600
            or info.st_uid!=os.getuid() or info.st_gid!=os.getgid()
            or info.st_nlink!=1 or info.st_size<1 or info.st_size>maximum
        ):raise SystemExit(f"ERROR: runtime snapshot source changed: {source_name}")
        destination=target/target_name
        output=os.open(destination,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        try:
            while True:
                chunk=os.read(descriptor,65536)
                if not chunk:break
                pending=memoryview(chunk)
                while pending:
                    written=os.write(output,pending)
                    if written<1:raise SystemExit("ERROR: runtime snapshot write failed")
                    pending=pending[written:]
            os.fsync(output)
        finally:os.close(output)
    finally:os.close(descriptor)
PY
"$VERIFY_RUNTIME" --declared-root "$RUNTIME_ROOT" --quiet "$RUNTIME_VERIFY_SNAPSHOT"
mv "$RUNTIME_VERIFY_SNAPSHOT/compose.env" "$TMP_OUTPUT/runtime-compose.env"
mv "$RUNTIME_VERIFY_SNAPSHOT/config/edge.env" "$TMP_OUTPUT/runtime-edge.env"
mv "$RUNTIME_VERIFY_SNAPSHOT/config/gateway-public-key.pem" "$TMP_OUTPUT/runtime-gateway-public-key.pem"
mv "$RUNTIME_VERIFY_SNAPSHOT/config/access-allowed-emails" "$TMP_OUTPUT/runtime-access-allowed-emails"
rm -f "$RUNTIME_VERIFY_SNAPSHOT/secrets/cloudflare-tunnel.token"
rmdir "$RUNTIME_VERIFY_SNAPSHOT/config" "$RUNTIME_VERIFY_SNAPSHOT/secrets" "$RUNTIME_VERIFY_SNAPSHOT"

python3 -I "$VERIFY_BUILD" --bundle "$TMP_OUTPUT" --repo-root "$REPO_ROOT" \
  --source-lock "$TRUSTED_SOURCE_LOCK" --toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK" --allow-extra >/dev/null
BUILD_DIR=$TMP_OUTPUT
IMAGE_ID=$(tr -d '\n' <"$BUILD_DIR/edge-image-reference.txt")
LOCAL_RETENTION_REFERENCE=$(tr -d '\n' <"$BUILD_DIR/local-image-retention-reference.txt")
SOURCE_DIGEST=$(read_lock "$BUILD_DIR/source.lock" SOURCE_TREE_SHA256)
SYFT_IMAGE=$(read_lock "$BUILD_DIR/toolchain.lock" SYFT_IMAGE)
GRYPE_IMAGE=$(read_lock "$BUILD_DIR/toolchain.lock" GRYPE_IMAGE)
DOCKER_VERSION=$(read_lock "$BUILD_DIR/toolchain.lock" DOCKER_SERVER_VERSION)
DOCKER_CLIENT_VERSION=$(read_lock "$BUILD_DIR/toolchain.lock" DOCKER_CLIENT_VERSION)
TARGET_PLATFORM=$(read_lock "$BUILD_DIR/toolchain.lock" TARGET_PLATFORM)
IMAGE_STORE_DRIVER=$(read_lock "$BUILD_DIR/toolchain.lock" IMAGE_STORE_DRIVER)
DOCKER_ROOT_CONFIG_DIR=$(read_lock "$BUILD_DIR/toolchain.lock" DOCKER_ROOT_CONFIG_DIR)
DOCKER_ROOT_CONFIG_SHA256=$(read_lock "$BUILD_DIR/toolchain.lock" DOCKER_ROOT_CONFIG_SHA256)
UID_VALUE=$(read_lock "$TMP_OUTPUT/runtime-compose.env" DJI4G_EDGE_UID)
GID_VALUE=$(read_lock "$TMP_OUTPUT/runtime-compose.env" DJI4G_EDGE_GID)
RUNTIME_IMAGE=$(read_lock "$TMP_OUTPUT/runtime-compose.env" DJI4G_EDGE_IMAGE)
ENABLED_PROFILES=$(read_lock "$TMP_OUTPUT/runtime-compose.env" DJI4G_ENABLED_PROFILES)
[ "$RUNTIME_IMAGE" = "$IMAGE_ID" ] || fail "snapshotted runtime does not select the audited image ID"
[ "$ENABLED_PROFILES" = edge ] || fail "snapshotted audit runtime is not edge-only"

DOCKER_TEMP_CONFIG_DIR=$(mktemp -d /tmp/dji4g-edge-docker-config.XXXXXX)
chmod 700 "$DOCKER_TEMP_CONFIG_DIR"
printf '{}\n' >"$DOCKER_TEMP_CONFIG_DIR/config.json";chmod 600 "$DOCKER_TEMP_CONFIG_DIR/config.json"
DOCKER_CONFIG_DIR=$DOCKER_TEMP_CONFIG_DIR
DOCKER_BIN=$(command -v docker 2>/dev/null || true)
SUDO_BIN=$(command -v sudo 2>/dev/null || true)
if [ -n "$DOCKER_BIN" ] && env -i PATH="$PATH" "$DOCKER_BIN" \
    --config "$DOCKER_CONFIG_DIR" --context default info >/dev/null 2>&1; then
  DOCKER_CMD=("$DOCKER_BIN")
elif [ -n "$SUDO_BIN" ]; then
  [ -d "$DOCKER_ROOT_CONFIG_DIR" ] && [ ! -L "$DOCKER_ROOT_CONFIG_DIR" ] || fail "root Docker config prerequisite is missing"
  [ "$(CDPATH= cd -- "$DOCKER_ROOT_CONFIG_DIR" && pwd -P)" = "$DOCKER_ROOT_CONFIG_DIR" ] || fail "root Docker config path must be physical"
  [ -f "$DOCKER_ROOT_CONFIG_DIR/config.json" ] && [ ! -L "$DOCKER_ROOT_CONFIG_DIR/config.json" ] || fail "root Docker config.json is missing/symlinked"
  [ "$(stat -f '%Lp' "$DOCKER_ROOT_CONFIG_DIR/config.json" 2>/dev/null || stat -c '%a' "$DOCKER_ROOT_CONFIG_DIR/config.json")" = 644 ] || fail "root Docker config.json must be root-owned mode 0644"
  [ "$(stat -f '%l' "$DOCKER_ROOT_CONFIG_DIR/config.json" 2>/dev/null || stat -c '%h' "$DOCKER_ROOT_CONFIG_DIR/config.json")" = 1 ] || fail "root Docker config.json must have exactly one hard link"
  trusted_path="$DOCKER_ROOT_CONFIG_DIR/config.json"
  while :; do
    [ "$(stat -f '%u' "$trusted_path" 2>/dev/null || stat -c '%u' "$trusted_path")" = 0 ] || fail "root Docker config path chain must be root-owned"
    trusted_mode=$(stat -f '%Lp' "$trusted_path" 2>/dev/null || stat -c '%a' "$trusted_path")
    [ $((8#$trusted_mode & 8#022)) -eq 0 ] || fail "root Docker config path chain is writable"
    [ "$trusted_path" != / ] || break;trusted_path=$(dirname -- "$trusted_path")
  done
  [ "$(shasum -a 256 "$DOCKER_ROOT_CONFIG_DIR/config.json" | awk '{print $1}')" = "$DOCKER_ROOT_CONFIG_SHA256" ] || fail "root Docker config differs from toolchain.lock"
  if env -i PATH="$PATH" "$SUDO_BIN" -n docker --config "$DOCKER_ROOT_CONFIG_DIR" --context default info >/dev/null 2>&1; then
    DOCKER_CONFIG_DIR=$DOCKER_ROOT_CONFIG_DIR;DOCKER_CMD=("$SUDO_BIN" -n docker)
  else fail "Docker daemon access is required";fi
else
  fail "Docker daemon access is required (directly or through existing sudo -n docker)"
fi
[ "$(docker_clean version --format '{{.Server.Version}}')" = "$DOCKER_VERSION" ] ||
  fail "Docker server version differs from the audited toolchain"
[ "$(docker_clean version --format '{{.Client.Version}}')" = "$DOCKER_CLIENT_VERSION" ] ||
  fail "Docker client version differs from the audited toolchain"
[ "$(docker_clean info --format '{{.OSType}}/{{.Architecture}}')" = "$TARGET_PLATFORM" ] ||
  fail "Docker daemon platform differs from the audited toolchain"
[ "$(docker_clean info --format '{{json .DriverStatus}}')" = \
    "[[\"driver-type\",\"$IMAGE_STORE_DRIVER\"]]" ] ||
  fail "Docker image store differs from the audited containerd toolchain"
[ "$(docker_clean info --format '{{json .SecurityOptions}}')" = \
    '["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]' ] ||
  fail "Docker daemon security options differ from the audited AppArmor/seccomp/cgroupns contract"
verify_exact_image() {
  local image=$1 expected platform digests
  platform=$(docker_clean image inspect --format '{{.Os}}/{{.Architecture}}' "$image" 2>/dev/null) || fail "required pinned image is not present locally: $image"
  [ "$platform" = "$TARGET_PLATFORM" ] || fail "required image platform changed: $image"
  if [[ "$image" == sha256:* ]]; then expected=$image
  else
    [[ "$image" == *@sha256:* ]] || fail "tool image is not digest-pinned"
    expected="sha256:${image##*@sha256:}"
    digests=$(docker_clean image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image")
    printf '%s\n' "$digests" | grep -Fqx "$image" || fail "tool image RepoDigest is not locally proven: $image"
  fi
  [ "$(docker_clean image inspect --format '{{.Id}}' "$image")" = "$expected" ] || fail "required image target ID changed: $image"
}
for image in "$IMAGE_ID" "$SYFT_IMAGE" "$GRYPE_IMAGE"; do verify_exact_image "$image";done
[ "$(docker_clean image inspect --format '{{.Id}}' "$LOCAL_RETENTION_REFERENCE" 2>/dev/null)" = "$IMAGE_ID" ] ||
  fail "build local retention reference no longer preserves the audited target"
[ "$(docker_clean image inspect --format '{{.Os}}/{{.Architecture}}' "$LOCAL_RETENTION_REFERENCE")" = "$TARGET_PLATFORM" ] ||
  fail "build local retention reference platform changed"
AUDIT_UID_GID="$(id -u):$(id -g)"
SYFT_VERSION=$(read_lock "$BUILD_DIR/toolchain.lock" SYFT_VERSION)
GRYPE_VERSION=$(read_lock "$BUILD_DIR/toolchain.lock" GRYPE_VERSION)
GRYPE_DB_SCHEMA_VERSION=$(read_lock "$BUILD_DIR/toolchain.lock" GRYPE_DB_SCHEMA_VERSION)
GRYPE_DB_MAX_AGE_HOURS=$(read_lock "$BUILD_DIR/toolchain.lock" GRYPE_DB_MAX_AGE_HOURS)
[ "$GRYPE_DB_SCHEMA_VERSION" = v6.1.9 ] || fail "locked Grype DB schema is unsupported"
[ "$GRYPE_DB_MAX_AGE_HOURS" = 48 ] || fail "locked Grype DB freshness window changed"
SYFT_TARGET_ID="sha256:${SYFT_IMAGE##*@sha256:}"
GRYPE_TARGET_ID="sha256:${GRYPE_IMAGE##*@sha256:}"
AUDIT_NONCE=$(env -i PATH="$PATH" python3 -I -c 'import secrets;print(secrets.token_hex(16))')
[[ "$AUDIT_NONCE" =~ ^[0-9a-f]{32}$ ]] || fail "could not create an unpredictable audit nonce"
SCANNER_BASE=(--runtime runc --ipc private --cgroupns private --network none --read-only \
  --user "$AUDIT_UID_GID" --cap-drop ALL --security-opt no-new-privileges:true \
  --security-opt apparmor=docker-default --security-opt seccomp=builtin \
  --pids-limit 256 --memory 512m --memory-swap 512m --cpus 1.0 --shm-size 64m \
  --log-driver none)
create_scanner "dji4g-edge-audit-${AUDIT_NONCE}-syft-version" syft-version "$SYFT_TARGET_ID" \
  "${SCANNER_BASE[@]}" "$SYFT_IMAGE" version -o json || fail "could not create Syft version container"
SYFT_VERSION_JSON=$(docker_clean container start -a "$SCANNER_CONTAINER_ID") || fail "Syft version container failed"
remove_scanner || fail "could not remove Syft version container"
[ "$(printf '%s\n' "$SYFT_VERSION_JSON" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["version"])')" = "$SYFT_VERSION" ] || fail "pinned Syft runtime version does not match toolchain.lock"
create_scanner "dji4g-edge-audit-${AUDIT_NONCE}-grype-version" grype-version "$GRYPE_TARGET_ID" \
  "${SCANNER_BASE[@]}" "$GRYPE_IMAGE" version -o json || fail "could not create Grype version container"
GRYPE_VERSION_JSON=$(docker_clean container start -a "$SCANNER_CONTAINER_ID") || fail "Grype version container failed"
remove_scanner || fail "could not remove Grype version container"
[ "$(printf '%s\n' "$GRYPE_VERSION_JSON" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["version"])')" = "$GRYPE_VERSION" ] || fail "pinned Grype runtime version does not match toolchain.lock"

ROOTFS_CONTAINER_NAME="dji4g-edge-audit-${AUDIT_NONCE}-rootfs"
ROOTFS_CONTAINER_EXPECTED_IMAGE=$IMAGE_ID
ROOTFS_CONTAINER_ATTEMPT=1
if ! ROOTFS_CONTAINER_ID=$(docker_clean container create --name "$ROOTFS_CONTAINER_NAME" \
    --label "io.maccellular.audit-nonce=$AUDIT_NONCE" \
    --label io.maccellular.audit-role=rootfs "$IMAGE_ID"); then
  fail "could not create the rootfs audit container"
fi
[[ "$ROOTFS_CONTAINER_ID" =~ ^[0-9a-f]{64}$ ]] || fail "Docker returned an invalid rootfs container ID"
docker_clean container export "$ROOTFS_CONTAINER_ID" >"$TMP_OUTPUT/rootfs.tar"
python3 -I "$INSPECT_ROOTFS" --archive "$TMP_OUTPUT/rootfs.tar" \
  --output "$TMP_OUTPUT/rootfs-report.json"
docker_clean container rm -f "$ROOTFS_CONTAINER_ID" >/dev/null
ROOTFS_CONTAINER_ID=""
ROOTFS_CONTAINER_NAME=""
ROOTFS_CONTAINER_EXPECTED_IMAGE=""
ROOTFS_CONTAINER_ATTEMPT=0
create_scanner "dji4g-edge-audit-${AUDIT_NONCE}-shell-check" shell-check "$IMAGE_ID" \
  --runtime runc --ipc private --cgroupns private --network none --read-only \
  --security-opt no-new-privileges:true --security-opt apparmor=docker-default \
  --security-opt seccomp=builtin \
  --entrypoint /bin/sh "$IMAGE_ID" -c true || fail "could not create shell absence probe"
if docker_clean container start -a "$SCANNER_CONTAINER_ID" >/dev/null 2>&1; then
  fail "scratch image unexpectedly contains a shell"
fi
remove_scanner || fail "could not remove shell absence probe"

AUDIT_CONTAINER_NAME="dji4g-edge-audit-${AUDIT_NONCE}-startup"
AUDIT_CONTAINER_EXPECTED_IMAGE=$IMAGE_ID
AUDIT_CONTAINER_ATTEMPT=1
if ! AUDIT_CONTAINER_ID=$(docker_clean container create \
  --name "$AUDIT_CONTAINER_NAME" \
  --label "io.maccellular.audit-nonce=$AUDIT_NONCE" \
  --label io.maccellular.audit-role=startup \
  --init \
  --runtime runc \
  --ipc private \
  --cgroupns private \
  --network none \
  --publish-all=false \
  --restart no \
  --log-driver json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  --read-only \
  --user "$UID_VALUE:$GID_VALUE" \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  --security-opt apparmor=docker-default \
  --security-opt seccomp=builtin \
  --pids-limit 128 \
  --memory 256m \
  --memory-swap 256m \
  --cpus 1.0 \
  --shm-size 64m \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=32m,mode=1777 \
  --env-file "$TMP_OUTPUT/runtime-edge.env" \
  --mount "type=bind,source=$TMP_OUTPUT/runtime-gateway-public-key.pem,target=/run/config/gateway-public-key.pem,readonly" \
  --mount "type=bind,source=$TMP_OUTPUT/runtime-access-allowed-emails,target=/run/config/access-allowed-emails,readonly" \
  "$IMAGE_ID"); then
  fail "could not create the constrained startup audit container"
fi
[[ "$AUDIT_CONTAINER_ID" =~ ^[0-9a-f]{64}$ ]] || fail "Docker returned an invalid audit container ID"
docker_clean container start "$AUDIT_CONTAINER_ID" >/dev/null || fail "could not start the constrained startup audit container"

HEALTH=""
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
  HEALTH=$(docker_clean inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$AUDIT_CONTAINER_ID")
  [ "$HEALTH" = healthy ] && break
  RUNNING=$(docker_clean inspect --format '{{.State.Running}}' "$AUDIT_CONTAINER_ID")
  [ "$RUNNING" = true ] || break
  sleep 2
done
[ "$HEALTH" = healthy ] || fail "exact image did not become healthy in the constrained startup smoke"
docker_clean inspect "$AUDIT_CONTAINER_ID" >"$TMP_OUTPUT/startup-inspect.json"
docker_clean logs "$AUDIT_CONTAINER_ID" >"$TMP_OUTPUT/startup.log" 2>&1

create_scanner "dji4g-edge-audit-${AUDIT_NONCE}-syft-scan" syft-scan "$SYFT_TARGET_ID" \
  "${SCANNER_BASE[@]}" --tmpfs /tmp:rw,noexec,nosuid,nodev,size=32m,mode=1777 \
  --mount "type=bind,source=$TMP_OUTPUT/image.tar,target=/input/image.tar,readonly" \
  --mount "type=bind,source=$TMP_OUTPUT,target=/output" \
  "$SYFT_IMAGE" scan docker-archive:/input/image.tar -o spdx-json=/output/sbom.spdx.json || fail "could not create Syft scan container"
docker_clean container start -a "$SCANNER_CONTAINER_ID" >/dev/null || fail "Syft scan failed"
remove_scanner || fail "could not remove Syft scan container"

mkdir "$TMP_OUTPUT/grype-db"
create_scanner "dji4g-edge-audit-${AUDIT_NONCE}-grype-db-update" grype-db-update "$GRYPE_TARGET_ID" \
  --runtime runc --ipc private --cgroupns private --network bridge --read-only \
  --user "$AUDIT_UID_GID" --cap-drop ALL --security-opt no-new-privileges:true \
  --security-opt apparmor=docker-default --security-opt seccomp=builtin \
  --pids-limit 256 --memory 512m --memory-swap 512m --cpus 1.0 --shm-size 64m --log-driver none \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=32m,mode=1777 \
  -e GRYPE_DB_CACHE_DIR=/work/grype-db \
  -e GRYPE_DB_VALIDATE_AGE=true -e GRYPE_DB_MAX_ALLOWED_BUILT_AGE=48h \
  --mount "type=bind,source=$TMP_OUTPUT,target=/work" \
  "$GRYPE_IMAGE" db update || fail "could not create Grype DB update container"
docker_clean container start -a "$SCANNER_CONTAINER_ID" >/dev/null || fail "Grype DB update failed"
remove_scanner || fail "could not remove Grype DB update container"
create_scanner "dji4g-edge-audit-${AUDIT_NONCE}-grype-db-status" grype-db-status "$GRYPE_TARGET_ID" \
  "${SCANNER_BASE[@]}" \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=32m,mode=1777 \
  -e GRYPE_DB_AUTO_UPDATE=false -e GRYPE_DB_CACHE_DIR=/work/grype-db \
  -e GRYPE_DB_VALIDATE_AGE=true -e GRYPE_DB_MAX_ALLOWED_BUILT_AGE=48h \
  --mount "type=bind,source=$TMP_OUTPUT,target=/work" \
  "$GRYPE_IMAGE" db status -o json || fail "could not create Grype DB status container"
docker_clean container start -a "$SCANNER_CONTAINER_ID" >"$TMP_OUTPUT/grype-db-status.json" || fail "Grype DB status failed"
remove_scanner || fail "could not remove Grype DB status container"
create_scanner "dji4g-edge-audit-${AUDIT_NONCE}-grype-scan" grype-scan "$GRYPE_TARGET_ID" \
  "${SCANNER_BASE[@]}" \
  --tmpfs /tmp:rw,noexec,nosuid,nodev,size=32m,mode=1777 \
  -e GRYPE_DB_AUTO_UPDATE=false -e GRYPE_DB_CACHE_DIR=/work/grype-db \
  -e GRYPE_DB_VALIDATE_AGE=true -e GRYPE_DB_MAX_ALLOWED_BUILT_AGE=48h \
  --mount "type=bind,source=$TMP_OUTPUT,target=/work" \
  "$GRYPE_IMAGE" sbom:/work/sbom.spdx.json --fail-on high -o json || fail "could not create Grype scan container"
if ! docker_clean container start -a "$SCANNER_CONTAINER_ID" >"$TMP_OUTPUT/cve-report.json"; then
  fail "Grype found a High/Critical vulnerability or could not complete the offline scan"
fi
remove_scanner || fail "could not remove Grype scan container"

docker_clean container rm -f "$AUDIT_CONTAINER_ID" >/dev/null
AUDIT_CONTAINER_ID=""
AUDIT_CONTAINER_NAME=""
AUDIT_CONTAINER_EXPECTED_IMAGE=""
AUDIT_CONTAINER_ATTEMPT=0
rm -f "$TMP_OUTPUT/rootfs.tar"
rm -rf "$TMP_OUTPUT/grype-db"
AUDITED_AT=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
python3 -I - "$TMP_OUTPUT" "$IMAGE_ID" "$SOURCE_DIGEST" "$UID_VALUE:$GID_VALUE" "$AUDITED_AT" "$RUNTIME_ROOT" \
  "$GRYPE_VERSION" "$GRYPE_DB_SCHEMA_VERSION" "$GRYPE_DB_MAX_AGE_HOURS" "$AUDIT_NONCE" "$TMP_OUTPUT" <<'PY'
import hashlib, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
names = {
    "build.json", "cve-report.json", "edge-image-reference.txt", "local-image-retention-reference.txt", "grype-db-status.json",
    "image-inspect.json", "image.tar", "image.tar.sha256", "rootfs-report.json",
    "runtime-compose.env", "runtime-edge.env", "runtime-gateway-public-key.pem",
    "runtime-access-allowed-emails", "sbom.spdx.json", "startup-inspect.json",
    "startup.log", "source.lock", "toolchain.lock",
}
manifest = {
    "format": 1,
    "audited_at": sys.argv[5],
    "image_id": sys.argv[2],
    "accepted_references": [sys.argv[2]],
    "source_tree_sha256": sys.argv[3],
    "runtime_uid_gid": sys.argv[4],
    "runtime_root": sys.argv[6],
    "audit_nonce": sys.argv[10],
    "audit_input_mount_root": sys.argv[11],
    "runtime_input_sha256": {},
    "policy": {
        "architecture": "linux/amd64", "cve_threshold": "high", "health": "passed",
        "read_only_arbitrary_uid": "passed", "rootfs": "scratch-no-shell",
        "sbom": "spdx-json", "startup": "passed", "grype_version": sys.argv[7],
        "grype_db_schema": sys.argv[8], "grype_db_max_age_hours": int(sys.argv[9]),
    },
    "artifacts": {},
}
for name in sorted(names):
    manifest["artifacts"][name] = hashlib.sha256((root / name).read_bytes()).hexdigest()
for logical,name in (
    ("compose.env","runtime-compose.env"),
    ("config/edge.env","runtime-edge.env"),
    ("config/gateway-public-key.pem","runtime-gateway-public-key.pem"),
    ("config/access-allowed-emails","runtime-access-allowed-emails"),
):
    manifest["runtime_input_sha256"][logical]=manifest["artifacts"][name]
(root / "evidence.json").write_text(json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n", encoding="ascii")
PY
chmod 600 "$TMP_OUTPUT"/*
python3 -I "$VERIFY_EVIDENCE" --evidence "$TMP_OUTPUT" --image "$IMAGE_ID" \
  --repo-root "$REPO_ROOT" --trusted-source-lock "$TRUSTED_SOURCE_LOCK" \
  --trusted-toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK"
OUTPUT_STAGE_ID=$(stat -f '%d:%i' "$TMP_OUTPUT" 2>/dev/null || stat -c '%d:%i' "$TMP_OUTPUT")
OUTPUT_COMMIT_ATTEMPT=1
COMMITTED_ID=$(env -i PATH="$PATH" python3 -I "$ATOMIC_COMMIT" --source "$TMP_OUTPUT" --target "$OUTPUT")
[ "$COMMITTED_ID" = "$OUTPUT_STAGE_ID" ] || fail "atomic evidence commit returned a different inode"
TMP_OUTPUT=""
trap '' HUP INT TERM
OUTPUT_COMMITTED=1
printf 'OK: complete local image evidence created at %s\n' "$OUTPUT" || true
printf 'OK: this evidence permits only the exact local image ID %s\n' "$IMAGE_ID" || true
exit 0
