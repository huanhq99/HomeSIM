# Public edge deployment skeleton

## 结论与当前状态

这是一个**严格默认关闭、尚未部署**的公网 VPS 骨架。它用于让 iOS/Android 在不常驻 Tailscale 的情况下，通过公网控制中继访问家中 Mac 上的 DJI 4G 网关：

```text
iOS / Android
    |
    | HTTPS / WSS
    v
phone.example.com -- Cloudflare Tunnel --> edge:8080（仅容器网络）
                                                ^
                                                |
                                      家中 Mac 主动建立 WSS
```

Tailscale 可以继续用于管理或故障排查，但不是手机访问的前置条件。家中 Mac 不需要公网入站端口；它只主动连接 VPS 控制面。Asterisk 的本地控制接口必须继续限制在家中 Mac 回环地址，不能被 Tunnel、edge 或 TURN 反向代理。

首发合同是 **edge-only**：只允许启动已实现的只读 `edge` 与
`cloudflared`。`start-turn` 在部署入口中固定失败并且不会触发任何
Docker 或 TURN 变更。Compose 中保留的 coturn/`turn` profile 只是未来
迁移材料，不是当前可执行的上线路径：

- `edge` profile：首发唯一可用 profile，启动只读 edge 和 `cloudflared`。
- `turn` profile：未来迁移占位；首发不支持启动。
- 不指定 profile 时不会选择任何服务。

本目录已经有可复现的 Linux/amd64 edge 镜像定义、source/toolchain lock 流程、SBOM/CVE/架构/rootfs/健康/startup 证据门以及受控回滚入口，但**尚未实际构建或审计镜像，也没有启动任何服务**。Tunnel token、TURN secret 和证书仍不在仓库。不能把静态测试当成生产镜像或公网部署证据。

## 未来 TURN 迁移中的两个公网名字

| 名称 | Cloudflare 状态 | 指向 | 用途 |
| --- | --- | --- | --- |
| `phone.example.com` | Proxied，由本项目独立 Named Tunnel 发布 | Tunnel 内部的 `http://edge:8080` | HTTPS/WSS 控制面 |
| `turn.example.com` | **DNS only** | VPS 公网 IPv4 | TURN UDP/TCP/TLS |

普通 Cloudflare Tunnel/CDN 不能代替通用 TURN/UDP 转发，因此 `turn.example.com` 不得打开橙云代理。此骨架只配置 IPv4；未完成 IPv6 监听和安全组验证前不要增加 AAAA 记录。

只复用现有公网项目的部署模式，不复用它们的 Tunnel token、容器网络或挂载目录。

## 骨架已经锁定的边界

- edge 没有 `ports`，只在 Compose bridge 网络中暴露容器端口 `8080`。
- cloudflared 只从 0600 文件读取 Tunnel token；token 不进入环境变量或命令行参数。
- coturn 仅加入 `172.30.247.0/28` 专用 bridge，固定为 `172.30.247.2`；它不使用 host network，也不加入 control network。这是待 VPS 实地检查的低冲突候选网段，如与既有 Docker 网络或宿主路由重叠则必须停止部署，并同步修改 Compose、generator、verifier 和测试。
- cloudflared/coturn 必须是显式 `name@sha256:<64 hex>`。edge 可以使用已推送并核验的 RepoDigest，也可以在 air-gap/VPS 本地加载后使用精确 `sha256:<target-id>`；后者只对 edge 开放，启动时会同时核对 Compose 的 `.Config.Image` 与容器实际 `.Image`。首发合同精确限定 Docker 29.1.3 containerd image store；该 target ID 是 OCI manifest/index digest，不假定等于 config digest。本地 tag 从不等同 RepoDigest。所有服务均为 `pull_policy: never`。
- Compose 中定义的容器均显式使用 `runtime: runc`、私有 IPC/cgroup namespace、
  只读根文件系统、`cap_drop: ALL`、`no-new-privileges:true`、
  `apparmor=docker-default` 与 `seccomp=builtin`，并设置 PID、CPU、内存和日志上限。
- 宿主 TCP 443 精确映射到 coturn 容器的非特权 TCP 5349，因此 coturn 无需加回 `NET_BIND_SERVICE` 或其他 capability。
- coturn 宿主只发布 TCP/UDP 3478、TCP 443 和 UDP 49160-49167；`no-rfc5780` 锁住 alternate-listener 行为，3479/444 不在配置或发布白名单。真实 coturn 镜像定版后还必须做容器内外监听 smoke。
- Compose 不挂载 Docker socket，不挂载其他项目、用户主目录或 NAS 路径，也不发布本地控制接口或端口 7576。
- TURN REST shared secret 只允许出现在 0600 secret 文件与生成的 0600 `turnserver.conf` 中。生成器只接受单行、无填充的 base64url 值，阻止追加配置指令。
- TURN 拒绝回环、组播和常见私网/保留网段 peer，降低把 TURN 当作内网扫描跳板的风险。

这些是**静态部署约束**。只读 edge 应用和内嵌的系统浏览器手机 UI 的
源码/本地测试已完成，但生产镜像、真实 Cloudflare Access/Tunnel、iOS 与
Android 实机端到端均尚未验收。不得声称 live build、公网访问或手机可用已通过。

## 未来 TURN/ICE 迁移的固定资源预算

本骨架有意保持一个适合单用户早期验收的极小范围：

| 项目 | 固定值 |
| --- | --- |
| TURN UDP/TCP | `3478` |
| TURN/TLS TCP | 宿主 `443` → 容器 `5349` |
| UDP relay | `49160-49167`（8 个端口） |
| 单用户 allocation quota | `4` |
| 总 allocation quota | `8` |
| 单 allocation 带宽上限 | `256000 bytes/s`（约 `2 Mbps`） |
| 总带宽预算 | `2000000 bytes/s`（约 `16 Mbps`） |
| allocation 最长生命期 | `600s` |

`no-tcp-relay` 禁用 RFC 6062 的 TCP peer relay，不会禁用客户端通过 TCP 3478 或 TLS 443 连接 TURN。DTLS 已关闭，因此无需开放 UDP 443。未来媒体迁移的移动端必须设为 relay-only（WebRTC `iceTransportPolicy: "relay"`），只接受 TURN relay candidate，不尝试或回退到 direct/host/srflx 媒体路径。

8 个 relay 端口只是受控首发预算，适合少量同时通话测试。容量增加必须同时评估端口范围、`total-quota`、带宽、腾讯流量费用和滥用监控，不能只扩大防火墙范围。

## 当前只读 edge 契约

Tunnel 证明的是 cloudflared 可以连接 Cloudflare，**不证明手机或家中网关身份**。当前程序只实现以下窄边界：

- `GET /healthz` 只返回服务存活，不泄露家中 Mac 是否在线。
- `GET /api/public/v1/state/snapshot` 必须通过 Cloudflare Access JWT 验签和精确邮箱白名单；只返回服务、通话和短信的粗粒度枚举状态，不含号码、正文、SDP、ICE 或命令。
- `WSS /internal/v1/gateways/connect` 只允许一个精确 gateway ID，经 P-256 challenge 签名建立单活租约；Mac 只发完整状态快照，事件、操作和任意转发都会被拒绝。
- edge 不信任 Tailscale、旧 Cloudflare 用户身份或 `X-Forwarded-*` 身份头；Cloudflare 转发元数据即使存在也不参与授权。
- 所有 mutation、enrollment、push 和 TURN 凭据签发开关都固定为 `false`。内嵌只读 UI 的源码和本地测试已完成，但真实 Cloudflare、iOS/Android 浏览器尚未验收，所以这个切片不等于手机已可用。

## 以后开放写操作前的必要契约

只有只读公网链路通过实机验收后，才能单独实现和审计下列能力：

1. 手机首次 enrollment 使用短时、单次、带服务端哈希的一次性码；手机本地生成不可导出的设备密钥，服务端保存 `device_id` 和公钥，可单设备撤销。
2. 每次 HTTP 写请求绑定 method、规范化 path、body hash、时间戳、nonce 和 device id 做签名；服务端执行时钟窗、nonce 去重、body 限额和速率限制。
3. WSS 建连先完成服务端 challenge 和设备签名验证，连接令牌短时轮换；家中 Mac 使用独立 gateway 身份，不能复用手机凭据。
4. 家中 Mac 只主动发起 WSS。公网 edge 只允许高层短信/通话命令，不提供任意 URL、任意 SIP 或本地控制路径透传。
5. TURN 临时用户名采用短时到期时间并绑定已 enrollment 的设备，密码由 edge 使用 shared secret 计算；凭据有效期不超过 10 分钟，不能把长期 shared secret 发给手机。
6. 日志默认脱敏，不记录 Tunnel token、设备私钥、TURN shared secret、短信正文、通话音频或完整 Authorization header。
7. APNs/FCM push 只携带不透明 `event_id` 和最小类型，手机回连受认证接口取数据；push payload 不携带短信正文或长期凭据。

若需要让 VPS/Cloudflare 也无法读取短信内容，必须在 enrollment 之后增加手机与家中 Mac 之间的应用层加密；HTTPS/Tunnel 本身不是这种端到端加密。通话媒体应使用 WebRTC DTLS-SRTP，TURN 只转发加密媒体。

## 持久化、幂等与 `unknown`

公网链路会在“操作已执行但 ACK 丢失”时产生不确定性。短信发送和呼叫等有副作用操作不能在超时后盲目重试：

- 手机为每次操作生成稳定的 `command_id` / idempotency key。
- edge 在返回接收成功前先持久化命令、设备、状态版本和审计时间。
- 家中 Mac 在执行前持久化 `command_id`，重复收到同一命令时返回原结果，不再次发短信或拨号。
- 明确区分 `queued`、`delivered_to_gateway`、`executing`、终态以及 `unknown`；失联或 ACK 丢失进入 `unknown`，由状态查询/人工确认收敛。
- WSS 断线重连按已确认序号补发事件，服务端和客户端均须按 event id 去重。
- push 不是交付凭证，APNs/FCM 成功只表示通知服务接受了消息。

这部分必须由 edge 与 Mac gateway 的数据库迁移、故障注入测试和恢复测试证明；Compose 无法提供这些语义。

## 首发 edge-only 运行配置

依赖：Bash、常见 Unix 工具、OpenSSL 与 Python 3。首发目标 VPS 的已观测
工具链合同如下；任一项不匹配都必须停止，不能用“版本差不多”代替：

| 项目 | 精确合同 | 目标现状 |
| --- | --- | --- |
| Docker client/server | `29.1.3` / `29.1.3` | 已观测，仍由脚本实时复验 |
| Docker daemon 平台 | `linux/amd64` | 已观测，仍由脚本实时复验 |
| image store | `io.containerd.snapshotter.v1` | 已观测，仍由脚本实时复验 |
| Docker `SecurityOptions` | 精确 `["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]` | build/audit/deploy 在变更前实时复验；任何缺项、多项、顺序或值变化均 fail closed |
| Compose 语义版本 | `2.40.3` | 已观测，仍由脚本实时复验 |
| Ubuntu Compose 包 | `docker-compose-v2=2.40.3+ds1-0ubuntu1~24.04.1` | 必须精确匹配 |
| Compose 系统插件 | `/usr/libexec/docker/cli-plugins/docker-compose` | SHA-256 `d87a11e944c990dc9f2186115b1136c1cbffffc870845caff0cbdcce0780f41d` |
| Buildx 系统插件 | `v0.30.1`，`/usr/libexec/docker/cli-plugins/docker-buildx` | **当前目标缺失**；SHA-256 `c37114fcd034025ec68e224657c8a5a850df472ded3ddcbca75ad3a7ebb9710d` |

Compose 在 `check` 中只解析 YAML，不连接 daemon。真实 build/audit/deploy 在第一次
变更前会精确核对上表工具链及插件物理路径、所有权、可写性、hash 和
daemon `SecurityOptions`。这是 production 前置门，不是当前已取得的 production
runtime evidence。
当前 VPS 缺失 Buildx，因此不具备 live build 前置条件，也不得宣称 live build
已通过。

管理员必须在脚本之外独立取得官方 Linux/amd64 Buildx v0.30.1 二进制，先在
安全输入位置校验上述 SHA-256，再安装为 root-owned 且组/其他用户不可写的
系统插件。不允许用用户目录、symlink 或 PATH 中的同名程序代替：

```bash
test "$(sha256sum /secure-input/docker-buildx | awk '{print $1}')" = \
  c37114fcd034025ec68e224657c8a5a850df472ded3ddcbca75ad3a7ebb9710d
sudo install -o root -g root -m 0755 \
  /secure-input/docker-buildx /usr/libexec/docker/cli-plugins/docker-buildx
```

使用已授权的 `sudo -n docker` 时，还必须由管理员建立锁定的空 root
Docker CLI 配置。目录链和文件必须 root-owned、不可被 group/world 写入，
`config.json` 必须是单硬链接、`0644` 且为精确的 `{}\n`，SHA-256 为
`ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356`：

```bash
test "$(sha256sum deploy/public-edge/image/docker-cli-root-config.json | awk '{print $1}')" = \
  ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356
sudo install -d -o root -g root -m 0755 /etc/dji4g-public-edge
sudo install -d -o root -g root -m 0755 /etc/dji4g-public-edge/docker-cli
sudo install -o root -g root -m 0644 \
  deploy/public-edge/image/docker-cli-root-config.json \
  /etc/dji4g-public-edge/docker-cli/config.json
test "$(sha256sum /etc/dji4g-public-edge/docker-cli/config.json | awk '{print $1}')" = \
  ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356
```

这份 canonical 文件不含凭据，`0644` 是为了让非 root wrapper 能在调用
`sudo -n docker` 前自行核对字节；安全边界是 root ownership、完整父链不可写、
单硬链接与精确 SHA，而不是保密。不得在这里加入 registry auth、proxy、
credential helper 或 CLI plugin 配置。

首发只生成 `edge` profile，完全不要求或保存 coturn 镜像、公网 IP、
shared secret、TLS 证书/私钥。

管理员只做一次父目录授权；runtime 与动作证据使用两个互不包含的物理目录。
`/srv/dji4g-public-edge` 必须由管理员预先创建为 `ubuntu:ubuntu` `0700`；
`runtime-v1` 必须尚不存在，随后由 `ubuntu` 通过 generator 创建。不要 sudo
generator/deploy 脚本，也不要把用户加入 docker group：

```bash
sudo install -d -o ubuntu -g ubuntu -m 0700 /srv/dji4g-public-edge
sudo install -d -o ubuntu -g ubuntu -m 0700 /srv/dji4g-public-edge-actions
```

随后全部由 `ubuntu` 运行，并为每版使用一个尚不存在的子目录。下面的尖括号仍是占位符：

```bash
cd /path/to/dji-4g-mac-phone

deploy/public-edge/generate-config.sh \
  --output /srv/dji4g-public-edge/runtime-v1 \
  --edge-image 'sha256:<audited-local-image-id>' \
  --cloudflared-image 'cloudflare/cloudflared@sha256:<64-lowercase-hex>' \
  --cloudflare-token-file /secure-input/cloudflare-tunnel.token \
  --edge-gateway-id 'home-gateway-1' \
  --gateway-public-key-file /secure-input/gateway-public-key.pem \
  --access-team-domain '<team>.cloudflareaccess.com' \
  --access-audience '<Cloudflare-Access-application-AUD>' \
  --access-allowed-emails-file /secure-input/access-allowed-emails \
  --profile edge
```

默认只是 dry-run，并且会完整验证 edge/cloudflared 镜像引用、整数、路径、权限、token、gateway/Access 身份和配置注入。首发不要生成任何包含 TURN 的 runtime。确认计划后，用完全相同的参数追加 `--apply`。生成成功后再次运行：

```bash
deploy/public-edge/verify-config.sh /srv/dji4g-public-edge/runtime-v1
```

生成后必须通过唯一部署入口再验证一次：

```bash
deploy/public-edge/deploy.sh \
  --runtime-root /srv/dji4g-public-edge/runtime-v1 \
  check
```

该入口拒绝 shell 中任何同名 `DJI4G_*` Compose 变量，在清洁环境中解析固定的八个生成值，并对规范化后的服务、profiles、镜像、挂载、端口、网络、资源和权限做完整白名单比对。不得绕过它直接执行 Compose。

生成目录为：

```text
/srv/dji4g-public-edge/runtime-v1/     0700
  compose.env                          0600
  config/                              0700
    edge.env                           0600
    gateway-public-key.pem             0600
    access-allowed-emails              0600
  secrets/                             0700
    cloudflare-tunnel.token            0600
```

首发 runtime 不得加入 TURN 输入或 profile。TURN 必须等真实 edge-only 链路验收后，
在后续独立迁移中重新设计、审计和授权。

`start-edge` 不会让 Compose 直接消费上述可编辑生成目录。它会在同一个、当前
runtime 用户拥有且模式为 `0700` 的父目录内，创建随机命名的私有 staging 目录，
用 `O_NOFOLLOW`、`fstat` 和同一文件描述符稳定读取五项输入，再以 no-replace
原子提交为 `.<runtime-name>.edge-snapshot.<random>`。快照三个目录均为 `0500`，
五个文件均为 current-user-owned、regular、单链接、`0400`；快照内的
`compose.env` 会把 root 与 edge env-file 改写为该随机快照的绝对路径。随后
Compose 的实际 `--env-file`，以及 edge 公钥/allowlist、cloudflared token 的所有
bind source，都只指向这份快照。即使生成目录在 `up` 期间发生 A→B→A 替换，
容器也不会读取 B。

成功启动后快照必须保留，因为它就是运行中容器的实际只读 bind source。已验证
回滚只会在动作证据落盘后删除其精确 snapshot inode；无法证明回滚完成时会保留
快照，避免移除仍可能被容器使用的文件。`stop-edge` 的 precheck 仍以精确
project/service/container 身份做紧急 remediation，不会因快照内容漂移而拒绝停止；
只有在 edge/cloudflared 容器和 control network 均已证明删除后，才按随机路径、
inode 与 exact tree 删除对应快照。多份、换 inode、symlink 或多余文件不会被
冒险删除，并会使完成证据降级为失败/incomplete 事实。
配置、allowlist、公钥或 token 轮换必须先走 `stop-edge`，再生成新的版本 runtime
并重新 `start-edge`；不要手改运行中的快照，也不要把旧快照当作可复用配置。

不要把生成目录放入 Git 仓库。不要提交 generator 的输入文件、输出文件或命令行历史中的 secret。

## 未来部署步骤（本次未执行）

以下顺序是 edge-only 上线 runbook，不是当前完成状态：

1. 按 [`image/README.md`](image/README.md) 的两阶段提交生成真实 `source.lock`，再进行双 build、rootfs、SBOM、Grype 和受限 startup 审计。当前这些 live 门尚未执行。
2. 对 edge 和 cloudflared 镜像做来源、版本、SBOM/CVE 与架构审计，记录不可变 digest；确认所选 cloudflared 版本支持 `tunnel run --token-file`，并在 VPS 上预先取得这些 digest。由于 `pull_policy: never`，缺少本地镜像时 Compose 必须失败。
3. 创建 Cloudflare Named Tunnel，将 `phone.example.com` 的 Public Hostname 指向 `http://edge:8080`，取得 token 并保存为 0600 文件。确认 Cloudflare 的公网 TLS 证书生效。
4. 明确允许 cloudflared 所需的公网出站连接；不要为 edge 增加任何宿主机入站端口。
5. 仅在全部前置项通过后，通过唯一安全入口显式启动 `edge` profile。下列命令本次没有运行：

```bash
deploy/public-edge/deploy.sh \
  --runtime-root /srv/dji4g-public-edge/runtime-v1 \
  --edge-evidence /srv/dji4g-public-edge/image-evidence-v1 \
  --state-evidence /srv/dji4g-public-edge-actions/start-edge-v1 \
  start-edge

deploy/public-edge/deploy.sh \
  --runtime-root /srv/dji4g-public-edge/runtime-v1 \
  --state-evidence /srv/dji4g-public-edge-actions/stop-edge-v1 \
  stop-edge
```

脚本必须以 `ubuntu` 运行。若它不能直接访问 daemon，只在 Docker/Compose 子调用处自动使用已有的 `sudo -n docker`，因此 runtime 文件和容器 UID:GID 仍是 `1000:1001`，不会变成 root。`start-edge` 会拒绝 orphan，等待 edge `/healthz` 和 cloudflared 本地 metrics `/ready` 同时健康，并验证二者没有退出或重启；失败时只尝试停止并删除这两个服务，绝不碰 coturn。若回滚不完整，证据会明确写成 incomplete，不会声称成功。cloudflared `tunnel ready` 能力虽已由固定 Compose 命令和静态策略锁定，仍须以最终选定镜像做一次真实 startup smoke。`stop-edge` 同样只处理 edge+cloudflared。

`image-evidence-v1` 是精确 edge 镜像在隔离审计容器中的 startup evidence，
不是 production Compose runtime evidence。只有在目标 VPS 实际执行后产生并
通过验证的 `start-edge-v1`/`stop-edge-v1` 动作证据，才能证明采集时的
production 容器/网络状态。运行时检查会精确绑定 `.AppArmorProfile ==
"docker-default"`，以及 `HostConfig.SecurityOpt` 中且仅有
`no-new-privileges:true`、`apparmor=docker-default`、`seccomp=builtin`；还会绑定
`runc`、私有 IPC/cgroup namespace、镜像 ID、UID:GID、资源、挂载、端口、健康、
重启次数与项目网络成员。Docker 29 API v1.52 的 network `Status`
会携带动态 IPAM 计数；验证器会精确检查其 schema、CIDR 和地址总量，
但不把动态计数冒充网络身份。`CpuRealtimePeriod/Runtime`、`CpuCount`、
`CpuPercent` 按 Docker 实际 JSON 键锁为 0，`Sysctls` 仅接受缺失或空值，
masked paths 仅接受固定基础集与 Docker 按 CPU 数字顺序追加的
`thermal_throttle` 安全屏蔽。即使该动作证据为 `completed`，也只证明采集时
的本机 production runtime 合同，不证明真实 Cloudflare、iOS/Android 或端到端链路已验收。

`--state-evidence` 的父目录必须是当前 `ubuntu` 用户拥有的真实规范路径且精确为 `0700`；它不能等于 runtime root、位于 runtime root 内，也不能包含 runtime root。启动/停止前后 coturn 必须同为不存在，或保持同一 container ID、image ID、StartedAt、RestartCount 和运行状态；只有精确不变时结果才会写 `completed`。第一次容器变更前，部署入口从上述不可变快照再次用同一文件描述符读取 `compose.env`、`edge.env`、gateway 公钥与 Access allowlist，写入私有 `runtime-inputs.pre.json` 预锚，并把这四项非秘密实际字节以四个 0600 artifact 一并封入动作证据。动作清单创建时，live 快照、记录字节、四项 SHA-256 与预锚必须精确相等；快照仍存在时，离线复验也会拒绝任何快照漂移。快照按已验证 rollback/stop 生命周期删除后，历史证据仍能用记录字节重放 Compose identity、edge env 与 pre/post runtime-state，而不是依赖已经删除的路径。Cloudflare token 仅存在于私有快照并供 cloudflared bind 使用；它不进入动作证据，也不记录可供离线猜测的摘要。

`completed` 的 action 目录有固定 artifact 集，验证时会重放 pre/post/网络清理状态门；`start-edge` 还会重跑完整镜像证据，两个动作都会重跑 source provenance。第一次容器变更后的日志、采证、验证、脱敏、权限或最终移动任一步失败，`start-edge` 都会尽力回滚并保留明确的 incomplete 证据；`stop-edge` 不会擅自重启服务，而是保留当时能取得的事实。失败日志在发布证据前会同时替换 Cloudflare token、TURN shared secret 和 TURN TLS 私钥字节，终检仍发现 secret 或私钥 marker 就会删除所有外部采集字节、把证据标成 incomplete，并将已被擦除的回滚证明降级为 `not-proven-after-evidence-erasure`，绝不保留 `completed` 措辞。临时证据目录不会被 EXIT trap 当作普通临时文件删除。

这套快照合同防的是原生成目录的并发替换、ABA 和普通误操作，不抵抗已经能以
同一 UID 修改权限、ptrace/控制部署进程，或控制 root/Docker daemon 的恶意主体；
这些仍属于下面明确列出的受信任宿主边界。不要把 `0500/0400` 夸大为密码学不可变。

6. 做 Mac WSS 断网重连、VPS 重启、真实 Cloudflare Access/JWKS、iOS/Android
系统浏览器与撤销测试。首发仍保持所有 mutation 固定关闭。

TURN 与 relay-only 媒体属于后续独立迁移：需要重新审计 coturn 镜像、凭据签发、
DNS-only 名称、证书、安全组/转发链、端口预算和 iOS/Android 蜂窝网络真实
relay-only 验收。首发文档不提供任何可执行的 TURN 启动命令。

## 证据与受信任边界

build、audit、deploy 的清洁 Docker CLI 配置会锁定 local `default` context，但它们
仍然信任宿主 Bash/Python/Git/Docker CLI、`PATH`、root 管理员、Docker daemon 以及
能修改同一仓库或输出父目录的同权限主体。锁定插件的系统路径、所有权、
不可写性与 hash 可以缩小边界，但不能把已被 root/daemon 控制的宿主证明为可信。

`action.json` 和镜像证据的自校验 hash 用于发现偶发损坏、丢文件或证据混搭；
它不是签名，不能抵抗能重写证据与清单的同权限恶意篡改。真正的跨主机/
跨人员防篡改需要将 Commit B、最终证据 digest 或数字签名锚定在该权限边界之外。
镜像发布另有外部写风险：只支持当前非 root 用户直接访问本地 Docker，
`sudo` 发布固定 fail closed，不会将用户的 registry auth 交给 root。

## 尚未完成，不能据此宣称公网可用

| 验收项 | 状态 |
| --- | --- |
| 只读 edge 源码、Access JWT、P-256 WSS 与 API 安全测试 | 已完成（仅本地） |
| Mac 主动 WSS 只读 snapshot 连接器源码与故障测试 | 已完成（仅本地） |
| edge 镜像 Dockerfile、源码/工具链锁与证据门 | 已完成（仅静态，source.lock 待 Commit A） |
| edge 生产镜像双 build、SBOM/CVE、startup 与 digest/ID 实证 | 未完成 |
| 真实 Cloudflare Access JWT/JWKS 与 service-token 兼容烟测 | 未完成 |
| 系统浏览器公网只读 UI 源码与本地测试 | 已完成（未部署、未实机验收） |
| iOS/Android + 真实 Cloudflare 的只读 UI 验收 | 未完成 |
| edge/cloudflared 生产镜像 digest 的来源/CVE 审计 | 未完成 |
| Cloudflare Tunnel、token 与 `phone.example.com` 路由 | 未完成 |
| Cloudflare 公网 TLS 证书确认 | 未完成 |
| TURN/DNS/证书/安全组与 relay-only 验收 | 后续独立迁移，不在首发合同内 |
| 家中 Mac 主动 WSS、短信与通话端到端集成 | 未完成 |
| APNs/FCM push | 未完成 |
| iOS/Android 蜂窝网络真实 TURN smoke | 后续独立迁移，不在首发合同内 |
| VPS 服务启动或部署 | **未执行** |
| 本目录静态正反向测试 | 已完成：image `110/110`、runtime `56/56`（仅静态，无 live Docker） |

## 仅本地静态测试

```bash
deploy/public-edge/test-static.sh
deploy/public-edge/image/test-static.sh --prelock
```

这两个是**无 Docker 的静态/合成 fixture 测试**：覆盖 edge-only 策略、变更前后
运行状态、锁定 Compose/Buildx 的路径与 hash、Git provenance、OCI descriptor 链、
SBOM/CVE/startup 证据、回滚、脱敏与证据原子提交的正反例。它们不连接
Docker daemon，不执行 SSH、DNS、防火墙、Cloudflare 或手机网络操作，也不触碰
modem。通过静态套件只表明策略与故障模型通过；真正 build、rootfs、SBOM、CVE、
startup、Cloudflare 和 iOS/Android 仍必须分别留下 live evidence。
