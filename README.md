# 家信 HomeSIM

个人 NAS SIM 网关：Docker 后端、手机短信网页、可添加到主屏幕的 PWA。

此版本基于 MacCellular 的公开源码实现 Linux NAS 适配。NasAnySim 的公开仓库没有核心源码，因此本项目没有复制或依赖其闭源镜像。上游源码、许可证、版权声明均保留；个人非商业用途请遵守根目录 LICENSE。

## 当前版本 0.4.1

0.4 加入多线路独立数据、按 SIM 保存网络设置、通话记录与回拨填号、Bark / Telegram / SMTP / Webhook 通知、诊断日志、兼容 eSIM 管理、认证 HTTP / SOCKS5 代理及代理流量统计。设备页按功能折叠，电话诊断区分 ICE 连接与两个方向的音频帧计数。H-Blog iPhone App 接入原生 WebRTC 与 CallKit，并提供可选 APNs 来电配对接口。功能边界、可选 Docker 权限、部署及回退见 [FEATURES-0.4.md](FEATURES-0.4.md)。

0.3.2 修复电话状态：按 `AT+CLCC` 的通话类型过滤数据/传真连接，避免将 LTE 数据连接显示为「未知号码、通话中」并阻止拨号。未知或无法解析的状态会阻止拨号。号码、已有语音通话与音频未就绪分别提示；拨打按钮只在近期线路状态空闲且浏览器音频已连接时启用。音频播放异常退出会清理会话。此修复不代表已验证真实电话或加入公网音频中继。

- 自动读取 SIM 本机号码；读不到可设置绑定当前 ICCID 的备用号码，换卡不沿用。标明模块读取或手动设置，均不代表运营商核验。
- 区分 NAS 服务、模块、SIM 和网络注册状态；串口心跳检查，状态过期后停止显示在线。

- 中文短信收发、长短信编解码、按号码查看会话、验证码与全文复制。
- 未读标记、全部已读、收藏会话、可恢复归档、联系人备注，状态持久保存在 NAS。
- 短信段数提示复用实际发送编码器，仅预览，不发送。
- JSON/CSV 导出短信、备注与阅读状态（不包含账户或安装码）。
- 浅色、深色、跟随系统主题；手机独立会话页；更新时保留阅读位置。
- 短信保存到 NAS 持久化目录，保留模块中的短信副本。
- 首次安装码创建账户，bcrypt 密码哈希，HttpOnly 会话，来源校验。
- 发送前持久化，提交结果不明不会自动重发；请求标识避免同一次点击重复提交。
- Linux ALSA USB 音频与浏览器 WebRTC/PCMU（8 kHz），拨号、接听、拒接、挂断与 DTMF。
- 主屏幕图标、manifest、只缓存网页外壳的 service worker。

**真实运营商通话与双向声音必须在实际模块上验证。** USB 声卡枚举或音频帧计数不能单独证明模块 DSP 语音路径可用。外网音频须部署并验证 TURN；仅转发网页端口不够。锁屏来电须签名后的原生 App 和 APNs 配置。本版不提供通话录音或后台 Web Push；eSIM 和蜂窝数据代理分别需要兼容卡与模块的数据网口。

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

`data/auth.json`、`data/messages.json`、`data/peers.json` 和 `data/setup-token` 均为私有运行文件。它们已排除在 Git 和 Docker 构建上下文之外。备份前停用服务，再备份整个 `data/` 目录。

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

## 0.2 升级说明

既有账户与短信直接沿用。旧短信默认未读；收藏、归档和联系人备注保存在 `data/peers.json`。归档不会删除模块或 NAS 上的短信，新短信仍按原会话的归档状态归类。自动已读仅作用于当前打开的会话、页面可见时。JSON/CSV 是数据导出，当前不提供导入；完整恢复请使用停用后备份的 `data/` 目录。升级前停用并备份 `data/`，保留旧镜像以便回退；新版本重启后需要重新登录。

## 0.3 号码与在线状态

在“我的设备”查看号码来源、SIM 卡号与在线状态，设置或移除备用号码。备用号码只写入 NAS 的 `data/lines.json`，按当前 ICCID 绑定，不写入 SIM。自动读取的号码优先显示；没有保存本机号码的 SIM 可能返回空值。模块约每 12 秒检测一次，同一卡的号码缓存一分钟；点击同步可重新检测。

线路在线表示模块响应、SIM 已识别且网络已注册或漫游，不代表互联网数据、短信送达或双向通话已经验收。浏览器 30 秒未获取新状态、或模块检测超过 45 秒，会显示状态过期或无法连接。

账户、短信与备注直接沿用。升级前停用并备份整个 `data/`，保留旧镜像。`lines.json` 是私有运行数据，不上传 GitHub，也不包含在短信导出中。
