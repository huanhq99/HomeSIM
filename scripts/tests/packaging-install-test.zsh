#!/bin/zsh
set -euo pipefail

ROOT=${0:A:h:h:h}
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/maccellular-packaging-install-test.XXXXXX")
trap '/bin/rm -rf -- "$TEST_ROOT"' EXIT HUP INT TERM

typeset -i PASSED=0

pass() {
  print -r -- "PASS: $1"
  PASSED+=1
}

fail() {
  print -u2 -r -- "FAIL: $1"
  exit 1
}

print -r -- 'int main(void) { return 0; }' >"$TEST_ROOT/stub.c"
xcrun clang -arch "$(uname -m)" -mmacosx-version-min=13.0 \
  "$TEST_ROOT/stub.c" -o "$TEST_ROOT/stub-binary"

make_runtime_package() {
  local destination=$1
  local version=$2
  mkdir -p "$destination/bin" "$destination/lib" "$destination/licenses"
  cp "$TEST_ROOT/stub-binary" "$destination/bin/djonehub-macos"
  cp "$TEST_ROOT/stub-binary" "$destination/lib/libusb-1.0.0.dylib"
  cp "$ROOT/packaging/djonehub" "$destination/djonehub"
  cp "$ROOT/packaging/install" "$destination/install"
  cp "$ROOT/LICENSE" "$destination/LICENSE"
  cp "$ROOT/LEGAL_AND_RESPONSIBLE_USE.md" "$destination/合法与负责任使用.md"
  cp "$ROOT/PRIVACY.md" "$destination/数据与隐私说明.md"
  cp "$ROOT/packaging/THIRD_PARTY_NOTICES.md" "$destination/THIRD_PARTY_NOTICES.md"
  cp "$ROOT/licenses/MaVo-LICENSE" "$destination/licenses/MaVo-LICENSE"
  print -r -- 'libusb test license fixture' >"$destination/licenses/libusb-COPYING"
  print -r -- "$version" >"$destination/VERSION"
  sh "$ROOT/scripts/copy-third-party-licenses.sh" "$destination"
  chmod 755 \
    "$destination/djonehub" \
    "$destination/install" \
    "$destination/bin/djonehub-macos" \
    "$destination/lib/libusb-1.0.0.dylib"
  codesign --force --sign - "$destination/bin/djonehub-macos" >/dev/null
  codesign --force --sign - "$destination/lib/libusb-1.0.0.dylib" >/dev/null
}

OLD_PACKAGE="$TEST_ROOT/package-old"
NEW_PACKAGE="$TEST_ROOT/package-new"
make_runtime_package "$OLD_PACKAGE" v1.0.0-rc.1
make_runtime_package "$NEW_PACKAGE" v1.0.0-rc.4

while IFS= read -r source_license; do
  relative=${source_license#"$ROOT"/}
  cmp -s "$source_license" "$OLD_PACKAGE/$relative" || fail "license copy mismatch: $relative"
done < <(find "$ROOT/third_party" "$ROOT/licenses" -type f \( -name LICENSE -o -name LICENSE.txt -o -name '*-LICENSE' \) -print | sort)
pass 'all repository third-party license texts are copied byte-for-byte'

CLI_ROOT="$TEST_ROOT/cli"
CLI_INSTALL="$CLI_ROOT/libexec/maccellular"
CLI_COMMAND="$CLI_ROOT/bin/maccellular"
mkdir -p "$CLI_ROOT"
DJONEHUB_INSTALL_NO_SUDO=1 \
DJONEHUB_INSTALL_DIR="$CLI_INSTALL" \
DJONEHUB_COMMAND_PATH="$CLI_COMMAND" \
  "$OLD_PACKAGE/install" >/dev/null
[[ "$(<"$CLI_INSTALL/VERSION")" == 'v1.0.0-rc.1' ]] || fail 'initial CLI version'
[[ "$(readlink "$CLI_COMMAND")" == "$CLI_INSTALL/djonehub" ]] || fail 'initial CLI command link'
pass 'CLI installer commits a validated staged package'

cat >"$CLI_INSTALL/djonehub" <<'EOF'
#!/bin/sh
set -eu
if [ "${1:-}" = stop ] && [ "${MACCELLULAR_TEST_DELETE_STAGED_COMMAND:-0}" = 1 ]; then
  case "${MACCELLULAR_TEST_COMMAND_PARENT:-}" in
    "${MACCELLULAR_TEST_ROOT}"/*)
      find "${MACCELLULAR_TEST_COMMAND_PARENT}" -maxdepth 1 -name '.maccellular.command.installing.*' -delete
      ;;
    *) exit 70 ;;
  esac
fi
EOF
chmod 755 "$CLI_INSTALL/djonehub"
print -r -- 'old install marker' >"$CLI_INSTALL/rollback-marker"

if MACCELLULAR_TEST_ROOT="$TEST_ROOT" \
  MACCELLULAR_TEST_COMMAND_PARENT="$CLI_ROOT/bin" \
  MACCELLULAR_TEST_DELETE_STAGED_COMMAND=1 \
  DJONEHUB_INSTALL_NO_SUDO=1 \
  DJONEHUB_INSTALL_DIR="$CLI_INSTALL" \
  DJONEHUB_COMMAND_PATH="$CLI_COMMAND" \
    "$NEW_PACKAGE/install" >"$TEST_ROOT/rollback.stdout" 2>"$TEST_ROOT/rollback.stderr"; then
  fail 'fault-injected CLI upgrade unexpectedly succeeded'
fi
[[ "$(<"$CLI_INSTALL/VERSION")" == 'v1.0.0-rc.1' ]] || fail 'CLI rollback version'
[[ -f "$CLI_INSTALL/rollback-marker" ]] || fail 'CLI rollback marker'
[[ "$(readlink "$CLI_COMMAND")" == "$CLI_INSTALL/djonehub" ]] || fail 'CLI rollback command link'
if find "$CLI_ROOT" -name '.maccellular.*ing.*' -o -name '.maccellular.previous.*' | grep -q .; then
  fail 'CLI rollback left transaction debris'
fi
pass 'CLI installer restores the old directory and command after commit failure'

ln -s /etc/hosts "$NEW_PACKAGE/escape-outside-package"
if DJONEHUB_INSTALL_NO_SUDO=1 \
  DJONEHUB_INSTALL_DIR="$CLI_INSTALL" \
  DJONEHUB_COMMAND_PATH="$CLI_COMMAND" \
    "$NEW_PACKAGE/install" >"$TEST_ROOT/symlink.stdout" 2>"$TEST_ROOT/symlink.stderr"; then
  fail 'CLI installer accepted an escaping package symlink'
fi
grep -Fq '发行包包含符号链接' "$TEST_ROOT/symlink.stderr" || fail 'CLI symlink rejection message'
[[ "$(<"$CLI_INSTALL/VERSION")" == 'v1.0.0-rc.1' ]] || fail 'CLI symlink rejection preserved old version'
pass 'CLI installer rejects package symlinks before replacement'

DMG_STAGE="$TEST_ROOT/dmg-stage"
DMG_HOME="$TEST_ROOT/dmg-home"
mkdir -p "$DMG_STAGE" "$DMG_HOME" "$DMG_STAGE/MacCellular.app/Contents/MacOS"
/usr/bin/ditto --norsrc --noextattr --noqtn --noacl "$OLD_PACKAGE" "$DMG_STAGE/maccellular"
cp "$TEST_ROOT/stub-binary" "$DMG_STAGE/MacCellular.app/Contents/MacOS/DJOneHubNotifier"
cp "$ROOT/macos/DJOneHubNotifier/Info.plist" "$DMG_STAGE/MacCellular.app/Contents/Info.plist"
cp "$ROOT/scripts/dmg/安装 MacCellular.command" "$DMG_STAGE/安装 MacCellular.command"
chmod 755 \
  "$DMG_STAGE/MacCellular.app/Contents/MacOS/DJOneHubNotifier" \
  "$DMG_STAGE/安装 MacCellular.command"
codesign --force --deep --sign - "$DMG_STAGE/MacCellular.app" >/dev/null
if ! HOME="$DMG_HOME" MACCELLULAR_4G_PHONE_INSTALL_VALIDATE_ONLY=1 \
  "$DMG_STAGE/安装 MacCellular.command" >"$TEST_ROOT/dmg.stdout" 2>"$TEST_ROOT/dmg.stderr"; then
  sed -n '1,160p' "$TEST_ROOT/dmg.stdout" >&2
  sed -n '1,160p' "$TEST_ROOT/dmg.stderr" >&2
  fail 'DMG validate-only command'
fi
grep -Fq 'LaunchAgent plist 预检通过' "$TEST_ROOT/dmg.stdout" || fail 'DMG validation output'
[[ ! -e "$DMG_HOME/Library/Application Support/MacCellular/runtime" ]] || fail 'DMG preflight changed runtime'
[[ ! -e "$DMG_HOME/Library/Application Support/MacCellular/notifier" ]] || fail 'DMG preflight changed notifier'
[[ ! -e "$DMG_HOME/Library/LaunchAgents/io.maccellular.backend.plist" ]] || fail 'DMG preflight changed backend plist'
[[ ! -e "$DMG_HOME/Library/LaunchAgents/io.maccellular.app.plist" ]] || fail 'DMG preflight changed app plist'
[[ "$(grep -c 'Add :Umask integer 63' "$ROOT/scripts/dmg/安装 MacCellular.command")" -eq 2 ]] || fail 'LaunchAgent Umask declarations'
pass 'DMG validate-only stages and validates both LaunchAgent plists without replacing the installation'

OLD_DEST="$DMG_HOME/Library/Application Support/MacCellular"
OLD_AGENTS="$DMG_HOME/Library/LaunchAgents"
mkdir -p "$OLD_DEST/runtime" "$OLD_DEST/notifier" "$OLD_AGENTS"
print -r -- 'old runtime' >"$OLD_DEST/runtime/rollback-marker"
print -r -- 'old notifier' >"$OLD_DEST/notifier/rollback-marker"
print -r -- 'old backend plist' >"$OLD_AGENTS/io.maccellular.backend.plist"
print -r -- 'old app plist' >"$OLD_AGENTS/io.maccellular.app.plist"

LAUNCHCTL_STUB="$TEST_ROOT/launchctl-stub"
cat >"$LAUNCHCTL_STUB" <<'EOF'
#!/bin/sh
set -eu
case "${1:-}" in
  print) exit 1 ;;
  bootout) exit 0 ;;
  bootstrap)
    count=0
    if [ -f "${MACCELLULAR_TEST_LAUNCHCTL_STATE}" ]; then count=$(cat "${MACCELLULAR_TEST_LAUNCHCTL_STATE}"); fi
    count=$((count + 1))
    printf '%s\n' "$count" >"${MACCELLULAR_TEST_LAUNCHCTL_STATE}"
    [ "$count" -lt 2 ]
    ;;
  *) exit 64 ;;
esac
EOF
chmod 755 "$LAUNCHCTL_STUB"
ROLLBACK_INSTALLER="$DMG_STAGE/安装 MacCellular rollback test.command"
sed "s#/bin/launchctl#$LAUNCHCTL_STUB#g" \
  "$DMG_STAGE/安装 MacCellular.command" >"$ROLLBACK_INSTALLER"
chmod 755 "$ROLLBACK_INSTALLER"
if HOME="$DMG_HOME" \
  MACCELLULAR_TEST_LAUNCHCTL_STATE="$TEST_ROOT/launchctl-state" \
    "$ROLLBACK_INSTALLER" >"$TEST_ROOT/dmg-rollback.stdout" 2>"$TEST_ROOT/dmg-rollback.stderr"; then
  fail 'fault-injected DMG upgrade unexpectedly succeeded'
fi
[[ "$(<"$OLD_DEST/runtime/rollback-marker")" == 'old runtime' ]] || fail 'DMG runtime rollback'
[[ "$(<"$OLD_DEST/notifier/rollback-marker")" == 'old notifier' ]] || fail 'DMG notifier rollback'
[[ "$(<"$OLD_AGENTS/io.maccellular.backend.plist")" == 'old backend plist' ]] || fail 'DMG backend plist rollback'
[[ "$(<"$OLD_AGENTS/io.maccellular.app.plist")" == 'old app plist' ]] || fail 'DMG app plist rollback'
if find "$OLD_DEST" "$OLD_AGENTS" \( -name '*.installing.*' -o -name '*.previous.*' \) -print | grep -q .; then
  fail 'DMG rollback left transaction debris'
fi
pass 'DMG installer restores runtime, App and both plists when App bootstrap fails'

zsh "$ROOT/scripts/tests/setup-assistant-test.zsh" >/dev/null
pass 'mobile-access setup assistant covers SMS and direct voice without exposing secrets in argv'

print -r -- "packaging installer tests passed: $PASSED"
