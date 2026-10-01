# 家信 0.4

网页新增线路选择、设备列表、按卡保存的网络设置、通话记录和回拨填号、通知、诊断日志、eSIM 操作与按设备代理。

每条线路分别保存短信、备注、通话记录和流量；备用号码与网络设置按 ICCID 保存在根数据目录。换卡后可以读取已保存设置并手动应用，服务不会自动选网或改变套餐。

已知 Baiwang AT 接口按 USB 拓扑发现；可手动添加其他模块。默认 Docker 配置仍映射原先的一条串口；`deploy/homesim/compose.multidevice.yaml` 扩展串口、ACM 和声卡设备访问，用于多模块与热插拔，需要确认后启用。没有使用 privileged。当前 NAS 主模块的稳定路径为 `/dev/serial/by-path/pci-0000:00:14.0-usb-0:2:1.2-port0`，启用时对应 `/host-dev/serial/by-path/pci-0000:00:14.0-usb-0:2:1.2-port0`。

通话记录由 NAS 轮询真实语音 CLCC 状态生成；数据呼叫不计入电话，查询失败不伪造挂断。服务或模块重启时未结束的记录标为结果未确认。轮询不能保证捕获两秒以内的短促来电，时长按检测时间估算。

通知支持 Bark、Telegram、SMTP（587，要求 STARTTLS）和 JSON Webhook。首次默认关闭，保存开启后只发送后续新事件。默认正文隐藏；明确勾选后才包含短信正文和号码。密钥写入 0600 文件，不由配置查询返回。发送失败写入诊断日志，避免不确定结果下重复推送。

APN/选网/USSD/eSIM 操作经同一串口命令队列执行，与拨号互斥。USSD 可能办理套餐，界面要求确认。eSIM 必须有兼容 eUICC 卡；支持检测、启停、重命名、删除和分阶段下载。操作前重新读取 EID，不能将普通 SIM 变为 eSIM。激活码不写入持久文件。重启后操作结果未知时要重新检测，不自动重试。

HTTP/CONNECT 与 SOCKS5 TCP 代理强制绑定当前模块的 USB 网口、使用认证、仅允许局域网或本机监听地址，不回退到 NAS 默认网卡。当前 NAS 未发现模块的数据网口，需要模块先建立数据连接。某些内核还要求 `compose.proxy.yaml` 的 NET_RAW 能力；尚未自动开启。代理不是 QMI/拨号管理器。流量仅统计经过家信代理的字节，五分钟采样，支持日/七天/三十天聚合，保留约三十天。

## H-Blog iPhone 电话

`ios/HBlog/HomeSIMCalls.swift` 提供原生 WebRTC、CallKit 拨打/接听/挂断/静音/DTMF。网页只传递用户操作给原生桥；NAS 登录 Cookie 不传入 JavaScript。原生 HTTP 请求不跟随跳转，不绕过 HTTPS 校验。WebRTC 固定使用 stasel/WebRTC 154.0.0，保留其 BSD 许可。

前台来电使用真实线路状态呈现 CallKit。原生音频在系统激活音频会话后启用；已经接通的原生电话不依赖 WKWebView 留在前台。系统/运营商双向音频需真机实测。

锁屏新来电需要 Apple Developer 签名、PushKit 与 NAS APNs：通过 `compose.apns.yaml` 只读挂载 p8，设置 Key ID / Team ID。App 设备页点击开启后配对，凭据存入系统钥匙串，NAS 只保存哈希；有效期九十天，可在 App 关闭并撤销。Debug 使用 sandbox，Release 使用 production；签名推送环境须对应。PushKit 仅用于新来电，收到后立即报告 CallKit，再核对 NAS 状态。APNs 拒绝只写脱敏日志，不重发过期来电。

外网音频需要 TURN。在 NAS 的私有 `.env` 设置 `HOMESIM_TURN_URLS` 和 `HOMESIM_TURN_SECRET`，后端、网页和原生 App 使用一小时凭据，共享密钥不进入前端。仅 frp 转发 8580 不会转发 WebRTC 音频。TURN、APNs、第二个模块、eSIM 卡和运营商实际通话尚需现场验证。

## 升级与回退

升级前保存 data、当前源码、Compose 文件和旧二进制，禁止在有音频进程时升级。0.4 可用原来的单模块 Docker 权限启动；USB 热插拔、代理额外能力与公网 TURN 分开启用。回退旧程序时保留新增数据文件，不还原整个 data，以免覆盖升级后新短信。

eSIM 依赖的 CI 证书包来自 https://euicc-manual.osmocom.org/docs/pki/ci/，补齐上游生成指令要求的文件。根 LICENSE、NOTICE、THIRD_PARTY_NOTICES.md 保留原项目条款。

0.4.1 关闭登录用户名字段的自动大写、自动纠错与拼写检查，避免移动端改变用户名。iOS 27 模拟器的新增网页输入测试无法可靠输入，未据此宣称 App 登录或电话成功；原有入口和连接设置测试通过。
