#!/bin/zsh
set -euo pipefail

ROOT=${0:A:h:h:h}
CHECKER="$ROOT/scripts/check-release-runtime-policy.sh"
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/maccellular-runtime-policy-test.XXXXXX")
trap '/bin/rm -rf -- "$TEST_ROOT"' EXIT HUP INT TERM

typeset -i PASSED=0

pass() {
  print -r -- "PASS: $1"
  PASSED+=1
}

expect_pass() {
  local name=$1
  shift
  if "$@" >"$TEST_ROOT/stdout" 2>"$TEST_ROOT/stderr"; then
    pass "$name"
    return
  fi
  print -u2 -r -- "FAIL: $name unexpectedly failed"
  sed -n '1,120p' "$TEST_ROOT/stderr" >&2
  exit 1
}

expect_fail() {
  local name=$1
  shift
  if "$@" >"$TEST_ROOT/stdout" 2>"$TEST_ROOT/stderr"; then
    print -u2 -r -- "FAIL: $name unexpectedly passed"
    exit 1
  fi
  if ! grep -q 'Release runtime policy: FAIL' "$TEST_ROOT/stderr"; then
    print -u2 -r -- "FAIL: $name did not report a policy failure"
    sed -n '1,120p' "$TEST_ROOT/stderr" >&2
    exit 1
  fi
  pass "$name"
}

mkdir -p "$TEST_ROOT/clean/bin"
print -rn -- 'ordinary release content' >"$TEST_ROOT/clean/bin/app"
ln -s 'app' "$TEST_ROOT/clean/bin/app-link"
expect_pass 'clean directory' "$CHECKER" "$TEST_ROOT/clean"

mkdir -p "$TEST_ROOT/original/Resources/ModuleVoice"
print -rn -- 'not even a real kernel module' >"$TEST_ROOT/original/Resources/ModuleVoice/qdc507_voice.ko"
expect_fail 'original runtime name and ModuleVoice directory' "$CHECKER" "$TEST_ROOT/original"

mkdir -p "$TEST_ROOT/renamed"
# Keep the test hermetic: first assert the production checker pins all three
# real payload hashes, then replace one hash only in a disposable checker copy
# to exercise renamed-payload detection with deterministic fixture bytes.
for hash in \
  88d47c15e61d1428a59c821fed804c2e6490e82859a085062f21966b58d167fc \
  3d82d3dec4f1e323201bba87156df9d41438e08314097353f2607f9117211d4a \
  ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c
do
  grep -Fq "$hash" "$CHECKER" || {
    print -u2 -r -- "FAIL: production checker is missing pinned hash $hash"
    exit 1
  }
done
print -rn -- 'deterministic renamed payload fixture' >"$TEST_ROOT/renamed/innocent-resource.bin"
FIXTURE_HASH=$(/usr/bin/shasum -a 256 "$TEST_ROOT/renamed/innocent-resource.bin" | /usr/bin/awk '{print $1}')
sed "s/ed3821682d5309969a01c764192c83feff9669c61ef237c69475cd1619cf296c/$FIXTURE_HASH/" \
  "$CHECKER" >"$TEST_ROOT/checker-with-test-hash.sh"
chmod 755 "$TEST_ROOT/checker-with-test-hash.sh"
expect_fail 'renamed payload with pinned SHA-256' "$TEST_ROOT/checker-with-test-hash.sh" "$TEST_ROOT/renamed"

mkdir -p "$TEST_ROOT/symlink"
ln -s '../outside/innocent.bin' "$TEST_ROOT/symlink/escaped-resource"
expect_fail 'symlink escaping the release root' "$CHECKER" "$TEST_ROOT/symlink"

mkdir -p "$TEST_ROOT/zip-source/app/ModuleVoice"
print -rn -- 'runtime' >"$TEST_ROOT/zip-source/app/ModuleVoice/mavo-pcm-bridge.armv7"
/usr/bin/ditto -c -k --keepParent "$TEST_ROOT/zip-source/app" "$TEST_ROOT/polluted.zip"
expect_fail 'polluted ZIP archive' "$CHECKER" "$TEST_ROOT/polluted.zip"

/usr/bin/tar -cf "$TEST_ROOT/polluted.tar" -C "$TEST_ROOT/zip-source" app
expect_fail 'polluted tar archive' "$CHECKER" "$TEST_ROOT/polluted.tar"
/usr/bin/tar -czf "$TEST_ROOT/polluted.tar.gz" -C "$TEST_ROOT/zip-source" app
expect_fail 'polluted tar.gz archive' "$CHECKER" "$TEST_ROOT/polluted.tar.gz"
cp "$TEST_ROOT/polluted.tar.gz" "$TEST_ROOT/polluted.tgz"
expect_fail 'polluted tgz archive' "$CHECKER" "$TEST_ROOT/polluted.tgz"
/usr/bin/tar -cjf "$TEST_ROOT/polluted.tar.bz2" -C "$TEST_ROOT/zip-source" app
expect_fail 'polluted tar.bz2 archive' "$CHECKER" "$TEST_ROOT/polluted.tar.bz2"

mkdir -p "$TEST_ROOT/dmg-source/ModuleVoice"
print -rn -- 'runtime' >"$TEST_ROOT/dmg-source/ModuleVoice/qdc507_aprv3.ko"
hdiutil create -quiet -volname RuntimePolicyTest -srcfolder "$TEST_ROOT/dmg-source" \
  -ov -format UDZO "$TEST_ROOT/polluted.dmg"
expect_fail 'polluted DMG image' "$CHECKER" "$TEST_ROOT/polluted.dmg"

mkdir -p "$TEST_ROOT/git-repo"
git -C "$TEST_ROOT/git-repo" init -q
cp "$TEST_ROOT/renamed/innocent-resource.bin" "$TEST_ROOT/git-repo/renamed-resource.bin"
git -C "$TEST_ROOT/git-repo" add renamed-resource.bin
expect_fail 'Git tracked renamed payload' "$TEST_ROOT/checker-with-test-hash.sh" \
  --git-tracked "$TEST_ROOT/git-repo"

# A generic archive name must not bypass the Git-index policy. Nest a tar.gz
# inside the tracked ZIP so this also proves recursive archive inspection.
mkdir -p "$TEST_ROOT/tracked-tar-source/ordinary-folder"
print -rn -- 'runtime hidden in nested tar' \
  >"$TEST_ROOT/tracked-tar-source/ordinary-folder/qdc507_voice.ko"
/usr/bin/tar -czf "$TEST_ROOT/ordinary-data.tar.gz" \
  -C "$TEST_ROOT/tracked-tar-source" ordinary-folder
mkdir -p "$TEST_ROOT/tracked-zip-source/resources"
cp "$TEST_ROOT/ordinary-data.tar.gz" "$TEST_ROOT/tracked-zip-source/resources/ordinary-data.tar.gz"
/usr/bin/ditto -c -k --keepParent "$TEST_ROOT/tracked-zip-source/resources" \
  "$TEST_ROOT/git-repo/ordinary-assets.zip"
git -C "$TEST_ROOT/git-repo" add ordinary-assets.zip
expect_fail 'Git tracked ZIP containing nested tar runtime' "$CHECKER" \
  --git-tracked "$TEST_ROOT/git-repo"
expect_fail 'nested archive depth limit' /usr/bin/env \
  MACCELLULAR_RUNTIME_POLICY_MAX_ARCHIVE_DEPTH=1 "$CHECKER" \
  "$TEST_ROOT/git-repo/ordinary-assets.zip"
grep -Fq 'nested archive depth limit exceeded' "$TEST_ROOT/stderr" || {
  print -u2 -r -- 'FAIL: nested archive did not report the depth limit'
  exit 1
}

mkdir -p "$TEST_ROOT/budget-source/files"
print -rn -- one >"$TEST_ROOT/budget-source/files/one.txt"
print -rn -- two >"$TEST_ROOT/budget-source/files/two.txt"
/usr/bin/ditto -c -k --keepParent "$TEST_ROOT/budget-source/files" "$TEST_ROOT/budget.zip"
expect_fail 'archive expansion file-count limit' /usr/bin/env \
  MACCELLULAR_RUNTIME_POLICY_MAX_EXPANDED_FILES=1 "$CHECKER" "$TEST_ROOT/budget.zip"
expect_fail 'archive expansion byte limit' /usr/bin/env \
  MACCELLULAR_RUNTIME_POLICY_MAX_EXPANDED_BYTES=1 "$CHECKER" "$TEST_ROOT/budget.zip"
grep -Fq 'expanded byte limit' "$TEST_ROOT/stderr" || {
  print -u2 -r -- 'FAIL: archive did not report the expanded byte limit'
  exit 1
}
expect_pass 'current Git tracked files' "$CHECKER" --git-tracked "$ROOT"

for package_script in package-macos-arm64.sh package-macos-universal.sh; do
  grep -Fq 'check-release-runtime-policy.sh" --git-tracked' "$ROOT/scripts/$package_script"
  grep -Fq 'check-release-runtime-policy.sh" "${STAGE_DIR}"' "$ROOT/scripts/$package_script"
  grep -Fq 'check-release-runtime-policy.sh" "${ARCHIVE}"' "$ROOT/scripts/$package_script"
done
for dmg_script in build-dmg.sh build-dmg-universal.sh; do
  grep -Fq 'check-release-runtime-policy.sh" "${STAGE}"' "$ROOT/scripts/$dmg_script"
  grep -Fq 'check-release-runtime-policy.sh" "${DMG}"' "$ROOT/scripts/$dmg_script"
done
pass 'all release entry points enforce the policy'

print -r -- "release runtime policy tests passed: $PASSED"
