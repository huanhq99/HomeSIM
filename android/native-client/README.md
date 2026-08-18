# MacCellular Native Client trust preview

`native-client` 是独立的 Android 前台签名客户端。它把 P-256 私钥留在
AndroidKeyStore，通过一次性 ticket 登记公钥，然后为每次请求申请服务端
challenge 并对精确 method/target/body hash 签名。它不是电话应用，也不会复用或伪造浏览器的
`Origin`、Fetch Metadata 或 CSRF headers。

安全引导端点：

- `POST /api/native/v1/enrollments`
- `POST /api/native/v1/auth/challenges`

签名后的业务路由：

- `GET /api/native/v1/session`
- `GET /api/native/v1/sms/sync`
- `POST /api/native/v1/sms/send`（只有显式 `sms.send` 授权时）
- `GET /api/native/v1/sms/operations?operation_id=...`（丢响应后只读查询）

应用只有 `INTERNET` 权限；没有通知、麦克风、WebRTC、
Core-Telecom、service、receiver、provider 或后台同步。短信和 cursor 只在当前
前台 Activity 内存中存在，离开前台即清空。发送安全槽只保存 opaque
operation/device ID、状态、时间和固定结果码，不保存号码、正文、body hash、
challenge 或签名，避免形成可离线验证猜测内容的 oracle。这个 preview 尚未通过真实 Android、Tailscale Serve、进程重启、
撤销和实际短信发送验收。

## 构建

要求 JDK 17 和 Android SDK 35。私有 origin 只允许由本次 Gradle 命令的
project property 提供，必须精确为 `https://<host>.ts.net`，不能带路径、端口、
尾斜杠、query、userinfo 或 fragment：

```sh
cd android
export JAVA_HOME=/path/to/jdk17
export ANDROID_HOME=/path/to/android-sdk
./gradlew --no-daemon \
  :native-client:testDebugUnitTest \
  :native-client:lintDebug \
  :native-client:assembleDebug \
  -PnativeGatewayOrigin=https://your-private-host.ts.net
```

缺少 `-PnativeGatewayOrigin` 必须构建失败。Debug APK 只用于自己的真机验收；
当前 release APK 未配置 signingConfig，因此只是 unsigned 构建证据，不能安装或
登记。正式登记前需要私有且稳定的 release signing key；从 debug 切到 release
通常需要卸载，AndroidKeyStore 身份会丢失，必须先撤销服务端旧 device。APK、
真实 origin、签名材料、ticket 和运行结果均不得进入 Git 或公共发行物。

Native SMS 每页最多 10 条；客户端响应上限为 4 MiB。首次 bootstrap 与后续
cursor 分页必须逐页完成，不应把全部短信一次载入内存。

## 一次性登记流程

1. 以现有远程网关参数启动 Mac 后端，并只为这次进程额外加
   `-native-enrollment-once`。远程 App Cap 必须包含 `devices.enroll`，登记给
   设备的默认 scopes 仍为 `status.read` 和 `sms.read`。
2. 只从 Mac 本机 `127.0.0.1:7576` 生成一张 ticket。下面均为占位符；不要把
   响应写进 shell history、日志或仓库：

   ```sh
   curl --fail-with-body -sS \
     -X POST \
     -H 'Content-Type: application/json' \
     --data '{"confirm":true,"owner":"allowed-login@example.invalid","scopes":["status.read","sms.read"]}' \
     http://127.0.0.1:7576/api/local/v1/native/enrollment/tickets
   ```

   若远程 allowlist 只有一个 login，可省略 `owner`。ticket 五分钟过期且一进程
   只能签发一次；prepare 成功即烧毁 secret。传输结果不明时绝不重用，停止进程后
   重新显式开启一次登记窗口。
3. 在前台应用内输入 `ticket_id` 和 `ticket_secret`。Android 先用 P-256 私钥
   签署服务端原样返回的 enrollment input，再完成登记。
4. 登记成功后停止带 opt-in 的进程，并按日常配置重启，不再携带
   `-native-enrollment-once`，同时从日常 App Cap 撤掉 `devices.enroll`，只保留
   实际需要的 `status.read`/`sms.read`。随后分别验证 session、SMS bootstrap、
   cursor 增量、断网、进程重启和错误 host 均 fail closed。

## 显式短信发送窗口

`sms.send` 不属于默认日常登记。只有在明确验收或使用窗口中，才可将
`sms.send` 加入新设备的 enrollment scopes，并在当次 Tailscale App Cap 中
逐字授予同一 action；后端全局 remote-control 开关也必须开启。`*` 不会
隐式激活 native 发送。确认本地没有 unresolved operation 后，应撤掉 App Cap
中的 `sms.send`，不要长期保留不需要的可付费权限；若还需查询未知结果，
则必须保留或在人工操作窗口中重新授予该 action。

发送时必须在前台二次确认收件人和正文。客户端只序列化一次最终
UTF-8 body；body 中的 `operation_id` 必须与唯一 `Idempotency-Key` 完全相同。
最终 POST 可能离开进程之前，Android 已先在 `noBackupFilesDir` 用 `AtomicFile`
持久一条 unknown guard；Mac 端也必须先 fsync 幂等 started 和 SMS pending，
才能触发模块。任何网络丢响应、进程死亡、分段部分发送或持久化不确定，
都必须保持锁定。最终 POST gate 一旦打开，随后收到的 native `403`/`503`
也不能证明较早的一次透明重放未执行；只有持久 operation 的签名 GET 结果
可以把允许的终态解锁：

- 不自动轮询、不后台重试、不用新 key 重发；
- 只能对原 `operation_id` 发起新的签名 GET 查询；这是人工、只读操作，
  可在 `in_flight` 时稍后再查，但不允许自动轮询或重发 POST；
- `completed` 字样不等于成功；只有 `HTTP 200 + code=completed` 表示模块已接受提交，
  仍不是手机送达回执；
- `not_found` 不证明短信未发送。只有人工核对短信历史或运营侧记录后，
  才可在 UI 中明确关闭该锁定记录。

## 撤销

撤销只从 Mac 本地完整 API 执行；设备端不能撤销或登记其他设备：

```sh
curl --fail-with-body -sS \
  -X POST \
  -H 'Content-Type: application/json' \
  --data '{"confirm":true,"device_id":"dev_<opaque-id>"}' \
  http://127.0.0.1:7576/api/local/v1/native/devices/revoke
```

撤销后旧 challenge 和签名请求必须失败，重启后仍保持撤销。设备 store 使用
`0700/0600`、追加事件和 HMAC head 来发现单文件损坏或尾事件丢失；它不能抵抗
已控制当前 macOS 账户/root 的攻击者同时回滚 metadata、head 和 events。任何
store degraded 或 head/event 不一致都应停止 native API，先保留现场和备份，
不得删除文件尝试“修复”。
