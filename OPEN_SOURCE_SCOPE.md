# 公开源码与发行范围

## 公开仓库包含

- MacCellular 的 Mac 后端、PWA、WebRTC/TURN 媒体、短信、通话、录音与 Web Push；
- macOS UAC helper、安装迁移工具、coturn 和可选 Asterisk 集成；
- USB/AT、eSIM 和模块管理兼容层；
- 自动化测试、模拟器、公开硬件验收记录和第三方许可证。

## 公开仓库不包含

- SIM、IMEI、IMSI、ICCID、电话号码、短信、录音和联系人；
- Cloudflare、TURN、VAPID、ARI 或其他部署凭据；
- 维护者的真实域名、服务器、账户、API 标识、生产配置和本机运行目录；
- `qdc507_aprv3.ko`、`qdc507_voice.ko`、`mavo-pcm-bridge.armv7`。

最后三份模块侧运行文件是当前已验证 QDC507 语音环境的依赖，但不由本仓库制作，
也不随源码或安装包再分发。`scripts/prepare-module-voice.sh` 由用户在自己的 Mac 上从
MaVo 固定源码版本直接准备本机缓存；安装包不代理、镜像或重新发布这些文件。仓库中的
host-side 代码和检查工具可以公开。

## 产品能力与发行物

在已验证的硬件和本机外部运行环境中，QDC507 直连电话、双向语音、DTMF、
挂断和录音已经实际工作。这个事实不等于任意 QDC507 都可直接使用。公开 1.0 安装包
只能承诺“自动检查已支持环境并给出明确结果”，不能把缺少的模块侧运行文件伪装成
安装包自带内容。

可选的外置 SIP/Asterisk 路径保留为兼容层，不是使用 MacCellular 必须购买的硬件。

1.0 公开产品只支持个人单模块、单实体 SIM，不提供卡池、批量插卡、改号、批量通信或
第三方线路转售。完整使用边界见 `LEGAL_AND_RESPONSIBLE_USE.md`。

## 许可证

根目录 `LICENSE` 与 required notice 必须随源码保留。MaVo host-side 适配和 Pion 等
第三方组件继续适用各自许可证，见 `THIRD_PARTY_NOTICES.md`。公开页面应使用
“source-available / 源码公开”描述当前许可，不使用容易被理解为 OSI 许可证的
“完全开源”表述。
