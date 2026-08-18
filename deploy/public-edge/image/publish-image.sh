#!/usr/bin/env bash
set -euo pipefail

umask 077

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../../.." && pwd -P)
VERIFY_EVIDENCE="$SCRIPT_DIR/verify-evidence.py"
TRUSTED_SOURCE_LOCK="$SCRIPT_DIR/source.lock"
TRUSTED_TOOLCHAIN_LOCK="$SCRIPT_DIR/toolchain.lock"
ATOMIC_COMMIT="$SCRIPT_DIR/atomic-commit.py"
EVIDENCE=""
REPOSITORY=""
TAG=""
OUTPUT=""
REGISTRY_AUTH_DIR=""
REGISTRY_AUTH_SNAPSHOT=""
CONFIRM=0
DOCKER_CMD=()
TMP_OUTPUT=""
DOCKER_CONFIG_DIR=""
OWNED_MUTABLE_ID=""
MUTABLE_ATTEMPT=0
OUTPUT_COMMIT_ATTEMPT=0
OUTPUT_COMMITTED=0
OUTPUT_STAGE_ID=""
REMOTE_PUSH_ATTEMPT=0

usage() {
  cat <<'EOF'
Usage:
  publish-image.sh --evidence /absolute/local-evidence \
    --repository registry.example/owner/djonehub-edge --tag v1 \
    --registry-auth-dir /absolute/private/docker-auth \
    --output /absolute/new/published-evidence --confirm-push

This is the only supported registry publication path. It is intentionally an
explicit external write. It verifies the complete local audit, pushes an
unpredictable retained remote tag, resolves the registry RepoDigest, proves that digest maps to the
same audited image ID, then outputs the exact name@sha256 reference and updated
evidence. It never treats a local tag as a RepoDigest. Registry authentication
is read only from the explicit 0700 directory (0600 config.json); ambient Docker
configuration, context, proxies and credential state are ignored. The remote
nonce tag is not automatically deleted; response loss requires reconciliation.
EOF
}

fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
cleanup() {
  local original_status=$?
  if [ "$MUTABLE_ATTEMPT" -eq 1 ] && [ -n "${MUTABLE:-}" ] && [ -n "${IMAGE_ID:-}" ]; then
    current=$(docker_clean image inspect --format '{{.Id}}' "$MUTABLE" 2>/dev/null || true)
    if [ "$current" = "$IMAGE_ID" ]; then
      docker_clean image rm "$MUTABLE" >/dev/null 2>&1 || true
    fi
  fi
  if [ -n "$TMP_OUTPUT" ]; then
    case "$TMP_OUTPUT" in */.dji4g-edge-publish.*) rm -rf -- "$TMP_OUTPUT" ;; esac
  fi
  if [ "$OUTPUT_COMMIT_ATTEMPT" -eq 1 ] && [ "$OUTPUT_COMMITTED" -eq 0 ] && \
      [ -n "$OUTPUT_STAGE_ID" ] && [ -d "$OUTPUT" ] && [ ! -L "$OUTPUT" ]; then
    current=$(stat -f '%d:%i' "$OUTPUT" 2>/dev/null || stat -c '%d:%i' "$OUTPUT" 2>/dev/null || true)
    [ "$current" != "$OUTPUT_STAGE_ID" ] || rm -rf -- "$OUTPUT"
  fi
  if [ -n "$DOCKER_CONFIG_DIR" ]; then
    case "$DOCKER_CONFIG_DIR" in
      /tmp/dji4g-edge-docker-config.*) rm -rf -- "$DOCKER_CONFIG_DIR" ;;
    esac
  fi
  if [ -n "$REGISTRY_AUTH_SNAPSHOT" ]; then
    case "$REGISTRY_AUTH_SNAPSHOT" in
      /tmp/dji4g-edge-registry-auth.*) rm -rf -- "$REGISTRY_AUTH_SNAPSHOT" ;;
    esac
  fi
  if [ "$REMOTE_PUSH_ATTEMPT" -eq 1 ] && [ "$OUTPUT_COMMITTED" -eq 0 ] && [ -n "${MUTABLE:-}" ]; then
    printf 'WARNING: no completed publication evidence was emitted; remote nonce tag may exist and requires explicit registry reconciliation: %s\n' "$MUTABLE" >&2
  fi
  return "$original_status"
}
trap cleanup EXIT
signal_exit() {
  local status=$1
  if [ "$REMOTE_PUSH_ATTEMPT" -eq 1 ] && [ -n "${MUTABLE:-}" ]; then
    printf 'WARNING: publication was interrupted after remote write began; nonce tag may exist and is not automatically deleted: %s\n' "$MUTABLE" >&2
  fi
  exit "$status"
}
trap 'signal_exit 129' HUP
trap 'signal_exit 130' INT
trap 'signal_exit 143' TERM

docker_clean() {
  env -i PATH="$PATH" "${DOCKER_CMD[@]}" \
    --config "$DOCKER_CONFIG_DIR" --context default "$@"
}

docker_registry() {
  # Registry credentials are explicit, while --context still fixes daemon
  # routing to the local default context.
  env -i PATH="$PATH" "${DOCKER_CMD[@]}" \
    --config "$REGISTRY_AUTH_SNAPSHOT" --context default "$@"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --evidence) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--evidence requires a value"; EVIDENCE=$2; shift 2 ;;
    --repository) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--repository requires a value"; REPOSITORY=$2; shift 2 ;;
    --tag) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--tag requires a value"; TAG=$2; shift 2 ;;
    --registry-auth-dir) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--registry-auth-dir requires a value"; REGISTRY_AUTH_DIR=$2; shift 2 ;;
    --output) [ "$#" -ge 2 ] && [ -n "$2" ] || fail "--output requires a value"; OUTPUT=$2; shift 2 ;;
    --confirm-push) CONFIRM=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

[ "$CONFIRM" -eq 1 ] || fail "--confirm-push is required because this writes an external registry"
[ -n "$REGISTRY_AUTH_DIR" ] || fail "--registry-auth-dir is required"
for value in "$EVIDENCE" "$OUTPUT"; do case "$value" in /*) ;; *) fail "evidence/output paths must be absolute" ;; esac; done
[ -d "$EVIDENCE" ] && [ ! -L "$EVIDENCE" ] || fail "evidence directory is invalid"
[ ! -e "$OUTPUT" ] || fail "output already exists"
[ -x "$ATOMIC_COMMIT" ] || fail "atomic output helper is missing or not executable"
for value in "$EVIDENCE"; do
  [ "$(CDPATH= cd -- "$value" && pwd -P)" = "$value" ] || fail "input paths must be physical and canonical"
done
PARENT=$(dirname -- "$OUTPUT");OUTPUT_BASE=$(basename -- "$OUTPUT")
case "$OUTPUT_BASE" in ""|.|..|*[!A-Za-z0-9_.-]*) fail "output basename is invalid" ;; esac
[ -d "$PARENT" ] && [ ! -L "$PARENT" ] || fail "output parent must exist"
[ "$(CDPATH= cd -- "$PARENT" && pwd -P)/$OUTPUT_BASE" = "$OUTPUT" ] || fail "output path must be physical and canonical"
[ "$(stat -f '%u' "$PARENT" 2>/dev/null || stat -c '%u' "$PARENT")" = "$(id -u)" ] || fail "output parent must be owned by the current user"
PARENT_MODE=$(stat -f '%Lp' "$PARENT" 2>/dev/null || stat -c '%a' "$PARENT")
case "$PARENT_MODE" in 700|710|750) ;; *) fail "output parent must be private" ;; esac
[[ "$REPOSITORY" =~ ^([a-z0-9]+([.-][a-z0-9]+)*\.[a-z0-9.-]+|localhost|[a-z0-9.-]+:[0-9]+)(/[a-z0-9]+([._-][a-z0-9]+)*)+$ ]] ||
  fail "repository must be fully qualified and lowercase"
[[ "$TAG" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$ ]] || fail "tag is invalid"
case "$REGISTRY_AUTH_DIR" in /*) ;; *) fail "registry auth directory must be absolute" ;; esac
[ -d "$REGISTRY_AUTH_DIR" ] && [ ! -L "$REGISTRY_AUTH_DIR" ] || fail "registry auth directory is invalid"
AUTH_PHYSICAL=$(CDPATH= cd -- "$REGISTRY_AUTH_DIR" && pwd -P)
[ "$AUTH_PHYSICAL" = "$REGISTRY_AUTH_DIR" ] || fail "registry auth directory must be a physical canonical path"
[ "$(stat -f '%u' "$REGISTRY_AUTH_DIR" 2>/dev/null || stat -c '%u' "$REGISTRY_AUTH_DIR")" = "$(id -u)" ] ||
  fail "registry auth directory must be owned by the current user"
[ "$(stat -f '%Lp' "$REGISTRY_AUTH_DIR" 2>/dev/null || stat -c '%a' "$REGISTRY_AUTH_DIR")" = 700 ] ||
  fail "registry auth directory must have mode 0700"
[ -f "$REGISTRY_AUTH_DIR/config.json" ] && [ ! -L "$REGISTRY_AUTH_DIR/config.json" ] ||
  fail "registry auth config.json is missing or symlinked"
[ "$(stat -f '%u' "$REGISTRY_AUTH_DIR/config.json" 2>/dev/null || stat -c '%u' "$REGISTRY_AUTH_DIR/config.json")" = "$(id -u)" ] &&
  [ "$(stat -f '%Lp' "$REGISTRY_AUTH_DIR/config.json" 2>/dev/null || stat -c '%a' "$REGISTRY_AUTH_DIR/config.json")" = 600 ] ||
  fail "registry auth config.json must be current-user owned mode 0600"
REGISTRY_AUTH_SNAPSHOT=$(mktemp -d /tmp/dji4g-edge-registry-auth.XXXXXX)
chmod 700 "$REGISTRY_AUTH_SNAPSHOT"
env -i PATH="$PATH" python3 -I - "$REGISTRY_AUTH_DIR/config.json" "$REGISTRY_AUTH_SNAPSHOT/config.json" <<'PY'
import os,stat,sys
source,target=sys.argv[1:]
descriptor=os.open(source,os.O_RDONLY|getattr(os,"O_NOFOLLOW",0))
try:
    info=os.fstat(descriptor)
    if (
        not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode)!=0o600
        or info.st_uid!=os.getuid() or info.st_gid!=os.getgid()
        or info.st_nlink!=1 or info.st_size<2 or info.st_size>1024*1024
    ):raise SystemExit("ERROR: registry auth source changed during snapshot")
    output=os.open(target,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    try:
        while True:
            chunk=os.read(descriptor,65536)
            if not chunk:break
            pending=memoryview(chunk)
            while pending:
                written=os.write(output,pending)
                if written<1:raise SystemExit("ERROR: registry auth snapshot write failed")
                pending=pending[written:]
        os.fsync(output)
    finally:os.close(output)
finally:os.close(descriptor)
PY
python3 -I - "$REGISTRY_AUTH_SNAPSHOT/config.json" <<'PY'
import json, pathlib, sys
path=pathlib.Path(sys.argv[1])
if path.stat().st_size < 2 or path.stat().st_size > 1024*1024:
    raise SystemExit("ERROR: registry auth config.json size is invalid")
try:
    value=json.loads(path.read_text(encoding="utf-8"))
except (OSError, UnicodeError, json.JSONDecodeError) as exc:
    raise SystemExit(f"ERROR: registry auth config.json is invalid: {exc}")
if not isinstance(value,dict) or set(value)!={"auths"} or not isinstance(value["auths"],dict) or not value["auths"]:
    raise SystemExit("ERROR: registry auth config must contain only a non-empty auths object")
if any(not isinstance(key,str) or not key or not isinstance(entry,dict) for key,entry in value["auths"].items()):
    raise SystemExit("ERROR: registry auth entry is invalid")
if any(set(entry)-{"auth","identitytoken","email","username","password"} for entry in value["auths"].values()):
    raise SystemExit("ERROR: registry auth entry contains unsupported fields")
PY

TMP_OUTPUT=$(mktemp -d "$PARENT/.dji4g-edge-publish.XXXXXX")
for artifact in "$EVIDENCE"/*; do
  [ -f "$artifact" ] && [ ! -L "$artifact" ] || fail "evidence snapshot source changed type"
  cp "$artifact" "$TMP_OUTPUT/"
done
python3 -I "$VERIFY_EVIDENCE" --evidence "$TMP_OUTPUT" --image "$(python3 -I - "$TMP_OUTPUT/evidence.json" <<'PY'
import json, pathlib, sys
print(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="ascii"))["image_id"])
PY
)" --repo-root "$REPO_ROOT" --trusted-source-lock "$TRUSTED_SOURCE_LOCK" \
  --trusted-toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK"
EVIDENCE=$TMP_OUTPUT
IMAGE_ID=$(python3 -I - "$EVIDENCE/evidence.json" <<'PY'
import json,pathlib,sys
print(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="ascii"))["image_id"])
PY
)
python3 -I "$VERIFY_EVIDENCE" --evidence "$EVIDENCE" --image "$IMAGE_ID" \
  --repo-root "$REPO_ROOT" --trusted-source-lock "$TRUSTED_SOURCE_LOCK" \
  --trusted-toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK"
read_lock() {
  local file=$1 key=$2 count line
  count=$(grep -Ec "^${key}=" "$file" || true)
  [ "$count" -eq 1 ] || fail "$key must appear exactly once in trusted toolchain.lock"
  line=$(grep -E "^${key}=" "$file")
  printf '%s' "${line#*=}"
}
IMAGE_STORE_DRIVER=$(read_lock "$TRUSTED_TOOLCHAIN_LOCK" IMAGE_STORE_DRIVER)
DOCKER_SERVER_VERSION=$(read_lock "$TRUSTED_TOOLCHAIN_LOCK" DOCKER_SERVER_VERSION)
DOCKER_CLIENT_VERSION=$(read_lock "$TRUSTED_TOOLCHAIN_LOCK" DOCKER_CLIENT_VERSION)
TARGET_PLATFORM=$(read_lock "$TRUSTED_TOOLCHAIN_LOCK" TARGET_PLATFORM)

DOCKER_CONFIG_DIR=$(mktemp -d /tmp/dji4g-edge-docker-config.XXXXXX)
chmod 700 "$DOCKER_CONFIG_DIR"
printf '{}\n' >"$DOCKER_CONFIG_DIR/config.json";chmod 600 "$DOCKER_CONFIG_DIR/config.json"
DOCKER_BIN=$(command -v docker 2>/dev/null || true)
SUDO_BIN=$(command -v sudo 2>/dev/null || true)
if [ -n "$DOCKER_BIN" ] && env -i PATH="$PATH" "$DOCKER_BIN" --config "$DOCKER_CONFIG_DIR" --context default info >/dev/null 2>&1; then DOCKER_CMD=("$DOCKER_BIN")
else fail "registry publication requires direct non-root local Docker access; sudo is intentionally unsupported for user-authenticated pushes"; fi
[ "$(docker_clean version --format '{{.Server.Version}}')" = "$DOCKER_SERVER_VERSION" ] ||
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
[ "$(docker_clean image inspect --format '{{.Id}}' "$IMAGE_ID" 2>/dev/null)" = "$IMAGE_ID" ] || fail "audited exact target image is not local"
[ "$(docker_clean image inspect --format '{{.Os}}/{{.Architecture}}' "$IMAGE_ID")" = "$TARGET_PLATFORM" ] || fail "audited local image platform changed"

TAG_NONCE=$(env -i PATH="$PATH" python3 -I -c 'import secrets;print(secrets.token_hex(16))')
[[ "$TAG_NONCE" =~ ^[0-9a-f]{32}$ ]] || fail "could not create an unpredictable publication tag"
MUTABLE="$REPOSITORY:${TAG}-${TAG_NONCE}"
if docker_clean image inspect "$MUTABLE" >/dev/null 2>&1; then
  fail "refusing to overwrite a pre-existing local publication tag: $MUTABLE"
fi
MUTABLE_ATTEMPT=1
docker_clean image tag "$IMAGE_ID" "$MUTABLE"
OWNED_MUTABLE_ID=$(docker_clean image inspect --format '{{.Id}}' "$MUTABLE")
[ "$OWNED_MUTABLE_ID" = "$IMAGE_ID" ] || fail "local publication tag does not resolve to the audited image"
printf 'REMOTE_WRITE_INTENT=%s\n' "$MUTABLE" >&2
REMOTE_PUSH_ATTEMPT=1
if ! docker_registry image push "$MUTABLE"; then
  fail "registry push failed or its response was lost; the remote nonce tag may exist and is not automatically deleted: $MUTABLE"
fi
IMMUTABLE=$(docker_clean image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$MUTABLE" |
  awk -v prefix="$REPOSITORY@sha256:" 'index($0,prefix)==1 {print}' | sort -u)
[ "$(printf '%s\n' "$IMMUTABLE" | awk 'NF {count++} END {print count+0}')" -eq 1 ] ||
  fail "push did not resolve exactly one repository digest"
[[ "$IMMUTABLE" =~ ^[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}$ ]] || fail "registry returned an invalid digest"
[ "$(docker_clean image inspect --format '{{.Id}}' "$IMMUTABLE")" = "$IMAGE_ID" ] ||
  fail "published repository digest does not equal the audited containerd target ID"
CURRENT_MUTABLE_ID=$(docker_clean image inspect --format '{{.Id}}' "$MUTABLE" 2>/dev/null) || fail "owned local publication tag disappeared"
[ "$CURRENT_MUTABLE_ID" = "$OWNED_MUTABLE_ID" ] || fail "owned local publication tag changed before cleanup"
docker_clean image rm "$MUTABLE" >/dev/null || fail "could not remove the owned local publication tag"
MUTABLE_ATTEMPT=0
OWNED_MUTABLE_ID=""

docker_clean image inspect "$IMMUTABLE" >"$TMP_OUTPUT/publication-inspect.json"
python3 -I - "$TMP_OUTPUT" "$IMMUTABLE" "$MUTABLE" "$REPOSITORY" "$TAG" <<'PY'
import hashlib, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
path = root / "evidence.json"
value = json.loads(path.read_text(encoding="ascii"))
value["accepted_references"] = [value["image_id"], sys.argv[2]]
value["artifacts"]["publication-inspect.json"] = hashlib.sha256((root / "publication-inspect.json").read_bytes()).hexdigest()
(root / "publication-record.json").write_text(json.dumps({
    "format":1,
    "immutable_reference":sys.argv[2],
    "remote_nonce_tag":sys.argv[3],
    "repository":sys.argv[4],
    "requested_tag":sys.argv[5],
    "remote_nonce_retention":"not-automatically-deleted",
    "response_loss_policy":"remote-nonce-tag-may-exist-reconcile-before-retry",
},sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
value["artifacts"]["publication-record.json"] = hashlib.sha256((root / "publication-record.json").read_bytes()).hexdigest()
path.write_text(json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n", encoding="ascii")
PY
chmod 600 "$TMP_OUTPUT"/*
python3 -I "$VERIFY_EVIDENCE" --evidence "$TMP_OUTPUT" --image "$IMMUTABLE" \
  --repo-root "$REPO_ROOT" --trusted-source-lock "$TRUSTED_SOURCE_LOCK" \
  --trusted-toolchain-lock "$TRUSTED_TOOLCHAIN_LOCK"
OUTPUT_STAGE_ID=$(stat -f '%d:%i' "$TMP_OUTPUT" 2>/dev/null || stat -c '%d:%i' "$TMP_OUTPUT")
OUTPUT_COMMIT_ATTEMPT=1
COMMITTED_ID=$(env -i PATH="$PATH" python3 -I "$ATOMIC_COMMIT" --source "$TMP_OUTPUT" --target "$OUTPUT")
[ "$COMMITTED_ID" = "$OUTPUT_STAGE_ID" ] || fail "atomic publication evidence commit returned a different inode"
TMP_OUTPUT=""
trap '' HUP INT TERM
OUTPUT_COMMITTED=1
printf 'OK: published immutable Compose reference: %s\n' "$IMMUTABLE" || true
printf 'OK: updated evidence: %s\n' "$OUTPUT" || true
exit 0
