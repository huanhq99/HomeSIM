# Public TURN deployment

这是 `turn.example.com` 的最小独立 coturn 部署切片。它只负责 WebRTC
公网媒体中继，不部署 Asterisk、短信、Cloudflare Tunnel、DNS 或防火墙。

## 固定边界

- Compose 项目固定为 `dji4g-public-turn`，只包含一个 coturn 服务。
- 使用独立 bridge `172.30.247.0/28`，coturn 固定为 `172.30.247.2`；不使用
  host network。
- 仅发布 `3478/udp`、`3478/tcp`、`443/tcp` 和
  `49160-49167/udp`。容器内 TLS listener 为 5349，由宿主机 443 映射。
- `external-ip` 使用 `公网 IPv4/172.30.247.2` 映射，要求 Docker/NAT 对
  relay UDP 端口保持一一映射。
- 只接受 `name@sha256:<digest>` 镜像，`pull_policy=never`；启动脚本不会
  自动拉取或改用 tag。
- 使用 coturn REST `use-auth-secret`。shared secret、生成后的配置、证书和
  私钥均为运行用户所有、0600；运行根及子目录为 0700。
- `no-rfc5780`，不配置 `alt-listening-port`、`tls-alt-listening-port`、
  `aux-server` 或 `alternate-server`；同时关闭 DTLS 和 TCP relay allocation。
- 容器为只读根文件系统，丢弃其他 capabilities，仅保留官方
  `turnserver` 绑定 443/3478 低端口必需的 `NET_BIND_SERVICE`，并启用
  `no-new-privileges`。官方镜像声明的 `/var/lib/coturn` volume 被
  显式 tmpfs 覆盖，停止时不遗留匿名 volume。
- `start` 最多等待 35 秒，只有容器内 `172.30.247.2:3478/tcp` listener
  通过健康检查才成功；失败会自动 `down` 本项目。

这套脚本不会触碰其他 Compose 项目。`stop` 只对固定项目执行 `down`，会
删除本项目容器和 bridge，但保留版本化 runtime 文件，因而可以直接回滚到
旧 runtime。

## VPS 前置条件

在腾讯 Ubuntu VPS 上，以非 root 运行用户执行；该用户已经可以使用：

```sh
sudo -n docker compose version
```

另外需要在云安全组和主机防火墙放行上述精确端口；
`turn.example.com` 必须是指向 VPS 公网 IPv4 的 DNS-only A 记录。普通
Cloudflare 代理或 Tunnel 不承载 TURN UDP。证书 SAN 必须包含
`turn.example.com`，有效期至少还剩 7 天。

这些公网前置条件不由本目录修改，也不能仅凭静态测试宣称已经生效。

## 公网 UDP 实际验收

默认命令只运行本地密闭测试，不向公网 TURN 发包：

```sh
./scripts/tests/public-turn-e2e.sh
```

安全组与主机端口都就绪后，用已安全复制到当前机器的绝对路径
0600 shared-secret 运行 live 验收：

```sh
MACCELLULAR_PUBLIC_TURN_E2E=1 ./scripts/tests/public-turn-e2e.sh \
  --secret-file /absolute/private/path/turn-auth-secret
```

探针使用 `internal/turnauth` 生成两分钟短时凭据，通过 Pion TURN v5
对 `turn.example.com:3478/udp` 执行真实 Allocate。它还会建立一个独立本机
UDP peer，用两份不同的随机 payload 验证 `peer -> relay` 和
`relay -> peer` 的数据完整性，并要求分配端口严格位于
`49160-49167` 。只有输出 `PASS` 才是公网 UDP 中继的完整证据；
任何缺失依赖、超时、鉴权、端口或 payload 错误都会非零退出。
shared-secret 内容不进入 argv，也不会输出到日志。

## 1. 准备私密输入

以下是示例路径。secret 使用不带填充的 base64url，43 至 128 字节：

```sh
install -d -m 0700 "$PWD/turn-private"
umask 077
openssl rand -base64 48 | tr '+/' '-_' | tr -d '=\n' >"$PWD/turn-private/turn-auth-secret"
chmod 0600 "$PWD/turn-private/turn-auth-secret"
```

将有效的 full chain 和未加密私钥复制为当前运行用户所有的普通文件；不要
直接传入 Certbot 的 symlink：

```sh
install -m 0600 /path/to/fullchain.pem "$PWD/turn-private/turn-tls-cert.pem"
install -m 0600 /path/to/privkey.pem "$PWD/turn-private/turn-tls-key.pem"
```

同一个 `turn-auth-secret` 还要以 0600 文件安全复制到家中 Mac，供
`-public-web-turn-secret-file` 使用；它绝不能发送给浏览器，浏览器只取得
短时 TURN REST 凭据。

## 2. 缓存并固定镜像

先从可信发布信息取得 coturn 镜像的确定 digest，再显式缓存这个 digest：

```sh
TURN_IMAGE='coturn/coturn@sha256:<64-lowercase-hex>'
sudo -n docker pull "$TURN_IMAGE"
sudo -n docker image inspect "$TURN_IMAGE"
```

部署脚本不会自动执行这一步，也不会在 digest 缺失时退回 tag。

## 3. 生成版本化 runtime

为 `/srv/dji4g-public-turn` 准备一个由当前用户拥有的 0700 父目录，然后先
dry-run；runtime 路径必须尚不存在：

```sh
./deploy/public-turn/prepare-runtime.sh \
  --runtime-root /srv/dji4g-public-turn/runtime-v1 \
  --image "$TURN_IMAGE" \
  --public-ip A.B.C.D \
  --auth-secret-file "$PWD/turn-private/turn-auth-secret" \
  --tls-cert-file "$PWD/turn-private/turn-tls-cert.pem" \
  --tls-key-file "$PWD/turn-private/turn-tls-key.pem"
```

确认计划后增加 `--apply`。脚本拒绝覆盖现有路径，并在生成失败时只清理它
刚创建的已知文件。

## 4. 检查、启动、查看与回滚

```sh
RUNTIME=/srv/dji4g-public-turn/runtime-v1

./deploy/public-turn/turnctl.sh --runtime-root "$RUNTIME" check
./deploy/public-turn/turnctl.sh --runtime-root "$RUNTIME" start
./deploy/public-turn/turnctl.sh --runtime-root "$RUNTIME" status
```

`check` 会重新核对 runtime、证书、secret/config 一致性、Compose 展开和
本地镜像 digest；不改变容器状态。回滚或停止：

```sh
./deploy/public-turn/turnctl.sh --runtime-root "$RUNTIME" stop
```

`stop` 不会因为证书已过期或 runtime 私密文件已损坏而拒绝恢复操作；
它只展开仓库内的固定 Compose 文件，并对固定项目名执行 `down`。

若要更新证书、secret、IP 或镜像，生成新的 `runtime-v2`，先停止旧版本，
再检查并启动新版本。旧目录保持不变，可按相反顺序回滚。当前切片不包含
自动续证；证书更新后必须生成新 runtime 并重启服务。

## 本地静态验证

```sh
./deploy/public-turn/test-static.sh
```

静态测试证明配置生成、权限、固定端口、命令边界和失败回滚逻辑；不证明
VPS 安全组、DNS、TLS 公网握手、TURN allocation 或真实双向通话已经通过。
