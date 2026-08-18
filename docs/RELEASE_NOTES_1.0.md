# MacCellular 1.0

MacCellular 把连接在常驻 Mac 上的受支持 USB 蜂窝模块，变成可从 iPhone、Android
和电脑浏览器使用的自托管电话与短信服务。

> `v1.0.0-rc.4` 是公开测试候选，不是生产就绪声明。自动化、安装包和既有真机通话
> 链路已经通过；模块重插后的完整恢复和最新版 MP4 录音手机播放仍需在正式
> `v1.0.0` 前完成最后实机复验。

## 下载前须知

这不是即插即用的托管电话服务。发行包不包含模块、SIM、域名、Cloudflare/Tunnel
账户、TURN 服务器、Apple 公证签名或模块侧语音运行文件。用户必须自行准备并配置
这些条件；只需要短信的用户可以不部署 TURN，公网电话则必须具备兼容语音环境和
可用的 TURN 服务。详见 [安装与首次配置](GETTING_STARTED.md)。

## 主要能力

- 可安装到手机主屏幕的 PWA；
- 呼入、呼出、接听、挂断和通话中 DTMF；
- 通过 TURN relay 的 WebRTC 双向语音；
- 短信增量同步、会话视图和发送；
- 最近通话、未接来电和本机通话录音；
- 同一通话只保留较完整的主录音，过滤结束阶段产生的短尾片段；
- Web Push 来电提醒；
- Cloudflare Access 与 Tunnel 公网入口，家庭网络无需开放入站端口；
- macOS DMG、普通用户安装器、手机访问配置向导和可重复卸载。
- 应用、配置和用户数据统一存放在 `Application Support/MacCellular`。

## 1.0 支持边界

- 首个正式目标是 Apple Silicon Mac 与已验证的 QDC507/Baiwang 模块组合；
- 手机客户端是 PWA，不是 App Store/TestFlight 原生应用；
- 1.0 只支持个人单模块、单实体 SIM，不提供卡池或批量通信能力；
- 模块侧语音运行文件不随源码或 DMG 再分发；配置向导可在用户确认后从原始项目准备；
- Android 原生壳、Windows、外置 SIP 网关和 VPS edge 均不属于 1.0 默认产品路径。

## 数据与隐私

短信、最近通话、录音和配置保存在用户自己的 Mac。公开仓库和发行包不包含维护者的
域名、服务器、账户、电话号码、短信、录音或部署凭据。

完整安装步骤见 [GETTING_STARTED.md](GETTING_STARTED.md)，来源与第三方边界见
仓库根目录 `THIRD_PARTY_NOTICES.md`。

合法用途、运营商、录音、个人信息和紧急呼叫边界见仓库根目录
`LEGAL_AND_RESPONSIBLE_USE.md`。
