#!/bin/zsh
set -eu
umask 077

SCRIPT_DIR=${0:A:h}
cd "$SCRIPT_DIR"

if [[ "$HOME" != /* || "$HOME" == / ]]; then
  print -u2 -- "安装失败：HOME 不是安全的绝对用户目录。"
  exit 64
fi

DEST="$HOME/Library/Application Support/MacCellular"
RUNTIME="$DEST/runtime"
NOTIFIER="$DEST/notifier"
APP_PATH="$NOTIFIER/MacCellular.app"
SOURCE_RUNTIME="$SCRIPT_DIR/maccellular"
SOURCE_APP="$SCRIPT_DIR/MacCellular.app"
LOG_DIR="$HOME/Library/Logs/MacCellular"
AGENTS="$HOME/Library/LaunchAgents"
UID_NUM=$(/usr/bin/id -u)
BACKEND_LABEL="io.maccellular.backend"
APP_LABEL="io.maccellular.app"
BACKEND_PLIST="$AGENTS/$BACKEND_LABEL.plist"
APP_PLIST="$AGENTS/$APP_LABEL.plist"
TOKEN=$$

RUNTIME_STAGE="$DEST/.runtime.installing.$TOKEN"
NOTIFIER_STAGE="$DEST/.notifier.installing.$TOKEN"
RUNTIME_BACKUP="$DEST/.runtime.previous.$TOKEN"
NOTIFIER_BACKUP="$DEST/.notifier.previous.$TOKEN"
BACKEND_PLIST_STAGE="$AGENTS/.$BACKEND_LABEL.installing.$TOKEN.plist"
APP_PLIST_STAGE="$AGENTS/.$APP_LABEL.installing.$TOKEN.plist"
BACKEND_PLIST_BACKUP="$AGENTS/.$BACKEND_LABEL.previous.$TOKEN.plist"
APP_PLIST_BACKUP="$AGENTS/.$APP_LABEL.previous.$TOKEN.plist"

SUCCESS=0
TRANSACTION_STARTED=0
SERVICES_STOPPED=0
OLD_BACKEND_LOADED=0
OLD_APP_LOADED=0
RUNTIME_BACKED_UP=0
NOTIFIER_BACKED_UP=0
BACKEND_PLIST_BACKED_UP=0
APP_PLIST_BACKED_UP=0
RUNTIME_INSTALLED=0
NOTIFIER_INSTALLED=0
BACKEND_PLIST_INSTALLED=0
APP_PLIST_INSTALLED=0

path_exists() {
  [[ -e "$1" || -L "$1" ]]
}

remove_tree() {
  case "$1" in
    "$RUNTIME"|"$NOTIFIER"|"$RUNTIME_STAGE"|"$NOTIFIER_STAGE"|"$RUNTIME_BACKUP"|"$NOTIFIER_BACKUP")
      /bin/rm -rf -- "$1"
      ;;
    *)
      print -u2 -- "拒绝删除未验证路径：$1"
      return 70
      ;;
  esac
}

remove_plist() {
  case "$1" in
    "$BACKEND_PLIST"|"$APP_PLIST"|"$BACKEND_PLIST_STAGE"|"$APP_PLIST_STAGE"|"$BACKEND_PLIST_BACKUP"|"$APP_PLIST_BACKUP")
      /bin/rm -f -- "$1"
      ;;
    *)
      print -u2 -- "拒绝删除未验证 plist：$1"
      return 70
      ;;
  esac
}

restore_entry() {
  local backup=$1
  local destination=$2
  local description=$3
  if path_exists "$backup"; then
    /bin/mv -- "$backup" "$destination" || \
      print -u2 -- "警告：未能恢复原${description}，备份仍在 $backup"
  fi
}

cleanup() {
  local exit_status=$?
  trap - EXIT HUP INT TERM
  set +e

  if [[ "$TRANSACTION_STARTED" -eq 0 ]]; then
    exit "$exit_status"
  fi

  if [[ "$SUCCESS" -eq 1 ]]; then
    remove_tree "$RUNTIME_STAGE" >/dev/null 2>&1
    remove_tree "$NOTIFIER_STAGE" >/dev/null 2>&1
    remove_tree "$RUNTIME_BACKUP" >/dev/null 2>&1
    remove_tree "$NOTIFIER_BACKUP" >/dev/null 2>&1
    remove_plist "$BACKEND_PLIST_STAGE" >/dev/null 2>&1
    remove_plist "$APP_PLIST_STAGE" >/dev/null 2>&1
    remove_plist "$BACKEND_PLIST_BACKUP" >/dev/null 2>&1
    remove_plist "$APP_PLIST_BACKUP" >/dev/null 2>&1
    exit "$exit_status"
  fi

  if [[ "$SERVICES_STOPPED" -eq 1 ]]; then
    /bin/launchctl bootout "gui/$UID_NUM/$APP_LABEL" >/dev/null 2>&1
    /bin/launchctl bootout "gui/$UID_NUM/$BACKEND_LABEL" >/dev/null 2>&1
  fi
  if [[ "$APP_PLIST_INSTALLED" -eq 1 ]]; then remove_plist "$APP_PLIST" >/dev/null 2>&1; fi
  if [[ "$BACKEND_PLIST_INSTALLED" -eq 1 ]]; then remove_plist "$BACKEND_PLIST" >/dev/null 2>&1; fi
  if [[ "$NOTIFIER_INSTALLED" -eq 1 ]]; then remove_tree "$NOTIFIER" >/dev/null 2>&1; fi
  if [[ "$RUNTIME_INSTALLED" -eq 1 ]]; then remove_tree "$RUNTIME" >/dev/null 2>&1; fi

  if [[ "$RUNTIME_BACKED_UP" -eq 1 ]]; then restore_entry "$RUNTIME_BACKUP" "$RUNTIME" "运行目录"; fi
  if [[ "$NOTIFIER_BACKED_UP" -eq 1 ]]; then restore_entry "$NOTIFIER_BACKUP" "$NOTIFIER" "App 目录"; fi
  if [[ "$BACKEND_PLIST_BACKED_UP" -eq 1 ]]; then restore_entry "$BACKEND_PLIST_BACKUP" "$BACKEND_PLIST" "后端 LaunchAgent"; fi
  if [[ "$APP_PLIST_BACKED_UP" -eq 1 ]]; then restore_entry "$APP_PLIST_BACKUP" "$APP_PLIST" "App LaunchAgent"; fi

  if [[ "$SERVICES_STOPPED" -eq 1 && "$OLD_BACKEND_LOADED" -eq 1 && -f "$BACKEND_PLIST" ]]; then
    /bin/launchctl bootstrap "gui/$UID_NUM" "$BACKEND_PLIST" >/dev/null 2>&1 || \
      print -u2 -- "警告：原后端文件已恢复，但未能重新载入。"
  fi
  if [[ "$SERVICES_STOPPED" -eq 1 && "$OLD_APP_LOADED" -eq 1 && -f "$APP_PLIST" ]]; then
    /bin/launchctl bootstrap "gui/$UID_NUM" "$APP_PLIST" >/dev/null 2>&1 || \
      print -u2 -- "警告：原 App 文件已恢复，但未能重新载入。"
  fi

  remove_tree "$RUNTIME_STAGE" >/dev/null 2>&1
  remove_tree "$NOTIFIER_STAGE" >/dev/null 2>&1
  remove_plist "$BACKEND_PLIST_STAGE" >/dev/null 2>&1
  remove_plist "$APP_PLIST_STAGE" >/dev/null 2>&1
  print -u2 -- "安装失败；已尝试恢复原版本。"
  exit "$exit_status"
}

trap cleanup EXIT
trap 'exit 130' HUP INT TERM

if [[ ! -d "$SOURCE_RUNTIME" || -L "$SOURCE_RUNTIME" || ! -d "$SOURCE_APP" || -L "$SOURCE_APP" ]]; then
  print -u2 -- "安装失败：DMG 中的运行目录或 App 不是独立目录。"
  exit 65
fi

for required in \
  djonehub \
  bin/djonehub-macos \
  lib/libusb-1.0.0.dylib \
  install \
  VERSION \
  LICENSE \
  合法与负责任使用.md \
  数据与隐私说明.md \
  THIRD_PARTY_NOTICES.md \
  licenses/MaVo-LICENSE \
  licenses/libusb-COPYING \
  third_party/euicc-go/LICENSE \
  third_party/euicc-go/bertlv/LICENSE \
  third_party/uicc-go/LICENSE \
  third_party/quectel-qmi-go/LICENSE \
  third_party/strftime/LICENSE \
  third_party/pkg-errors/LICENSE \
  third_party/x-sys/LICENSE \
  third_party/x-text/LICENSE \
  third_party/multierr/LICENSE.txt
do
  if [[ ! -f "$SOURCE_RUNTIME/$required" ]]; then
    print -u2 -- "安装失败：DMG 缺少 maccellular/$required。"
    exit 65
  fi
done
if [[ ! -f "$SOURCE_APP/Contents/Info.plist" || ! -x "$SOURCE_APP/Contents/MacOS/DJOneHubNotifier" ]]; then
  print -u2 -- "安装失败：DMG 中的 MacCellular.app 不完整。"
  exit 65
fi

PACKAGE_VERSION=$(/bin/cat "$SOURCE_RUNTIME/VERSION")
case "$PACKAGE_VERSION" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *)
    print -u2 -- "安装失败：DMG 中的 VERSION 无效：$PACKAGE_VERSION"
    exit 65
    ;;
esac
PACKAGE_APP_VERSION=${PACKAGE_VERSION#v}
PACKAGE_APP_VERSION=${PACKAGE_APP_VERSION%%-*}
SOURCE_APP_VERSION=$(/usr/bin/plutil -extract CFBundleShortVersionString raw "$SOURCE_APP/Contents/Info.plist")
if [[ "$SOURCE_APP_VERSION" != "$PACKAGE_APP_VERSION" ]]; then
  print -u2 -- "安装失败：运行包版本 $PACKAGE_VERSION 与 App 版本 $SOURCE_APP_VERSION 不一致。"
  exit 65
fi

echo "MacCellular $PACKAGE_VERSION 一键安装"
echo "安装目录：$DEST"
echo

/bin/mkdir -p "$DEST" "$AGENTS" "$LOG_DIR"
/bin/chmod 700 "$DEST" "$LOG_DIR"
TRANSACTION_STARTED=1
remove_tree "$RUNTIME_STAGE" >/dev/null 2>&1 || true
remove_tree "$NOTIFIER_STAGE" >/dev/null 2>&1 || true
remove_tree "$RUNTIME_BACKUP" >/dev/null 2>&1 || true
remove_tree "$NOTIFIER_BACKUP" >/dev/null 2>&1 || true
remove_plist "$BACKEND_PLIST_STAGE" >/dev/null 2>&1 || true
remove_plist "$APP_PLIST_STAGE" >/dev/null 2>&1 || true
remove_plist "$BACKEND_PLIST_BACKUP" >/dev/null 2>&1 || true
remove_plist "$APP_PLIST_BACKUP" >/dev/null 2>&1 || true

# Stage complete replacements beside their final directories, so every commit is a same-filesystem rename.
/usr/bin/ditto --norsrc --noextattr --noqtn --noacl "$SOURCE_RUNTIME" "$RUNTIME_STAGE"
/bin/mkdir -m 700 "$NOTIFIER_STAGE"
/usr/bin/ditto --norsrc --noextattr --noqtn --noacl "$SOURCE_APP" "$NOTIFIER_STAGE/MacCellular.app"
if /usr/bin/find "$RUNTIME_STAGE" "$NOTIFIER_STAGE" -type l -print -quit | /usr/bin/grep -q .; then
  print -u2 -- "安装失败：DMG 包含符号链接，已拒绝安装。"
  exit 65
fi
/bin/chmod 755 \
  "$RUNTIME_STAGE/djonehub" \
  "$RUNTIME_STAGE/install" \
  "$RUNTIME_STAGE/bin/djonehub-macos" \
  "$RUNTIME_STAGE/lib/libusb-1.0.0.dylib" \
  "$NOTIFIER_STAGE/MacCellular.app/Contents/MacOS/DJOneHubNotifier"

if /usr/bin/find "$RUNTIME_STAGE" "$NOTIFIER_STAGE" \
  \( -iname '*.ko' -o -iname '*.armv7' -o -name ModuleVoice \) -print | /usr/bin/grep -q .; then
  print -u2 -- "安装失败：DMG 包含不应分发的模块侧语音运行时。"
  exit 65
fi

/usr/bin/codesign --verify --strict "$RUNTIME_STAGE/bin/djonehub-macos"
/usr/bin/codesign --verify --strict "$RUNTIME_STAGE/lib/libusb-1.0.0.dylib"
/usr/bin/codesign --verify --deep --strict "$NOTIFIER_STAGE/MacCellular.app"
/usr/bin/plutil -lint "$NOTIFIER_STAGE/MacCellular.app/Contents/Info.plist" >/dev/null
if [[ "$(/usr/bin/plutil -extract CFBundleDisplayName raw "$NOTIFIER_STAGE/MacCellular.app/Contents/Info.plist")" != "MacCellular" ]]; then
  print -u2 -- "安装失败：App 显示名称与 MacCellular 不一致。"
  exit 65
fi
if [[ "$(/usr/bin/plutil -extract CFBundleShortVersionString raw "$NOTIFIER_STAGE/MacCellular.app/Contents/Info.plist")" != "$PACKAGE_APP_VERSION" ]]; then
  print -u2 -- "安装失败：暂存 App 版本与运行包版本不一致。"
  exit 65
fi
CURRENT_ARCH=$(/usr/bin/uname -m)
for binary in \
  "$RUNTIME_STAGE/bin/djonehub-macos" \
  "$RUNTIME_STAGE/lib/libusb-1.0.0.dylib" \
  "$NOTIFIER_STAGE/MacCellular.app/Contents/MacOS/DJOneHubNotifier"
do
  /usr/bin/lipo "$binary" -verify_arch "$CURRENT_ARCH"
done

/usr/bin/plutil -create xml1 "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :Label string $BACKEND_LABEL" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments array" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:0 string $RUNTIME/bin/djonehub-macos" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:1 string -listen" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:2 string 127.0.0.1:7576" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :RunAtLoad bool true" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :KeepAlive bool true" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ThrottleInterval integer 5" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProcessType string Background" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :Umask integer 63" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :StandardOutPath string $LOG_DIR/launchd.log" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :StandardErrorPath string $LOG_DIR/launchd.log" "$BACKEND_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :WorkingDirectory string $RUNTIME" "$BACKEND_PLIST_STAGE"

/usr/bin/plutil -create xml1 "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :Label string $APP_LABEL" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments array" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:0 string $APP_PATH/Contents/MacOS/DJOneHubNotifier" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :RunAtLoad bool true" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :KeepAlive bool true" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ThrottleInterval integer 10" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :ProcessType string Interactive" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :Umask integer 63" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :StandardOutPath string $LOG_DIR/app.log" "$APP_PLIST_STAGE"
/usr/libexec/PlistBuddy -c "Add :StandardErrorPath string $LOG_DIR/app.log" "$APP_PLIST_STAGE"
/usr/bin/plutil -lint "$BACKEND_PLIST_STAGE" "$APP_PLIST_STAGE" >/dev/null
/bin/chmod 600 "$BACKEND_PLIST_STAGE" "$APP_PLIST_STAGE"

if [[ "${MACCELLULAR_4G_PHONE_INSTALL_VALIDATE_ONLY:-0}" == "1" ]]; then
  SUCCESS=1
  echo "安装包、代码签名、架构、版本、许可证与 LaunchAgent plist 预检通过；未修改现有安装。"
  exit 0
fi

if /bin/launchctl print "gui/$UID_NUM/$BACKEND_LABEL" >/dev/null 2>&1; then OLD_BACKEND_LOADED=1; fi
if /bin/launchctl print "gui/$UID_NUM/$APP_LABEL" >/dev/null 2>&1; then OLD_APP_LOADED=1; fi
SERVICES_STOPPED=1
/bin/launchctl bootout "gui/$UID_NUM/$APP_LABEL" >/dev/null 2>&1 || true
/bin/launchctl bootout "gui/$UID_NUM/$BACKEND_LABEL" >/dev/null 2>&1 || true

if path_exists "$RUNTIME"; then
  RUNTIME_BACKED_UP=1
  /bin/mv -- "$RUNTIME" "$RUNTIME_BACKUP"
fi
if path_exists "$NOTIFIER"; then
  NOTIFIER_BACKED_UP=1
  /bin/mv -- "$NOTIFIER" "$NOTIFIER_BACKUP"
fi
if path_exists "$BACKEND_PLIST"; then
  BACKEND_PLIST_BACKED_UP=1
  /bin/mv -- "$BACKEND_PLIST" "$BACKEND_PLIST_BACKUP"
fi
if path_exists "$APP_PLIST"; then
  APP_PLIST_BACKED_UP=1
  /bin/mv -- "$APP_PLIST" "$APP_PLIST_BACKUP"
fi

/bin/mv -- "$RUNTIME_STAGE" "$RUNTIME"
RUNTIME_INSTALLED=1
/bin/mv -- "$NOTIFIER_STAGE" "$NOTIFIER"
NOTIFIER_INSTALLED=1
/bin/mv -- "$BACKEND_PLIST_STAGE" "$BACKEND_PLIST"
BACKEND_PLIST_INSTALLED=1
/bin/mv -- "$APP_PLIST_STAGE" "$APP_PLIST"
APP_PLIST_INSTALLED=1

/bin/launchctl bootstrap "gui/$UID_NUM" "$BACKEND_PLIST"
/bin/launchctl bootstrap "gui/$UID_NUM" "$APP_PLIST"

printf '正在启动 MacCellular'
READY=0
for i in {1..50}; do
  if /bin/launchctl print "gui/$UID_NUM/$BACKEND_LABEL" 2>/dev/null | /usr/bin/grep -q 'state = running' && \
    /usr/bin/curl -fsS --max-time 1 http://127.0.0.1:7576/api/platform 2>/dev/null | \
      /usr/bin/grep -Fq "\"version\":\"$PACKAGE_APP_VERSION\""; then
    READY=1
    break
  fi
  /bin/sleep 0.2
done
if [[ "$READY" -ne 1 ]]; then
  print -u2 -- "，后端未能通过运行状态与 HTTP 就绪校验。"
  print -u2 -- "请查看 $LOG_DIR/launchd.log"
  exit 1
fi

SUCCESS=1
echo "，安装完成。"
echo "主服务与 MacCellular App 已注册为登录自启。"
/usr/bin/open "$APP_PATH" >/dev/null 2>&1 || /usr/bin/open http://127.0.0.1:7576 >/dev/null 2>&1 || true
