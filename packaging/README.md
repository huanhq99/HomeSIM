# MacCellular for macOS

MacCellular 把连接在 Mac 上的兼容 USB 蜂窝模块变成自托管的电话与短信终端。
macOS 1.0 首发支持 Apple Silicon 和已验证的 QDC507/Baiwang 组合。

> **这不是插上模块即可使用的托管电话服务。** 安装包提供程序和配置向导，但不附带
> USB 蜂窝模块、实体 SIM、域名、Cloudflare 账户、Tunnel、TURN 服务器或模块侧语音
> 运行文件。用户必须自行准备这些资源，并确认 SIM、固件和运营商支持所需功能。

## 使用前准备

- 本机短信：Apple Silicon Mac、兼容模块、本人合法控制且已正常注册网络的实体 SIM；
- 手机远程短信：另需自有域名、Cloudflare Access application、Named Tunnel 和访问策略；
- 手机远程电话：另需已验证的模块语音环境、模块运行文件、独立 coturn、TURN 域名、
  证书和公网可达端口；
- 后台来电提醒：iPhone 需要把 PWA 添加到主屏幕，并允许通知。

安装向导不会替用户购买、创建或托管上述服务。只需短信时可以不部署 TURN；需要公网
电话时，不能跳过 TURN 和模块语音环境。

## 安装

### DMG

1. 打开 `MacCellular-macOS-…dmg`；
2. 双击“安装 MacCellular.command”；
3. 安装完成后打开 MacCellular App；
4. 需要手机远程访问时，再双击“配置手机访问.command”并按提示填写自己的域名和 Cloudflare 信息。

DMG 已包含后端、macOS App、音频 helper、libusb、安装器及许可证。普通用户不需要安装 Go 或手工复制 LaunchAgent。

### CLI ZIP

完整解压后运行：

```sh
./install
maccellular start
```

CLI 默认只监听 `127.0.0.1:7576`，适合本机管理和开发者诊断。面向手机的公网 PWA 应使用 DMG 中的配置向导。

## 已验证能力

- USB/libusb 模块发现、SIM/LTE 状态和短信收发；
- QDC507 直连呼入、呼出、接听、挂断和 DTMF；
- 双向 WebRTC 语音、最近通话和通话录音；
- iPhone/Android PWA、Cloudflare Access/Tunnel 和 TURN relay；
- 来电 Web Push、安装升级、服务回滚和数据迁移。

这些结论适用于仓库中列出的已验证硬件、固件和模块侧语音环境。其他模块应先运行兼容性检查。

## 数据与隐私

短信、最近通话、录音和配置保存在当前用户的 Mac，不上传到项目维护者服务器。公网域名、Cloudflare、TURN 和通知密钥全部由用户自行配置，发行包不包含维护者的生产部署信息。

模块侧的三个语音运行文件不随本仓库或安装包再分发。安装向导会检查兼容环境；缺少时会给出明确结果，而不会静默修改模块。

许可证和第三方来源见包内 `LICENSE` 与 `THIRD_PARTY_NOTICES.md`；合法用途、运营商、
录音、个人信息和紧急呼叫边界见 `合法与负责任使用.md`；默认数据流和卸载保留策略见
`数据与隐私说明.md`。
