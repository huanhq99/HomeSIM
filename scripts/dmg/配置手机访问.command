#!/bin/zsh
set -eu
umask 077

SCRIPT_DIR=${0:A:h}
if [[ -x "$SCRIPT_DIR/maccellular/setup/public-web-mac/install-local.sh" ]]; then
  PACKAGE_ROOT="$SCRIPT_DIR/maccellular"
  INSTALLER="$PACKAGE_ROOT/setup/public-web-mac/install-local.sh"
  BACKEND="$PACKAGE_ROOT/bin/djonehub-macos"
  HELPER="$SCRIPT_DIR/MacCellular.app/Contents/MacOS/DJOneHubNotifier"
  VOICE_PREPARER="$PACKAGE_ROOT/setup/prepare-module-voice.sh"
else
  REPO_ROOT=${SCRIPT_DIR:h:h}
  INSTALLER="$REPO_ROOT/deploy/public-web-mac/install-local.sh"
  BACKEND="$REPO_ROOT/dist/djonehub-macos-arm64"
  HELPER="$REPO_ROOT/macos/DJOneHubNotifier/.build-output/DJOneHubNotifier"
  VOICE_PREPARER="$REPO_ROOT/scripts/prepare-module-voice.sh"
fi

if [[ ! -x "$INSTALLER" || ! -x "$BACKEND" ]]; then
  print -u2 -- "未找到完整的 MacCellular 安装文件。请从完整 DMG 运行本向导。"
  exit 66
fi

CLOUDFLARED=""
for candidate in \
  "$SCRIPT_DIR/maccellular/bin/cloudflared" \
  /opt/homebrew/bin/cloudflared \
  /usr/local/bin/cloudflared \
  /usr/local/sbin/cloudflared
do
  if [[ -x "$candidate" ]]; then
    CLOUDFLARED=$candidate
    break
  fi
done
if [[ -z "$CLOUDFLARED" ]]; then
  print -- "尚未安装 cloudflared。"
  if [[ -x /opt/homebrew/bin/brew || -x /usr/local/bin/brew ]]; then
    print -n -- "现在通过 Homebrew 安装 cloudflared？[y/N] "
    read -r install_cloudflared
    if [[ "$install_cloudflared" == [yY] ]]; then
      BREW=/opt/homebrew/bin/brew
      [[ -x "$BREW" ]] || BREW=/usr/local/bin/brew
      "$BREW" install cloudflared
      CLOUDFLARED=${BREW:h}/cloudflared
    fi
  fi
fi
if [[ -z "$CLOUDFLARED" || ! -x "$CLOUDFLARED" ]]; then
  print -u2 -- "请先安装 cloudflared，再重新打开本向导：https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/"
  exit 69
fi

print -- ""
print -- "MacCellular 手机访问配置"
print -- "1) 短信"
print -- "2) 电话与短信（推荐，需已配置 TURN）"
print -n -- "请选择 [2]: "
read -r profile_choice
profile_choice=${profile_choice:-2}
case "$profile_choice" in
  1) PROFILE=sms ;;
  2) PROFILE=direct-voice ;;
  *) print -u2 -- "请输入 1 或 2。"; exit 64 ;;
esac

prompt() {
  local variable_name=$1
  local label=$2
  local value=""
  while [[ -z "$value" ]]; do
    print -n -- "$label: "
    read -r value
  done
  typeset -g "$variable_name=$value"
}

prompt PUBLIC_HOST "手机访问域名（例如 phone.example.com）"
prompt TEAM_DOMAIN "Cloudflare Access team domain"
prompt AUDIENCE "Cloudflare Access AUD"
prompt EMAIL "允许登录的邮箱"

TEMP_DIR=$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/maccellular-setup.XXXXXX")
cleanup() {
  /bin/rm -rf -- "$TEMP_DIR"
}
trap cleanup EXIT HUP INT TERM

TOKEN_FILE="$TEMP_DIR/cloudflared-token"
print -n -- "Cloudflare Tunnel token（输入不会显示）: "
read -rs TOKEN
print -- ""
[[ -n "$TOKEN" ]] || { print -u2 -- "Tunnel token 不能为空。"; exit 64; }
print -r -- "$TOKEN" > "$TOKEN_FILE"
chmod 600 "$TOKEN_FILE"
unset TOKEN

ARGS=(
  --profile "$PROFILE"
  --public-host "$PUBLIC_HOST"
  --team-domain "$TEAM_DOMAIN"
  --aud "$AUDIENCE"
  --email "$EMAIL"
  --binary "$BACKEND"
  --cloudflared "$CLOUDFLARED"
  --token-file "$TOKEN_FILE"
  --non-interactive
)

if [[ "$PROFILE" == direct-voice ]]; then
  [[ -x "$HELPER" ]] || {
    print -u2 -- "DMG 缺少电话音频 helper，请重新下载完整安装包。"
    exit 66
  }
  prompt TURN_HOST "TURN 域名（例如 turn.example.com）"
  if ! "$VOICE_PREPARER" check >/dev/null 2>&1; then
    print -- "当前 Mac 还没有 QDC507 语音运行文件。"
    print -n -- "现在从原始 MaVo 项目下载到本机缓存？[Y/n] "
    read -r prepare_voice
    prepare_voice=${prepare_voice:-Y}
    if [[ "$prepare_voice" != [yY] ]]; then
      print -u2 -- "电话模式需要先准备 QDC507 语音运行文件。"
      exit 69
    fi
    "$VOICE_PREPARER" install
  fi
  TURN_FILE="$TEMP_DIR/turn-secret"
  print -n -- "TURN shared secret（输入不会显示）: "
  read -rs TURN_SECRET
  print -- ""
  [[ -n "$TURN_SECRET" ]] || { print -u2 -- "TURN shared secret 不能为空。"; exit 64; }
  print -r -- "$TURN_SECRET" > "$TURN_FILE"
  chmod 600 "$TURN_FILE"
  unset TURN_SECRET

  VAPID_FILE="$TEMP_DIR/vapid-private-key"
  /usr/bin/python3 - <<'PY' > "$VAPID_FILE"
import base64
import secrets

order = int("FFFFFFFF00000000FFFFFFFFFFFFFFFFBCE6FAADA7179E84F3B9CAC2FC632551", 16)
value = secrets.randbelow(order - 1) + 1
print(base64.urlsafe_b64encode(value.to_bytes(32, "big")).rstrip(b"=").decode("ascii"))
PY
  chmod 600 "$VAPID_FILE"
  ARGS+=(
    --turn-host "$TURN_HOST"
    --helper-binary "$HELPER"
    --turn-secret-file "$TURN_FILE"
    --vapid-private-key-file "$VAPID_FILE"
  )
fi

print -- ""
print -- "正在检查配置，不会修改当前服务……"
"$INSTALLER" check "${ARGS[@]}"

print -- ""
print -n -- "检查通过。现在安装并启动手机访问？[y/N] "
read -r confirm_apply
if [[ "$confirm_apply" != [yY] ]]; then
  print -- "已完成检查，未修改服务。"
  exit 0
fi

"$INSTALLER" apply "${ARGS[@]}"
print -- ""
print -- "配置完成。请在手机浏览器打开：https://$PUBLIC_HOST/"
print -- "iPhone 建议通过“分享 → 添加到主屏幕”安装 PWA。"
