# 更新记录

## 1.0.0-rc.4 · 公开预发布候选

- 在 README、首次安装和发行说明中显著列出用户必须自备的模块、SIM、域名、
  Cloudflare Access/Tunnel、TURN 与模块语音环境，避免将项目描述成即插即用服务。
- 公开候选移除不再引用的旧研发截图；当前用户入口仅保留使用虚构线路标签和零通信
  数据制作的 MacCellular 产品截图。
- 继续以 source-available 非商业许可发布测试候选，不把 PolyForm Noncommercial
  误称为 OSI 开源许可证；`v1.0.0` 正式版仍需完成剩余真机耐久验收。

## 1.0.0-rc.3 · 异常退出恢复

- 修复 macOS 后台异常退出后遗留本地语音控制 socket，导致下次启动反复退出、网页
  无法访问的问题。仅在确认遗留 socket 已无人监听时自动恢复；正在工作的实例仍会
  拒绝第二个进程占用。
- 模块临时拔出仍按设备离线处理，不把物理断开误报为新硬件故障；插回模块后的真机
  通话、录音播放和重连恢复继续作为 1.0 发布前实机验收项。

## 1.0.0-rc.2 · 负责任发布候选

- 将公开定位收窄为个人单模块、单实体 SIM，不宣传卡池、批量插卡、批量拨号、群发
  短信、验证码代收或第三方话务转售；新增合法与负责任使用边界及官方法规入口。
- 新增数据与隐私说明，明确本机存储、Cloudflare/TURN/Web Push 等第三方边界、录音
  责任、卸载保留策略和公开 issue 脱敏要求。
- 新增 GitHub issue/PR 模板，拒绝改号、伪造主叫、骚扰、诈骗、运营商限制规避等
  功能请求，并要求贡献者确认合法控制测试设备、SIM 与账户。
- MP4 浏览器录音改为停止时生成完整文件，避免周期切片偶发产生无法独立播放的片段；
  自动化回归已通过，仍待解锁 Mac 后完成一次手机端实机播放复验。
- 合法使用与隐私说明随 macOS/Windows 发行包一同交付；安装器会检查这两份文件存在，
  避免仓库说明与用户实际下载内容脱节。

## 1.0.0-rc.1 · 公开发行候选整理中

- 将产品名称和主叙事统一为 MacCellular：面向个人自托管的实体 SIM
  电话与短信服务，而不是上游项目的私人备份或硬件实验集合。
- 重写 README、首次安装、支持、贡献、源码范围和发布检查表；旧实验结论保留在
  历史与硬件验证文档，不再作为新用户入口。
- 明确产品层自主实现范围：PWA、远程媒体、电话状态机、短信与录音持久化、Web
  Push、Cloudflare 接入、安装迁移与运维工具；同时保留 VoHive/DJOneHub、MaVo、
  Pion 和 libusb 的来源及许可证。
- QDC507 直连电话已完成真实呼入、呼出、双向语音、DTMF、挂断和录音验证；修复
  连续挂断重拨时复用旧 D4/UAC 路由导致对方声音全静音的问题，每通电话在 ATD/ATA
  前重新建立模块音频会话。
- 录音上传按通话归并：短尾片段不再生成第二条录音；如果较完整的录音稍后到达，
  自动保留较长版本并清理同一通话的短版本。
- 全新安装的后端、配置、短信、通话记录、录音和日志统一进入
  `Application Support/MacCellular`；配置器可从旧 `DJOneHub` 私人测试目录读取迁移源，
  复用程序和配置并增量迁移短信，不再让普通用户维护两套运行目录。
- 修复 macOS 自带 Bash 3.2 下安装器夹具会提前退出却被误判为通过的问题；当前夹具
  必须执行到最后一项断言才报告 PASS。
- DMG 改在系统临时目录组装，并在生成后重新挂载验证其中的 App 签名，避免同步目录
  自动附加的 Finder 元数据让下载后的 App 无法通过 macOS 校验。
- 把维护者的生产域名参数从安装器默认值中拆出，为普通用户的自有域名部署做
  准备；本候选会生成本地 DMG 用于验收，但不会创建 Git tag、上传安装包或发布
  GitHub Release。

## 2026-08-15 · 上游 v1.2.9 兼容

- 参考同型 QDC507 的闭源实现线索，将 `CLCC` 改为完整严格解析后仅保留
  mode-0 语音行，不再让空号码 mode-1 数据会话阻断通话操作；确认语音空闲后
  加入 5 秒重拨冷却。
- 新增默认关闭的 QDC507 公网直连外呼链路：网页必须先建立 relay-only
  WebRTC 媒体，服务端再将号码、空闲通话代际、媒体租约、UAC 和物理 USB 身份
  绑定到同一 AT session，只有最后门禁成功才发 `ATD`。尚未做 legacy UAC 真机拨号。

- 吸收上游对旧 UAC/完整 UAC 配置的兼容语义：已有 USB 音频布局不再被强行改写，
  仅在需要时补 IMS/VoLTE；USB 重枚举期间的 `USBCFG ERROR` 作为暂态等待处理。
- 保留本分支更严格的设备身份、USB location、通话空闲、写入前回读和失败回滚约束；
  不重复执行已经回滚的 QDC507 硬件实验。

- 公网 PWA 的 external-sip 来电控制补齐拒接与 DTMF：仅在独立 profile 显式开启
  `-sip-reject-incoming` / `-sip-send-dtmf` 后可用；QDC507 的冻结 QPCMV/ATA
  路径和主动拨号仍保持关闭。前端静态资源缓存版本同步更新。

## 2026-08-14 · v1.3.0-private.1（MacCellular 私有版）

- 使用独立 App 标识、LaunchAgent、数据目录和 `127.0.0.1:7576`，与上游版隔离。
- 默认关闭模块短信自动删除、自动 DHCP 修复和自动射频/模块重启。
- 新增当前用户私有的追加式短信历史：入站先持久再展示，发件记录
  `pending/submitted/unknown`，崩溃恢复不猜测也不自动重发；PWA 改用
  opaque ID 和 HMAC cursor 增量同步，不在浏览器持久短信内容。
- 新增独立 Android native v1 trust preview：以 AndroidKeyStore P-256、
  一次性登记 ticket、单次 challenge 和签名精确 request target/body 读取
  session 与每页最多 10 条的增量短信。只有登记 scope、当次 App Cap 与
  全局控制开关均显式授予 `sms.send` 时，才允许前台二次确认发送；
  PII-free no-backup 单槽记录、服务端持久幂等与签名查询使丢响应时只锁定、
  绝不自动重发。不放宽浏览器 CSRF，也没有通话控制。
- 未发现 VID/PID 已核验的 DJI 网络服务时，拒绝从本地管理 MAC 地址猜测 `en*`。
- 保留模块原生 `QPCMV` + USB Audio Class 研发路线，目标组合关闭 ADB、打开
  UAC；第二次实机验证明确失败并回滚，因此不再作为生产通话路线。外置 MaVo
  运行时不安装、不使用，也不进入 Git/DMG。
- 生产语音候选改为先采购并验收精确 SIM、运营商、SKU 与固件匹配的外置
  VoLTE-to-SIP 网关；不能复用或解锁当前冻结的 QPCMV/ATA/ATH 分支。
- 新增与 QPCMV 完全隔离、默认关闭的外置语音软件切片：
  `internal/sipgateway` 锁定 call identity、boot/revision、unknown outcome 与媒体
  代际；Asterisk 22.8+ ARI/JSON `chan_websocket` adapter 负责 SIP 侧控制和 PCMU；
  `internal/sipwebrtc` 负责受限 SIP↔WebRTC 桥；独立
  `/api/remote/v2/voice/*` runtime/API 负责前台 offer、接听、结束与只读核对。
  PWA 已接入 v2 snapshot、人工麦克风准备、Answer、End 和仅由用户按钮触发的
  reconcile；页面隐藏会销毁媒体与敏感状态，unknown outcome 绝不自动重试或核对。
  新增有界 `fsync` 恢复存储：provider token 加密落盘，Answer/End 在
  provider mutation 前持久 `Arm`，unknown 永不重发。重启只使用 HTTP GET
  inspector，仅精确 `ended` 自动写 `provider_ended` tombstone；其余状态
  进入人工 PBX 恢复。运行期 adapter 的 info、Inspect、Prepare、Answer/End 与
  正常 cleanup 改为全部通过已认证的同一 ARI event WebSocket incarnation 上的
  `RESTRequest`/`RESTResponse`；连接 EOF、响应错配或代际漂移 fail closed 到
  recovery-required，不使用独立 HTTP mutation fallback，也不重连替代实例。
  Asterisk 冷启动固定精确 `asterisk_id`，使用 PBX
  timestamp cutoff 与第二次 info pin 拒绝旧通话，不把 UserEvent 当作跨 topic
  全序屏障。软件测试仍只证明默认关闭的实现边界：真实网关/
  SIM/运营商/SKU/固件、生产 orphan watchdog、同号码 SMS API
  迁移、稳定签名 Android 发行包、Android/iOS 生命周期与真实双向音频
  均未验收，因此不构成可部署语音能力。
- 新增固定 Asterisk `22.10.1` 的最小容器部署切片：固定源码/基础镜像/pjproject
  输入，提供私有配置生成与验证、精确模块 allowlist、loopback-only ARI、独立
  control/recovery 凭据、IP-only PCMU endpoint 和只读无特权容器。Hermetic static
  合同通过 `21/21`，disposable loopback real-Asterisk smoke 通过 `16/16`；最终
  `--provenance=false` arm64 本地镜像 ID/RepoDigest 为
  `sha256:d4167acc2bfdd581e23d6d532df76858d8f7c752f014567e2a3717c9d3359f51`。
  镜像物理裁剪为 35 个动态模块加 16 个内建模块；固定容器子网为
  `172.31.255.248/29`、地址为 `172.31.255.250`，PJSIP `local_net` 使用容器
  子网并渲染 `external_signaling_port`；部署禁止拉取且禁止自动重启。live 临时根
  位于 Git 忽略的仓库本地目录以支持 Colima bind，成功后清理。独立 real
  software-media harness 已通过真实 SIP `INVITE`/`BYE`、RTP、媒体 WebSocket、
  external voice v2 snapshot/Offer/Answer/End、双向不同 PCMU pattern 与最终零
  ARI channel/bridge。实体蜂窝网关/SIM、运营商、公网手机、生产 orphan watchdog
  和物理双方可懂语音仍未验收。
- 新增隔离的公网只读 edge 源码与部署骨架。目标控制面为
  `phone.example.com` + 独立 Cloudflare Tunnel，Mac 仅主动建立出站 WSS，
  Tailscale 退回管理/故障回退用途；浏览器 HTTP 使用 Cloudflare Access JWT、
  精确 audience 与邮箱白名单，gateway 使用固定 ID 和 P-256 签名。当前唯一公开
  业务能力是粗粒度状态 snapshot，enrollment、mutation、push、TURN credential
  issuance 全部关闭；`turn.example.com` 仅预留为未来 DNS-only 直连 coturn，
  公网 WebRTC 必须 relay-only。该工作只完成本地实现和审计：没有公网部署，
  没有 DNS/防火墙/VPS 写入、生产镜像、真实 Cloudflare smoke、手机 UI、TURN、
  SIP/通话或音频验收；ARI、USB/AT、`7576` 和旧 `/api/remote` 均未公开。
  部署配置/Compose 静态套件使用官方临时 Docker Compose v2.40.3 通过 `34/34`，
  临时二进制随后清理；该结果不代表镜像产出或实际部署。
- 增加 USB/UAC 与 CoreAudio 精确预检、无通话的一次性 `QPCMV=0` 归一化、
  原始配置备份和失败自动回滚。该路线尚未通过真实通话双向听音验收。
- 增加 MacCellular 项目规则、交接文件、隐私排除和硬件验收边界。
- 保留 PolyForm Noncommercial、VoHive required notice 和 MaVo MIT 完整许可证。
- 新增独立 loopback 远程网关、Tailscale App Capability 授权、显式
  Origin/CSRF 防护、持久幂等账本和脱敏审计。
- 新增 Android-first PWA 和固定 Pion WebRTC v4.2.18 的 PCMU/8 kHz 媒体
  核心；Swift 的真实 UAC 代码路径已通过同 UID UDS 接入 Go/Pion，并通过合成
  媒体、自测、Debug/Release 和 TSAN 软件验证。尚未进行真实模块来电的远程双向
  声音验收，因此不宣称远程通话可用。
- 增加默认关闭的 `-remote-incoming-answer` 实验开关；只有精确 Tailscale 身份、
  `calls.control + calls.media`、WebRTC/IPC/UAC 新鲜就绪、同一通话/媒体代际、同一
  USB 生命周期及同一 AT session 内 QPCMV/ATA 重验全部通过时才可能接听。远程
  拨号、拒接和 DTMF 继续返回 `409`。
- 增加第二个默认关闭的 `-remote-rescue-hangup` 止损开关；只有本网关曾接听、
  同一激活代际曾完成双向媒体证明的精确来电，才可在
  `calls.hangup + calls.media`、持久幂等账本和精确 USB/身份/CLCC/QPCMV 重验后发一次
  `ATH`。明确拒绝或结果歧义都消耗权限，不回退 `AT+CHUP`；代码未部署或实机验收。
- WebRTC 媒体只允许一个精确 Tailnet 对端（IPv4 `/32` 或 IPv6 `/128`），PWA
  仅支持前台。第二次受控初始化曾枚举到 `2c7c:0125`，但 `AT+QPCMV?` 明确返回
  `ERROR` 后自动回滚并重启至 `2ca3:4006`；SIM/LTE/PS 已恢复，无活动通话或音频，
  `7586` 已停止。一次性授权已经消耗，不允许第三次持久硬件写入。

> 下列旧版本条目记录当时的开发状态，不代表 v1.3 当前硬件验收结论。

## 2026-08-13 · v1.2.4（公开发布）

- 将既有网页管理能力整理为独立 macOS App：电话、通话记录、短信、通讯录和设置统一入口。
- 补充拨号、接听、拒接、挂断、DTMF、通话记录、录音入口、本机号码读取与短信自动清理。
- 保留早期版本的 USB 4G、Wi-Fi 优先/4G 兜底、来电和短信提醒、GPS、eSIM、AT 调试及网络自动恢复能力。
- 发布 macOS Universal DMG（arm64 + x86_64）与 Windows x86-64 `DJOneHub.exe` 候选包；Windows 仍待真实硬件验证。
- README 增加脱敏真实截图、历史说明入口和公开范围说明。
- 新增 MaVo host-side adaptation 的 MIT 许可与引用说明；模块侧语音运行时不包含在仓库或 Release 中。

## 2026-08-08 · v1.0.0-rc1（私有候选版）

- 打通 macOS 双向语音通话：支持拨打、接听、拒接、挂断、静音、DTMF 与本地录音。
- 采用模块 USB Audio 8 kHz 单声道路由，并在通话结束后恢复设备采样率；当前仍有轻微、间歇性的滋滋声待继续优化。
- 将日常功能从网页控制台迁移到独立 SwiftUI App，重新设计拨号、通话中、最近通话、短信、通讯录和设置页面，并更新 App 图标。
- 短信支持对话式收发与内容预览；通讯录可读取本机联系人并用于拨号、短信和来电识别。
- 保留短信与 4G 同时在线、来电/短信系统提醒、GPS、eSIM、网络策略和后台守护。
- 生成 macOS Universal DMG，App、后端与 libusb 均包含 arm64 + x86_64；Intel 尚未真机验证。
- 生成 Windows amd64 私有候选包，提供 COM 口发现、Web 控制台及安装/卸载脚本；Windows 不支持 macOS 专属的通话音频、厂商 USB AT/eSIM、原生网络策略、通讯录与地图能力。
- 本候选版仅在本地准备，尚未推送、打标签或创建 GitHub Release。
- 公开源码准备调整：未来公开版不包含模块侧语音运行时及其二进制，不宣传开箱双向语音通话；详情见 `OPEN_SOURCE_SCOPE.md`。

## 2026-08-07 · v1.0（菜单栏信号 / 短信 / 通讯录 / 设置大改版）

- 菜单栏 4G 信号恢复为白色、与系统状态栏一致，字号与电量显示一致。
- 短信改为 iMessage 风格：对话气泡、底部输入框、可直接选取通讯录联系人并自动带入号码。
- 通讯录按「姓在前名在后」显示、复用系统联系人照片，新增手动刷新按钮与提示文字。
- 设置页整合为「状态 / 通用 / 网络 / eSIM / 服务控制」，状态卡自动刷新（2 秒），GPS 与 4G 策略每 5 秒自动同步。
- 运营商显示中文（中国移动 / 中国联通 / 中国电信 / 中国广电，支持英文名与 PLMN 识别）。
- 休眠唤醒后自动恢复 4G 网络；「完全退出」后重新打开应用会自动恢复后端与守护服务。
- 英文界面翻译补全，设置内字体层级与排版统一。
- 本版本仅同步源码，暂未公开发布安装包与 Release。

## 2026-08-05 · v0.1.7-preview（新电脑 4G 上网自动修复）

- 修复“新电脑首次插入调试好的模块无法获得有效 IPv4、无法上网”的问题：
  - 放宽 4G 网卡网络服务识别：不再严格要求硬件端口名为 `Baiwang`，`Baiwang 2`、服务名含 `Baiwang`、本地化名称均可自动识别。
  - 自动启用被 macOS 禁用的 4G 网卡服务（之前禁用状态会被跳过、什么都不做）。
  - 若系统尚未为模块 USB 网卡创建网络服务（新电脑常见），自动通过本地管理 MAC 识别模块网卡并创建 `Baiwang` 网络服务、配置 DHCP，无需手动打开“系统设置”。
  - DHCP 续租持续失败时输出模块 `AT+QCFG="usbnet"` 模式日志，便于诊断网卡未枚举的原因。
- 来电、短信功能不受影响（走 AT 通道）。
- 旧版本均保留：v0.1.6-preview（信号自检与自动找回）、v0.1.5-preview（DHCP 自动续租）、v0.1.4-preview（Windows 实验版）、v0.1.3 / v0.1.2 / v0.1.1-preview（macOS）。


## 2026-08-05 · v0.1.6-preview（信号自检与自动找回）

- 新增信号自检循环（每 8 秒）：持续探测注册状态与信号强度，USB AT 桥掉线时自动重连。
- 信号丢失后分级自动找回：
  - 连续 3 次未注册（约 24 秒）：`AT+CGATT=1` + `AT+COPS=0` 强制重附着并自动选网；
  - 连续 6 次（约 48 秒）：`AT+CFUN=0/1` 射频软重启（USB 保持在线）；
  - 连续 9 次（约 72 秒）：`AT+CFUN=1,1` 整机重启，限频 10 分钟一次。
- 修复 USB AT 打开卡死问题：`openDJIUSBAT()` 增加 12 秒超时保护，超时后 30 秒退避重试，避免 libusb 阻塞导致整个应用/状态接口挂死。
- 每 60 秒检查 4G 网卡 DHCP 地址（正常时静默，不刷日志）。
- 旧版本均保留：v0.1.5-preview（DHCP 自动续租）、v0.1.4-preview（Windows 实验版）、v0.1.3 / v0.1.2 / v0.1.1-preview（macOS）。


## 2026-08-01 · v0.1.5-preview（4G 自动联网修复）

- 新增“模块重连后自动续租 DHCP”：模块 USB 重连、AT 桥重新打开后，自动检查 4G 网卡（Baiwang）是否获得有效 IPv4 地址；没有则自动执行 `networksetup -setdhcp` 续租并等待，最多 30 秒，无需手动重启模块。
- 修复场景：模块掉线重连后 USB 网卡 `en8` 链路恢复但 DHCP 无响应，导致 Wi-Fi 断开时无法自动切换 4G 上网。
- 旧版本均保留：v0.1.4-preview（Windows 实验版）、v0.1.3-preview（macOS 通用）、v0.1.2 / v0.1.1-preview（macOS arm64）。


## 2026-08-01 · v0.1.4-preview（Windows 实验版）

- 新增 Windows 版可执行文件 `DJOneHub-Windows-amd64-v0.1.4-preview.exe`：amd64 单文件，Web 管理界面已内嵌，解压后直接运行，无需安装。
- 通过串口（虚拟 COM 口）连接的 DJI 4G 模块功能可用：短信、GPS、来电提醒、网络信息、Web 管理面板。
- **已知限制**：USB 直连 AT 桥依赖 macOS + libusb，Windows 上不可用，eSIM 管理与 USB AT 通道受限。
- **风险提示**：Windows 版为实验性构建，仅在 macOS 上交叉编译验证，未在真实 Windows + 模块环境实测，可能出现问题，请谨慎下载使用。
- 旧版 macOS 安装包（v0.1.1 / v0.1.2 / v0.1.3）均保留，未覆盖。


## 2026-07-31 · v0.1.3-preview（支持 Intel Mac，通用安装包）

- 新增通用（universal）安装包 `DJOneHub-macOS-universal-v0.1.3-preview.dmg`：一个安装包同时支持 Apple Silicon（M 系列）与 Intel（x86_64）Mac，macOS 13 及以上。
- 主程序、libusb 运行库、通知助手均为 arm64 + x86_64 双架构。
- 本版本不包含菜单栏网速显示（与 v0.1.2 一致）。
- **风险提示**：通用包仅在 Apple Silicon 上交叉编译并验证架构/签名，**未在真实 Intel Mac 上实际测试**，在 Intel 机型上可能出现兼容性问题，请谨慎下载使用。


## 2026-07-31 · v0.1.2-preview（移除菜单栏网速显示）

- 移除菜单栏“实时下载/上传速度”显示，菜单栏只保留 GPS 与 4G 信号图标。
- 管理页面内的实时网速与本次流量统计不受影响。
- 来电/短信提醒、GPS 面板、4G 信号图标等行为保持不变。
- 如果你更喜欢菜单栏显示实时网速，可继续使用 v0.1.1-preview 安装包（旧版保留，未覆盖）。


## 2026-07-30 · GPS 面板与菜单栏信号

本批更新让模块状态在菜单栏直接可见：GPS 定位面板、4G 信号与实时网速。

### 新增

- **原生 GPS 地图面板**：点击菜单栏 GPS 图标打开浮动面板，展示当前位置、卫星数与 HDOP 等定位详情；定位搜索带动画，超时后自动停止扫描并加快状态恢复。面板采用“控制中心卡片”样式，并新增总览玻璃卡片。
- **菜单栏 4G 信号图标**：USB 4G 接管默认网络时，菜单栏显示四格信号与“4G”标识，点击直达控制面板；图标方案预览见 `design-previews/cellular-status-icon-styles.html`。
- **菜单栏实时网速**：显示当前默认网络的下载/上传速率，每秒刷新。
- **菜单栏 GPS 状态指示**：卫星图标与信号格，搜索定位时带动画，超时后自动停止扫描。

### 改进

- 短信面板排版与可读性优化。
- notifier 轮询串行化，避免并发轮询漏掉来电/短信提醒。
