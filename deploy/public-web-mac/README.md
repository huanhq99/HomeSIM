# MacCellular 短信与来电网页（家庭 Mac）

这是第一版可交付路径：手机访问 `https://phone.example.com/`，Cloudflare
Access 完成登录，Cloudflare Tunnel 从家庭 Mac 主动向外建立连接，再转到仅监听
`127.0.0.1:7578` 的短信 PWA。

这条路径不要求手机安装 Tailscale，也不开放家庭网络入站端口。默认 `sms` profile
的公网监听器只注册页面、状态、短信同步、短信发送和短信刷新；`7576` 本机管理页、
`7577` 旧 Tailscale 网关、电话、媒体、ARI、USB 与 AT 接口都不在 Tunnel 路由中。
后端 LaunchAgent 还显式使用 `-sms-only-runtime`，仅保留短信后台工作，不启动
通话轮询、GPS 轮询或启动时 GPS 状态读取。可选的 `direct-voice` profile 在同一
Cloudflare Access 页面上只增加来电 WebRTC 媒体所需的受限接口；本机管理、USB、
AT、拨号、DTMF 与管理员接口仍不进入公网路由。

## Cloudflare 一次性配置

在 Cloudflare Zero Trust 中完成以下两项：

1. 新建 Self-hosted Access application，域名精确为
   `phone.example.com`，Allow policy 只包含自己的登录邮箱。记录该应用的
   Application Audience (AUD) Tag 和账户的 `<team>.cloudflareaccess.com` 域名。
2. 新建 remotely-managed Tunnel，Published application 只配置：

   - Hostname: `phone.example.com`
   - Service: `http://127.0.0.1:7578`

Tunnel token 只写入本机 mode `0600` 文件。不要把 token、AUD、邮箱、短信或
号码提交到 Git。Cloudflare 官方也要求源站验证 `Cf-Access-Jwt-Assertion`；
本项目会再次核验签名、issuer、AUD 与邮箱 allowlist，不依赖可伪造的用户头。

## 本机上线（先检查，再安装）

Cloudflare 的 Team domain、AUD、登录邮箱和 Tunnel token 到手后，在仓库根目录运行：

```sh
./deploy/public-web-mac/install-local.sh check \
  --public-host phone.example.com \
  --team-domain YOUR_TEAM.cloudflareaccess.com \
  --aud YOUR_ACCESS_AUD \
  --email you@your-domain.example

./deploy/public-web-mac/install-local.sh apply \
  --public-host phone.example.com \
  --team-domain YOUR_TEAM.cloudflareaccess.com \
  --aud YOUR_ACCESS_AUD \
  --email you@your-domain.example
```

第一条始终是 dry-run，只检查并临时渲染，不改运行状态；第二条必须明确写出
`apply` 才会安装并启动。将示例域名和账户替换为自己的配置；若 token
文件尚不存在，token 只通过终端隐藏读取，不进入命令参数或日志。

生产进程固定使用用户私有的运行目录：

```text
~/Library/Application Support/MacCellular/
  bin/djonehub-macos.arm64-cgo
  bin/DJOneHubNotifier              # 仅 direct-voice profile 使用
  bin/cloudflared
  config/access-allowed-emails
  config/cloudflared-token
  config/public-web-turn-secret     # direct-voice/external-sip 使用
  config/public-web-vapid-private-key # direct-voice/external-sip 使用
  config/asterisk-deployment.env     # 仅 external-sip profile 使用
  config/asterisk-ari-control.password # 仅 external-sip profile 使用
  config/asterisk-ari-inspect.password # 仅 external-sip profile 使用
  logs/
  data/sms-store/
  data/push/subscriptions.json      # 运行时订阅数据，不参与回滚
  data/call-recordings/             # 通话录音的 Mac 归档副本
  data/call-history.json            # 最近通话与未接来电，跨服务重启保留
```

`bin`、`config`、`data`、`logs` 和两个数据存储目录均为 mode `0700`；可执行文件
安装为 mode `0700`，配置文件安装为 mode `0600`。LaunchAgent 只引用这里的原子
安装副本，因此仓库移动、后台重新登录或代码目录更新都不会让正在运行的服务依赖
`Documents` 路径。

首次 `apply` 会自动把旧的 `<repo>/local/public-web-mac` 当作迁移来源。旧短信存储
只做增量迁移：目标缺少的文件才复制，同名文件必须逐字节一致，不同内容会在写入前
停止；安装器绝不覆盖或删除任一侧已有短信。旧目录本身也不会删除。已有真实邮箱
白名单不会被隐式改写；仅含 `example.com` 的占位白名单会在目标副本中替换成输入的
真实邮箱。旧 binary 也只在尚无已安装生产副本时作为首次迁移后备；一旦
`Application Support` 中已有 binary，后续裸跑 `apply` 会优先保留该生产版，不会被
Documents 里的旧版回退。也可显式指定新的 binary、allowlist 或 token 来源：

```sh
./deploy/public-web-mac/install-local.sh \
  --public-host phone.example.com \
  --binary '/绝对路径/djonehub-macos.arm64-cgo' \
  --allowlist-file '/绝对路径/access-allowed-emails' \
  --token-file '/绝对路径/cloudflared-token'
./deploy/public-web-mac/install-local.sh apply \
  --public-host phone.example.com \
  --binary '/绝对路径/djonehub-macos.arm64-cgo' \
  --allowlist-file '/绝对路径/access-allowed-emails' \
  --token-file '/绝对路径/cloudflared-token'
```

默认 `sms` profile 的安装器会完成以下动作：

- 校验 team domain、AUD、邮箱、token、文件权限、签名以及后端和 cloudflared 的
  原生 arm64 架构；
- 原子安装 MacCellular 后端、cloudflared、allowlist、token，并渲染和检查 LaunchAgent；
- 只 bootout/bootstrap `io.maccellular.phone` 与
  `io.maccellular.phone.cloudflared`；
- 确认两个进程保持运行，并确认 `127.0.0.1:7578` 在没有 Access JWT 时返回
  预期的 `401`；
- 失败时恢复先前的 binary、cloudflared、两个配置文件和两个 plist；日志与短信
  数据始终保留，任何已增量复制的短信也不会因服务回滚而删除。

后端 LaunchAgent 使用 `/usr/bin/caffeinate -s` 包裹 MacCellular 后端。它只在服务运行且
Mac 接交流电时阻止系统睡眠；不会修改全局 `pmset`，也不会阻止显示器休眠。
模板中的 `-sms-only-runtime` 是短信专用后台开关，`-public-web-control` 是短信
发送的显式开关。模板故意不传 `-port`，并显式传入 `-usb-at-only`：这台 Mac
上的 DJI/Baiwang 模块使用已验证的 libusb AT 接口；另一台 LG 设备暴露的
`/dev/cu.usbmodem*` 不是 DJI AT 口，不应在每次服务启动时等待错误串口探测
超时。该开关默认关闭，其他启动方式仍保留原有自动发现行为。

不要直接使用 `brew services start cloudflared`：它不知道本项目的 token 文件，
也不能证明只连接 `7578`。使用本目录的专用 LaunchAgent。

## 验收顺序

1. 本机确认 `127.0.0.1:7578` 已监听，`7576/7577` 仍仅 loopback。
2. 未登录访问 `phone.example.com` 应进入 Cloudflare Access 登录页。
3. 非 allowlist 邮箱必须拒绝；allowlist 邮箱登录后默认进入短信页。
4. 手机浏览器刷新短信，核对新收到的一条测试短信。
5. 只发送一条可识别的测试短信；页面与本机 durable SMS history 都出现同一结果。
6. 重复同一个网页操作不得导致模块重复发送。

默认 `sms` profile 不承诺公网电话音频；下面的独立 profile 只在 TURN、helper 和
实体 QDC507 双向音频都通过对应验收后启用。`direct-voice` 可通过 Web Push
在页面不处于前台时发出来电提醒，但通知本身不会在后台接听或打开麦克风。

## 默认关闭的公网直连语音 profile

`direct-voice` 不会被裸跑安装器启用。每次检查或安装都必须同时显式提供：

- 新 MacCellular arm64 后端；安装器会运行 `-h` 并确认同时支持
  `-public-web-direct-voice`、`-phone-relay-runtime` 与 Web Push 所需的四个启动参数；
- 已通过 `--self-test` 的 arm64 MacCellular helper（内部可执行文件仍名为 `DJOneHubNotifier`）；
  可用 `macos/DJOneHubNotifier/build-app.sh` 生成并使用其
  `dist/DJOneHubNotifier` 独立签名产物；
- 用户持有、mode `0600`、不在本目录和未跟踪仓库路径中的 coturn REST shared
  secret 文件。安装器不会把 secret 内容打印到输出。
- 用户持有、mode `0600` 的 VAPID 私钥文件。文件必须只有一行：将
  32 字节 P-256 私有标量编码为不带 `=` padding 的标准 base64url，标量必须
  在 `1..N-1` 范围内。安装器会解码、检查标准编码和曲线阶，且不打印私钥内容。

先检查，不改现网：

```sh
./deploy/public-web-mac/install-local.sh check \
  --profile direct-voice \
  --public-host phone.example.com \
  --turn-host turn.example.com \
  --binary '/绝对路径/djonehub-macos.arm64-cgo' \
  --helper-binary '/绝对路径/DJOneHubNotifier' \
  --turn-secret-file '/绝对私有路径/coturn-rest-secret' \
  --vapid-private-key-file '/绝对私有路径/vapid-private-key'
```

只有实体 QDC507 双向音频验收完成后，才把同一条命令中的 `check` 改为 `apply`。
该 profile 继续保留短信与来电轮询，使用 `-phone-relay-runtime` 关闭无关 GPS 轮询，
并使用用户通过 `--turn-host` 指定的 TURN 域名、UDP/TCP `3478`、TURNS/TCP `443`、5 分钟短期凭据和
relay-only ICE。它不启用 `-public-web-external-voice`，也不公开本机媒体 socket。
它还会显式启用 `-public-web-push`，VAPID subject 自动使用
`https://<public-host>`，订阅库固定为用户私有的
`data/push/subscriptions.json`。`data/push` 为 mode `0700`；已存在的订阅库必须为
mode `0600` 且不能是符号链接。

通话接通并建立双向网页音频后会自动开始录音，不再逐通电话弹确认。录音结束时先在
当前手机的 PWA 本地录音库保存一份，再同步到 Mac 的
`~/Library/Application Support/MacCellular/data/call-recordings`。Mac 目录中的音频文件
旁边会保存同名 JSON 元数据，便于后续按时间、号码和方向整理。Mac 暂时不可达时手机
副本仍然保留，录音页可手动重试同步。NAS 归档应从这个 Mac 目录定期复制；不要把通话
录音是否成功依赖于 NAS 当时在线。迁移到另一台 Mac 时，将该目录和短信数据一起迁移
即可。需要改存放位置时可显式传 `-public-web-recordings-dir`，生产环境默认路径不必改。
最近通话另外保存在 `data/call-history.json`，最多保留 100 条呼入、呼出和未接记录；服务
更新或 Mac 重启不会再清空。迁移 Mac 时应与短信数据、录音目录一起复制。

安装事务会把 helper 安装为 mode `0700`，TURN secret 与 VAPID 私钥安装为
mode `0600`，并加入
`io.maccellular.phone.media-helper` LaunchAgent。切换时严格按 Tunnel → helper →
backend 停止，再按 backend → mode `0600` Unix socket 就绪 → helper 稳定 → Tunnel
启动；最终同时确认三个进程、socket 类型/权限和 `7578` 未携带 JWT 时的 `401`。
任何一步失败都会恢复之前的 binary、私钥、其他配置、三个 plist 与原加载
状态。订阅库是与短信库相同的“只向前数据”：安装器不创建文件内容、不覆盖、不删除，
也不会在服务回滚时恢复旧副本。

## 外置 VoLTE→SIP 网关 profile（推荐的网页通话路径）

`external-sip` 是实体 VoLTE→SIP 网关的生产入口，不依赖 QDC507 的 USB 音频，也不安装
`DJOneHubNotifier`。它保留当前 QDC507 的短信轮询，同时把电话媒体交给独立 Asterisk
22/ARI + Pion + TURN relay-only 链路。当前 profile 默认开放实体来电、网页接听、拒接、
双向媒体、DTMF 和挂断；主动拨号仍默认关闭。只有明确提供已验收的 Asterisk PJSIP
外呼 endpoint，并同时加 `--sip-dial-outgoing`，网页才会显示/启用拨号；这不会替代
实体网关、运营商或公网媒体验收。

网关先接到 Mac 的独立直连网段：Mac `192.168.250.1/30`、网关
`192.168.250.2/30`，关闭 DHCP/路由/DNS，SIP 使用免注册静态 IP peer，目标为
`192.168.250.1:5060/UDP`，codec 只留 PCMU/8000 Hz/mono/20 ms。Asterisk 生产目录必须
按 [Asterisk 部署说明](../asterisk/README.md) 生成并通过 `verify-config.sh`；这不是
1.0 的默认 QDC507 直连路径，未接实体网关前不要 `--apply`。

到货并完成本地 SIP/RTP 验收后，先 dry-run：

```sh
./deploy/public-web-mac/install-local.sh check \
  --profile external-sip \
  --public-host phone.example.com \
  --turn-host turn.example.com \
  --binary '/绝对路径/djonehub-macos.arm64-cgo' \
  --turn-secret-file '/绝对私有路径/coturn-rest-secret' \
  --vapid-private-key-file '/绝对私有路径/vapid-private-key' \
  --voice-gateway-id sc211 \
  --asterisk-deployment-env '/绝对私有路径/asterisk-prod-v1/deployment.env' \
  --asterisk-ari-password-file '/绝对私有路径/asterisk-prod-v1/secrets/ari-control.password' \
  --asterisk-recovery-ari-password-file '/绝对私有路径/asterisk-prod-v1/secrets/ari-inspect.password' \
  --sip-recovery-store "$HOME/Library/Application Support/MacCellular/data/external-voice/recovery.json" \
  --sip-media-udp-min 55000 \
  --sip-media-udp-max 55063
```

如需显式开启网页主动拨号，在同一命令追加（默认不加）：

```sh
  --asterisk-ari-outgoing-endpoint gateway-out \
  --sip-dial-outgoing
```

`gateway-out` 必须是本地 Asterisk 已验收、只允许目标号码策略的 PJSIP endpoint；
安装器会把 endpoint 与开关写入 LaunchAgent，并在缺一项时拒绝配置。没有实体网关、
拨号计划和手机端媒体验收时，不要开启该开关。

确认输出中的 Asterisk entity/version/policy、TURN、Web Push 和 SIP media 参数均为目标
设备值后，才把同一命令的 `check` 改为 `apply`。该事务只启动 MacCellular 后端与 cloudflared
两个 LaunchAgent；Asterisk 容器需按其独立 runbook 在本地网络验收后启动。切回 `sms`
不会删除 external-sip 的私有配置或恢复数据，但会移除公网语音参数；订阅库和外置语音
recovery store 都是只向前数据。

外置网关如果接走了原来的那张 SIM，QDC507 就不能继续同时提供同号短信；最省事的第一版
是使用第二张 SIM/号码，短信继续走 QDC507，电话走外置网关。若必须同号迁移，必须另行
实现并验收该网关的 SMS API，不能假设 SIP 网关会自动保留现有短信历史。

恢复默认短信模式只需按原方式运行 `sms` profile（`--profile sms` 可省略）。它不会
要求 helper、TURN secret 或 VAPID 私钥，也不会加载语音 helper；已安装的私有
helper/secret/key 副本和订阅数据会保留，但 SMS LaunchAgent 中不带任何 Web Push 参数。

用户仍必须在网页里主动点击开启来电提醒并授予系统通知权限；iPhone/iPad 需先把
PWA 加到主屏幕。通知点击只会打开或切回 `/remote/`，接听和麦克风仍必须在可见
页面内由用户操作。实体 QDC507 或外置 VoLTE 网关的双向音频未通过前，不要把 LaunchAgent
在线当作通话已可用。
