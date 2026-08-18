# 默认部署方案

MacCellular 的默认部署目标是一台常驻家中的 Apple Silicon Mac、一块受支持的 USB
蜂窝模块，以及用户自己控制的公网域名。

本仓库不托管公共入口，也不提供域名、Cloudflare、TURN、SIM、运营商套餐或模块侧
运行文件。部署者必须自行取得并维护这些资源；默认方案描述的是推荐拓扑，不是下载
后自动存在的基础设施。

## 日常访问

- 手机通过 Cloudflare Access 保护的 PWA 使用电话和短信；
- 家庭 Mac 通过 Cloudflare Tunnel 主动建立外连，不开放家庭网络入站端口；
- 公网语音使用独立 coturn，浏览器媒体采用 relay-only；
- Tailscale 只作为维护和故障诊断后备，不是普通用户日常入口。

## 本机边界

公网入口只注册用户需要的页面、短信、电话、媒体和通知接口。USB、AT、硬件实验、
本机管理、调试和服务控制接口保持在 loopback 或本机进程间通道中。

## 数据边界

短信、最近通话、录音和 Push 订阅保存在 Mac 的 Application Support 目录。用户可以
再把录音和通信数据备份到自己的 NAS，但 NAS 不是实时通话链路的依赖。

## 发布配置

公开仓库只使用 `phone.example.com`、`turn.example.com` 和示例账户。维护者实际使用
的域名、服务器、邮箱、Cloudflare 标识、Tunnel、TURN、VAPID 和其他部署参数不进入
源码、文档、测试夹具、截图或发行包。
