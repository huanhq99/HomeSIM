# Asterisk 22.10.1 外置语音部署切片

本目录把现有默认关闭的 ARI/JSON `chan_websocket` adapter 接到一个可审计、
最小暴露面的 Asterisk 容器。它不控制 QDC507，不执行 `ATA`/`ATD`，也不证明
任何 VoLTE-to-SIP 设备、运营商、SIM、Android/iOS 或双向音频已经通过验收。

## 固定输入

- Asterisk `22.10.1`
- 官方源码：<https://downloads.asterisk.org/pub/telephony/asterisk/asterisk-22.10.1.tar.gz>
- 官方 SHA-256：`0953564c44fa49827f3c9d70ca6e80db83828c9848440852c6be44c961855353`
- Debian `bookworm-slim` 镜像固定到 `source.lock` 中的 digest
- Dockerfile frontend 也固定到 `source.lock` 中的 digest
- bundled pjproject `2.17` 在 Asterisk 构建前单独下载并验证固定 SHA-256；
  不允许构建规则退回到只校验 MD5 的隐式下载

Dockerfile 在编译前验证两份源码 SHA-256，并在镜像中保留 Asterisk 的
`LICENSE`/`COPYING` 与 pjproject 的 `COPYING`。当前镜像仅用于 维护者的私有、
非商业部署，不进入现有 DMG/ZIP。
公开分发镜像前仍须保存完整对应源码、构建输入、基础系统包许可证/SBOM，并单独
复核 Asterisk GPL-2.0 与仓库 PolyForm Noncommercial 边界；本说明不是法律意见。
公开 DMG/ZIP 还存在独立缺口：必须聚合并核验构建产物实际链接的每个 Go 模块
许可证；当前尚未完成。

## 生成私有配置

主机需要一个支持 `openssl passwd -6` 的 OpenSSL 3（macOS 自带的旧
LibreSSL 不够）、Docker CLI、Buildx 和 Compose。生成器和验证器都会先检查
`PATH`，再检查 Apple Silicon 与 Intel Homebrew 的常见 OpenSSL 3 路径；找不到
就安全失败。如果同时安装了 Intel 与 Apple Silicon Homebrew，运行前应显式把
原生 `docker`/`docker-buildx` 放在 `PATH` 前面，不要混用两套架构。

生成物默认放在仓库已忽略的 `local/asterisk/`，目录权限为 `0700`、文件为
`0600`。它包含随机、稳定的本地管理 `asterisk_id`、两个独立 ARI 密码和部署
地址，绝不能提交 Git。

先 dry-run；以下地址只是格式示例，必须换成已核验的软路由和蜂窝网关地址：

```sh
deploy/asterisk/generate-config.sh \
  --gateway-ip 192.168.50.20 \
  --sip-bind-ip 192.168.50.10 \
  --lan-cidr 192.168.50.0/24
```

确认计划后，用完全相同的参数加 `--apply`。生成器拒绝覆盖已有目录；轮换配置时
应先停止服务、保留旧目录和证据，再生成到一个新路径。

```sh
deploy/asterisk/generate-config.sh \
  --gateway-ip 192.168.50.20 \
  --sip-bind-ip 192.168.50.10 \
  --lan-cidr 192.168.50.0/24 \
  --apply

deploy/asterisk/verify-config.sh \
  /absolute/path/to/dji-4g-mac-phone/local/asterisk
```

配置合同包括：

- 容器内 ARI/媒体 WebSocket 监听 `0.0.0.0:8088`，但宿主只发布
  `127.0.0.1:<ARI_PORT>`；这使 macOS Docker 端口转发可达，同时不向 LAN 暴露 ARI。
- SIP/UDP 和 RTP/UDP 只能发布到生成时给出的精确私有宿主地址，不能 wildcard。
- 容器网络固定为 `172.31.255.248/29`，容器地址固定为
  `172.31.255.250`，并拒绝与物理 LAN 重叠。PJSIP `local_net` 使用该容器子网，
  `external_signaling_port` 使用生成时的宿主 SIP 端口；不能把物理 appliance LAN
  错当成容器本地网络。
- PJSIP 的 endpoint identifier 顺序固定为仅 `ip`，只按精确网关源 IP 识别
  一个入呼 endpoint，未知来源或 username/anonymous 匹配均拒绝；只允许
  PCMU/`ulaw`，`direct_media=no`，没有出呼 registration、SIP 密码或
  outbound auth。
- RTP 端口范围必须从偶数开始、包含完整 RTP/RTCP 对，且最多 64 个端口。
- dialplan 在 `Stasis` 前设置固定 `DJONEHUB_POLICY_ID`、`GROUP` 单呼门和
  `TIMEOUT(absolute)`；`Stasis` 返回后立即 `Hangup()`。实际 ARI WebSocket
  断开后的孤儿通话时延仍必须由真实 Asterisk 故障注入证明。
- `chan_websocket` 控制帧固定为 JSON；ARI `channelvars` 只暴露
  `MEDIA_WEBSOCKET_CONNECTION_ID` 与 `DJONEHUB_POLICY_ID`。
- Asterisk 模块自动加载被禁用；镜像构建时物理删除所有未批准的动态模块，模块
  目录只保留规范清单中的 35 个 `.so`；实际运行闭包另含 16 个内建模块。
  `modules.conf` 只有经实际启动验证的 PJSIP/PCMU、ARI/Stasis、JSON
  `chan_websocket`、ARI dial holding bridge、双通道 bridge 和有界 dialplan
  依赖闭包，且每个都是启动
  必须项。`app_system`、
  `app_originate`、anonymous/username endpoint identifier 与 outbound
  registration 不会加载。
- CDR、CEL、AMI、SIP transfer 和 connected-line 更新明确关闭；
  pjproject 日志最高为 warning，避免原始 SIP 包进入日志。Stasis taskpool
  初始大小为 2、最大为 8，PBX RTP 腿不配置 STUN/TURN。
- 固定的 Asterisk `22.10.1` 在配置框架注册时会输出一条未分级的
  `minimum_size`/legacy `threadpool` 诊断；上游源码将该选项注册到同时包含
  `threadpool` 与 `taskpool` 的 type 组，但文档只把它声明为 `taskpool`
  选项。本配置不写该键；live gate 要求这条精确诊断只出现一次，且
  拒绝其他 `ERROR`/`WARNING`/配置或模块错误。
- 镜像自带的 Asterisk 文档和 ARI schema 保留在只读 `/var/lib/asterisk`；
  仅数据库与密钥状态写入私有 `/var/lib/asterisk-private` 绑定目录，避免空数据目录
  遮蔽 schema 并导致 Stasis 启动失败。
- `maccellular_voice_control` 是 read-write 用户，只供显式 Answer/End adapter；
  `maccellular_voice_inspect` 是 read-only 用户，只供 GET-only 重启恢复与验证。DJOneHub
  分别接收两组文件凭据，不能把 control 凭据复用到 recovery inspector。

## 构建与显式启动

先渲染 Compose，确认最终端口没有 `0.0.0.0` 或省略宿主地址：

```sh
docker compose \
  --env-file /absolute/path/to/local/asterisk/compose.env \
  -f deploy/asterisk/compose.yaml \
  --profile explicit-pbx config
```

构建不会启动服务。使用 Buildx 直接构建并加载固定本地镜像，避免
独立 Compose 回退到不支持 Dockerfile `RUN --network=none` 的旧 builder：

```sh
docker-buildx build --load --provenance=false \
  --tag maccellular-asterisk:22.10.1-local \
  --file deploy/asterisk/Dockerfile \
  deploy/asterisk
```

如果 Buildx 以 Docker CLI 插件安装，上述命令改为 `docker buildx build`。
`scripts/tests/asterisk22-integration.sh --live` 会自动选择这两种方式。

只有精确网关/SIM/运营商/SKU/固件和网络计划已经核对后，才执行
Compose `up -d --no-build`。停止时
不要使用 `down -v`：私有 bind-mounted 日志和恢复证据应保留。
Compose 只接受 `source.lock` 固定的镜像引用，`pull_policy: never` 防止隐式拉取；
`restart: "no"` 保持 fail closed，避免在 live Adapter 的 restart/ABA 门验收前由容器
自动重启到新 PBX incarnation。

## 2026-08-15 本机 smoke 证据

- `--static`：21/21 通过。
- 显式 `--live`：16/16 通过；运行的是真实 Asterisk `22.10.1`
  arm64 二进制，但是使用纯合成 loopback 配置的一次性容器。
- `--provenance=false` 本地镜像 ID/RepoDigest：
  `sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51`。
- 镜像内物理模块目录精确为 35 个动态模块，运行时总闭包为这 35 个动态模块加
  16 个内建模块；未批准模块既未启动，也不存在于镜像模块目录中。
- 固定容器子网/地址、PJSIP `local_net`/`external_signaling_port`、禁止拉取和
  禁止自动重启均通过静态与 live 检查。
- live harness 把临时配置与凭据放在 Git 忽略的仓库本地
  `local/asterisk-integration-tests/` 下，以支持 Colima bind；成功后已确认该
  Compose project 无容器残留，临时测试根也已删除。

这份证据只证明镜像、模块闭包、容器限权、loopback 端口、合成
dialplan、timerfd、PJSIP 配置与 ARI 双账号边界。另一条
`scripts/tests/public-voice-e2e.sh --live` 软件媒体门已使用真实 SIP
`INVITE`/`BYE`、RTP、media WebSocket 和 external voice v2 验证双向 PCMU
pattern 与清理。该门仍未运行外置蜂窝网关、实体 SIM、公网手机或可懂
音频，因此不是生产通话验收。

## DJOneHub 参数映射

从私有 `deployment.env` 读取精确值，主程序至少需要：

```text
-voice-provider=asterisk
-voice-gateway-id=<stable-private-id>
-asterisk-ari-url=http://127.0.0.1:<ARI_PORT>/ari
-asterisk-ari-application=<APPLICATION>
-asterisk-ari-incoming-argument=<ARGUMENT>
-asterisk-ari-incoming-context=<CONTEXT>
-asterisk-ari-incoming-endpoint=<ENDPOINT>
-asterisk-expected-entity-id=<ENTITY_ID>
-asterisk-expected-version=22.10.1
-asterisk-incoming-policy-id=<POLICY_ID>
-asterisk-ari-username=<CONTROL_USER>
-asterisk-ari-password-file=</absolute/generated/root/secrets/ari-control.password>
-asterisk-recovery-ari-username=<INSPECT_USER>
-asterisk-recovery-ari-password-file=</absolute/generated/root/secrets/ari-inspect.password>
```

不要把密码放进命令行、Compose 环境、日志或 Handoff。`-sip-answer-incoming` 和
`-sip-end-active` 继续保持关闭，直到真实 dialplan/RTP、孤儿清理、同一 PBX
HTTP+WS、重启时旧 event WS 未 EOF 前的 live-mutation incarnation/ABA 隔离、
Android 前台媒体和 LTE-only 双向通话逐项验收。

同一张实体 SIM 不能同时留在 QDC507 和外置蜂窝网关中。若需要保留同一号码，
迁移 SIM 的同时必须另行验收网关的短信 API；否则应使用第二张 SIM/号码。
