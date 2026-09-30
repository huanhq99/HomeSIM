# 家信 HomeSIM

个人 NAS SIM 网关：Docker 后端、手机短信网页、可添加到主屏幕的 PWA。

此版本基于 MacCellular 的公开源码实现 Linux NAS 适配。NasAnySim 的公开仓库没有核心源码，因此本项目没有复制或依赖其闭源镜像。上游源码、许可证、版权声明均保留；个人非商业用途请遵守根目录 LICENSE。

## 当前版本 0.1.0

- 中文短信收发、长短信编解码、按号码查看会话、验证码复制。
- 短信保存到 NAS 持久化目录，保留模块中的短信副本。
- 首次安装码创建账户，bcrypt 密码哈希，HttpOnly 会话，来源校验。
- 发送前持久化，提交结果不明不会自动重发；请求标识避免同一次点击重复提交。
- Linux ALSA USB 音频与浏览器 WebRTC/PCMU（8 kHz），拨号、接听、拒接、挂断与 DTMF。
- 主屏幕图标、manifest、只缓存网页外壳的 service worker。

**通话代码已实现，但真实运营商通话与双向声音必须在实际模块上验证。** USB 声卡枚举不能单独证明模块 DSP 语音路径可用。此版本不含后台 Web Push、TURN 外网中继、通话录音或完整最近通话历史。现有语音媒体连接仅针对同一局域网；不能把它宣称为 NasAnySim 全功能替代品。

## 使用条件

Linux amd64/arm64、Docker Compose、可用实体 SIM、USB AT 串口。语音需要已准备好的模块 UAC 语音路径、ALSA 设备，以及手机访问时受浏览器信任的 HTTPS。此项目不刷固件、不改 IMEI、不下载或加载模块侧内核文件。

先检查设备，不要仅根据编号猜测 AT 口：

```bash
uname -m
ls -l /dev/serial/by-id/ /dev/ttyUSB* /dev/ttyACM*
cat /proc/asound/cards
fuser -v /dev/ttyUSB2
```

## 在 NAS 从源码构建

```bash
git clone https://github.com/huanhq99/HomeSIM.git
cd HomeSIM
cp .env.example .env
```

在 `.env` 中设置自己 NAS 的 `HOMESIM_BIND_IP` 和实际 `HOMESIM_TTY`；默认仅监听本机 `127.0.0.1`。确保 AT 口没有被 ModemManager 或其他程序占用。应用不会杀死其他服务以抢占串口。

```bash
docker compose build
docker compose up -d
docker compose ps
```

Go 依赖下载需要时可使用自己的镜像源，例如：

```bash
docker compose build --build-arg GOPROXY=https://goproxy.cn,direct
```

只映射实际串口和 `/dev/snd`，没有 privileged、Docker socket 或整个宿主机根目录挂载。host 网络用于局域网 ICE；HTTP 必须绑定你指定的 NAS LAN IP 或本机地址。WebRTC 使用 UDP `8590–8600`。

## 首次账户

服务启动后，在你自己的 NAS 终端查看 `data/setup-token`（不要发到聊天或提交 GitHub）。打开 `http://NAS局域网IP:8580/`，填写安装码，并自行设置用户名和密码。密码为 12–72 字节。已有账户时安装码不再有效；服务重启会要求重新登录。

`data/auth.json`、`data/messages.json` 和 `data/setup-token` 均为私有运行文件。它们已排除在 Git 和 Docker 构建上下文之外。备份前停用服务，再备份整个 `data/` 目录。

## HTTPS 和主屏幕

推荐使用 NAS 已有的 Lucky、Nginx、Caddy 等反向代理，配置你自己的域名与受信任证书，代理到 NAS 的 `8580` 端口。保留原始 `Host` 和 `Origin`；代理必须覆盖 `X-Forwarded-Proto`，再按你的部署设置 `HOMESIM_TRUST_PROXY=1`。不要把未配置好的 HTTP 服务直接转发到公网。

- iPhone：Safari 打开 HTTPS 地址，分享 → 添加到主屏幕。
- Android：Chrome/Edge 打开 HTTPS 地址，通过安装菜单添加。
- 只用局域网 HTTP 时可以查看短信，但浏览器通常不允许麦克风和 service worker。

首次拨打或发送只由用户在页面发起。先使用自己的另一部电话验证接收，再验证发送和双向通话。本仓库测试不会实际拨号或发短信。

## 测试与构建

```bash
go test -race ./cmd/homesim ./internal/modem ./pkg/smscodec
node --check cmd/homesim/web/app.js
docker compose config -q
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o homesim ./cmd/homesim
```

HomeSIM 的入口是 `cmd/homesim`。其他目录和 `UPSTREAM_README.md` 保留上游代码与说明，用于追溯来源；上游 Mac 安装器不用于此 NAS 版本。

## 来源

- MacCellular，基于公开 commit `e443b233`，PolyForm Noncommercial 1.0.0。
- VoHive/DJOneHub 模块控制基础及 required notice，见 LICENSE。
- Pion WebRTC 及短信、串口等依赖沿用各自许可证，见 THIRD_PARTY_NOTICES.md 与 licenses/。

本项目不包含或再分发 NasAnySim 的闭源运行镜像，也不包含 MaVo 的模块侧语音运行文件。
