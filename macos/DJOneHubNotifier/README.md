# MacCellular macOS App

MacCellular 的 macOS 原生 App。网页关闭后仍可显示来电和短信，并承载本机
CoreAudio/UAC 主机边界：

- 每秒读取来电状态，显示 iOS 风格悬浮卡片。
- “拒接”调用 DJOneHub 挂断接口；“详情”打开管理页面。
- 每三秒读取短信列表，只提醒启动后新收到的短信。
- 对目标模块执行只读 UAC/CoreAudio 预检，并在后端确认同一通话代际后管理本机
  音频生命周期；模块配置和 AT 通话控制仍由 Go 后端串行执行。
- 不安装、不启动也不依赖外置 MaVo 模块侧运行时。QDC507 当前只接受短信/
  数据能力，模块原生 `QPCMV` UAC 已冻结为研发路径；生产语音候选是另行验收的
  VoLTE-to-SIP 网关、Asterisk 与独立 v2 WebRTC 链路。
- 不改变短信模式、上网模式或网络切换规则。

## 构建

```bash
./build-app.sh
```

构建脚本会使用项目内缓存、执行内置自检、生成临时签名的 App，并验证签名和 `Info.plist`。

输出位置：

```text
dist/DJOneHubNotifier.app
```

## 验证

```bash
dist/DJOneHubNotifier.app/Contents/MacOS/DJOneHubNotifier --health-check
dist/DJOneHubNotifier.app/Contents/MacOS/DJOneHubNotifier --preview call
dist/DJOneHubNotifier.app/Contents/MacOS/DJOneHubNotifier --preview sms
```

`--health-check` 只输出接口解析状态和条数，不输出号码或短信内容。

UAC/CoreAudio 描述符检查只属于 preflight。当前尚未用真实通话验证双向声音；
UDS/Pion 只完成合成 PCM 测试。旧 QDC507 拨号、拒接、DTMF 与通用挂断仍返回
`409`；另行实现的外置 SIP v2 Answer/End 也默认关闭，尚不能视为生产能力。

## 常驻运行

默认安装位置：

```text
~/Library/Application Support/MacCellular/notifier/MacCellular.app
```

发行包的安装脚本会按当前 macOS 用户目录生成独立 LaunchAgent，再通过 `launchctl bootstrap` 注册。助手要求 MacCellular 版后端继续监听 `http://127.0.0.1:7576/`。
