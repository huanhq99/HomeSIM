#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../../.." && pwd -P)
PRELOCK=0
PASS=0
TMP_ROOT=""

[ "${1:-}" != --prelock ] || PRELOCK=1
[ "$#" -le 1 ] || { printf 'ERROR: only --prelock is accepted\n' >&2; exit 1; }

pass() { PASS=$((PASS + 1)); printf 'ok %d - %s\n' "$PASS" "$1"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
expect_fail() {
  local label=$1; shift
  if "$@" >/dev/null 2>&1; then fail "$label unexpectedly succeeded"; fi
  pass "$label"
}
cleanup() {
  if [ -n "$TMP_ROOT" ]; then
    case "$TMP_ROOT" in */dji4g-edge-image-static.*) rm -rf -- "$TMP_ROOT" ;; esac
  fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

for script in "$SCRIPT_DIR"/*.sh "$SCRIPT_DIR/../"*.sh; do bash -n "$script"; done
for script in "$SCRIPT_DIR"/*.py "$SCRIPT_DIR/../"*.py; do
  PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$script" <<'PY'
import ast, pathlib, sys
ast.parse(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"), filename=sys.argv[1])
PY
done
pass "all image/deployment scripts parse without Docker or network"

grep -Fqx '# syntax=docker.io/docker/dockerfile:1.20.0@sha256:26147acbda4f14c5add9946e2fd2ed543fc402884fd75146bd342a7f6271dc1d' "$SCRIPT_DIR/Dockerfile" || fail "Dockerfile frontend is not pinned"
grep -Fqx 'ARG GO_BUILDER_IMAGE' "$SCRIPT_DIR/Dockerfile" || fail "builder image must be a required build argument"
if grep -Eq '^ARG GO_BUILDER_IMAGE=' "$SCRIPT_DIR/Dockerfile"; then fail "builder image ARG must not have a default"; fi
[ "$(grep -Ec '^FROM scratch$' "$SCRIPT_DIR/Dockerfile")" -eq 1 ] || fail "runtime must be scratch"
awk 'seen {print} /^FROM scratch$/ {seen=1}' "$SCRIPT_DIR/Dockerfile" | grep -Eq 'RUN|/bin/sh|/bin/bash' && fail "scratch runtime contains a shell/build step"
grep -Fq 'USER 65532:65532' "$SCRIPT_DIR/Dockerfile" || fail "image lacks non-root default"
grep -Fq 'HEALTHCHECK ' "$SCRIPT_DIR/Dockerfile" || fail "image lacks a native healthcheck"
grep -Fq 'go clean -cache' "$SCRIPT_DIR/Dockerfile" || fail "binary reproducibility comparison is absent"
grep -Fq 'find /out/rootfs -type d -exec touch -d "@${SOURCE_DATE_EPOCH}" {} +' "$SCRIPT_DIR/Dockerfile" || fail "rootfs directory timestamps are not normalized"
grep -Fq 'io.maccellular.toolchain-lock-sha256="${TOOLCHAIN_LOCK_SHA256}"' "$SCRIPT_DIR/Dockerfile" || fail "toolchain lock label is absent"
grep -Fq -- '--build-arg "GO_BUILDER_IMAGE=$BUILDER_IMAGE"' "$SCRIPT_DIR/build-image.sh" || fail "build does not supply the locked builder argument"
grep -Fq 'buildx' "$SCRIPT_DIR/build-image.sh" || fail "build does not explicitly require Buildx"
grep -Fq -- '--builder default' "$SCRIPT_DIR/build-image.sh" || fail "build does not pin the local default Buildx builder"
grep -Fq -- '--provenance=false' "$SCRIPT_DIR/build-image.sh" || fail "build does not disable implicit provenance attestations"
grep -Fq -- '--sbom=false' "$SCRIPT_DIR/build-image.sh" || fail "build does not disable implicit SBOM attestations"
pass "Dockerfile pins inputs and defines a shell-free non-root health contract"

EXPECTED_KEYS='FORMAT TARGET_PLATFORM DOCKER_SERVER_VERSION DOCKER_CLIENT_VERSION IMAGE_STORE_DRIVER DOCKER_ROOT_CONFIG_DIR DOCKER_ROOT_CONFIG_SHA256 COMPOSE_CLI_VERSION COMPOSE_PACKAGE_NAME COMPOSE_PACKAGE_VERSION COMPOSE_PLUGIN_PATH COMPOSE_LINUX_AMD64_SHA256 COMPOSE_STATIC_DARWIN_ARM64_VERSION COMPOSE_STATIC_DARWIN_ARM64_SHA256 CLOUDFLARED_IMAGE BUILDX_VERSION BUILDX_PLUGIN_PATH BUILDX_LINUX_AMD64_SHA256 DOCKERFILE_FRONTEND GO_BUILDER_IMAGE GO_BUILDER_TARGET_DIGEST GO_VERSION GOPROXY GOSUMDB SYFT_IMAGE SYFT_VERSION GRYPE_IMAGE GRYPE_VERSION GRYPE_DB_SCHEMA_VERSION GRYPE_DB_MAX_AGE_HOURS'
ACTUAL_KEYS=$(sed 's/=.*//' "$SCRIPT_DIR/toolchain.lock" | tr '\n' ' ' | sed 's/ $//')
[ "$ACTUAL_KEYS" = "$EXPECTED_KEYS" ] || fail "toolchain.lock schema changed"
grep -Eq '^SYFT_IMAGE=anchore/syft@sha256:[0-9a-f]{64}$' "$SCRIPT_DIR/toolchain.lock" || fail "Syft is not digest-pinned"
grep -Eq '^GRYPE_IMAGE=anchore/grype@sha256:[0-9a-f]{64}$' "$SCRIPT_DIR/toolchain.lock" || fail "Grype is not digest-pinned"
pass "builder, scanner and deployment tool versions are locked"

for script in "$SCRIPT_DIR/build-image.sh" "$SCRIPT_DIR/audit-image.sh" "$SCRIPT_DIR/../deploy.sh"; do
  grep -Fq 'root Docker config.json must be root-owned mode 0644' "$script" ||
    fail "$(basename "$script") does not enforce the readable root-owned canonical config mode"
  grep -Fq 'root Docker config.json must have exactly one hard link' "$script" ||
    fail "$(basename "$script") does not enforce the canonical config hard-link boundary"
done
grep -Fq 'sudo install -o root -g root -m 0644' "$SCRIPT_DIR/../README.md" ||
  fail "production runbook does not install the non-secret canonical root config readably"
grep -Fq 'with mode `0644`' "$SCRIPT_DIR/README.md" ||
  fail "image runbook does not document the canonical root config mode"
pass "build, audit, deploy and runbooks agree on root-owned readable canonical Docker config"

DEPLOY="$SCRIPT_DIR/../deploy.sh"
grep -Fq 'DOCKER_CMD=("$sudo_bin" -n docker)' "$DEPLOY" || fail "controlled daemon sudo fallback is missing"
grep -Fq -- '--config "$DOCKER_CONFIG_DIR" --context default' "$DEPLOY" || fail "local default daemon/config pin is missing"
grep -Fq -- '--env-file "$COMPOSE_ENV_FILE"' "$DEPLOY" || fail "Compose does not consume the selected verified env file under sudo"
grep -Fq 'RUNTIME_SNAPSHOT_STAGE=$(mktemp -d "$parent/.$basename.edge-stage.XXXXXX")' "$DEPLOY" ||
  fail "start-edge does not create a randomly named private snapshot stage"
grep -Fq 'STATE_RUNTIME_ROOT=$RUNTIME_SNAPSHOT' "$DEPLOY" ||
  fail "runtime evidence is not bound to the immutable snapshot path"
grep -Fq 'rollback_status=%s' "$DEPLOY" || fail "rollback result status is not explicit"
grep -Fq 'rollback=incomplete' "$DEPLOY" || fail "incomplete rollback is not distinguished"
grep -Fq 'rm -f -s -v edge cloudflared' "$DEPLOY" || fail "edge-only removal target changed"
if grep -Eq 'rollback[^\n]*\|\|[[:space:]]*true' "$DEPLOY"; then fail "rollback failure is masked"; fi
if grep -Eq 'sudo[[:space:]]+-n[[:space:]]+(bash|sh|.*deploy\.sh)' "$DEPLOY"; then fail "whole-script sudo is forbidden"; fi
pass "daemon sudo, exact edge-only rollback and failure wording are explicit"

TMP_BASE=${TMPDIR:-/tmp}
TMP_CREATED=$(mktemp -d "$TMP_BASE/dji4g-edge-image-static.XXXXXX")
TMP_ROOT=$(CDPATH= cd -- "$TMP_CREATED" && pwd -P)

ATOMIC_PARENT="$TMP_ROOT/atomic";mkdir "$ATOMIC_PARENT"
mkdir "$ATOMIC_PARENT/stage" "$ATOMIC_PARENT/output"
printf '%s\n' staged >"$ATOMIC_PARENT/stage/marker"
expect_fail "atomic output commit rejects a concurrently-created target directory" \
  python3 -B -I "$SCRIPT_DIR/atomic-commit.py" \
    --source "$ATOMIC_PARENT/stage" --target "$ATOMIC_PARENT/output"
[ -f "$ATOMIC_PARENT/stage/marker" ] && [ ! -e "$ATOMIC_PARENT/output/stage" ] || \
  fail "failed no-replace commit moved/nested the staged directory"
rmdir "$ATOMIC_PARENT/output"
ATOMIC_STAGE_ID=$(python3 -B -I "$SCRIPT_DIR/atomic-commit.py" \
  --source "$ATOMIC_PARENT/stage" --target "$ATOMIC_PARENT/output")
[[ "$ATOMIC_STAGE_ID" =~ ^[0-9]+:[0-9]+$ ]] && [ -f "$ATOMIC_PARENT/output/marker" ] && \
  [ ! -e "$ATOMIC_PARENT/stage" ] || fail "atomic output commit did not preserve the staged inode"
pass "atomic output commit publishes exactly once by verified inode"

SOURCE_REPO="$TMP_ROOT/source-repo"
mkdir -p "$SOURCE_REPO/cmd/djonehub-edge" "$SOURCE_REPO/internal/publicedge" \
  "$SOURCE_REPO/internal/publicedgeserver" "$SOURCE_REPO/third_party/example" \
  "$SOURCE_REPO/deploy/public-edge/image" "$SOURCE_REPO/deploy/public-edge/templates"
printf 'module example.invalid/edge\n\ngo 1.26\n' >"$SOURCE_REPO/go.mod"
printf '%s\n' '# synthetic empty sum' >"$SOURCE_REPO/go.sum"
printf '%s\n' 'package main' 'func main() {}' >"$SOURCE_REPO/cmd/djonehub-edge/main.go"
printf '%s\n' 'package publicedge' >"$SOURCE_REPO/internal/publicedge/publicedge.go"
printf '%s\n' 'package publicedgeserver' >"$SOURCE_REPO/internal/publicedgeserver/server.go"
printf '%s\n' 'pinned third party input' >"$SOURCE_REPO/third_party/example/README"
printf '%s\n' 'package main' 'func main() {}' >"$SOURCE_REPO/deploy/public-edge/image/healthcheck.go"
for name in Dockerfile audit-image.sh build-image.sh publish-image.sh \
  atomic-commit.py sanitize-image-archive.py \
  docker-cli-root-config.json refresh-source-lock.py toolchain.lock verify-build-bundle.py \
  verify-evidence.py verify-source.py; do
  cp "$SCRIPT_DIR/$name" "$SOURCE_REPO/deploy/public-edge/image/$name"
done
for name in compose.yaml deploy.sh generate-config.sh verify-compose-policy.py \
  verify-action-evidence.py verify-config.sh verify-runtime-state.py; do
  cp "$SCRIPT_DIR/../$name" "$SOURCE_REPO/deploy/public-edge/$name"
done
cp "$SCRIPT_DIR/../templates/"* "$SOURCE_REPO/deploy/public-edge/templates/"
# The synthetic audit runtime deliberately contains only the fields needed by
# audit-image.sh itself.  Commit this no-op verifier into the isolated fixture
# repository so provenance still binds the exact helper that the copied audit
# workflow executes; never dirty a selected workflow after source.lock exists.
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$SOURCE_REPO/deploy/public-edge/verify-config.sh"
chmod 700 "$SOURCE_REPO/deploy/public-edge/verify-config.sh"
FIXTURE_BUILDX="$TMP_ROOT/docker-buildx-fixture"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$FIXTURE_BUILDX"
chmod 500 "$FIXTURE_BUILDX"
FIXTURE_BUILDX_SHA=$(shasum -a 256 "$FIXTURE_BUILDX" | awk '{print $1}')
sed -i.bak "s/^BUILDX_LINUX_AMD64_SHA256=.*/BUILDX_LINUX_AMD64_SHA256=$FIXTURE_BUILDX_SHA/" \
  "$SOURCE_REPO/deploy/public-edge/image/toolchain.lock"
rm "$SOURCE_REPO/deploy/public-edge/image/toolchain.lock.bak"
sed -i.bak "s|^BUILDX_PLUGIN_PATH=.*|BUILDX_PLUGIN_PATH=$FIXTURE_BUILDX|" \
  "$SOURCE_REPO/deploy/public-edge/image/toolchain.lock"
rm "$SOURCE_REPO/deploy/public-edge/image/toolchain.lock.bak"
FIXTURE_TOOLCHAIN="$SOURCE_REPO/deploy/public-edge/image/toolchain.lock"
# The audit collision test only reaches the rootfs inspector after a synthetic
# export.  Commit this bounded fixture implementation too, so provenance still
# covers the exact workflow file that audit-image.sh will invoke.
printf '%s\n' \
  '#!/usr/bin/env python3' \
  'import pathlib, sys' \
  'args=sys.argv[1:]; pathlib.Path(args[args.index("--output")+1]).write_text("{}\\n",encoding="ascii")' \
  >"$SOURCE_REPO/deploy/public-edge/image/inspect-rootfs.py"
git -C "$SOURCE_REPO" init -q
git -C "$SOURCE_REPO" config user.name 'Static Test'
git -C "$SOURCE_REPO" config user.email static@example.invalid
git -C "$SOURCE_REPO" add -- .
GIT_AUTHOR_DATE=2026-01-02T03:04:05Z GIT_COMMITTER_DATE=2026-01-02T03:04:05Z \
  git -C "$SOURCE_REPO" commit -qm 'locked source fixture'
SOURCE_REVISION=$(git -C "$SOURCE_REPO" rev-parse HEAD)
PYTHONDONTWRITEBYTECODE=1 python3 -B "$SCRIPT_DIR/refresh-source-lock.py" \
  --repo-root "$SOURCE_REPO" --output "$SOURCE_REPO/source.lock" --revision "$SOURCE_REVISION" >/dev/null
PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$SCRIPT_DIR/verify-source.py" \
  --repo-root "$SOURCE_REPO" --lock "$SOURCE_REPO/source.lock" >/dev/null
pass "source lock is derived from a real reachable commit and exact selected Git blobs"

printf '\n' >>"$SOURCE_REPO/deploy/public-edge/image/toolchain.lock"
expect_fail "uncommitted toolchain.lock cannot satisfy source provenance" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" \
    --lock "$SOURCE_REPO/source.lock"
git -C "$SOURCE_REPO" show HEAD:deploy/public-edge/image/toolchain.lock \
  >"$SOURCE_REPO/deploy/public-edge/image/toolchain.lock"

printf '\n# uncommitted verifier change\n' \
  >>"$SOURCE_REPO/deploy/public-edge/image/verify-build-bundle.py"
expect_fail "dirty workflow verifier cannot mint a replacement source lock" \
  python3 -B "$SCRIPT_DIR/refresh-source-lock.py" --repo-root "$SOURCE_REPO" \
    --output "$TMP_ROOT/dirty-workflow.lock" --revision "$SOURCE_REVISION"
git -C "$SOURCE_REPO" show HEAD:deploy/public-edge/image/verify-build-bundle.py \
  >"$SOURCE_REPO/deploy/public-edge/image/verify-build-bundle.py"

printf '\n# uncommitted deployment verifier change\n' \
  >>"$SOURCE_REPO/deploy/public-edge/verify-config.sh"
expect_fail "dirty runtime/deployment policy cannot satisfy source provenance" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" \
    --lock "$SOURCE_REPO/source.lock"
git -C "$SOURCE_REPO" show HEAD:deploy/public-edge/verify-config.sh \
  >"$SOURCE_REPO/deploy/public-edge/verify-config.sh"

env GIT_DIR=/attacker/git-dir GIT_COMMON_DIR=/attacker/common \
  GIT_WORK_TREE=/attacker/worktree GIT_INDEX_FILE=/attacker/index \
  GIT_OBJECT_DIRECTORY=/attacker/objects GIT_ALTERNATE_OBJECT_DIRECTORIES=/attacker/alternate \
  GIT_CONFIG=/attacker/config GIT_CONFIG_GLOBAL=/attacker/global GIT_CONFIG_SYSTEM=/attacker/system \
  GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.fsmonitor GIT_CONFIG_VALUE_0=/attacker/hook \
  GIT_REPLACE_REF_BASE=refs/attacker GIT_NAMESPACE=attacker \
  PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$SCRIPT_DIR/verify-source.py" \
    --repo-root "$SOURCE_REPO" --lock "$SOURCE_REPO/source.lock" >/dev/null
pass "ambient Git paths, objects, replace namespace and config cannot redirect provenance"

LINKED_WORKTREE="$TMP_ROOT/source-linked-worktree"
git -C "$SOURCE_REPO" worktree add -q --detach "$LINKED_WORKTREE" "$SOURCE_REVISION"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$SCRIPT_DIR/verify-source.py" \
  --repo-root "$LINKED_WORKTREE" --lock "$SOURCE_REPO/source.lock" >/dev/null
pass "a normal linked Git worktree resolves its physical worktree and common Git directories"

GIT_DIR_PATH=$(git -C "$SOURCE_REPO" rev-parse --absolute-git-dir)
mkdir -p "$GIT_DIR_PATH/info"
: >"$GIT_DIR_PATH/info/grafts"
expect_fail "legacy repository graft file is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" --lock "$SOURCE_REPO/source.lock"
rm "$GIT_DIR_PATH/info/grafts"

ORIGINAL_SOURCE_REVISION=$SOURCE_REVISION
printf '%s\n' 'package publicedge' '// replacement commit content' >"$SOURCE_REPO/internal/publicedge/publicedge.go"
git -C "$SOURCE_REPO" add -- internal/publicedge/publicedge.go
GIT_AUTHOR_DATE=2026-01-02T03:05:06Z GIT_COMMITTER_DATE=2026-01-02T03:05:06Z \
  git -C "$SOURCE_REPO" commit -qm 'replacement provenance fixture'
SOURCE_REVISION=$(git -C "$SOURCE_REPO" rev-parse HEAD)
PYTHONDONTWRITEBYTECODE=1 python3 -B "$SCRIPT_DIR/refresh-source-lock.py" \
  --repo-root "$SOURCE_REPO" --output "$TMP_ROOT/replacement-source.lock" \
  --revision "$SOURCE_REVISION" >/dev/null
cp "$TMP_ROOT/replacement-source.lock" "$TMP_ROOT/replace-forged-source.lock"
sed -i.bak "s/^SOURCE_REVISION=.*/SOURCE_REVISION=$ORIGINAL_SOURCE_REVISION/" \
  "$TMP_ROOT/replace-forged-source.lock"
rm "$TMP_ROOT/replace-forged-source.lock.bak"
git -C "$SOURCE_REPO" replace "$ORIGINAL_SOURCE_REVISION" "$SOURCE_REVISION"
expect_fail "real Git replace ref cannot substitute another commit into provenance" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" \
    --lock "$TMP_ROOT/replace-forged-source.lock"
git -C "$SOURCE_REPO" replace -d "$ORIGINAL_SOURCE_REVISION" >/dev/null
cp "$TMP_ROOT/replacement-source.lock" "$SOURCE_REPO/source.lock"

cp "$SOURCE_REPO/source.lock" "$TMP_ROOT/source-fake-revision.lock"
sed -i.bak "s/^SOURCE_REVISION=.*/SOURCE_REVISION=$(printf 'f%.0s' {1..40})/" "$TMP_ROOT/source-fake-revision.lock"
rm "$TMP_ROOT/source-fake-revision.lock.bak"
expect_fail "forged/nonexistent source revision is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" --lock "$TMP_ROOT/source-fake-revision.lock"

cp "$SOURCE_REPO/source.lock" "$TMP_ROOT/source-fake-epoch.lock"
sed -i.bak 's/^SOURCE_DATE_EPOCH=.*/SOURCE_DATE_EPOCH=1/' "$TMP_ROOT/source-fake-epoch.lock"
rm "$TMP_ROOT/source-fake-epoch.lock.bak"
expect_fail "hand-edited source epoch is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" --lock "$TMP_ROOT/source-fake-epoch.lock"

cp "$SOURCE_REPO/source.lock" "$TMP_ROOT/source-fake-created.lock"
sed -i.bak 's/^SOURCE_CREATED_RFC3339=.*/SOURCE_CREATED_RFC3339=1970-01-01T00:00:01Z/' "$TMP_ROOT/source-fake-created.lock"
rm "$TMP_ROOT/source-fake-created.lock.bak"
expect_fail "hand-edited source created timestamp is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" --lock "$TMP_ROOT/source-fake-created.lock"

cp "$SOURCE_REPO/source.lock" "$TMP_ROOT/source-fake-tree.lock"
sed -i.bak "s/^SOURCE_TREE_SHA256=.*/SOURCE_TREE_SHA256=$(printf 'e%.0s' {1..64})/" "$TMP_ROOT/source-fake-tree.lock"
rm "$TMP_ROOT/source-fake-tree.lock.bak"
expect_fail "hand-edited selected-tree digest is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" --lock "$TMP_ROOT/source-fake-tree.lock"

printf '%s\n' 'package publicedge' '// uncommitted change' >"$SOURCE_REPO/internal/publicedge/publicedge.go"
expect_fail "selected working-tree content must equal the locked commit blob" \
  python3 -B -I "$SCRIPT_DIR/verify-source.py" --repo-root "$SOURCE_REPO" --lock "$SOURCE_REPO/source.lock"
git -C "$SOURCE_REPO" show HEAD:internal/publicedge/publicedge.go >"$SOURCE_REPO/internal/publicedge/publicedge.go"

RACE_CONTEXT="$TMP_ROOT/race-context"; mkdir "$RACE_CONTEXT"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$SCRIPT_DIR/verify-source.py" \
  "$SOURCE_REPO" "$SOURCE_REPO/source.lock" "$RACE_CONTEXT" <<'PY'
import importlib.util,pathlib,sys
module_path=pathlib.Path(sys.argv[1]);root=pathlib.Path(sys.argv[2]);lock_path=pathlib.Path(sys.argv[3]);destination=pathlib.Path(sys.argv[4])
spec=importlib.util.spec_from_file_location("race_verify_source",module_path);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
lock=module.load_lock(lock_path);files=module.selected_files(root);blobs=module.verify_repository_binding(root,lock,files)
target=root/"internal/publicedge/publicedge.go";target.write_text("unlocked race bytes\n",encoding="ascii")
module.stage(root,destination,blobs,int(lock["SOURCE_DATE_EPOCH"]),int(lock["SOURCE_FILE_COUNT"]),lock["SOURCE_TREE_SHA256"])
expected=module.git(root,"cat-file","blob",blobs["internal/publicedge/publicedge.go"],text=False)
if (destination/"internal/publicedge/publicedge.go").read_bytes()!=expected:raise SystemExit("race bytes entered context")
PY
git -C "$SOURCE_REPO" show HEAD:internal/publicedge/publicedge.go >"$SOURCE_REPO/internal/publicedge/publicedge.go"
pass "context staging exports locked commit blobs despite a concurrent worktree rewrite"

cp "$SOURCE_REPO/source.lock" "$SOURCE_REPO/deploy/public-edge/image/source.lock"
chmod 755 "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
  "$SOURCE_REPO/deploy/public-edge/image/verify-source.py" \
  "$SOURCE_REPO/deploy/public-edge/image/verify-build-bundle.py"
"$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
  --output "$TMP_ROOT/dry-build-output" --repository local.invalid/maccellular/edge >/dev/null
[ ! -e "$TMP_ROOT/dry-build-output" ] || fail "build dry-run wrote output"
pass "build dry-run invokes the exact repository/source-lock binding without Docker"
cp "$TMP_ROOT/source-fake-revision.lock" "$SOURCE_REPO/deploy/public-edge/image/source.lock"
expect_fail "build rejects a hand-edited source revision before Docker" \
  "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
    --output "$TMP_ROOT/dry-build-forged" --repository local.invalid/maccellular/edge
cp "$SOURCE_REPO/source.lock" "$SOURCE_REPO/deploy/public-edge/image/source.lock"

if [ "$PRELOCK" -eq 0 ]; then
  [ -f "$SCRIPT_DIR/source.lock" ] || fail "source.lock is missing; commit selected inputs, then refresh it"
  mkdir "$TMP_ROOT/context"
  PYTHONDONTWRITEBYTECODE=1 python3 -B -I "$SCRIPT_DIR/verify-source.py" \
    --repo-root "$REPO_ROOT" --lock "$SCRIPT_DIR/source.lock" --copy-to "$TMP_ROOT/context"
  [ -f "$TMP_ROOT/context/Dockerfile" ] && [ -f "$TMP_ROOT/context/image/healthcheck.go" ] || fail "minimal context staging failed"
  [ ! -e "$TMP_ROOT/context/.git" ] && [ ! -e "$TMP_ROOT/context/.env" ] || fail "minimal context included repository metadata or credentials"
  pass "source.lock matches and stages only the explicit build context"
else
  pass "prelock phase intentionally deferred source.lock verification"
  CURRENT_HEAD=$(git -C "$REPO_ROOT" rev-parse HEAD)
  expect_fail "dirty/uncommitted selected inputs cannot mint source.lock" \
    python3 -B "$SCRIPT_DIR/refresh-source-lock.py" --repo-root "$REPO_ROOT" \
      --output "$TMP_ROOT/source.lock" --revision "$CURRENT_HEAD"
fi

STATE="$SCRIPT_DIR/../verify-runtime-state.py"
STATE_COMPOSE_FILE="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd -P)/compose.yaml"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$TMP_ROOT" "$STATE_COMPOSE_FILE" <<'PY'
import copy,json,pathlib,sys
root=pathlib.Path(sys.argv[1]);compose=str(pathlib.Path(sys.argv[2]).resolve());runtime=str(root/"state-runtime");edge="sha256:"+"1"*64;cf="cloudflare/cloudflared@sha256:"+"2"*64;turn="coturn/coturn@sha256:"+"3"*64
(root/"state-runtime/config").mkdir(parents=True)
edge_env={"DJI4G_EDGE_LISTEN_ADDR":"0.0.0.0:8080","DJI4G_EDGE_PUBLIC_HOST":"phone.example.com","DJI4G_EDGE_GATEWAY_ID":"synthetic-gateway","DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE":"/run/config/gateway-public-key.pem","DJI4G_ACCESS_TEAM_DOMAIN":"synthetic.cloudflareaccess.com","DJI4G_ACCESS_AUDIENCE":"synthetic_audience","DJI4G_ACCESS_ALLOWED_EMAILS_FILE":"/run/config/access-allowed-emails","DJI4G_EDGE_TRUST_FORWARDED_IDENTITY":"false","DJI4G_EDGE_ENROLLMENT_ENABLED":"false","DJI4G_EDGE_MUTATIONS_ENABLED":"false","DJI4G_EDGE_PUSH_ENABLED":"false","DJI4G_EDGE_TURN_ISSUANCE_ENABLED":"false"}
(root/"state-runtime/config/edge.env").write_text("".join(f"{key}={value}\n" for key,value in edge_env.items()),encoding="ascii")
ids={"edge":"4"*64,"cloudflared":"5"*64,"coturn":"6"*64,"attacker":"7"*64}
netids={"control":"8"*64,"turn":"9"*64};endpoint_ids={"edge":"a"*64,"cloudflared":"b"*64,"coturn":"c"*64,"attacker":"d"*64}
addresses={"edge":("172.31.0.2","172.31.0.1",16),"cloudflared":("172.31.0.3","172.31.0.1",16),"coturn":("172.30.247.2","172.30.247.1",28)}
def item(service,image,health=None):
 actual=service if service in {"edge","cloudflared","coturn"} else "cloudflared"
 state={"Status":"running","Running":True,"Paused":False,"Restarting":False,"OOMKilled":False,"Dead":False,"ExitCode":0,"Error":"","StartedAt":"2026-01-02T03:05:00Z","FinishedAt":"0001-01-01T00:00:00Z"}
 if health is not None:state["Health"]={"Status":health}
 binds={"edge":{"/run/config/gateway-public-key.pem":runtime+"/config/gateway-public-key.pem","/run/config/access-allowed-emails":runtime+"/config/access-allowed-emails"},"cloudflared":{"/run/secrets/cloudflare-tunnel-token":runtime+"/secrets/cloudflare-tunnel.token"},"coturn":{"/etc/coturn/turnserver.conf":runtime+"/config/turnserver.conf","/run/secrets/turn-tls-cert.pem":runtime+"/secrets/turn-tls-cert.pem","/run/secrets/turn-tls-key.pem":runtime+"/secrets/turn-tls-key.pem"}}[actual]
 limits={"edge":(128,268435456,1000000000,"control","32m"),"cloudflared":(64,134217728,500000000,"control","16m"),"coturn":(128,268435456,1000000000,"turn","32m")}[actual]
 network_name="dji4g-public-edge_public-edge-"+limits[3]
 port_bindings={};resolved_ports={"8080/tcp":None} if actual=="edge" else {}
 if actual=="coturn":
  port_bindings={"3478/tcp":[{"HostIp":"0.0.0.0","HostPort":"3478"}],"3478/udp":[{"HostIp":"0.0.0.0","HostPort":"3478"}],"5349/tcp":[{"HostIp":"0.0.0.0","HostPort":"443"}]}
  for port in range(49160,49168):port_bindings[f"{port}/udp"]=[{"HostIp":"0.0.0.0","HostPort":str(port)}]
  resolved_ports=port_bindings
 requested=[{"Type":"bind","Source":source,"Target":target,"ReadOnly":True,"Consistency":"","BindOptions":{"Propagation":"rprivate"}} for target,source in binds.items()]
 resolved=[{"Type":"bind","Source":source,"Destination":target,"Mode":"ro","RW":False,"Propagation":"rprivate"} for target,source in binds.items()]
 host={"ReadonlyRootfs":True,"Privileged":False,"CapAdd":None,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges:true","apparmor=docker-default","seccomp=builtin"],"Init":True,"PidsLimit":limits[0],"Memory":limits[1],"MemorySwap":limits[1],"MemoryReservation":0,"MemorySwappiness":None,"NanoCpus":limits[2],"CpuShares":0,"CpuPeriod":0,"CpuQuota":0,"CpuRealtimePeriod":0,"CpuRealtimeRuntime":0,"CpuCount":0,"CpuPercent":0,"CpusetCpus":"","CpusetMems":"","NetworkMode":network_name,"AutoRemove":False,"PublishAllPorts":False,"PortBindings":port_bindings,"PidMode":"","IpcMode":"private","UTSMode":"","UsernsMode":"","CgroupnsMode":"private","Runtime":"runc","CgroupParent":"","Cgroup":"","Sysctls":{},"OomKillDisable":False,"OomScoreAdj":0,"BlkioWeight":0,"BlkioWeightDevice":None,"BlkioDeviceReadBps":None,"BlkioDeviceWriteBps":None,"BlkioDeviceReadIOps":None,"BlkioDeviceWriteIOps":None,"DeviceCgroupRules":None,"StorageOpt":{},"ShmSize":67108864,"Isolation":"","Dns":[],"DnsOptions":[],"DnsSearch":[],"LogConfig":{"Type":"json-file","Config":{"max-size":"10m","max-file":"3"}},"ExtraHosts":[],"Devices":[],"DeviceRequests":[],"VolumesFrom":[],"Links":[],"GroupAdd":[],"RestartPolicy":{"Name":"no","MaximumRetryCount":0},"ContainerIDFile":"","VolumeDriver":"","IOMaximumIOps":0,"IOMaximumBandwidth":0,"MaskedPaths":["/proc/acpi","/proc/asound","/proc/interrupts","/proc/kcore","/proc/keys","/proc/latency_stats","/proc/sched_debug","/proc/scsi","/proc/timer_list","/proc/timer_stats","/sys/devices/virtual/powercap","/sys/firmware"],"ReadonlyPaths":["/proc/bus","/proc/fs","/proc/irq","/proc/sys","/proc/sysrq-trigger"],"Mounts":requested,"Tmpfs":{"/tmp":"rw,noexec,nosuid,nodev,size="+limits[4]+",mode=1777"},"Ulimits":[{"Name":"nofile","Hard":65536,"Soft":65536}] if actual=="coturn" else []}
 address,gateway,prefix=addresses[actual];name=f"dji4g-public-edge-{service}-1";aliases=[name,service];dns=[*aliases,ids[service][:12]]
 endpoint={"IPAMConfig":{"IPv4Address":"172.30.247.2"} if actual=="coturn" else None,"Links":None,"Aliases":aliases,"MacAddress":"02:42:ac:1f:00:02","DriverOpts":None,"GwPriority":0,"NetworkID":netids[limits[3]],"EndpointID":endpoint_ids[service],"Gateway":gateway,"IPAddress":address,"IPPrefixLen":prefix,"IPv6Gateway":"","GlobalIPv6Address":"","GlobalIPv6PrefixLen":0,"DNSNames":dns}
 target_id=image if image.startswith("sha256:") else "sha256:"+image.rsplit("@sha256:",1)[1]
 if actual=="edge":
  entrypoint,cmd,path,args,workdir,exposed,environment=["/djonehub-edge"],None,"/djonehub-edge",[],"",{"8080/tcp":{}},edge_env
  healthcheck={"Test":["CMD","/djonehub-edge-healthcheck"],"Interval":10000000000,"Timeout":3000000000,"StartPeriod":5000000000,"Retries":3}
 elif actual=="cloudflared":
  cmd=["tunnel","--no-autoupdate","--metrics","127.0.0.1:2000","run","--token-file","/run/secrets/cloudflare-tunnel-token"]
  entrypoint,path,args,workdir,exposed,environment=["cloudflared","--no-autoupdate"],"cloudflared",["--no-autoupdate",*cmd],"/home/nonroot",None,{"PATH":"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin","SSL_CERT_FILE":"/etc/ssl/certs/ca-certificates.crt"}
  healthcheck={"Test":["CMD","cloudflared","tunnel","--metrics","127.0.0.1:2000","ready"],"Interval":10000000000,"Timeout":5000000000,"StartPeriod":10000000000,"Retries":6}
 else:entrypoint,cmd,path,args,workdir,exposed,environment,healthcheck=None,None,"turnserver",[],"",None,{},None
 compose_labels={"com.docker.compose.config-hash":("e" if service=="edge" else "f")*64,"com.docker.compose.container-number":"1","com.docker.compose.depends_on":"edge:service_healthy:false" if service=="cloudflared" else "","com.docker.compose.image":target_id,"com.docker.compose.oneoff":"False","com.docker.compose.project":"dji4g-public-edge","com.docker.compose.project.config_files":compose,"com.docker.compose.project.environment_file":runtime+"/compose.env","com.docker.compose.project.working_dir":str(pathlib.Path(compose).parent),"com.docker.compose.service":service,"com.docker.compose.version":"2.40.3"}
 config={"Hostname":ids[service][:12],"Domainname":"","Image":image,"User":"1000:1001","AttachStdin":False,"AttachStdout":True,"AttachStderr":True,"Tty":False,"OpenStdin":False,"StdinOnce":False,"NetworkDisabled":False,"MacAddress":"","OnBuild":None,"StopTimeout":None,"Shell":None,"StopSignal":"SIGTERM" if actual=="edge" else "","Volumes":None,"Env":[f"{key}={value}" for key,value in environment.items()],"Entrypoint":entrypoint,"Cmd":cmd,"WorkingDir":workdir,"ExposedPorts":exposed,"Healthcheck":healthcheck,"Labels":compose_labels}
 return {"Id":ids[service],"Name":"/"+name,"Image":target_id,"Path":path,"Args":args,"Platform":"linux","AppArmorProfile":"docker-default","ProcessLabel":"","MountLabel":"","Config":config,"HostConfig":host,"NetworkSettings":{"Ports":resolved_ports,"Networks":{network_name:endpoint}},"Mounts":resolved,"State":state,"RestartCount":0}
def network(kind,containers):
 subnet,gateway=("172.31.0.0/16","172.31.0.1") if kind=="control" else ("172.30.247.0/28","172.30.247.1")
 members={}
 for container in containers:
  service=container["Config"]["Labels"]["com.docker.compose.service"]
  address=container["NetworkSettings"]["Networks"]["dji4g-public-edge_public-edge-"+kind]["IPAddress"]
  members[container["Id"]]={"Name":container["Name"][1:],"EndpointID":endpoint_ids[service],"MacAddress":"02:42:ac:1f:00:02","IPv4Address":address+"/"+subnet.rsplit("/",1)[1],"IPv6Address":""}
 addresses=2 ** (32-int(subnet.rsplit("/",1)[1]));in_use=len(members)+3
 return {"Name":"dji4g-public-edge_public-edge-"+kind,"Id":netids[kind],"Created":"2026-01-02T03:04:00Z","Scope":"local","Driver":"bridge","EnableIPv4":True,"EnableIPv6":False,"IPAM":{"Driver":"default","Options":None,"Config":[{"Subnet":subnet,"Gateway":gateway}]},"Internal":False,"Attachable":False,"Ingress":False,"ConfigFrom":{"Network":""},"ConfigOnly":False,"Containers":members,"Options":{},"Labels":{"com.docker.compose.network":"public-edge-"+kind,"com.docker.compose.project":"dji4g-public-edge","com.docker.compose.version":"2.40.3"},"Status":{"IPAM":{"Subnets":{subnet:{"IPsInUse":in_use,"DynamicIPsAvailable":addresses-in_use}}}}}
def document(containers,control=None,turnnet=None):
 networks={"control":control,"turn":turnnet};return {"containers":containers,"networks":networks,"project_network_names":sorted(value["Name"] for value in networks.values() if value is not None)}
def write(name,value):(root/name).write_text(json.dumps(value,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
valid_containers=[item("edge",edge,"healthy"),item("cloudflared",cf,"healthy")];valid=document(valid_containers,network("control",valid_containers))
empty=document([]);write("state-valid.json",valid);write("state-empty.json",empty)
orphan=copy.deepcopy(valid);orphan["containers"].append(item("attacker",cf));write("state-orphan.json",orphan)
for name,change in (("restarted",lambda v:v["containers"][0].__setitem__("RestartCount",1)),("restart-missing",lambda v:v["containers"][0].pop("RestartCount")),("restart-string",lambda v:v["containers"][0].__setitem__("RestartCount","0")),("restart-bool",lambda v:v["containers"][0].__setitem__("RestartCount",False)),("rw-mount",lambda v:v["containers"][0]["Mounts"][0].__setitem__("RW",True)),("host-pid",lambda v:v["containers"][0]["HostConfig"].__setitem__("PidMode","host")),("root-group",lambda v:v["containers"][0]["HostConfig"].__setitem__("GroupAdd",["0"])),("host-sysctl",lambda v:v["containers"][0]["HostConfig"].__setitem__("Sysctls",{"kernel.hostname":"escaped"})),("cpu-realtime",lambda v:v["containers"][0]["HostConfig"].__setitem__("CpuRealtimeRuntime",950000)),("unmasked-proc",lambda v:v["containers"][0]["HostConfig"].__setitem__("MaskedPaths",[])),("unconfined-apparmor",lambda v:v["containers"][0].__setitem__("AppArmorProfile","")),("interactive",lambda v:v["containers"][0]["Config"].__setitem__("OpenStdin",True)),("compose-label",lambda v:v["containers"][0]["Config"]["Labels"].__setitem__("com.docker.compose.version","attacker")),("compose-path",lambda v:v["containers"][0]["Config"]["Labels"].__setitem__("com.docker.compose.project.config_files",runtime+"/compose.yaml")),("endpoint-mac",lambda v:v["containers"][0]["NetworkSettings"]["Networks"]["dji4g-public-edge_public-edge-control"].__setitem__("MacAddress","02:42:ac:1f:00:09")),("wrong-argv",lambda v:v["containers"][1]["Args"].append("--post-quantum")),("cloud-unhealthy",lambda v:v["containers"][1]["State"]["Health"].__setitem__("Status","unhealthy")),("wrong-id",lambda v:v["containers"][0].__setitem__("Image","sha256:"+"9"*64))):
 value=copy.deepcopy(valid);change(value);write("state-"+name+".json",value)
binds_fallback=copy.deepcopy(valid);binds_fallback["containers"][0]["HostConfig"]["Binds"]=[runtime+"/config/gateway-public-key.pem:/run/config/gateway-public-key.pem:ro"];write("state-binds-fallback.json",binds_fallback)
published=copy.deepcopy(valid);published["containers"][0]["HostConfig"]["PortBindings"]={"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"18080"}]};published["containers"][0]["NetworkSettings"]["Ports"]={"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"18080"}]};write("state-published-port.json",published)
extra=copy.deepcopy(valid);extra["containers"][0]["NetworkSettings"]["Networks"]["hostile-cross-project"]={"Gateway":"10.0.0.1","IPAddress":"10.0.0.2","IPPrefixLen":24};write("state-extra-network.json",extra)
mutating=copy.deepcopy(valid);mutating["containers"][0]["Config"]["Env"]=[value if not value.startswith("DJI4G_EDGE_MUTATIONS_ENABLED=") else "DJI4G_EDGE_MUTATIONS_ENABLED=true" for value in mutating["containers"][0]["Config"]["Env"]];write("state-mutating-env.json",mutating)
external=copy.deepcopy(valid);external["containers"][0]["HostConfig"]["LogConfig"]={"Type":"syslog","Config":{"syslog-address":"tcp://attacker.invalid:514"}};write("state-external-log.json",external)
foreign=copy.deepcopy(valid);foreign["networks"]["control"]["Containers"][ids["attacker"]]={"Name":"foreign","EndpointID":endpoint_ids["attacker"],"MacAddress":"02:42:ac:1f:00:09","IPv4Address":"172.31.0.9/16","IPv6Address":""};write("state-foreign-member.json",foreign)
bad_status=copy.deepcopy(valid);bad_status["networks"]["control"]["Status"]["IPAM"]["Subnets"]["172.31.0.0/16"]["DynamicIPsAvailable"]-=1;write("state-bad-network-status.json",bad_status)
alias=copy.deepcopy(valid);alias["containers"][1]["NetworkSettings"]["Networks"]["dji4g-public-edge_public-edge-control"]["DNSNames"].append("edge");write("state-alias-hijack.json",alias)
sysctls_omitted=copy.deepcopy(valid)
for container in sysctls_omitted["containers"]:container["HostConfig"].pop("Sysctls")
write("state-sysctls-omitted.json",sysctls_omitted)
thermal_masked=copy.deepcopy(valid);thermal_masked["containers"][0]["HostConfig"]["MaskedPaths"].extend(f"/sys/devices/system/cpu/cpu{cpu}/thermal_throttle" for cpu in range(11));write("state-thermal-masked.json",thermal_masked)
thermal_unordered=copy.deepcopy(valid);thermal_unordered["containers"][0]["HostConfig"]["MaskedPaths"].extend(["/sys/devices/system/cpu/cpu1/thermal_throttle","/sys/devices/system/cpu/cpu0/thermal_throttle"]);write("state-thermal-unordered.json",thermal_unordered)
turn_container=item("coturn",turn);turn_network=network("turn",[turn_container]);stopped=document([turn_container],None,turn_network);write("state-stopped.json",stopped)
turn_root_group=copy.deepcopy(stopped);turn_root_group["containers"][0]["HostConfig"]["GroupAdd"]=["0"];write("state-turn-root-group.json",turn_root_group)
turn_external_log=copy.deepcopy(stopped);turn_external_log["containers"][0]["HostConfig"]["LogConfig"]={"Type":"syslog","Config":{"syslog-address":"tcp://attacker.invalid:514"}};write("state-turn-external-log.json",turn_external_log)
with_turn=document(valid_containers+[turn_container],network("control",valid_containers),turn_network);write("state-valid-with-turn.json",with_turn)
turn_status_drift=copy.deepcopy(with_turn);turn_status_drift["networks"]["turn"]["Status"]["IPAM"]["Subnets"]["172.30.247.0/28"]={"IPsInUse":5,"DynamicIPsAvailable":11};write("state-turn-status-drift.json",turn_status_drift)
turn_restarted=copy.deepcopy(with_turn);turn_restarted["containers"][2]["RestartCount"]=1;write("state-turn-restarted.json",turn_restarted)
turn_new_start=copy.deepcopy(with_turn);turn_new_start["containers"][2]["State"]["StartedAt"]="2026-01-02T03:06:00Z";write("state-turn-new-start.json",turn_new_start)
turn_host_drift=copy.deepcopy(with_turn);turn_host_drift["containers"][2]["HostConfig"]["Memory"]=536870912;write("state-turn-host-drift.json",turn_host_drift)
drift=copy.deepcopy(valid);drift["containers"][0]["HostConfig"]["PidMode"]="host";write("state-drift-pre-stop.json",drift)
PY
EDGE="sha256:$(printf '1%.0s' {1..64})"; CF="cloudflare/cloudflared@sha256:$(printf '2%.0s' {1..64})"; TURN="coturn/coturn@sha256:$(printf '3%.0s' {1..64})"
RUNTIME_STATE_ROOT="$TMP_ROOT/state-runtime"
state_verify() { python3 -B -I "$STATE" --runtime-root "$RUNTIME_STATE_ROOT" --compose-file "$STATE_COMPOSE_FILE" "$@"; }
state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-valid.json" >/dev/null
pass "post-start state accepts exact healthy non-root containers"
state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-sysctls-omitted.json" >/dev/null
pass "Compose-omitted empty Sysctls is accepted while non-empty sysctls remain forbidden"
state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-thermal-masked.json" >/dev/null
pass "Docker 29 safe per-CPU thermal mask extension is accepted"
expect_fail "Compose orphan is rejected" state_verify --phase pre --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 "$TMP_ROOT/state-orphan.json"
expect_fail "startup restart is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-restarted.json"
expect_fail "missing RestartCount evidence is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-restart-missing.json"
expect_fail "string RestartCount evidence is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-restart-string.json"
expect_fail "boolean RestartCount evidence is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-restart-bool.json"
expect_fail "resolved writable bind mount is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-rw-mount.json"
expect_fail "legacy auto-creating HostConfig.Binds fallback is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-binds-fallback.json"
expect_fail "direct public port binding on edge is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-published-port.json"
expect_fail "cross-project extra network attachment is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-extra-network.json"
expect_fail "foreign project-network endpoint is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-foreign-member.json"
expect_fail "network alias hijack is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-alias-hijack.json"
expect_fail "host PID namespace escape is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-host-pid.json"
expect_fail "supplementary root group is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-root-group.json"
expect_fail "container sysctl override is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-host-sysctl.json"
expect_fail "Docker JSON real-time CPU resource override is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-cpu-realtime.json"
expect_fail "Docker 29 IPAM Status accounting drift is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-bad-network-status.json"
expect_fail "Docker 29 masked proc/sys policy removal is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-unmasked-proc.json"
expect_fail "Docker 29 CPU thermal mask order/duplication drift is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-thermal-unordered.json"
expect_fail "unconfined AppArmor runtime is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-unconfined-apparmor.json"
expect_fail "interactive stdin runtime drift is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-interactive.json"
expect_fail "Compose identity/version label drift is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-compose-label.json"
expect_fail "Compose source-file identity cannot be forged as the runtime root" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-compose-path.json"
expect_fail "container/network endpoint MAC disagreement is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-endpoint-mac.json"
expect_fail "edge mutation environment bypass is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-mutating-env.json"
expect_fail "cloudflared effective argv drift is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-wrong-argv.json"
expect_fail "external/unbounded runtime log driver is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-external-log.json"
expect_fail "cloudflared readiness failure is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-cloud-unhealthy.json"
expect_fail "container .Image mismatch is rejected for local edge ID" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-wrong-id.json"
state_verify --phase pre-stop-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 "$TMP_ROOT/state-drift-pre-stop.json" >/dev/null
pass "pre-stop identity gate permits drifted edge confinement so exact remediation remains reachable"
expect_fail "pre-stop remediation still rejects a foreign control-network endpoint" \
  state_verify --phase pre-stop-edge --edge-image "$EDGE" --cloudflared-image "$CF" \
    --coturn-image "$TURN" --uid-gid 1000:1001 "$TMP_ROOT/state-foreign-member.json"
state_verify --phase post-stop-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-stopped.json" >/dev/null
pass "edge-only rollback/stop proves the exact coturn fingerprint is unchanged"
state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-valid-with-turn.json" >/dev/null
pass "start-edge preserves an existing coturn container exactly"
state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-turn-status-drift.json" >/dev/null
pass "Docker 29 dynamic IPAM Status counters do not impersonate network identity"
DISABLED_TURN="invalid.local/dji4g-coturn-disabled@sha256:$(printf '0%.0s' {1..64})"
state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$DISABLED_TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-valid-with-turn.json" >/dev/null
pass "edge-only coturn sentinel does not impersonate an existing immutable coturn baseline"
expect_fail "pre-existing coturn supplementary root group is rejected" state_verify --phase pre-start-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$DISABLED_TURN" --uid-gid 1000:1001 "$TMP_ROOT/state-turn-root-group.json"
expect_fail "pre-existing coturn external log transport is rejected" state_verify --phase pre-start-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$DISABLED_TURN" --uid-gid 1000:1001 "$TMP_ROOT/state-turn-external-log.json"
expect_fail "coturn disappearance during edge action is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-valid.json"
expect_fail "coturn appearance when absent before edge action is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-empty.json" "$TMP_ROOT/state-valid-with-turn.json"
expect_fail "coturn restart count change is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-turn-restarted.json"
expect_fail "coturn StartedAt change is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-turn-new-start.json"
expect_fail "coturn host configuration drift is rejected" state_verify --phase post-edge --edge-image "$EDGE" --cloudflared-image "$CF" --coturn-image "$TURN" --uid-gid 1000:1001 --baseline "$TMP_ROOT/state-stopped.json" "$TMP_ROOT/state-turn-host-drift.json"

BUILD_BUNDLE="$TMP_ROOT/build-bundle"; mkdir "$BUILD_BUNDLE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$BUILD_BUNDLE" "$SOURCE_REPO/source.lock" "$FIXTURE_TOOLCHAIN" <<'PY'
import gzip, hashlib, io, json, pathlib, shutil, sys, tarfile
r=pathlib.Path(sys.argv[1]); source_path=pathlib.Path(sys.argv[2]); tool_path=pathlib.Path(sys.argv[3])
source=dict(line.split("=",1) for line in source_path.read_text(encoding="ascii").splitlines())
tool=dict(line.split("=",1) for line in tool_path.read_text(encoding="ascii").splitlines())
shutil.copyfile(source_path,r/"source.lock");shutil.copyfile(tool_path,r/"toolchain.lock")
source_lock_sha=hashlib.sha256((r/"source.lock").read_bytes()).hexdigest()
tool_lock_sha=hashlib.sha256((r/"toolchain.lock").read_bytes()).hexdigest()
labels={
 "org.opencontainers.image.title":"DJOneHub public edge",
 "org.opencontainers.image.description":"Fail-closed read-only public control edge",
 "org.opencontainers.image.source":"https://github.com/example/maccellular",
 "org.opencontainers.image.revision":source["SOURCE_REVISION"],
 "org.opencontainers.image.created":source["SOURCE_CREATED_RFC3339"],
 "io.maccellular.source-tree-sha256":source["SOURCE_TREE_SHA256"],
 "io.maccellular.source-lock-sha256":source_lock_sha,
 "io.maccellular.toolchain-lock-sha256":tool_lock_sha,
 "io.maccellular.runtime":"scratch-no-shell","io.maccellular.surface":"public-read-only",
}
runtime={"Entrypoint":["/djonehub-edge"],"Cmd":None,"User":"65532:65532","ExposedPorts":{"8080/tcp":{}},"StopSignal":"SIGTERM","Labels":labels,"Healthcheck":{"Test":["CMD","/djonehub-edge-healthcheck"],"Interval":10000000000,"Timeout":3000000000,"StartPeriod":5000000000,"Retries":3}}
layer_buffer=io.BytesIO()
with tarfile.open(fileobj=layer_buffer,mode="w") as layer_archive:
 data=b"synthetic static edge binary\n";info=tarfile.TarInfo("djonehub-edge")
 info.size=len(data);info.mode=0o755;info.uid=0;info.gid=0;info.mtime=int(source["SOURCE_DATE_EPOCH"])
 layer_archive.addfile(info,io.BytesIO(data))
layer_bytes=layer_buffer.getvalue()
layer_blob=gzip.compress(layer_bytes,compresslevel=9,mtime=0)
diff_id="sha256:"+hashlib.sha256(layer_bytes).hexdigest()
config={"architecture":"amd64","os":"linux","created":source["SOURCE_CREATED_RFC3339"],"config":runtime,"rootfs":{"type":"layers","diff_ids":[diff_id]},"history":[]}
config_bytes=json.dumps(config,sort_keys=True,separators=(",",":")).encode("ascii")
config_digest="sha256:"+hashlib.sha256(config_bytes).hexdigest(); config_name="blobs/sha256/"+config_digest.removeprefix("sha256:")
layer_digest="sha256:"+hashlib.sha256(layer_blob).hexdigest();layer_name="blobs/sha256/"+layer_digest.removeprefix("sha256:")
image_manifest={"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":config_digest,"size":len(config_bytes)},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":layer_digest,"size":len(layer_blob)}]}
image_manifest_bytes=json.dumps(image_manifest,sort_keys=True,separators=(",",":")).encode("ascii")
manifest_digest="sha256:"+hashlib.sha256(image_manifest_bytes).hexdigest();manifest_name="blobs/sha256/"+manifest_digest.removeprefix("sha256:")
image=manifest_digest
index={"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":manifest_digest,"size":len(image_manifest_bytes),"platform":{"os":"linux","architecture":"amd64"}}]}
index_bytes=json.dumps(index,sort_keys=True,separators=(",",":")).encode("ascii")
layout_bytes=b'{"imageLayoutVersion":"1.0.0"}'
manifest=json.dumps([{"Config":config_name,"RepoTags":None,"Layers":[layer_name]}],sort_keys=True,separators=(",",":")).encode("ascii")
def add(archive,name,data):
 info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=int(source["SOURCE_DATE_EPOCH"]);archive.addfile(info,__import__("io").BytesIO(data))
with tarfile.open(r/"image.tar","w") as archive:
 for name,data in (("oci-layout",layout_bytes),("index.json",index_bytes),("manifest.json",manifest),(config_name,config_bytes),(layer_name,layer_blob),(manifest_name,image_manifest_bytes)):add(archive,name,data)
(r/"image.tar.sha256").write_text(hashlib.sha256((r/"image.tar").read_bytes()).hexdigest()+"  image.tar\n",encoding="ascii")
(r/"edge-image-reference.txt").write_text(image+"\n",encoding="ascii")
local_ref="registry.invalid/owner/edge:dji4g-build-"+source["SOURCE_TREE_SHA256"][:16]+"-"+"a"*32+"-a"
(r/"local-image-retention-reference.txt").write_text(local_ref+"\n",encoding="ascii")
inspect=[{"Id":image,"Os":"linux","Architecture":"amd64","Created":source["SOURCE_CREATED_RFC3339"],"Config":runtime}]
(r/"image-inspect.json").write_text(json.dumps(inspect,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
names={"edge-image-reference.txt","local-image-retention-reference.txt","image-inspect.json","image.tar","image.tar.sha256","source.lock","toolchain.lock"}
build={"format":2,"image_id":image,"local_retention_reference":local_ref,"target_platform":tool["TARGET_PLATFORM"],"source_revision":source["SOURCE_REVISION"],"source_created_rfc3339":source["SOURCE_CREATED_RFC3339"],"source_tree_sha256":source["SOURCE_TREE_SHA256"],"source_lock_sha256":source_lock_sha,"toolchain_lock_sha256":tool_lock_sha,"reproducible_image_id":True,"artifacts":{name:hashlib.sha256((r/name).read_bytes()).hexdigest() for name in sorted(names)}}
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
PY
python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$BUILD_BUNDLE" \
  --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
  --toolchain-lock "$FIXTURE_TOOLCHAIN" >/dev/null
pass "containerd OCI archive binds gzip descriptor bytes, decompressed DiffID, config, inspect, and image ID"

NAMED_ARCHIVE="$TMP_ROOT/build-named-archive"; cp -R "$BUILD_BUNDLE" "$NAMED_ARCHIVE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$NAMED_ARCHIVE" <<'PY'
import hashlib,io,json,os,pathlib,sys,tarfile
r=pathlib.Path(sys.argv[1]);source=dict(line.split("=",1) for line in (r/"source.lock").read_text().splitlines())
with tarfile.open(r/"image.tar","r") as old:
    members={member.name:old.extractfile(member).read() for member in old.getmembers() if member.isfile()}
compat=json.loads(members["manifest.json"]);compat[0]["RepoTags"]=["victim.invalid/unrelated:keep"]
members["manifest.json"]=json.dumps(compat,sort_keys=True,separators=(",",":")).encode("ascii")
index=json.loads(members["index.json"]);index["manifests"][0]["annotations"]={"org.opencontainers.image.ref.name":"victim.invalid/unrelated:keep"}
members["index.json"]=json.dumps(index,sort_keys=True,separators=(",",":")).encode("ascii")
temporary=r/"image-named.tar"
with tarfile.open(temporary,"w") as archive:
    for name,data in members.items():
        info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=int(source["SOURCE_DATE_EPOCH"]);archive.addfile(info,io.BytesIO(data))
os.replace(temporary,r/"image.tar")
(r/"image.tar.sha256").write_text(hashlib.sha256((r/"image.tar").read_bytes()).hexdigest()+"  image.tar\n")
build=json.loads((r/"build.json").read_text())
for name in ("image.tar","image.tar.sha256"):build["artifacts"][name]=hashlib.sha256((r/name).read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "air-gap archive repository tags and OCI ref-name annotations are rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$NAMED_ARCHIVE" \
    --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
    --toolchain-lock "$FIXTURE_TOOLCHAIN"

NESTED_INDEX="$TMP_ROOT/build-nested-index"; cp -R "$BUILD_BUNDLE" "$NESTED_INDEX"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$NESTED_INDEX" <<'PY'
import hashlib, io, json, os, pathlib, sys, tarfile
r=pathlib.Path(sys.argv[1]);source=dict(line.split("=",1) for line in (r/"source.lock").read_text().splitlines())
with tarfile.open(r/"image.tar","r") as old:
 compatibility=old.extractfile("manifest.json").read();layout=old.extractfile("oci-layout").read()
 top=json.load(old.extractfile("index.json"));manifest_descriptor=top["manifests"][0]
 manifest_path="blobs/sha256/"+manifest_descriptor["digest"].split(":",1)[1]
 manifest=old.extractfile(manifest_path).read()
 compatibility_document=json.loads(compatibility);config_path=compatibility_document[0]["Config"]
 config=old.extractfile(config_path).read();layer_path=compatibility_document[0]["Layers"][0]
 layer=old.extractfile(layer_path).read()
inner={"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[manifest_descriptor]}
inner_bytes=json.dumps(inner,sort_keys=True,separators=(",",":")).encode("ascii")
inner_digest="sha256:"+hashlib.sha256(inner_bytes).hexdigest();inner_path="blobs/sha256/"+inner_digest.split(":",1)[1]
top["manifests"]=[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":inner_digest,"size":len(inner_bytes),"platform":{"os":"linux","architecture":"amd64"}}]
top_bytes=json.dumps(top,sort_keys=True,separators=(",",":")).encode("ascii")
temporary=r/"image-nested-index.tar"
def add(archive,name,data):
 info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=int(source["SOURCE_DATE_EPOCH"]);archive.addfile(info,io.BytesIO(data))
with tarfile.open(temporary,"w") as archive:
 for name,data in (("oci-layout",layout),("index.json",top_bytes),("manifest.json",compatibility),(config_path,config),(layer_path,layer),(manifest_path,manifest),(inner_path,inner_bytes)):add(archive,name,data)
os.replace(temporary,r/"image.tar")
(r/"image.tar.sha256").write_text(hashlib.sha256((r/"image.tar").read_bytes()).hexdigest()+"  image.tar\n")
(r/"edge-image-reference.txt").write_text(inner_digest+"\n")
inspect=json.loads((r/"image-inspect.json").read_text());inspect[0]["Id"]=inner_digest
(r/"image-inspect.json").write_text(json.dumps(inspect,sort_keys=True,separators=(",",":"))+"\n")
build=json.loads((r/"build.json").read_text());build["image_id"]=inner_digest
for name in ("edge-image-reference.txt","image-inspect.json","image.tar","image.tar.sha256"):build["artifacts"][name]=hashlib.sha256((r/name).read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$NESTED_INDEX" \
  --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
  --toolchain-lock "$FIXTURE_TOOLCHAIN" >/dev/null
pass "containerd target ID may be a bounded nested OCI index digest bound through the manifest chain"

WRONG_LAYER="$TMP_ROOT/build-wrong-layer"; cp -R "$BUILD_BUNDLE" "$WRONG_LAYER"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$WRONG_LAYER" <<'PY'
import gzip, hashlib, io, json, os, pathlib, sys, tarfile
r=pathlib.Path(sys.argv[1]); source=dict(line.split("=",1) for line in (r/"source.lock").read_text().splitlines())
with tarfile.open(r/"image.tar","r") as old:
 compatibility=json.load(old.extractfile("manifest.json"));config_name=compatibility[0]["Config"]
 config_bytes=old.extractfile(config_name).read();layout=old.extractfile("oci-layout").read()
 index=json.load(old.extractfile("index.json"));manifest_path="blobs/sha256/"+index["manifests"][0]["digest"].split(":",1)[1]
 image_manifest=json.load(old.extractfile(manifest_path))
layer_buffer=io.BytesIO()
with tarfile.open(fileobj=layer_buffer,mode="w") as layer_archive:
 data=b"different uncompressed layer bytes\n";info=tarfile.TarInfo("djonehub-edge")
 info.size=len(data);info.mode=0o755;info.uid=0;info.gid=0;info.mtime=int(source["SOURCE_DATE_EPOCH"])
 layer_archive.addfile(info,io.BytesIO(data))
replacement=gzip.compress(layer_buffer.getvalue(),compresslevel=9,mtime=0)
layer_digest="sha256:"+hashlib.sha256(replacement).hexdigest();layer_name="blobs/sha256/"+layer_digest.split(":",1)[1]
image_manifest["layers"][0]["digest"]=layer_digest;image_manifest["layers"][0]["size"]=len(replacement)
image_manifest_bytes=json.dumps(image_manifest,sort_keys=True,separators=(",",":")).encode("ascii")
manifest_digest="sha256:"+hashlib.sha256(image_manifest_bytes).hexdigest();new_manifest_path="blobs/sha256/"+manifest_digest.split(":",1)[1]
index["manifests"][0]["digest"]=manifest_digest;index["manifests"][0]["size"]=len(image_manifest_bytes)
index_bytes=json.dumps(index,sort_keys=True,separators=(",",":")).encode("ascii")
compatibility[0]["Layers"]=[layer_name];compatibility_bytes=json.dumps(compatibility,sort_keys=True,separators=(",",":")).encode("ascii")
temporary=r/"image-layer-b.tar"
def add(archive,name,data):
 info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=int(source["SOURCE_DATE_EPOCH"]);archive.addfile(info,io.BytesIO(data))
with tarfile.open(temporary,"w") as archive:
 for name,data in (("oci-layout",layout),("index.json",index_bytes),("manifest.json",compatibility_bytes),(config_name,config_bytes),(layer_name,replacement),(new_manifest_path,image_manifest_bytes)):add(archive,name,data)
os.replace(temporary,r/"image.tar")
(r/"image.tar.sha256").write_text(hashlib.sha256((r/"image.tar").read_bytes()).hexdigest()+"  image.tar\n")
build=json.loads((r/"build.json").read_text())
new_image=manifest_digest
(r/"edge-image-reference.txt").write_text(new_image+"\n")
inspect=json.loads((r/"image-inspect.json").read_text());inspect[0]["Id"]=new_image
(r/"image-inspect.json").write_text(json.dumps(inspect,sort_keys=True,separators=(",",":"))+"\n")
build["image_id"]=new_image
for name in ("edge-image-reference.txt","image-inspect.json","image.tar","image.tar.sha256"):build["artifacts"][name]=hashlib.sha256((r/name).read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "coherently rehashed compressed layer is still rejected by the locked uncompressed DiffID" \
  python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$WRONG_LAYER" \
    --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
    --toolchain-lock "$FIXTURE_TOOLCHAIN"

WRONG_ARCHIVE="$TMP_ROOT/build-wrong-archive"; cp -R "$BUILD_BUNDLE" "$WRONG_ARCHIVE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$WRONG_ARCHIVE" <<'PY'
import hashlib, io, json, os, pathlib, sys, tarfile
r=pathlib.Path(sys.argv[1]); source=dict(line.split("=",1) for line in (r/"source.lock").read_text().splitlines())
with tarfile.open(r/"image.tar","r") as old:
 compatibility=json.load(old.extractfile("manifest.json"));config=json.load(old.extractfile(compatibility[0]["Config"]));layout=old.extractfile("oci-layout").read()
 index=json.load(old.extractfile("index.json"));manifest_path="blobs/sha256/"+index["manifests"][0]["digest"].split(":",1)[1]
 image_manifest=json.load(old.extractfile(manifest_path));layer_path=compatibility[0]["Layers"][0];layer=old.extractfile(layer_path).read()
config["config"]["Env"]=["ARCHIVE_B=1"]
config_bytes=json.dumps(config,sort_keys=True,separators=(",",":")).encode("ascii")
config_digest="sha256:"+hashlib.sha256(config_bytes).hexdigest();config_name="blobs/sha256/"+config_digest.split(":",1)[1]
image_manifest["config"]["digest"]=config_digest;image_manifest["config"]["size"]=len(config_bytes)
image_manifest_bytes=json.dumps(image_manifest,sort_keys=True,separators=(",",":")).encode("ascii")
manifest_digest="sha256:"+hashlib.sha256(image_manifest_bytes).hexdigest();new_manifest_path="blobs/sha256/"+manifest_digest.split(":",1)[1]
index["manifests"][0]["digest"]=manifest_digest;index["manifests"][0]["size"]=len(image_manifest_bytes)
index_bytes=json.dumps(index,sort_keys=True,separators=(",",":")).encode("ascii")
compatibility[0]["Config"]=config_name;compatibility_bytes=json.dumps(compatibility,sort_keys=True,separators=(",",":")).encode("ascii")
temporary=r/"image-b.tar"
with tarfile.open(temporary,"w") as archive:
 for name,data in (("oci-layout",layout),("index.json",index_bytes),("manifest.json",compatibility_bytes),(config_name,config_bytes),(layer_path,layer),(new_manifest_path,image_manifest_bytes)):
  info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=int(source["SOURCE_DATE_EPOCH"]);archive.addfile(info,io.BytesIO(data))
os.replace(temporary,r/"image.tar")
(r/"image.tar.sha256").write_text(hashlib.sha256((r/"image.tar").read_bytes()).hexdigest()+"  image.tar\n")
build=json.loads((r/"build.json").read_text())
for name in ("image.tar","image.tar.sha256"):build["artifacts"][name]=hashlib.sha256((r/name).read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "inspect A plus rehashed archive B is rejected by the exact OCI target/config chain" \
  python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$WRONG_ARCHIVE" \
    --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
    --toolchain-lock "$FIXTURE_TOOLCHAIN"

ZSTD_ARCHIVE="$TMP_ROOT/build-zstd-archive"; cp -R "$BUILD_BUNDLE" "$ZSTD_ARCHIVE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$ZSTD_ARCHIVE" <<'PY'
import hashlib, io, json, os, pathlib, sys, tarfile
r=pathlib.Path(sys.argv[1]);source=dict(line.split("=",1) for line in (r/"source.lock").read_text().splitlines())
with tarfile.open(r/"image.tar","r") as old:
 compatibility=json.load(old.extractfile("manifest.json"));config_path=compatibility[0]["Config"];layer_path=compatibility[0]["Layers"][0]
 config=old.extractfile(config_path).read();layer=old.extractfile(layer_path).read();layout=old.extractfile("oci-layout").read()
 index=json.load(old.extractfile("index.json"));manifest_path="blobs/sha256/"+index["manifests"][0]["digest"].split(":",1)[1]
 image_manifest=json.load(old.extractfile(manifest_path))
image_manifest["layers"][0]["mediaType"]="application/vnd.oci.image.layer.v1.tar+zstd"
manifest_bytes=json.dumps(image_manifest,sort_keys=True,separators=(",",":")).encode("ascii")
digest="sha256:"+hashlib.sha256(manifest_bytes).hexdigest();new_manifest_path="blobs/sha256/"+digest.split(":",1)[1]
index["manifests"][0]["digest"]=digest;index["manifests"][0]["size"]=len(manifest_bytes)
index_bytes=json.dumps(index,sort_keys=True,separators=(",",":")).encode("ascii");compatibility_bytes=json.dumps(compatibility,sort_keys=True,separators=(",",":")).encode("ascii")
temporary=r/"image-zstd.tar"
with tarfile.open(temporary,"w") as archive:
 for name,data in (("oci-layout",layout),("index.json",index_bytes),("manifest.json",compatibility_bytes),(config_path,config),(layer_path,layer),(new_manifest_path,manifest_bytes)):
  info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=int(source["SOURCE_DATE_EPOCH"]);archive.addfile(info,io.BytesIO(data))
os.replace(temporary,r/"image.tar")
(r/"image.tar.sha256").write_text(hashlib.sha256((r/"image.tar").read_bytes()).hexdigest()+"  image.tar\n")
build=json.loads((r/"build.json").read_text())
new_image=digest
(r/"edge-image-reference.txt").write_text(new_image+"\n")
inspect=json.loads((r/"image-inspect.json").read_text());inspect[0]["Id"]=new_image
(r/"image-inspect.json").write_text(json.dumps(inspect,sort_keys=True,separators=(",",":"))+"\n")
build["image_id"]=new_image
for name in ("edge-image-reference.txt","image-inspect.json","image.tar","image.tar.sha256"):build["artifacts"][name]=hashlib.sha256((r/name).read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "zstd layer mediaType is rejected without an explicitly locked decoder" \
  python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$ZSTD_ARCHIVE" \
    --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
    --toolchain-lock "$FIXTURE_TOOLCHAIN"

for variant in extra-secret unreferenced-blob; do
  EXTRA_ARCHIVE="$TMP_ROOT/build-$variant";cp -R "$BUILD_BUNDLE" "$EXTRA_ARCHIVE"
  PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$EXTRA_ARCHIVE" "$variant" <<'PY'
import hashlib,io,json,pathlib,sys,tarfile
r=pathlib.Path(sys.argv[1]);variant=sys.argv[2];data=b"unapproved archive payload\n"
name="secret.txt" if variant=="extra-secret" else "blobs/sha256/"+hashlib.sha256(data).hexdigest()
with tarfile.open(r/"image.tar","a") as archive:
    info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;archive.addfile(info,io.BytesIO(data))
(r/"image.tar.sha256").write_text(hashlib.sha256((r/"image.tar").read_bytes()).hexdigest()+"  image.tar\n")
build=json.loads((r/"build.json").read_text())
for artifact in ("image.tar","image.tar.sha256"):
    build["artifacts"][artifact]=hashlib.sha256((r/artifact).read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
  expect_fail "self-rehashed OCI archive $variant carrier is rejected" \
    python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$EXTRA_ARCHIVE" \
      --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
      --toolchain-lock "$FIXTURE_TOOLCHAIN"
done

WRONG_INSPECT="$TMP_ROOT/build-wrong-inspect"; cp -R "$BUILD_BUNDLE" "$WRONG_INSPECT"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$WRONG_INSPECT" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);inspect=json.loads((r/"image-inspect.json").read_text())
inspect[0]["Config"]["Labels"]["io.maccellular.toolchain-lock-sha256"]="f"*64
(r/"image-inspect.json").write_text(json.dumps(inspect,sort_keys=True,separators=(",",":"))+"\n")
build=json.loads((r/"build.json").read_text());build["artifacts"]["image-inspect.json"]=hashlib.sha256((r/"image-inspect.json").read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "rehashed inspect with a forged toolchain label is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$WRONG_INSPECT" \
    --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
    --toolchain-lock "$FIXTURE_TOOLCHAIN"

IMAGE_VOLUME="$TMP_ROOT/build-image-volume"; cp -R "$BUILD_BUNDLE" "$IMAGE_VOLUME"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$IMAGE_VOLUME" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);inspect=json.loads((r/"image-inspect.json").read_text())
inspect[0]["Config"]["Volumes"]={"/unexpected-writable":{}}
(r/"image-inspect.json").write_text(json.dumps(inspect,sort_keys=True,separators=(",",":"))+"\n")
build=json.loads((r/"build.json").read_text());build["artifacts"]["image-inspect.json"]=hashlib.sha256((r/"image-inspect.json").read_bytes()).hexdigest()
(r/"build.json").write_text(json.dumps(build,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "image-declared anonymous writable volume is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$IMAGE_VOLUME" \
    --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
    --toolchain-lock "$FIXTURE_TOOLCHAIN"

WRONG_BUILD="$TMP_ROOT/build-wrong-manifest"; cp -R "$BUILD_BUNDLE" "$WRONG_BUILD"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$WRONG_BUILD/build.json" <<'PY'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]);value=json.loads(p.read_text());value["source_revision"]="f"*40
p.write_text(json.dumps(value,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "hand-edited build.json source revision is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$WRONG_BUILD" \
    --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
    --toolchain-lock "$FIXTURE_TOOLCHAIN"

BUILD_FAKE_BIN="$TMP_ROOT/build-fake-bin"; mkdir "$BUILD_FAKE_BIN"
cp "$BUILD_BUNDLE/image.tar" "$BUILD_FAKE_BIN/image.tar"
cp "$BUILD_BUNDLE/image-inspect.json" "$BUILD_FAKE_BIN/image-inspect.json"
cp "$BUILD_BUNDLE/edge-image-reference.txt" "$BUILD_FAKE_BIN/image-id"
cat >"$BUILD_FAKE_BIN/docker" <<'SH'
#!/usr/bin/env bash
set -eu
fake_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
if [ "${1:-}" != --config ] || [ ! -d "${2:-}" ] || [ "${3:-}" != --context ] || [ "${4:-}" != default ]; then exit 77; fi
config_dir=$2;printf '%s\n' "$config_dir" >>"$fake_dir/config-paths";shift 4
[ "$(cat "$config_dir/config.json" 2>/dev/null)" = "{}" ] || exit 78
entry_count=$(find "$config_dir" -mindepth 1 -print | wc -l | tr -d " ")
if [ "$entry_count" -ne 1 ]; then
 [ -d "$config_dir/cli-plugins" ] && [ -f "$config_dir/cli-plugins/docker-buildx" ] || exit 78
 [ "$entry_count" = 3 ] || exit 78
 [ "$(shasum -a 256 "$config_dir/cli-plugins/docker-buildx" | awk '{print $1}')" = "$(tr -d "\\n" <"$fake_dir/buildx-sha")" ] || exit 78
fi
for name in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH BUILDKIT_HOST BUILDX_BUILDER BUILDX_CONFIG HOME; do
 [ -z "${!name+x}" ] || exit 79
done
printf '%s\n' "$*" >>"$fake_dir/calls"
if [ "${1:-}" = info ]; then
 case " $* " in
  *"{{.OSType}}/{{.Architecture}}"*) printf '%s\n' linux/amd64 ;;
  *"{{json .DriverStatus}}"*) printf '%s\n' '[["driver-type","io.containerd.snapshotter.v1"]]' ;;
  *"{{json .SecurityOptions}}"*)
   if [ "$(tr -d '\n' <"$fake_dir/mode")" = security-mismatch ];then printf '%s\n' '[]'
   else printf '%s\n' '["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]';fi ;;
 esac
 exit 0
fi
if [ "${1:-}" = version ]; then printf '%s\n' 29.1.3; exit 0; fi
if [ "${1:-}" = buildx ] && [ "${2:-}" = version ]; then printf '%s\n' 'github.com/docker/buildx v0.30.1 synthetic';exit 0;fi
if [ "${1:-}" = buildx ] && [ "${2:-}" = inspect ]; then printf '%s\n' 'Name: default' 'Driver: docker' 'Status: running';exit 0;fi
if [ "${1:-}" = buildx ] && [ "${2:-}" = build ]; then
 count=0;[ ! -f "$fake_dir/build-count" ] || count=$(tr -d '\n' <"$fake_dir/build-count");count=$((count+1));printf '%s\n' "$count" >"$fake_dir/build-count"
 metadata_file="";tag="";previous=""
 for value in "$@"; do
  [ "$previous" != --metadata-file ] || metadata_file=$value
  [ "$previous" != -t ] || tag=$value
  previous=$value
 done
 [ -n "$metadata_file" ] && [ -n "$tag" ] || exit 78
 target=$(tr -d '\n' <"$fake_dir/image-id")
 case "$tag" in *-a|*-b) ;; *) exit 78 ;; esac
 { [ ! -f "$fake_dir/tag-refs" ] || cat "$fake_dir/tag-refs";printf '%s\n' "$tag"; } |
   awk 'NF && !seen[$0]++' >"$fake_dir/tag-refs.new"
 mv "$fake_dir/tag-refs.new" "$fake_dir/tag-refs"
 mode=$(tr -d '\n' <"$fake_dir/mode")
 if [ "$mode" = signal-no-metadata ] && [ "$count" -eq 1 ]; then : >"$fake_dir/build-started";sleep 1;exit 0;fi
 [ "$(tr -d '\n' <"$fake_dir/mode")" != metadata-mismatch ] || target="sha256:$(printf '9%.0s' {1..64})"
 printf '{"containerimage.config.digest":"sha256:%s","containerimage.digest":"%s"}\n' "$(printf '8%.0s' {1..64})" "$target" >"$metadata_file"
 exit 0
fi
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
 ref=${!#}
 case "$ref" in
  *:dji4g-build-*-a|*:dji4g-build-*-b)
   mode=$(tr -d '\n' <"$fake_dir/mode");count=0
   [ ! -f "$fake_dir/build-count" ] || count=$(tr -d '\n' <"$fake_dir/build-count")
   exists=0
   if [ "$mode" = tag-collision ]; then exists=1
   elif [ -f "$fake_dir/tag-refs" ] && grep -Fqx "$ref" "$fake_dir/tag-refs"; then exists=1
   fi
   [ "$exists" -eq 1 ] || exit 1
   case " $* " in *"{{.Id}}"*) cat "$fake_dir/image-id" ;; *) cat "$fake_dir/image-inspect.json" ;; esac
   exit 0 ;;
 esac
 case " $* " in
  *"{{.Os}}/{{.Architecture}} {{.Config.Env}}"*) printf '%s\n' 'linux/amd64 [GOLANG_VERSION=1.26.3]' ;;
  *"{{.Os}}/{{.Architecture}}"*) printf '%s\n' 'linux/amd64' ;;
  *"{{range .RepoDigests}}"*) printf '%s\n' "$ref" ;;
 *"{{.Id}}"*)
   if [ "$ref" = "$(tr -d '\n' <"$fake_dir/image-id")" ];then
    [ -s "$fake_dir/tag-refs" ] || exit 1
    cat "$fake_dir/image-id";exit 0
   fi
   case " ${!#} " in
    *golang@sha256:*) printf '%s\n' 'sha256:3bf5b04541eb4a37fe62aa1bc9c98a1dec09db9d2e79c1d2eb54e3c9d08dbca9' ;;
    *dockerfile:1.20.0@sha256:*) printf '%s\n' 'sha256:26147acbda4f14c5add9946e2fd2ed543fc402884fd75146bd342a7f6271dc1d' ;;
    *) cat "$fake_dir/image-id" ;;
   esac ;;
  *)
   case " ${!#} " in *dockerfile:1.20.0@sha256:*) : ;; *) cat "$fake_dir/image-inspect.json" ;; esac ;;
 esac
 exit 0
fi
if [ "${1:-}" = image ] && [ "${2:-}" = save ]; then
 [ "${3:-}" = "$(tr -d '\n' <"$fake_dir/image-id")" ] || exit 78
 [ -s "$fake_dir/tag-refs" ] || exit 1
 cat "$fake_dir/image.tar";exit 0
fi
if [ "${1:-}" = image ] && [ "${2:-}" = rm ]; then
 ref=${3:-};printf '%s\n' "$ref" >>"$fake_dir/tag-removals"
 if [ -f "$fake_dir/tag-refs" ];then
  grep -Fvx "$ref" "$fake_dir/tag-refs" >"$fake_dir/tag-refs.new" || true
  mv "$fake_dir/tag-refs.new" "$fake_dir/tag-refs"
 fi
 exit 0
fi
exit 64
SH
chmod 700 "$BUILD_FAKE_BIN/docker"
printf '%s\n' "$FIXTURE_BUILDX_SHA" >"$BUILD_FAKE_BIN/buildx-sha"
BUILD_APPLY_PARENT="$TMP_ROOT/build-apply-parent";mkdir "$BUILD_APPLY_PARENT";chmod 700 "$BUILD_APPLY_PARENT"
printf '%s\n' success >"$BUILD_FAKE_BIN/mode"
POISON_HOME="$TMP_ROOT/build-poison-home";mkdir -p "$POISON_HOME/.docker"
printf '%s\n' '{"currentContext":"attacker","proxies":{"default":{"httpProxy":"http://secret@attacker.invalid"}}}' >"$POISON_HOME/.docker/config.json"
BAD_BUILDX="$TMP_ROOT/docker-buildx-wrong";printf '%s\n' '#!/usr/bin/env bash' 'exit 1' >"$BAD_BUILDX";chmod 500 "$BAD_BUILDX"
expect_fail "unlocked Buildx binary is rejected before any daemon mutation" \
  env PATH="$BUILD_FAKE_BIN:$PATH" "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
    --output "$BUILD_APPLY_PARENT/wrong-buildx" --repository local.invalid/maccellular/edge \
    --buildx-bin "$BAD_BUILDX" --apply
printf '%s\n' security-mismatch >"$BUILD_FAKE_BIN/mode"
rm -f "$BUILD_FAKE_BIN/build-count"
expect_fail "build rejects a daemon without the exact AppArmor/seccomp/cgroupns contract before mutation" \
  env PATH="$BUILD_FAKE_BIN:$PATH" "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
    --output "$BUILD_APPLY_PARENT/security-mismatch" --repository local.invalid/maccellular/edge \
    --buildx-bin "$FIXTURE_BUILDX" --apply
[ ! -f "$BUILD_FAKE_BIN/build-count" ] || fail "security-option mismatch reached Buildx mutation"
printf '%s\n' tag-collision >"$BUILD_FAKE_BIN/mode"
expect_fail "pre-existing unpredictable build tag collision is rejected before build mutation" \
  env PATH="$BUILD_FAKE_BIN:$PATH" "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
    --output "$BUILD_APPLY_PARENT/tag-collision" --repository local.invalid/maccellular/edge \
    --buildx-bin "$FIXTURE_BUILDX" --apply
[ ! -f "$BUILD_FAKE_BIN/build-count" ] || fail "tag collision reached a build mutation"
[ ! -s "$BUILD_FAKE_BIN/tag-removals" ] || fail "tag collision cleanup deleted a pre-existing tag"
printf '%s\n' metadata-mismatch >"$BUILD_FAKE_BIN/mode"
expect_fail "Buildx target metadata mismatch is rejected even when config and target digests differ" \
  env PATH="$BUILD_FAKE_BIN:$PATH" "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
    --output "$BUILD_APPLY_PARENT/metadata-mismatch" --repository local.invalid/maccellular/edge \
    --buildx-bin "$FIXTURE_BUILDX" --apply
[ ! -e "$BUILD_APPLY_PARENT/metadata-mismatch" ] || fail "metadata mismatch published a build bundle"
rm -f "$BUILD_FAKE_BIN/build-count" "$BUILD_FAKE_BIN/tag-removals"
printf '%s\n' success >"$BUILD_FAKE_BIN/mode"
env HOME="$POISON_HOME" DOCKER_HOST=tcp://attacker.invalid:2376 DOCKER_CONTEXT=attacker \
  DOCKER_CONFIG=/attacker/docker-config BUILDKIT_HOST=tcp://attacker.invalid:1234 \
  PATH="$BUILD_FAKE_BIN:$PATH" "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
    --output "$BUILD_APPLY_PARENT/success" --repository local.invalid/maccellular/edge \
    --buildx-bin "$FIXTURE_BUILDX" --apply >/dev/null
pass "build uses the hash-locked Buildx and only the local default containerd daemon/config"
[ "$(wc -l <"$BUILD_FAKE_BIN/tag-removals" | tr -d ' ')" = 1 ] &&
  grep -Eq -- '-b$' "$BUILD_FAKE_BIN/tag-removals" &&
  ! grep -Eq -- '-a$' "$BUILD_FAKE_BIN/tag-removals" || fail "build did not retain exactly its owned local A reference"
BUILD_RETAINED_REF=$(tr -d '\n' <"$BUILD_APPLY_PARENT/success/local-image-retention-reference.txt")
[[ "$BUILD_RETAINED_REF" == *-a ]] || fail "build output omitted the retained local image reference"
[ "$(awk 'NF {count++} END {print count+0}' "$BUILD_FAKE_BIN/tag-refs")" = 1 ] &&
  grep -Fqx "$BUILD_RETAINED_REF" "$BUILD_FAKE_BIN/tag-refs" ||
  fail "fake containerd store does not retain exactly the build-owned A reference"
python3 -B -I "$SCRIPT_DIR/verify-build-bundle.py" --bundle "$BUILD_APPLY_PARENT/success" \
  --repo-root "$SOURCE_REPO" --source-lock "$SOURCE_REPO/source.lock" \
  --toolchain-lock "$FIXTURE_TOOLCHAIN" >/dev/null || fail "retained-reference build bundle did not verify"
pass "Docker29 build keeps one random owned reference so its exact target survives for audit"
while IFS= read -r config_dir; do [ ! -e "$config_dir" ] || fail "build left its controlled Docker config behind"; done <"$BUILD_FAKE_BIN/config-paths"

rm -f "$BUILD_FAKE_BIN/build-count" "$BUILD_FAKE_BIN/build-started" "$BUILD_FAKE_BIN/config-paths"
printf '%s\n' signal-no-metadata >"$BUILD_FAKE_BIN/mode"
set +e
PATH="$BUILD_FAKE_BIN:$PATH" "$SOURCE_REPO/deploy/public-edge/image/build-image.sh" \
  --output "$BUILD_APPLY_PARENT/signal" --repository local.invalid/maccellular/edge \
  --buildx-bin "$FIXTURE_BUILDX" --apply \
  >"$TMP_ROOT/build-signal.log" 2>&1 &
BUILD_SIGNAL_PID=$!
set -e
for _ in $(seq 1 100); do [ -f "$BUILD_FAKE_BIN/build-started" ] && break;sleep 0.02;done
[ -f "$BUILD_FAKE_BIN/build-started" ] || fail "build signal fixture never reached its first mutation"
kill -TERM "$BUILD_SIGNAL_PID"
set +e;wait "$BUILD_SIGNAL_PID";BUILD_SIGNAL_STATUS=$?;set -e
[ "$BUILD_SIGNAL_STATUS" -eq 143 ] || fail "build did not preserve SIGTERM status 143"
[ "$(tr -d '\n' <"$BUILD_FAKE_BIN/build-count")" = 1 ] || fail "build continued to a second mutation after SIGTERM"
[ ! -e "$BUILD_APPLY_PARENT/signal" ] || fail "cancelled build published an output bundle"
tail -n 1 "$BUILD_FAKE_BIN/tag-removals" | grep -Eq -- '-a$' || fail "cancelled build did not recover/remove its label-bound first tag without metadata ACK"
while IFS= read -r config_dir; do [ ! -e "$config_dir" ] || fail "cancelled build left its controlled Docker config behind"; done <"$BUILD_FAKE_BIN/config-paths"
pass "SIGTERM in the Buildx metadata ACK gap recovers the random label-bound tag and stops the next mutation"

AUDIT_FIXTURE_DIR="$SOURCE_REPO/deploy/public-edge/image"
cp "$SCRIPT_DIR/audit-image.sh" "$AUDIT_FIXTURE_DIR/audit-image.sh"
chmod 700 "$AUDIT_FIXTURE_DIR/audit-image.sh"

AUDIT_RUNTIME="$TMP_ROOT/audit-runtime"; mkdir -p "$AUDIT_RUNTIME/config" "$AUDIT_RUNTIME/secrets"
AUDIT_IMAGE=$(tr -d '\n' <"$BUILD_BUNDLE/edge-image-reference.txt")
printf '%s\n' \
  "DJI4G_EDGE_UID=1000" \
  "DJI4G_EDGE_GID=1001" \
  "DJI4G_EDGE_IMAGE=$AUDIT_IMAGE" \
  "DJI4G_CLOUDFLARED_IMAGE=$(grep '^CLOUDFLARED_IMAGE=' "$FIXTURE_TOOLCHAIN" | cut -d= -f2-)" \
  "DJI4G_COTURN_IMAGE=invalid.local/dji4g-coturn-disabled@sha256:$(printf '0%.0s' {1..64})" \
  "DJI4G_PUBLIC_EDGE_ROOT=$AUDIT_RUNTIME" \
  "DJI4G_EDGE_ENV_FILE=$AUDIT_RUNTIME/config/edge.env" \
  "DJI4G_ENABLED_PROFILES=edge" \
  >"$AUDIT_RUNTIME/compose.env"
printf '%s\n' \
  'DJI4G_EDGE_LISTEN_ADDR=0.0.0.0:8080' \
  'DJI4G_EDGE_PUBLIC_HOST=phone.example.com' \
  'DJI4G_EDGE_GATEWAY_ID=synthetic-gateway' \
  'DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE=/run/config/gateway-public-key.pem' \
  'DJI4G_ACCESS_TEAM_DOMAIN=synthetic.cloudflareaccess.com' \
  'DJI4G_ACCESS_AUDIENCE=synthetic_audience' \
  'DJI4G_ACCESS_ALLOWED_EMAILS_FILE=/run/config/access-allowed-emails' \
  'DJI4G_EDGE_TRUST_FORWARDED_IDENTITY=false' \
  'DJI4G_EDGE_ENROLLMENT_ENABLED=false' \
  'DJI4G_EDGE_MUTATIONS_ENABLED=false' \
  'DJI4G_EDGE_PUSH_ENABLED=false' \
  'DJI4G_EDGE_TURN_ISSUANCE_ENABLED=false' \
  >"$AUDIT_RUNTIME/config/edge.env"
printf '%s\n' '-----BEGIN PUBLIC KEY-----' 'c3ludGhldGljLXB1YmxpYy1rZXk=' '-----END PUBLIC KEY-----' >"$AUDIT_RUNTIME/config/gateway-public-key.pem"
printf '%s\n' 'owner@example.invalid' >"$AUDIT_RUNTIME/config/access-allowed-emails"
printf '%s\n' 'synthetic-cloudflare-token' >"$AUDIT_RUNTIME/secrets/cloudflare-tunnel.token"
chmod 600 "$AUDIT_RUNTIME/compose.env" "$AUDIT_RUNTIME/config/edge.env" \
  "$AUDIT_RUNTIME/config/gateway-public-key.pem" "$AUDIT_RUNTIME/config/access-allowed-emails" \
  "$AUDIT_RUNTIME/secrets/cloudflare-tunnel.token"

AUDIT_FAKE_BIN="$TMP_ROOT/audit-fake-bin"; mkdir "$AUDIT_FAKE_BIN"
printf '%s\n' '#!/usr/bin/env bash' \
  'case "${1:-}" in -u) printf "%s\\n" 1000 ;; -g) printf "%s\\n" 1001 ;; *) exit 64 ;; esac' \
  >"$AUDIT_FAKE_BIN/id"
cat >"$AUDIT_FAKE_BIN/stat" <<'SH'
#!/usr/bin/env bash
case " $* " in
 *"%u"*) printf '%s\n' 1000 ;;
 *"%Lp"*) printf '%s\n' 700 ;;
 *"%d:%i"*) printf '%s\n' 1:2 ;;
 *) exit 64 ;;
esac
SH
cat >"$AUDIT_FAKE_BIN/docker" <<'SH'
#!/usr/bin/env bash
set -eu
fake_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
printf '%s\n' "$*" >>"$fake_dir/calls"
if [ "${1:-}" != --config ] || [ ! -d "${2:-}" ] || [ "${3:-}" != --context ] || [ "${4:-}" != default ]; then exit 77; fi
config_dir=$2;shift 4
[ "$(cat "$config_dir/config.json" 2>/dev/null)" = "{}" ] || exit 78
for name in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH BUILDKIT_HOST BUILDX_BUILDER BUILDX_CONFIG HOME; do
 if [ -n "${!name+x}" ]; then printf '%s\n' "$name" >>"$fake_dir/ambient-leak";exit 79;fi
done
if [ "${1:-}" = info ]; then
 case " $* " in *"{{json .DriverStatus}}"*) printf '%s\n' '[["driver-type","io.containerd.snapshotter.v1"]]' ;; *"{{json .SecurityOptions}}"*) printf '%s\n' '["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]' ;; *"{{.OSType}}/{{.Architecture}}"*) printf '%s\n' linux/amd64 ;; esac
 exit 0
fi
if [ "${1:-}" = version ]; then printf '%s\n' 29.1.3; exit 0; fi
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
 ref=${!#}
 case " $* " in
  *"{{.Os}}/{{.Architecture}}"*) printf '%s\n' linux/amd64 ;;
  *"{{.Id}}"*)
   if [[ "$ref" = sha256:* ]];then printf '%s\n' "$ref"
   elif [[ "$ref" = *@sha256:* ]];then printf 'sha256:%s\n' "${ref##*@sha256:}"
   elif [[ "$ref" = *:dji4g-build-*-a ]];then cat "$fake_dir/image-id"
   else exit 1;fi ;;
  *"{{range .RepoDigests}}"*) printf '%s\n' "$ref" ;;
 esac
 exit 0
fi
if [ "${1:-}" = container ] && [ "${2:-}" = inspect ];then
 requested=${3:-}
 [ -f "$fake_dir/orphan-name" ] && [ "$requested" = "$(tr -d '\n' <"$fake_dir/orphan-name")" ] || exit 1
 orphan_id=$(tr -d '\n' <"$fake_dir/orphan-id")
 orphan_image=$(tr -d '\n' <"$fake_dir/orphan-image")
 orphan_nonce=$(tr -d '\n' <"$fake_dir/orphan-nonce")
 orphan_role=$(tr -d '\n' <"$fake_dir/orphan-role")
 printf '[{"Id":"%s","Name":"/%s","Image":"%s","Config":{"Labels":{"io.maccellular.audit-nonce":"%s","io.maccellular.audit-role":"%s"}}}]\n' \
   "$orphan_id" "$requested" "$orphan_image" "$orphan_nonce" "$orphan_role"
 exit 0
fi
if [ "${1:-}" = container ] && [ "${2:-}" = create ]; then
  return_id() { printf '%s\n' "$1"; }
  signal_owner() {
    owner=$(ps -o ppid= -p "$PPID" | tr -d ' ')
    kill -TERM "$owner"
  }
  case " $* " in
   *io.maccellular.audit-role=syft-version*) id=$(printf 'e%.0s' {1..64});return_id "$id";exit 0 ;;
   *io.maccellular.audit-role=grype-version*) id=$(printf 'f%.0s' {1..64});return_id "$id";exit 0 ;;
   *io.maccellular.audit-role=syft-scan*)
    id=$(printf '1%.0s' {1..64});return_id "$id"
    if [ "$(tr -d '\n' <"$fake_dir/mode")" = scanner-signal ];then signal_owner;fi
    exit 0 ;;
   *io.maccellular.audit-role=shell-check*) id=$(printf 'c%.0s' {1..64});return_id "$id";exit 0 ;;
   *io.maccellular.audit-role=rootfs*)
    if [ "$(tr -d '\n' <"$fake_dir/mode")" = rootfs-collision ];then exit 125;fi
    if [ "$(tr -d '\n' <"$fake_dir/mode")" = audit-collision-mutate-runtime ];then
     runtime=$(tr -d '\n' <"$fake_dir/runtime-root")
     sed 's/^DJI4G_COTURN_IMAGE=.*/DJI4G_COTURN_IMAGE=invalid.local\/dji4g-coturn-disabled@sha256:1111111111111111111111111111111111111111111111111111111111111111/' "$runtime/compose.env" >"$runtime/compose.env.new";mv "$runtime/compose.env.new" "$runtime/compose.env"
     sed 's/^DJI4G_EDGE_GATEWAY_ID=.*/DJI4G_EDGE_GATEWAY_ID=concurrent-replacement/' "$runtime/config/edge.env" >"$runtime/config/edge.env.new";mv "$runtime/config/edge.env.new" "$runtime/config/edge.env"
     printf '%s\n' '-----BEGIN PUBLIC KEY-----' 'Y29uY3VycmVudC1yZXBsYWNlbWVudA==' '-----END PUBLIC KEY-----' >"$runtime/config/gateway-public-key.pem"
     printf '%s\n' attacker@example.invalid >"$runtime/config/access-allowed-emails"
     chmod 600 "$runtime/compose.env" "$runtime/config/edge.env" "$runtime/config/gateway-public-key.pem" "$runtime/config/access-allowed-emails"
    fi
    if [ "$(tr -d '\n' <"$fake_dir/mode")" = rootfs-no-ack ];then
     previous="";name=""
     for value in "$@";do [ "$previous" != --name ] || name=$value;previous=$value;done
     nonce=${name#dji4g-edge-audit-};nonce=${nonce%-rootfs}
     id=$(printf '3%.0s' {1..64})
     printf '%s\n' "$name" >"$fake_dir/orphan-name"
     printf '%s\n' "$id" >"$fake_dir/orphan-id"
     printf '%s\n' "${!#}" >"$fake_dir/orphan-image"
     printf '%s\n' "$nonce" >"$fake_dir/orphan-nonce"
     printf '%s\n' rootfs >"$fake_dir/orphan-role"
     signal_owner;exit 125
    fi
    id=$(printf 'a%.0s' {1..64});return_id "$id"
    if [ "$(tr -d '\n' <"$fake_dir/mode")" = rootfs-signal ];then signal_owner;fi
    exit 0 ;;
   *io.maccellular.audit-role=startup*)
    case "$(tr -d '\n' <"$fake_dir/mode")" in audit-collision|audit-collision-mutate-runtime) exit 125 ;; esac
    id=$(printf 'd%.0s' {1..64});return_id "$id";exit 0 ;;
   *io.maccellular.audit-role=grype-db-update*|*io.maccellular.audit-role=grype-db-status*|*io.maccellular.audit-role=grype-scan*)
    id=$(printf '2%.0s' {1..64});return_id "$id";exit 0 ;;
  esac
  exit 64
fi
if [ "${1:-}" = container ] && [ "${2:-}" = export ]; then printf '%s\n' synthetic-rootfs; exit 0; fi
if [ "${1:-}" = container ] && [ "${2:-}" = rm ]; then
  printf '%s\n' "$*" >>"$fake_dir/removals"
  if [ -f "$fake_dir/orphan-id" ] && [ "${!#}" = "$(tr -d '\n' <"$fake_dir/orphan-id")" ];then
    rm -f "$fake_dir/orphan-name" "$fake_dir/orphan-id" "$fake_dir/orphan-image" "$fake_dir/orphan-nonce" "$fake_dir/orphan-role"
  fi
  exit 0
fi
if [ "${1:-}" = container ] && [ "${2:-}" = start ];then
 id=${!#}
 case "$id" in
  e*) printf '%s\n' '{"version":"1.51.0"}';exit 0 ;;
  f*) printf '%s\n' '{"version":"0.117.0"}';exit 0 ;;
  c*) exit 127 ;;
  *) exit 0 ;;
 esac
fi
if [ "${1:-}" = inspect ];then
 case " $* " in
  *".State.Health"*) printf '%s\n' healthy ;;
  *".State.Running"*) printf '%s\n' true ;;
  *) printf '%s\n' '[]' ;;
 esac
 exit 0
fi
if [ "${1:-}" = logs ];then printf '%s\n' 'synthetic bounded log';exit 0;fi
exit 64
SH
chmod 700 "$AUDIT_FAKE_BIN/docker" "$AUDIT_FAKE_BIN/id" "$AUDIT_FAKE_BIN/stat"
printf '%s\n' "$AUDIT_IMAGE" >"$AUDIT_FAKE_BIN/image-id"
printf '%s\n' "$AUDIT_RUNTIME" >"$AUDIT_FAKE_BIN/runtime-root"
AUDIT_OUTPUT_PARENT="$TMP_ROOT/audit-output"; mkdir "$AUDIT_OUTPUT_PARENT";chmod 700 "$AUDIT_OUTPUT_PARENT"

printf '%s\n' rootfs-collision >"$AUDIT_FAKE_BIN/mode"
if env HOME="$POISON_HOME" DOCKER_HOST=tcp://attacker.invalid:2376 DOCKER_CONTEXT=attacker \
    DOCKER_CONFIG=/attacker/docker-config BUILDKIT_HOST=tcp://attacker.invalid:1234 \
    PATH="$AUDIT_FAKE_BIN:$PATH" "$AUDIT_FIXTURE_DIR/audit-image.sh" \
    --build-dir "$BUILD_APPLY_PARENT/success" --runtime-root "$AUDIT_RUNTIME" \
    --output "$AUDIT_OUTPUT_PARENT/rootfs-collision" --apply >"$TMP_ROOT/audit-rootfs-collision.log" 2>&1; then
  fail "rootfs audit container name collision unexpectedly succeeded"
fi
PREEXISTING_ROOTFS_ID=$(printf 'b%.0s' {1..64})
if grep -Fq "$PREEXISTING_ROOTFS_ID" "$AUDIT_FAKE_BIN/removals" 2>/dev/null; then
  fail "rootfs name collision deleted a pre-existing container"
fi
[ ! -s "$AUDIT_FAKE_BIN/ambient-leak" ] || fail "audit inherited ambient Docker/BuildKit routing"
if ! grep -Eq 'container create .*audit-role=rootfs' "$AUDIT_FAKE_BIN/calls" 2>/dev/null; then
  sed -n '1,120p' "$TMP_ROOT/audit-rootfs-collision.log" >&2
  fail "rootfs collision test did not reach its intended create mutation"
fi
pass "audit ignores ambient and persistent Docker routing/proxy configuration"
grep -Fq "image inspect --format {{.Id}} $BUILD_RETAINED_REF" "$AUDIT_FAKE_BIN/calls" ||
  fail "audit did not bind the build-retained local reference before its first container mutation"
BUILD_DAEMON_CHECK="$TMP_ROOT/build-daemon-check";mkdir "$BUILD_DAEMON_CHECK";printf '{}\n' >"$BUILD_DAEMON_CHECK/config.json"
env -i PATH="$PATH" "$BUILD_FAKE_BIN/docker" --config "$BUILD_DAEMON_CHECK" --context default \
  image inspect --format '{{.Id}}' "$AUDIT_IMAGE" >/dev/null ||
  fail "fake Docker29 target disappeared while its retained build reference still existed"
env -i PATH="$PATH" "$BUILD_FAKE_BIN/docker" --config "$BUILD_DAEMON_CHECK" --context default \
  image rm "$BUILD_RETAINED_REF" >/dev/null
if env -i PATH="$PATH" "$BUILD_FAKE_BIN/docker" --config "$BUILD_DAEMON_CHECK" --context default \
    image inspect --format '{{.Id}}' "$AUDIT_IMAGE" >/dev/null 2>&1;then
  fail "fake Docker29 target survived deletion of its last local reference"
fi
pass "build-to-audit keeps the exact containerd target reachable and models last-reference deletion"

rm -f "$AUDIT_FAKE_BIN/removals"
AUDIT_RUNTIME_BACKUP="$TMP_ROOT/audit-runtime-backup";cp -R "$AUDIT_RUNTIME" "$AUDIT_RUNTIME_BACKUP"
printf '%s\n' audit-collision-mutate-runtime >"$AUDIT_FAKE_BIN/mode"
if PATH="$AUDIT_FAKE_BIN:$PATH" "$AUDIT_FIXTURE_DIR/audit-image.sh" \
    --build-dir "$BUILD_APPLY_PARENT/success" --runtime-root "$AUDIT_RUNTIME" \
    --output "$AUDIT_OUTPUT_PARENT/startup-collision" --apply >/dev/null 2>&1; then
  fail "startup audit container name collision unexpectedly succeeded"
fi
EXPECTED_ROOTFS_ID=$(printf 'a%.0s' {1..64})
grep -Fqx "container rm -f $EXPECTED_ROOTFS_ID" "$AUDIT_FAKE_BIN/removals" ||
  fail "audit cleanup did not target its returned rootfs container ID"
if grep -Eq 'container rm( -f)? dji4g-edge-(rootfs|audit)-' "$AUDIT_FAKE_BIN/removals"; then
  fail "audit cleanup targeted a requested container name"
fi
STARTUP_CREATE_CALL=$(grep 'container create .*audit-role=startup' "$AUDIT_FAKE_BIN/calls" | tail -n 1)
printf '%s\n' "$STARTUP_CREATE_CALL" | grep -Fq -- '--env-file ' || fail "startup audit omitted the snapshotted edge environment"
if printf '%s\n' "$STARTUP_CREATE_CALL" | grep -Fq "$AUDIT_RUNTIME/config/";then
  fail "startup audit consumed a concurrently replaced original runtime input"
fi
rm -rf "$AUDIT_RUNTIME";mv "$AUDIT_RUNTIME_BACKUP" "$AUDIT_RUNTIME"
pass "audit name collisions never delete pre-existing containers; cleanup uses returned IDs only"
pass "audit verifies and consumes only its private runtime-input snapshot after source replacement"

rm -f "$AUDIT_FAKE_BIN/removals"
printf '%s\n' rootfs-signal >"$AUDIT_FAKE_BIN/mode"
set +e
PATH="$AUDIT_FAKE_BIN:$PATH" "$AUDIT_FIXTURE_DIR/audit-image.sh" \
  --build-dir "$BUILD_APPLY_PARENT/success" --runtime-root "$AUDIT_RUNTIME" \
  --output "$AUDIT_OUTPUT_PARENT/rootfs-signal" --apply \
  >"$TMP_ROOT/audit-rootfs-signal.log" 2>&1
AUDIT_SIGNAL_STATUS=$?
set -e
[ "$AUDIT_SIGNAL_STATUS" -eq 143 ] || fail "rootfs create/registration SIGTERM did not preserve status 143"
ROOTFS_SIGNAL_ID=$(printf 'a%.0s' {1..64})
grep -Fqx "container rm -f $ROOTFS_SIGNAL_ID" "$AUDIT_FAKE_BIN/removals" ||
  fail "rootfs create/registration SIGTERM did not remove the exact returned container ID"
[ ! -e "$AUDIT_OUTPUT_PARENT/rootfs-signal" ] || fail "rootfs SIGTERM published completed evidence"
pass "rootfs create atomically records returned-ID ownership before SIGTERM cleanup"

rm -f "$AUDIT_FAKE_BIN/removals" "$AUDIT_FAKE_BIN/orphan-"*
printf '%s\n' rootfs-no-ack >"$AUDIT_FAKE_BIN/mode"
set +e
PATH="$AUDIT_FAKE_BIN:$PATH" "$AUDIT_FIXTURE_DIR/audit-image.sh" \
  --build-dir "$BUILD_APPLY_PARENT/success" --runtime-root "$AUDIT_RUNTIME" \
  --output "$AUDIT_OUTPUT_PARENT/rootfs-no-ack" --apply \
  >"$TMP_ROOT/audit-rootfs-no-ack.log" 2>&1
AUDIT_NO_ACK_STATUS=$?
set -e
[ "$AUDIT_NO_ACK_STATUS" -eq 143 ] || fail "rootfs response-loss SIGTERM did not preserve status 143"
ROOTFS_NO_ACK_ID=$(printf '3%.0s' {1..64})
grep -Fqx "container rm -f $ROOTFS_NO_ACK_ID" "$AUDIT_FAKE_BIN/removals" ||
  fail "rootfs response-loss cleanup did not recover its nonce/label-bound container ID"
[ ! -e "$AUDIT_OUTPUT_PARENT/rootfs-no-ack" ] || fail "rootfs response-loss SIGTERM published completed evidence"
[ ! -e "$AUDIT_FAKE_BIN/orphan-id" ] || fail "rootfs response-loss cleanup left its fake daemon orphan"
pass "container-create response loss recovers only the random name plus exact nonce/role/image identity"

rm -f "$AUDIT_FAKE_BIN/removals"
printf '%s\n' scanner-signal >"$AUDIT_FAKE_BIN/mode"
set +e
PATH="$AUDIT_FAKE_BIN:$PATH" "$AUDIT_FIXTURE_DIR/audit-image.sh" \
  --build-dir "$BUILD_APPLY_PARENT/success" --runtime-root "$AUDIT_RUNTIME" \
  --output "$AUDIT_OUTPUT_PARENT/scanner-signal" --apply \
  >"$TMP_ROOT/audit-scanner-signal.log" 2>&1
AUDIT_SCANNER_SIGNAL_STATUS=$?
set -e
[ "$AUDIT_SCANNER_SIGNAL_STATUS" -eq 143 ] || fail "scanner create/registration SIGTERM did not preserve status 143"
SCANNER_SIGNAL_ID=$(printf '1%.0s' {1..64})
STARTUP_SIGNAL_ID=$(printf 'd%.0s' {1..64})
grep -Fqx "container rm -f $SCANNER_SIGNAL_ID" "$AUDIT_FAKE_BIN/removals" ||
  fail "scanner SIGTERM left its exact returned scanner container orphaned"
grep -Fqx "container rm -f $STARTUP_SIGNAL_ID" "$AUDIT_FAKE_BIN/removals" ||
  fail "scanner SIGTERM left its exact startup container orphaned"
[ ! -e "$AUDIT_OUTPUT_PARENT/scanner-signal" ] || fail "scanner SIGTERM published completed evidence"
pass "scanner create/start records returned-ID ownership and SIGTERM removes every recorded container ID"

EVIDENCE="$TMP_ROOT/evidence"; mkdir "$EVIDENCE"; cp -R "$BUILD_BUNDLE"/. "$EVIDENCE"/
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$EVIDENCE" <<'PY'
import datetime, hashlib, json, pathlib, sys
r=pathlib.Path(sys.argv[1]);build=json.loads((r/"build.json").read_text());tool=dict(line.split("=",1) for line in (r/"toolchain.lock").read_text().splitlines());image=build["image_id"];runtime="/srv/dji4g-public-edge/runtime-v1";audit_nonce="4"*32;mount_root="/srv/dji4g-public-edge/.dji4g-edge-audit.fixture123";image_labels=json.loads((r/"image-inspect.json").read_text())[0]["Config"]["Labels"]
env={
"DJI4G_EDGE_LISTEN_ADDR":"0.0.0.0:8080","DJI4G_EDGE_PUBLIC_HOST":"phone.example.com","DJI4G_EDGE_GATEWAY_ID":"synthetic-gateway",
"DJI4G_EDGE_GATEWAY_PUBLIC_KEY_FILE":"/run/config/gateway-public-key.pem","DJI4G_ACCESS_TEAM_DOMAIN":"synthetic.cloudflareaccess.com",
"DJI4G_ACCESS_AUDIENCE":"synthetic_audience","DJI4G_ACCESS_ALLOWED_EMAILS_FILE":"/run/config/access-allowed-emails",
"DJI4G_EDGE_TRUST_FORWARDED_IDENTITY":"false","DJI4G_EDGE_ENROLLMENT_ENABLED":"false","DJI4G_EDGE_MUTATIONS_ENABLED":"false",
"DJI4G_EDGE_PUSH_ENABLED":"false","DJI4G_EDGE_TURN_ISSUANCE_ENABLED":"false"}
compose_env={
"DJI4G_PUBLIC_EDGE_ROOT":runtime,"DJI4G_EDGE_ENV_FILE":runtime+"/config/edge.env","DJI4G_ENABLED_PROFILES":"edge",
"DJI4G_EDGE_UID":"1000","DJI4G_EDGE_GID":"1001","DJI4G_EDGE_IMAGE":image,
"DJI4G_CLOUDFLARED_IMAGE":tool["CLOUDFLARED_IMAGE"],"DJI4G_COTURN_IMAGE":"invalid.local/dji4g-coturn-disabled@sha256:"+"0"*64}
(r/"runtime-compose.env").write_text("".join(f"{key}={value}\n" for key,value in compose_env.items()),encoding="ascii")
(r/"runtime-edge.env").write_text("".join(f"{key}={value}\n" for key,value in env.items()),encoding="ascii")
(r/"runtime-gateway-public-key.pem").write_text("-----BEGIN PUBLIC KEY-----\nc3ludGhldGljLXB1YmxpYy1rZXk=\n-----END PUBLIC KEY-----\n",encoding="ascii")
(r/"runtime-access-allowed-emails").write_text("owner@example.invalid\n",encoding="ascii")
binds={"/run/config/gateway-public-key.pem":mount_root+"/runtime-gateway-public-key.pem","/run/config/access-allowed-emails":mount_root+"/runtime-access-allowed-emails"}
requested=[{"Type":"bind","Source":source,"Target":target,"ReadOnly":True,"Consistency":"","BindOptions":{"Propagation":"rprivate"}} for target,source in binds.items()]
resolved=[{"Type":"bind","Source":source,"Destination":target,"Mode":"ro","RW":False,"Propagation":"rprivate"} for target,source in binds.items()]
network_id="a"*64;endpoint_id="b"*64
startup={"Id":"d"*64,"Name":"/dji4g-edge-audit-"+audit_nonce+"-startup","Image":image,"Path":"/djonehub-edge","Args":[],"RestartCount":0,"Platform":"linux","AppArmorProfile":"docker-default","ProcessLabel":"","MountLabel":"",
"State":{"Status":"running","Running":True,"Restarting":False,"OOMKilled":False,"Dead":False,"ExitCode":0,"Health":{"Status":"healthy"}},
"Config":{"Hostname":"d"*12,"Domainname":"","Image":image,"User":"1000:1001","AttachStdin":False,"AttachStdout":False,"AttachStderr":False,"Tty":False,"OpenStdin":False,"StdinOnce":False,"NetworkDisabled":False,"MacAddress":"","OnBuild":None,"StopTimeout":None,"Shell":None,"StopSignal":"SIGTERM","Labels":image_labels|{"io.maccellular.audit-nonce":audit_nonce,"io.maccellular.audit-role":"startup"},"Entrypoint":["/djonehub-edge"],"Cmd":None,"Volumes":None,"WorkingDir":"","ExposedPorts":{"8080/tcp":{}},"Healthcheck":{"Test":["CMD","/djonehub-edge-healthcheck"],"Interval":10000000000,"Timeout":3000000000,"StartPeriod":5000000000,"Retries":3},"Env":[key+"="+value for key,value in env.items()]},
"HostConfig":{"ReadonlyRootfs":True,"Privileged":False,"CapAdd":None,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges:true","apparmor=docker-default","seccomp=builtin"],"Init":True,"PidsLimit":128,"Memory":268435456,"MemorySwap":268435456,"MemoryReservation":0,"NanoCpus":1000000000,"CpuShares":0,"CpuPeriod":0,"CpuQuota":0,"CpuRealtimePeriod":0,"CpuRealtimeRuntime":0,"CpuCount":0,"CpuPercent":0,"CpusetCpus":"","CpusetMems":"","NetworkMode":"none","AutoRemove":False,"PublishAllPorts":False,"PortBindings":{},"PidMode":"","IpcMode":"private","UTSMode":"","UsernsMode":"","CgroupnsMode":"private","Runtime":"runc","CgroupParent":"","Cgroup":"","Sysctls":{},"OomScoreAdj":0,"BlkioWeight":0,"ShmSize":67108864,"Isolation":"","LogConfig":{"Type":"json-file","Config":{"max-size":"10m","max-file":"3"}},"ExtraHosts":[],"Devices":[],"DeviceRequests":[],"VolumesFrom":[],"Links":[],"RestartPolicy":{"Name":"no","MaximumRetryCount":0},"ContainerIDFile":"","VolumeDriver":"","IOMaximumIOps":0,"IOMaximumBandwidth":0,"MaskedPaths":["/proc/acpi","/proc/asound","/proc/interrupts","/proc/kcore","/proc/keys","/proc/latency_stats","/proc/sched_debug","/proc/scsi","/proc/timer_list","/proc/timer_stats","/sys/devices/virtual/powercap","/sys/firmware"],"ReadonlyPaths":["/proc/bus","/proc/fs","/proc/irq","/proc/sys","/proc/sysrq-trigger"],"Tmpfs":{"/tmp":"rw,noexec,nosuid,nodev,size=32m,mode=1777"},"Mounts":requested},"NetworkSettings":{"Ports":{},"Networks":{"none":{"IPAMConfig":None,"Links":None,"Aliases":None,"MacAddress":"","DriverOpts":None,"GwPriority":0,"NetworkID":network_id,"EndpointID":endpoint_id,"Gateway":"","IPAddress":"","IPPrefixLen":0,"IPv6Gateway":"","GlobalIPv6Address":"","GlobalIPv6PrefixLen":0,"DNSNames":None}}},"Mounts":resolved}
now=datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)
built=(now-datetime.timedelta(hours=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
extra={
"cve-report.json":{"matches":[],"source":{"type":"sbom-file","target":"/work/sbom.spdx.json"},"distro":{"name":"","version":"","idLike":None},"descriptor":{"name":"grype","version":"0.117.0","configuration":{},"db":{"status":{"schemaVersion":"v6.1.9","from":"https://grype.anchore.io/databases/v6/vulnerability-db_v6.1.9_2026-08-15T01:31:06Z_1786757466.tar.zst?checksum=sha256%3A"+"c"*64,"built":built,"path":"/work/grype-db/6/vulnerability.db","valid":True},"providers":{}}}},"grype-db-status.json":{"schemaVersion":"v6.1.9","from":"https://grype.anchore.io/databases/v6/vulnerability-db_v6.1.9_2026-08-15T01:31:06Z_1786757466.tar.zst?checksum=sha256%3A"+"c"*64,"built":built,"path":"/work/grype-db/6/vulnerability.db","valid":True},
"rootfs-report.json":{"architecture":"amd64","ca_bundle":True,"edge_static_elf":True,"healthcheck_static_elf":True,"no_credentials":True,"no_shell":True,"no_source":True},
"sbom.spdx.json":{"spdxVersion":"SPDX-2.3","dataLicense":"CC0-1.0","SPDXID":"SPDXRef-DOCUMENT","name":"edge-image","documentNamespace":"https://anchore.com/syft/edge-synthetic","creationInfo":{"creators":["Organization: Anchore, Inc","Tool: syft-1.51.0"],"created":now.strftime("%Y-%m-%dT%H:%M:%SZ")},"packages":[{"name":"edge","SPDXID":"SPDXRef-Package-edge"}],"relationships":[{"spdxElementId":"SPDXRef-DOCUMENT","relationshipType":"DESCRIBES","relatedSpdxElement":"SPDXRef-Package-edge"}]},
"startup-inspect.json":[startup],
"startup.log":"Public edge read-only service is ready\n"}
for name,value in extra.items():
 p=r/name
 if isinstance(value,str):p.write_text(value,encoding="ascii")
 else:p.write_text(json.dumps(value,separators=(",",":"))+"\n",encoding="ascii")
names={p.name for p in r.iterdir()}
hashes={name:hashlib.sha256((r/name).read_bytes()).hexdigest() for name in names}
runtime_hashes={logical:hashes[name] for logical,name in {"compose.env":"runtime-compose.env","config/edge.env":"runtime-edge.env","config/gateway-public-key.pem":"runtime-gateway-public-key.pem","config/access-allowed-emails":"runtime-access-allowed-emails"}.items()}
manifest={"format":1,"audited_at":now.strftime("%Y-%m-%dT%H:%M:%SZ"),"image_id":image,"accepted_references":[image],"source_tree_sha256":build["source_tree_sha256"],"runtime_uid_gid":"1000:1001","runtime_root":runtime,"audit_nonce":audit_nonce,"audit_input_mount_root":mount_root,"runtime_input_sha256":runtime_hashes,"policy":{"architecture":"linux/amd64","cve_threshold":"high","health":"passed","read_only_arbitrary_uid":"passed","rootfs":"scratch-no-shell","sbom":"spdx-json","startup":"passed","grype_version":"0.117.0","grype_db_schema":"v6.1.9","grype_db_max_age_hours":48},"artifacts":hashes}
(r/"evidence.json").write_text(json.dumps(manifest,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
PY
EVIDENCE_IMAGE=$(tr -d '\n' <"$EVIDENCE/edge-image-reference.txt")
python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$EVIDENCE" --image "$EVIDENCE_IMAGE" \
  --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
  --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"
pass "complete synthetic image evidence passes without Docker"
RUNTIME_MAP_TAMPER="$TMP_ROOT/evidence-runtime-map-tamper";cp -R "$EVIDENCE" "$RUNTIME_MAP_TAMPER"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$RUNTIME_MAP_TAMPER" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);p=r/"runtime-access-allowed-emails";p.write_text("attacker@example.invalid\n",encoding="ascii")
m=json.loads((r/"evidence.json").read_text());m["artifacts"][p.name]=hashlib.sha256(p.read_bytes()).hexdigest()
(r/"evidence.json").write_text(json.dumps(m,sort_keys=True,separators=(",",":"))+"\n",encoding="ascii")
PY
expect_fail "self-rehashed runtime artifact cannot diverge from its separate audit-input digest map" \
  python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$RUNTIME_MAP_TAMPER" \
    --image "$AUDIT_IMAGE" --repo-root "$SOURCE_REPO" \
    --trusted-source-lock "$SOURCE_REPO/source.lock" --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"
EVIDENCE_AUDITED_AT=$(python3 -B -I - "$EVIDENCE/evidence.json" <<'PY'
import json,pathlib,sys
print(json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="ascii"))["audited_at"])
PY
)
python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$EVIDENCE" --image "$EVIDENCE_IMAGE" \
  --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
  --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN" --as-of "$EVIDENCE_AUDITED_AT"
pass "archival image-evidence replay binds freshness to the recorded action time"

PUBLISH_AUTH="$TMP_ROOT/publish-auth";mkdir "$PUBLISH_AUTH";chmod 700 "$PUBLISH_AUTH"
printf '%s\n' '{"auths":{"registry.invalid":{"auth":"dXNlcjpwYXNz"}}}' >"$PUBLISH_AUTH/config.json"
chmod 600 "$PUBLISH_AUTH/config.json"
PUBLISH_FAKE_BIN="$TMP_ROOT/publish-fake-bin";mkdir "$PUBLISH_FAKE_BIN"
printf '%s\n' "$PUBLISH_AUTH" >"$PUBLISH_FAKE_BIN/auth-dir"
cp "$PUBLISH_AUTH/config.json" "$PUBLISH_FAKE_BIN/expected-auth.json"
cat >"$PUBLISH_FAKE_BIN/docker" <<'SH'
#!/usr/bin/env bash
set -eu
fake_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
if [ "${1:-}" != --config ] || [ ! -d "${2:-}" ] || [ "${3:-}" != --context ] || [ "${4:-}" != default ]; then exit 77;fi
config_dir=$2;shift 4
for name in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH BUILDKIT_HOST BUILDX_BUILDER BUILDX_CONFIG HOME;do
 [ -z "${!name+x}" ] || exit 78
done
printf '%s\n' "$config_dir" >>"$fake_dir/config-paths"
printf '%s\n' "$*" >>"$fake_dir/calls"
if [ "${1:-}" = info ];then
 if [ -f "$fake_dir/mode" ] && [ "$(tr -d '\n' <"$fake_dir/mode")" = auth-mutate ] && [ ! -f "$fake_dir/auth-mutated" ];then
  original=$(tr -d '\n' <"$fake_dir/auth-dir")
  printf '%s\n' '{"auths":{"registry.invalid":{"auth":"YXR0YWNrZXI6YXR0YWNrZXI="}}}' >"$original/config.json"
  chmod 600 "$original/config.json";: >"$fake_dir/auth-mutated"
 fi
 case " $* " in
  *"{{json .DriverStatus}}"*) printf '%s\n' '[["driver-type","io.containerd.snapshotter.v1"]]' ;;
  *"{{json .SecurityOptions}}"*) printf '%s\n' '["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]' ;;
  *"{{.OSType}}/{{.Architecture}}"*) printf '%s\n' linux/amd64 ;;
 esac
 exit 0
fi
if [ "${1:-}" = version ];then printf '%s\n' 29.1.3;exit 0;fi
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ];then
 ref=${!#}
 if [[ "$ref" != sha256:* ]] && [[ "$ref" != *@sha256:* ]];then
  [ -f "$fake_dir/tag-ref" ] && [ "$ref" = "$(tr -d '\n' <"$fake_dir/tag-ref")" ] || exit 1
 fi
 if [[ "$ref" = *@sha256:* ]];then immutable=$ref
 else immutable="${ref%:*}@sha256:$(printf '7%.0s' {1..64})";fi
 case " $* " in
  *"{{.Id}}"*) tr -d '\n' <"$fake_dir/image-id";printf '\n' ;;
  *"{{.Os}}/{{.Architecture}}"*) printf '%s\n' linux/amd64 ;;
  *"{{range .RepoDigests}}"*) : >"$fake_dir/after-push";printf '%s\n' "$immutable" ;;
  *) [ ! -f "$fake_dir/push-returned" ] || : >"$fake_dir/after-push";printf '[{"Id":"%s","RepoDigests":["%s"]}]\n' "$(tr -d '\n' <"$fake_dir/image-id")" "$immutable" ;;
 esac
 exit 0
fi
if [ "${1:-}" = image ] && [ "${2:-}" = tag ];then printf '%s\n' "${4:-}" >"$fake_dir/tag-ref";: >"$fake_dir/tagged";exit 0;fi
if [ "${1:-}" = image ] && [ "${2:-}" = push ];then
 original=$(tr -d '\n' <"$fake_dir/auth-dir")
 [ "$config_dir" != "$original" ] || exit 79
 case "$config_dir" in /tmp/dji4g-edge-registry-auth.*) ;; *) exit 79 ;; esac
 cmp -s "$config_dir/config.json" "$fake_dir/expected-auth.json" || exit 79
 rm -f "$fake_dir/after-push";: >"$fake_dir/push-started"
 [ ! -f "$fake_dir/mode" ] || [ "$(tr -d '\n' <"$fake_dir/mode")" != signal ] || sleep 1
 : >"$fake_dir/push-returned";exit 0
fi
if [ "${1:-}" = image ] && [ "${2:-}" = rm ];then
 [ -f "$fake_dir/tag-ref" ] && [ "${3:-}" = "$(tr -d '\n' <"$fake_dir/tag-ref")" ] || exit 1
 rm -f "$fake_dir/tag-ref";exit 0
fi
exit 64
SH
chmod 700 "$PUBLISH_FAKE_BIN/docker"
printf '%s\n' "$EVIDENCE_IMAGE" >"$PUBLISH_FAKE_BIN/image-id"
printf '%s\n' signal >"$PUBLISH_FAKE_BIN/mode"
set +e
env HOME="$POISON_HOME" DOCKER_HOST=tcp://attacker.invalid:2376 DOCKER_CONTEXT=attacker \
  DOCKER_CONFIG=/attacker/docker-config BUILDKIT_HOST=tcp://attacker.invalid:1234 \
  PATH="$PUBLISH_FAKE_BIN:$PATH" "$AUDIT_FIXTURE_DIR/publish-image.sh" \
    --evidence "$EVIDENCE" --repository registry.invalid/owner/edge --tag signal \
    --registry-auth-dir "$PUBLISH_AUTH" --output "$TMP_ROOT/publish-signal-output" \
    --confirm-push >"$TMP_ROOT/publish-signal.log" 2>&1 &
PUBLISH_SIGNAL_PID=$!
set -e
for _ in $(seq 1 100);do [ -f "$PUBLISH_FAKE_BIN/push-started" ] && break;sleep 0.02;done
[ -f "$PUBLISH_FAKE_BIN/push-started" ] || fail "publish signal fixture never reached registry push"
kill -TERM "$PUBLISH_SIGNAL_PID"
set +e;wait "$PUBLISH_SIGNAL_PID";PUBLISH_SIGNAL_STATUS=$?;set -e
[ "$PUBLISH_SIGNAL_STATUS" -eq 143 ] || fail "publish did not preserve SIGTERM status 143"
[ -f "$PUBLISH_FAKE_BIN/tagged" ] && [ -f "$PUBLISH_FAKE_BIN/push-returned" ] || fail "publish signal fixture did not complete its blocking child"
[ ! -f "$PUBLISH_FAKE_BIN/after-push" ] || fail "publish continued to post-push mutation/evidence after SIGTERM"
[ ! -e "$TMP_ROOT/publish-signal-output" ] || fail "cancelled publish emitted completed evidence"
grep -Fq 'remote write began; nonce tag may exist and is not automatically deleted' "$TMP_ROOT/publish-signal.log" ||
  fail "interrupted publication hid the possible permanent remote nonce tag"
while IFS= read -r config_dir;do
 [ "$config_dir" = "$PUBLISH_AUTH" ] || [ ! -e "$config_dir" ] || fail "publish left its controlled Docker config behind"
done <"$PUBLISH_FAKE_BIN/config-paths"
pass "publish pins local daemon/auth config and SIGTERM prevents every post-push mutation"

rm -f "$PUBLISH_FAKE_BIN/push-started" "$PUBLISH_FAKE_BIN/push-returned" \
  "$PUBLISH_FAKE_BIN/after-push" "$PUBLISH_FAKE_BIN/tag-ref" "$PUBLISH_FAKE_BIN/auth-mutated" \
  "$PUBLISH_FAKE_BIN/config-paths" "$PUBLISH_FAKE_BIN/calls"
cp "$PUBLISH_FAKE_BIN/expected-auth.json" "$PUBLISH_AUTH/config.json";chmod 600 "$PUBLISH_AUTH/config.json"
printf '%s\n' auth-mutate >"$PUBLISH_FAKE_BIN/mode"
PUBLISHED_EVIDENCE="$TMP_ROOT/published-evidence"
env PATH="$PUBLISH_FAKE_BIN:$PATH" "$AUDIT_FIXTURE_DIR/publish-image.sh" \
  --evidence "$EVIDENCE" --repository registry.invalid/owner/edge --tag v1 \
  --registry-auth-dir "$PUBLISH_AUTH" --output "$PUBLISHED_EVIDENCE" --confirm-push \
  >"$TMP_ROOT/publish-success.log" 2>&1
[ -f "$PUBLISH_FAKE_BIN/auth-mutated" ] || fail "publish auth TOCTOU fixture never replaced the original input"
cmp -s "$PUBLISH_AUTH/config.json" "$PUBLISH_FAKE_BIN/expected-auth.json" &&
  fail "publish auth TOCTOU fixture did not actually change the source after snapshot"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$PUBLISHED_EVIDENCE/publication-record.json" <<'PY'
import json,pathlib,re,sys
value=json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="ascii"))
if value.get("remote_nonce_retention")!="not-automatically-deleted":raise SystemExit("remote tag retention is missing")
if value.get("response_loss_policy")!="remote-nonce-tag-may-exist-reconcile-before-retry":raise SystemExit("response-loss policy is missing")
if re.fullmatch(r"registry\.invalid/owner/edge:v1-[0-9a-f]{32}",str(value.get("remote_nonce_tag"))) is None:raise SystemExit("remote nonce tag is invalid")
PY
python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$PUBLISHED_EVIDENCE" \
  --image "$(python3 -B -I - "$PUBLISHED_EVIDENCE/publication-record.json" <<'PY'
import json,pathlib,sys
print(json.loads(pathlib.Path(sys.argv[1]).read_text())["immutable_reference"])
PY
)" --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
  --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN" >/dev/null || fail "published receipt did not verify"
while IFS= read -r config_dir;do
 [ "$config_dir" = "$PUBLISH_AUTH" ] || [ ! -e "$config_dir" ] || fail "successful publish left its private auth/daemon snapshot behind"
done <"$PUBLISH_FAKE_BIN/config-paths"
pass "publication uses an immutable auth snapshot and records permanent remote nonce-tag response-loss semantics"

BAD_PUBLISH_AUTH="$TMP_ROOT/publish-auth-bad";mkdir "$BAD_PUBLISH_AUTH";chmod 700 "$BAD_PUBLISH_AUTH"
printf '%s\n' '{"auths":{"registry.invalid":{"auth":"dXNlcjpwYXNz"}},"currentContext":"attacker","proxies":{"default":{"httpProxy":"http://secret@attacker.invalid"}}}' >"$BAD_PUBLISH_AUTH/config.json"
chmod 600 "$BAD_PUBLISH_AUTH/config.json"
expect_fail "registry auth config cannot carry daemon context or proxy policy" \
  "$AUDIT_FIXTURE_DIR/publish-image.sh" --evidence "$EVIDENCE" \
    --repository registry.invalid/owner/edge --tag bad-auth --registry-auth-dir "$BAD_PUBLISH_AUTH" \
    --output "$TMP_ROOT/publish-bad-auth" --confirm-push

RW_EVIDENCE="$TMP_ROOT/evidence-rw-volume"; cp -R "$EVIDENCE" "$RW_EVIDENCE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$RW_EVIDENCE" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);path=r/"startup-inspect.json";startup=json.loads(path.read_text())
startup[0]["Mounts"].append({"Type":"volume","Name":"anonymous","Source":"/var/lib/docker/volumes/anonymous/_data","Destination":"/unexpected-writable","Driver":"local","Mode":"","RW":True,"Propagation":""})
path.write_text(json.dumps(startup,separators=(",",":"))+"\n")
manifest=json.loads((r/"evidence.json").read_text());manifest["artifacts"]["startup-inspect.json"]=hashlib.sha256(path.read_bytes()).hexdigest()
(r/"evidence.json").write_text(json.dumps(manifest,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "startup evidence with an anonymous writable volume is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$RW_EVIDENCE" --image "$EVIDENCE_IMAGE" \
    --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
    --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"

HOSTILE_STARTUP="$TMP_ROOT/evidence-hostile-startup"; cp -R "$EVIDENCE" "$HOSTILE_STARTUP"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$HOSTILE_STARTUP" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);path=r/"startup-inspect.json";value=json.loads(path.read_text());container=value[0]
container["HostConfig"]["PidMode"]="host"
container["HostConfig"]["PortBindings"]={"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"18080"}]}
container["HostConfig"]["LogConfig"]={"Type":"syslog","Config":{"syslog-address":"tcp://attacker.invalid:514"}}
container["HostConfig"]["Devices"]=[{"PathOnHost":"/dev/mem","PathInContainer":"/dev/mem","CgroupPermissions":"rwm"}]
container["NetworkSettings"]["Networks"]["hostile"]={"IPAddress":"10.0.0.2"}
path.write_text(json.dumps(value,separators=(",",":"))+"\n")
manifest=json.loads((r/"evidence.json").read_text());manifest["artifacts"]["startup-inspect.json"]=hashlib.sha256(path.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(manifest,sort_keys=True,separators=(",",":"))+"\n")
PY
expect_fail "startup host namespace, public port, external log, device, and extra network are rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$HOSTILE_STARTUP" --image "$EVIDENCE_IMAGE" \
    --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
    --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"

for variant in root-group cpu-realtime unmasked-proc thermal-unordered unconfined-apparmor interactive; do
  HOST_DEFAULT_EVIDENCE="$TMP_ROOT/evidence-host-default-$variant";cp -R "$EVIDENCE" "$HOST_DEFAULT_EVIDENCE"
  PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$HOST_DEFAULT_EVIDENCE" "$variant" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);variant=sys.argv[2];p=r/"startup-inspect.json";value=json.loads(p.read_text());host=value[0]["HostConfig"]
if variant=="root-group":host["GroupAdd"]=["0"]
elif variant=="cpu-realtime":host["CpuRealtimeRuntime"]=950000
elif variant=="unmasked-proc":host["MaskedPaths"]=[]
elif variant=="thermal-unordered":host["MaskedPaths"].extend(["/sys/devices/system/cpu/cpu1/thermal_throttle","/sys/devices/system/cpu/cpu0/thermal_throttle"])
elif variant=="unconfined-apparmor":value[0]["AppArmorProfile"]=""
else:value[0]["Config"]["OpenStdin"]=True
p.write_text(json.dumps(value,separators=(",",":"))+"\n")
m=json.loads((r/"evidence.json").read_text());m["artifacts"]["startup-inspect.json"]=hashlib.sha256(p.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(m)+"\n")
PY
  expect_fail "startup Docker host confinement rejects $variant" \
    python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$HOST_DEFAULT_EVIDENCE" --image "$EVIDENCE_IMAGE" \
      --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
      --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"
done

SAFE_HOST_DEFAULTS="$TMP_ROOT/evidence-safe-host-defaults";cp -R "$EVIDENCE" "$SAFE_HOST_DEFAULTS"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$SAFE_HOST_DEFAULTS" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);p=r/"startup-inspect.json";value=json.loads(p.read_text());host=value[0]["HostConfig"]
host.pop("Sysctls",None);host["MaskedPaths"].extend(f"/sys/devices/system/cpu/cpu{cpu}/thermal_throttle" for cpu in range(11))
p.write_text(json.dumps(value,separators=(",",":"))+"\n")
m=json.loads((r/"evidence.json").read_text());m["artifacts"]["startup-inspect.json"]=hashlib.sha256(p.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(m,sort_keys=True,separators=(",",":"))+"\n")
PY
python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$SAFE_HOST_DEFAULTS" --image "$EVIDENCE_IMAGE" \
  --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
  --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"
pass "startup accepts omitted empty Sysctls and Docker safe CPU thermal masks"

for variant in synthetic stale future invalid; do
  DB_EVIDENCE="$TMP_ROOT/evidence-db-$variant"; cp -R "$EVIDENCE" "$DB_EVIDENCE"
  PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$DB_EVIDENCE" "$variant" <<'PY'
import datetime,hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);variant=sys.argv[2];path=r/"grype-db-status.json";status=json.loads(path.read_text())
if variant=="synthetic":status={"built":"synthetic"}
elif variant=="stale":status["built"]=(datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(hours=72)).strftime("%Y-%m-%dT%H:%M:%SZ")
elif variant=="future":status["built"]=(datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=2)).strftime("%Y-%m-%dT%H:%M:%SZ")
elif variant=="invalid":status["valid"]=False;status["error"]="checksum mismatch"
path.write_text(json.dumps(status,separators=(",",":"))+"\n")
manifest=json.loads((r/"evidence.json").read_text());manifest["artifacts"]["grype-db-status.json"]=hashlib.sha256(path.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(manifest,sort_keys=True,separators=(",",":"))+"\n")
PY
  expect_fail "Grype DB $variant status evidence is rejected" \
    python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$DB_EVIDENCE" --image "$EVIDENCE_IMAGE" \
      --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
      --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"
done

MINIMAL_SBOM="$TMP_ROOT/evidence-minimal-sbom";cp -R "$EVIDENCE" "$MINIMAL_SBOM"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$MINIMAL_SBOM" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);p=r/"sbom.spdx.json";p.write_text('{"spdxVersion":"SPDX-2.3","packages":[{"name":"edge"}]}\n',encoding="ascii")
m=json.loads((r/"evidence.json").read_text());m["artifacts"]["sbom.spdx.json"]=hashlib.sha256(p.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(m)+"\n")
PY
expect_fail "self-rehashed minimal synthetic SPDX report is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$MINIMAL_SBOM" --image "$EVIDENCE_IMAGE" \
    --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
    --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"

MINIMAL_CVE="$TMP_ROOT/evidence-minimal-cve";cp -R "$EVIDENCE" "$MINIMAL_CVE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$MINIMAL_CVE" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);p=r/"cve-report.json";p.write_text('{"matches":[]}\n',encoding="ascii")
m=json.loads((r/"evidence.json").read_text());m["artifacts"]["cve-report.json"]=hashlib.sha256(p.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(m)+"\n")
PY
expect_fail "self-rehashed minimal synthetic Grype report is rejected" \
  python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$MINIMAL_CVE" --image "$EVIDENCE_IMAGE" \
    --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
    --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"

IGNORED_CVE="$TMP_ROOT/evidence-ignored-cve";cp -R "$EVIDENCE" "$IGNORED_CVE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$IGNORED_CVE" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);p=r/"cve-report.json";value=json.loads(p.read_text());value["ignoredMatches"]=[{"reason":"synthetic suppression"}]
p.write_text(json.dumps(value)+"\n");m=json.loads((r/"evidence.json").read_text());m["artifacts"]["cve-report.json"]=hashlib.sha256(p.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(m)+"\n")
PY
expect_fail "non-empty Grype ignoredMatches cannot hide a finding" \
  python3 -B -I "$SCRIPT_DIR/verify-evidence.py" --evidence "$IGNORED_CVE" --image "$EVIDENCE_IMAGE" \
    --repo-root "$SOURCE_REPO" --trusted-source-lock "$SOURCE_REPO/source.lock" \
    --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"

HIGH_CVE="$TMP_ROOT/evidence-high-cve";cp -R "$EVIDENCE" "$HIGH_CVE"
PYTHONDONTWRITEBYTECODE=1 python3 -B -I - "$HIGH_CVE" <<'PY'
import hashlib,json,pathlib,sys
r=pathlib.Path(sys.argv[1]);p=r/"cve-report.json";value=json.loads(p.read_text());value["matches"]=[{"artifact":{"name":"edge"},"vulnerability":{"id":"CVE-2099-0001","severity":"High"}}];p.write_text(json.dumps(value)+"\n",encoding="ascii")
m=json.loads((r/"evidence.json").read_text());m["artifacts"]["cve-report.json"]=hashlib.sha256(p.read_bytes()).hexdigest();(r/"evidence.json").write_text(json.dumps(m)+"\n")
PY
expect_fail "High CVE evidence is rejected" python3 -B -I "$SCRIPT_DIR/verify-evidence.py" \
  --evidence "$HIGH_CVE" --image "$EVIDENCE_IMAGE" --repo-root "$SOURCE_REPO" \
  --trusted-source-lock "$SOURCE_REPO/source.lock" --trusted-toolchain-lock "$FIXTURE_TOOLCHAIN"

printf 'PASS: %d hermetic image/static checks; Docker daemon and network were not used.\n' "$PASS"
