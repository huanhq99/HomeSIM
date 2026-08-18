# MacCellular Android 前台壳

这是 Android 第一阶段的受限 WebView 客户端，只复用 Mac 端已有的同源 PWA。
仓库提供两个能力隔离的构建 flavor：

- `viewer`：默认前台查看状态、读取/刷新/发送短信，只申请 `INTERNET`；
- `callForeground`：独立测试包，额外只申请 `RECORD_AUDIO`，用于验证现有 PWA
  的前台来电媒体链。它必须显式 opt-in 才能构建，且不等于启用 Mac 的接听开关。

两者都复用 Tailscale Serve 身份、App Cap、CSRF 和持久幂等账本；都不提供后台
来电、系统电话 UI、原生通知或可靠后台同步，也不开放拨号、拒接、DTMF 或通用
挂断。相机、定位、文件、联系人、通知和后台服务权限均不存在。

它不是最终原生电话客户端。独立的 [`native-client`](native-client/README.md)
已经实现设备公钥登记和签名只读 SMS，但真正的 Android 日用电话版仍需 FCM、
Core-Telecom、CallStyle、已验证的 SIP/WebRTC 媒体源和真机生命周期证据。

## 构建要求

- Android 8.1（API 27）或更高版本的设备；
- JDK 17；
- Android SDK Platform 35；
- Android Build Tools 35.0.1；
- Gradle 8.9（仓库 wrapper 会固定并校验发行包）。

网关地址是私有运行配置，禁止写入 Git。它必须是精确的
`https://<host>.ts.net/remote/`，使用默认 HTTPS 端口且不能带 query、fragment 或
userinfo。

```sh
cd android
export JAVA_HOME=/path/to/jdk17
export ANDROID_HOME="$HOME/Library/Android/sdk"
export MACCELLULAR_GATEWAY_URL='https://your-private-host.ts.net/remote/'
./gradlew testViewerDebugUnitTest lintViewerDebug assembleViewerDebug
```

默认调试 APK 位于
`app/build/outputs/apk/viewer/debug/app-viewer-debug.apk`，已被 `.gitignore` 排除。
调试签名只用于自有设备测试；私钥、APK 和真实网关主机名均不得提交。

只有准备做前台通话软件/真机验收时，才可单独构建测试包：

```sh
./gradlew testCallForegroundDebugUnitTest lintCallForegroundDebug \
  assembleCallForegroundDebug -PenableForegroundCalls=true
```

缺少 `-PenableForegroundCalls=true` 时构建会 fail closed。该包使用独立
`.calltest` application ID 和明显的“前台通话验收”标签，可与默认 viewer 并存。
首次网页请求麦克风时先拒绝该次请求并申请 Android 系统权限；授权后用户必须再次
点击连接音频，并在原生对话框中选择“仅本次允许”。当前 PWA 每次新 prepare 都会
重新发起 Web 麦克风请求并重新确认；壳层只向精确编译期 origin、当前已 commit 的前台 WebView 授权恰好一个
`AUDIO_CAPTURE` resource，绝不把未知 resource 数组原样放行。

## 安全边界

- 构建时将唯一网关 origin 固定进 APK；缺失或格式不符会失败。
- WebView 只允许该 origin 的 `/remote/` 静态资源、`/api/remote/v1/` 请求，
  以及独立外置语音合同的 `/api/remote/v2/voice/` 请求；其他 v2 前缀仍拒绝。
- 明文 HTTP、用户 CA、TLS 忽略、跨源跳转、混合内容、下载、文件选择、弹窗、
  第三方 cookie、文件/内容访问和 JavaScript bridge 全部禁用。
- Safe Browsing 命中时直接返回安全页且不提交私有 URL 报告。
- JavaScript 只为受控 PWA 启用；Android 不向页面暴露任何 JavaScript bridge。
- 页面退到后台即显示隐私遮罩并销毁当前 WebView，恢复后重新全量同步。
- `FLAG_SECURE` 禁止截图和最近任务预览；Android backup/device transfer 均禁用。
- DOM storage 只保留 PWA 的未决幂等键，避免响应丢失后重复发送短信；短信正文不由
  原生层另行持久化。

真实私有构建会按设计把唯一 `.ts.net` origin 固定进 APK，因此 APK 只留在忽略的
本机构建目录，不提交 Git、也不作为公共发行物。除该预期 origin 外，APK 不应包含
登录凭据、短信、号码或设备标识。

Android 设备必须先连接正确的 Tailscale 网络。远程入口只能是 Tailscale Serve
到 Mac 的 `127.0.0.1:7577`；禁止 Funnel、通用反代或直接暴露端口。

`callForeground` 仍只是前台验收工具。Mac 的 `-remote-incoming-answer` 默认
关闭；模块没有完成受控初始化、媒体租约不完整或服务器门禁失败时，Android 获得
麦克风权限也不能触发 `ATA`。QDC507 的目标组合验证已经失败并回滚，且不允许
第三次持久模块写入。独立外置语音 v2 已可由该 flavor 做前台软件/真机验收，但
`-voice-provider`、`-sip-answer-incoming` 与 `-sip-end-active` 默认关闭；
`callForeground` 与合成/伪 Asterisk 测试不授权启用。软件恢复闭环已实现：
有界 `fsync` 存储加密 provider token，mutation 前
持久 `Arm`，unknown 永不重发；重启只用 GET inspector，只有精确
`ended` 自动 tombstone，`incoming`/`active`/身份不匹配/Resolve 失败均人工
PBX 恢复。Asterisk 启动用 PBX timestamp cutoff、精确 `asterisk_id` 与
第二次 info pin，不把 UserEvent 当作全序屏障。这些都只有软件证据。
在精确网关/SIM/运营商/SKU/固件、Asterisk dialplan/RTP/orphan watchdog、
唯一 `asterisk_id` 与 HTTP+WS 同实例、同号码 SMS API 迁移、Android 前台
真实双向音频、生命周期和恢复故障注入通过前不得启用。真正的日用电话客户端
仍需 FCM、Core-Telecom、原生媒体生命周期和稳定私有 Release 签名；当前
调试或未签名产物不是生产发行包。iOS 仍需后续 PushKit/CallKit 与真实双向语音验收。
