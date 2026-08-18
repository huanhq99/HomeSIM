# 源码结构与产品归属

## MacCellular 产品主线

| 路径 | 作用 |
| --- | --- |
| `cmd/djonehub-macos/remote/` | iPhone/PWA 产品界面、电话、短信、录音、来电提醒 |
| `cmd/djonehub-macos/public_web_gateway.go` | 用户公网域名的受限网页入口 |
| `cmd/djonehub-macos/remote_media.go` | QDC507 直连 WebRTC/TURN 媒体 |
| `internal/remotevoice/`、`internal/sipwebrtc/` | 公网中继与外置语音媒体核心 |
| `deploy/public-web-mac/` | 家中 Mac 安装、迁移、启动和回滚 |
| `deploy/public-turn/` | 公网 TURN 中继部署与探针 |
| `integration/`、`scripts/tests/` | 真实软件媒体和公网链路验收 |

## 硬件与历史兼容层

`cmd/djonehub-macos` 中 USB/AT、短信和模块管理代码，以及部分 macOS App 代码，继承
并演进自 DJOneHub/VoHive 历史。内部路径和符号暂不全量改名，以免破坏持久数据、
LaunchAgent、构建脚本和现有部署。

## 实验与非生产路径

- `android/`：前台壳和原生信任实验，不是当前日常客户端；
- `deploy/public-edge/`：较重的 VPS edge 方案，不是 1.0 默认安装路径；
- `tools/qdc507-*`：硬件实验、兼容验证和回滚工具，不属于普通用户首次安装；
- `cmd/djonehub-macos/mavo_phase*`：未接入生产入口的阶段性硬件实验核心；
- `docs/history/`：私人研发树中的上游、旧版本和实验记录；公开源码候选不包含该目录。

## 整理原则

新功能优先进入清晰的产品主线路径；历史实验必须注明是否部署。对外文档以
`README.md` 和 `docs/PRODUCT_IDENTITY.md` 为入口；公开候选只保留用户安装、产品架构和
当前发布所需文档，不再把完整实验日志堆到 README 或手机用户界面中。
