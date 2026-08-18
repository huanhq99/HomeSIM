#!/bin/zsh
set -eu
umask 077

if [[ "$HOME" != /* || "$HOME" == / ]]; then
  print -u2 -- "卸载失败：HOME 不是安全的绝对用户目录。"
  exit 64
fi

UID_NUM=$(/usr/bin/id -u)
DEST="$HOME/Library/Application Support/MacCellular"
AGENTS="$HOME/Library/LaunchAgents"

echo "正在卸载 MacCellular..."
/bin/launchctl bootout "gui/$UID_NUM/io.maccellular.app" >/dev/null 2>&1 || true
/bin/launchctl bootout "gui/$UID_NUM/io.maccellular.backend" >/dev/null 2>&1 || true
/bin/rm -f -- \
  "$AGENTS/io.maccellular.backend.plist" \
  "$AGENTS/io.maccellular.app.plist"
/bin/rm -rf -- "$DEST/runtime" "$DEST/notifier"
if [[ -d "$DEST" ]]; then
  /usr/bin/find "$DEST" -maxdepth 1 -type d \
    \( -name '.runtime.installing.*' -o -name '.runtime.previous.*' \
       -o -name '.notifier.installing.*' -o -name '.notifier.previous.*' \) \
    -exec /bin/rm -rf -- {} +
fi
if [[ -d "$AGENTS" ]]; then
  /usr/bin/find "$AGENTS" -maxdepth 1 -type f \
    \( -name '.io.maccellular.backend.installing.*.plist' \
       -o -name '.io.maccellular.backend.previous.*.plist' \
       -o -name '.io.maccellular.app.installing.*.plist' \
       -o -name '.io.maccellular.app.previous.*.plist' \) \
    -exec /bin/rm -f -- {} +
fi

echo "程序、LaunchAgent 与 App 已卸载。"
echo "短信、录音、设置、模块回滚备份及本机外置语音运行时均已保留。"
