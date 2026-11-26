# p2page 开发文档

简体中文 | [English](DEVELOPMENT.en.md)

本文档面向修改 p2page 代码的开发者，说明架构、代码结构、信令与数据通道协议、构建测试、调试方式和修改约束。部署与配置项见 [USAGE.md](USAGE.md)，代理协作约束见 [AGENTS.md](../AGENTS.md)。

## 1. 设计目标与非目标

### 目标

- 用普通浏览器访问明确授权的内网 Web 应用。
- 公网 Server 只承担账户、路由、授权和 WebRTC 信令，不承载应用数据。
- 同一个 Go 二进制同时提供 `server` 和 `client` 子命令。
- 前端资源内嵌，无 Node.js 构建步骤，无外部数据库服务。
- 默认收紧安全边界：回环监听、Host-only Cookie、Origin 校验、上游 origin 白名单、秘密只存摘要。
- 在浏览器能力范围内兼容常见应用：Fetch/XHR、SSE、WebSocket、Range、上传下载、Cookie。

### 非目标

- 不做通用 HTTP 反向代理，不服务 `curl`、原生客户端或爬虫。
- 不做匿名网络、抗审查或隐藏 Client 真实网络位置。
- 不通用重写上游 HTML/JS，不修改上游 CSP。
- 不做多租户高可用集群；当前是单实例 SQLite 方案。
- 不提供 TURN 服务，只支持接入部署方自备的 TURN。

## 2. 代码结构

```text
cmd/p2page/main.go             子命令解析、信号处理、server/client 入口
internal/wire/wire.go          共享协议常量、JSON 类型、头部清理、帧写入器
internal/server/server.go      Server 配置、HTTP 路由、信令、SDP 校验、维护任务
internal/server/manage.go      数据库 schema、认证、面板、配置变更、邀请码
internal/server/web/           内嵌管理页面、应用外壳、Service Worker、桥接脚本
internal/client/client.go      Client 配置、信令连接、PeerConnection、上游白名单
internal/client/stream.go      HTTP/WebSocket 数据通道代理、响应改写、上传背压
internal/client/cookies.go     应用 Cookie 存储、会话能力、Cookie 兼容控制通道
deploy/                        Caddy 与 systemd 示例、配置样例
internal/server/*_test.go      Server/浏览器集成测试
internal/client/*_test.go      Client Cookie、流、TLS 测试
```

前端没有独立的 `package.json` 或构建目录。`internal/server/web/*` 通过 `//go:embed web/*` 编译进二进制。修改 HTML/CSS/JS 后重新 `go build` 即可生效。

## 3. 架构与生命周期

### 3.1 Server 启动

`cmd/p2page/main.go` 解析 `server|client -config path.json`，创建带 `SIGINT`/`SIGTERM` 的 context，调用对应 `Run`。

`server.Run`：

1. 以严格 JSON 解码配置（`DisallowUnknownFields`）。
2. 填充默认 `listen`；校验 `domain`、`database`、`admin_username`。
3. `open` 数据库，设置 WAL、外键、busy timeout 和单连接。
4. `init` 创建 schema、签名密钥，并在首次启动时用 `P2PAGE_ADMIN_PASSWORD` 创建唯一管理员。
5. 启动 `maintain` 周期任务。
6. 启动 `http.Server`，设置读写超时和头部大小限制。
7. 收到取消信号后关闭所有信令连接，再优雅关闭 HTTP。

### 3.2 应用访问生命周期

1. 浏览器请求 `https://<app>-<user>.<domain>/...`。
2. Server 从 Host 拆出 `<app>-<user>`，校验用户名和应用名格式，查询应用。
3. Public 应用直接放行；Private 应用校验 host-only 授权 Cookie，未授权时跳转管理域 `/authorize`。
4. 管理域登录后生成短时、单次 `handoff` 票据，通过表单 POST 到应用子域 `/__p2page/authorize` 换取授权 Cookie。
5. 已授权请求返回应用外壳 `shell.html`，或返回内嵌静态资源。
6. 外壳创建 `RTCPeerConnection`，POST `/__p2page/offer`，Server 将 SDP 转发给对应 Client。
7. Client 校验目标 origin、ICE URL、协议版本，建立 PeerConnection 并返回 answer。
8. DataChannel 打开后，浏览器通过 Service Worker 把应用请求发给 Client；Client 访问上游并回传响应。

### 3.3 Client 启动

`client.Run`：

1. 严格 JSON 解码配置。
2. 校验 `server` 为裸 HTTPS origin、token 长度 43、`upstreams` 非空。
3. 为每个上游建立 `http.Transport` 和 WebSocket dialer，解析 `ca_file`、`insecure_skip_verify`、`public_origin_headers`，并建立 origin 映射。
4. 处理 `allowed_ice_urls` 的三种状态。
5. 连接 `wss://<server>/__p2page/agent`，携带 `Authorization: Bearer <token>`。
6. 收到信令 Offer 后，为每个 peer 创建独立 context，协商 PeerConnection，处理 control/request 两类 DataChannel。
7. 信令断开时按指数退避重连。

## 4. HTTP 路由与访问控制

### 4.1 管理域

管理域（`r.Host == domain`）由 `manage` 处理：

- `/__p2page/agent`：Client 信令 WebSocket。
- `/__p2page/style.css`、`/__p2page/login.js`：内嵌资源。
- `/`、`/login`、`/register`：登录和邀请码注册。
- `/dashboard`：账户、Client、应用、用户和邀请管理；POST 需要精确 Origin。
- `/logout`：仅 POST。
- `/authorize`：私有应用登录跳转，签发短时 handoff 票据。
- 其他路径 404。

所有管理 POST 都会检查 `sameOrigin`，即 `Origin == "https://" + r.Host`。

### 4.2 应用子域

应用子域由 `ServeHTTP` 处理：

- `/__p2page/authorize`：接收 handoff 票据。
- `/__p2page/reauthorize`：私有应用重新授权入口。
- `/__p2page/shell`：应用外壳。
- `/__p2page/offer`：接收浏览器 SDP Offer。
- `/__p2page/shell.js`、`worker.js`、`socket.js`、`style.css`：内嵌资源。
- 其他非 `/__p2page/` 路径：返回外壳，由 Service Worker 和应用路径接管。

`/__p2page/` 为保留前缀；Client 的 `destination` 也会拒绝该前缀，防止上游占用。

### 4.3 认证与授权

- 用户名 `[a-z0-9]{1,30}`；密码 12–72 字节；显示名最多 256 字节。
- 登录会话：随机 32 字节 token，数据库只存 SHA-256 摘要；Cookie 名 `__Host-p2page-session`，`Secure`、`HttpOnly`、`SameSite=Lax`，七天。
- 私有应用授权：`__Host-p2page-grant`，绑定应用 ID、会话摘要和过期时间；七天。
- handoff 票据：HMAC 签名，一分钟有效；`handoffs` 表记录 nonce，保证单次使用。
- 邀请码：随机 32 字节，只存摘要，七天有效，事务内原子消费。
- 密码使用 bcrypt；登录失败也执行一次 dummy hash，降低用户名枚举的时序差异。
- 管理员唯一；停用用户会删除其会话并断开其 Client 信令。

## 5. 信令协议

信令只经过 Server 的 `/__p2page/agent` WebSocket，不承载应用数据。消息类型是 `wire.Signal`：

| 字段 | 说明 |
| --- | --- |
| `id` | Peer 标识；Client 侧要求长度 32。 |
| `sdp` | Offer/Answer SDP。 |
| `target` | 应用上游 URL。 |
| `origin` | 浏览器侧应用 origin，例如 `https://app-user.example.com`。 |
| `appId` | 应用 ID。 |
| `version` | 协议版本，当前为 `wire.Version = 2`。 |
| `ice` | Server 下发给 Client 的 ICE 配置。 |
| `error` | Client 拒绝协商的原因。 |
| `closed` | 通知对端 peer 已关闭。 |

### 5.1 Client 接入

`server.attach`：

- 按来源 IP 限流（120 次/分钟）。
- 要求 `Authorization: Bearer <43 字符 token>`。
- 要求没有 `Origin` 头。
- 查询 `clients` 与 `users`，要求用户未停用。
- 同一 Client 只允许一个信令连接；重复连接以 `1008 client already connected` 关闭。
- 设置 256 KiB 读限制、45 秒读超时、15 秒 ping。
- 用 `pending` 保存待响应的 Offer，用 `peers` 保存活动 peer。

### 5.2 Offer/Answer

`server.offer`：

- 只接受 POST，校验精确 Origin。
- 按 Host + 来源 IP 限流（100 次/分钟）。
- 请求体最多 220 KiB。
- `applicationOffer` 要求 SDP 以 `v=0` 开头，且恰好一个 `m=application`；STUN 模式移除 `typ relay` 候选。
- 校验 `Version == wire.Version`。
- 每个 Client 最多 16 个待协商 Offer、128 个活动 peer，超出返回 429。
- 将 `Signal` 发给 Client，等待最多 25 秒；Client 的 answer 必须版本一致、无 error、未 closed。
- 失败返回 502/503/504；成功后返回 `{"sdp": "..."}`。

### 5.3 撤销与维护

`server.maintain` 每 20 秒：

- 删除过期 session 和 handoff。
- 清理过期限流记录。
- 遍历活动 peer，重新查询应用和用户，比较应用 ID、target、client、public、mode、ICE 配置，并校验私有应用的会话；失效时向 Client 发送 `closed` 并删除 peer。

因此删除 Client/应用、停用用户、修改应用配置会在约 20 秒内断开现有连接。

## 6. DataChannel 协议

### 6.1 通道

Client 只接受无序/可靠配置之外的通道，即要求 `Ordered()` 且没有 `MaxRetransmits`/`MaxPacketLifeTime`：

- `control`：每个 peer 只允许一个。承载 Cookie 状态和脚本 Cookie 写入。
- `request`：每个 peer 最多 64 个并发，用于 HTTP/WebSocket 流。

全局并发流上限为 256（`client.streams`）。浏览器侧还有 32 个 HTTP、8 个 SSE、16 个 WebSocket 的独立配额。

### 6.2 帧格式

每个 DataChannel 消息是 `1 字节类型 + 负载`，最大帧 60 KiB（`wire.Frame`）。类型定义必须与 `internal/server/web/shell.js` 的 `P` 对象一致：

| 值 | Go 常量 | 用途 |
| --- | --- | --- |
| 1 | `Start` | 请求开始，JSON `wire.Request` |
| 2 | `Data` | 请求体或响应体分块 |
| 3 | `End` | HTTP 请求体/响应体结束 |
| 4 | `Headers` | 响应头 JSON `wire.Response` |
| 5 | `Failure` | 失败原因字符串 |
| 6 | `Credit` | 上传/下载窗口信用 |
| 7 | `TextEnd` | WebSocket 文本消息结束 |
| 8 | `BinaryEnd` | WebSocket 二进制消息结束 |
| 9 | `Close` | WebSocket 关闭 JSON |
| 10 | `Cookies` | control 通道上的 Cookie 状态 |

常量：

- `Chunk = 32 KiB`：请求/响应分块上限。
- `Frame = 60 KiB`：单帧上限，负载必须小于该值。
- `Window = 32`：上传信用窗口。
- `MaxWS = 16 MiB`：单条 WebSocket 消息上限。
- `Prefix = "/__p2page/"`：保留路径。
- `Version = 2`：协议版本。

> **同步要求**：修改帧类型、协议版本或上述常量时，必须同时修改 `internal/wire/wire.go`、`internal/server/web/shell.js`、`internal/client/*` 和 Server 的版本校验；必要时提升 `Version` 并更新文档与浏览器测试。

### 6.3 HTTP 请求流

1. 浏览器打开 `request` DataChannel，发送 `Start` + JSON `wire.Request`。
2. Client 读取 10 秒内的 Start，校验路径、方法、目标 origin。
3. Client 对 `CONNECT`、`TRACE`、`TRACK` 直接失败。
4. 若有请求体，Client 向浏览器发送 `Credit Window`，浏览器按信用发送 `Data` 分块，最后发送 `End`。
5. Client 用 `io.Pipe` 把上传流交给 `http.Transport`，并在消费后回传 `Credit`。
6. 上游响应头通过 `Headers` 返回；响应体按浏览器信用发送 `Data`，最后 `End`。
7. 失败发送 `Failure` 字符串。

响应处理：

- `Set-Cookie` 不转发到公网，而是写入 Client 内存 Cookie jar，并通过 control 通道同步可见 Cookie。
- 同源 `Location` 在基础路径下改写。
- HTML 响应注入 `/__p2page/socket.js`，并调整 `Content-Length`、移除旧验证器。
- 默认不转发浏览器声明的 `X-Forwarded-*`、`X-Real-IP`、`Forwarded`、`X-P2Page-*`；`proxyHeaders` 统一清理。

### 6.4 WebSocket 流

`serveSocket`：

- 用同一套上游白名单和 TLS 配置拨号 `ws`/`wss`。
- 支持子协议、文本和二进制消息；单条最大 16 MiB。
- 上行：浏览器 `Data` 分块 + `TextEnd`/`BinaryEnd`；下行同理。
- 关闭：浏览器发送 `Close` JSON `{code, reason}`；Client 校验 code（1000 或 3000–4999）和 reason 长度后转发。
- 只支持应用窗口内的同源 WebSocket，不支持 Web Worker 内的 WebSocket。

### 6.5 流控

- **上传**：Client 先给浏览器 `Window` 个信用；浏览器每发一个 `Data` 消耗一个；Client 消费后回 `Credit`。
- **下载**：浏览器按自己的信用窗口向 Client 发送 `Credit`；Client 每发一个 `Data` 消耗一个。
- `Writer.Send` 串行化写入，并设置 20 秒写截止时间。
- 任何帧过大、类型非法、信用越界或通道关闭都会取消整个流。

## 7. Cookie 子系统

### 7.1 存储模型

`sessionStore` 以 `{appID, target, SHA-256(browser capability)}` 为键保存 Cookie jar：

- 浏览器在应用子域的 `localStorage` 保存随机 32 字节 capability（base64url，43 字符），通过 control 通道的 `open` 消息交给 Client。
- Client 只在 DTLS 上接收该 capability，不能用它选择应用或上游；这两者由已认证的 Offer 绑定。
- 每个 jar 最多 64 个 Cookie，总预算约 8 KiB；全局最多 512 个 browser session。
- 空闲七天或容量淘汰时删除。
- Client 重启后内存清空，用户需要重新登录应用。

### 7.2 Cookie 语义

- `Domain` 一律丢弃，Cookie 不跨子域。
- 路径按应用外部可见路径空间归一化。
- 保留 `__Host-`/`__Secure-` 约束；拒绝 `__Host-p2page-` 前缀。
- 脚本写入不能覆盖或删除 `HttpOnly` 条目。
- `SameSite` 为近似实现：当 Referer 能识别跨站入口时不发送 `Strict` Cookie；无 Referer 时无法可靠区分。
- 可见 Cookie 通过 `wire.CookieState` 以 revision 同步；`document.cookie` 兼容层由 `shell.js` 和 `socket.js` 提供。
- 原生 Cookie Store API 和多标签实时同步不保证一致。

### 7.3 与浏览器外壳的交互

- `control` 通道：`open`（浏览器 → Client，携带 session capability）、`ready`（Client → 浏览器，携带 Cookie 状态）、`set`（脚本写 Cookie）、`ack`（Client 确认并返回新状态）。
- 请求通道：`wire.Request.Credentials` 表示是否携带 Cookie；`CookieRevision` 用于避免重复下发。
- 响应 `Set-Cookie` 在 Client 内消费，不暴露给公网。

## 8. 浏览器外壳与 Service Worker

`internal/server/web/` 没有打包步骤，直接由 Go 嵌入：

- `shell.html` / `shell.js`：创建 PeerConnection、control/request 通道、悬浮球、路由镜像、Cookie 兼容层。
- `worker.js`：注册在 `/` scope，接管应用请求，把 Fetch 事件转换为 DataChannel 流；处理首次导航和顶层 POST。
- `socket.js`：注入到上游 HTML，桥接同源 WebSocket 和 `document.cookie`。
- `dashboard.html`、`auth.html`、`handoff.html`、`reauthorize.html`：管理面页面。
- `style.css`：管理面和应用外壳样式。

浏览器侧限制：

- 只接受 `Ordered()`、可靠传输的 DataChannel。
- HTTP/SSE/WebSocket 分别限流排队。
- 页面刷新后通过 Service Worker 请求固定外壳入口，应用路径仍通过 DataChannel 转发。
- 首次访问或强制刷新绕过 Service Worker 时，路径和查询参数仍会到达 Server。

## 9. 数据库

`manage.go` 中的 schema 使用 `CREATE TABLE IF NOT EXISTS`，没有独立迁移框架：

| 表 | 内容 |
| --- | --- |
| `settings` | 签名密钥等键值。 |
| `users` | 用户名、显示名、bcrypt 密码、管理员/停用标记；唯一管理员索引。 |
| `invites` | 邀请码摘要、过期时间、使用者、撤销标记。 |
| `sessions` | 登录会话摘要、用户、过期时间。 |
| `handoffs` | 单次 handoff nonce 和过期时间。 |
| `clients` | Client 标签、token 摘要、所属用户。 |
| `apps` | 应用名、显示名、target、public、mode、ICE JSON、所属用户/Client。 |

修改 schema 时需要：

- 兼容已有数据库，不能只改 `CREATE TABLE IF NOT EXISTS` 就假设旧表会更新；
- 为旧数据提供迁移或向后兼容读取；
- 保持外键级联语义：删除用户 → Client → 应用；
- 不记录明文 token、邀请码或密码。

## 10. 安全不变量

修改代码时不要破坏以下约束：

1. **数据面不经过 Server**：Server 不代理应用 HTTP/WebSocket 内容，不保存应用 Cookie。
2. **公网入口必须由反向代理终止 TLS**：Server 默认只监听回环，不提供 TLS。
3. **管理 POST 必须校验精确 Origin**：不要放宽 `sameOrigin`。
4. **私有应用授权必须绑定应用和会话**：不要让管理员或其他用户自动获得访问权。
5. **秘密只存摘要**：Client token、邀请码、会话都只保存摘要；不要记录明文。
6. **Cookie 隔离**：应用 Cookie 不跨应用、不跨上游、不跨浏览器会话；`Domain` 必须丢弃；`HttpOnly` 不可被脚本覆盖。
7. **上游白名单按 origin**：不要按路径放宽；不要自动跟随重定向或继承环境代理。
8. **不伪造访问者 IP**：清理 `Forwarded`、`X-Forwarded-*`、`X-Real-IP`；只在可信代理网段读取 `X-Real-IP`。
9. **SDP 只允许 DataChannel**：要求单个 `m=application`；STUN 模式移除 relay 候选。
10. **协议版本一致**：Server、Client、浏览器外壳的 `Version` 必须同步。
11. **TURN 凭据可能公开**：Public 应用页面会下发 ICE 配置；不要假设凭据保密。
12. **日志脱敏**：不记录 Cookie、Authorization、token、邀请码或管理表单。

## 11. 构建与运行

### 11.1 环境

- Go 1.24 或更高版本（`go.mod` 为 `go 1.24.0`）。
- 前端不需要 Node.js；Node.js 22+ 只用于可选的浏览器集成测试。
- SQLite 使用 `modernc.org/sqlite`，不依赖 CGO。

### 11.2 编译

```sh
go build -trimpath -o p2page ./cmd/p2page
```

可选静态编译：

```sh
CGO_ENABLED=0 go build -trimpath -o p2page ./cmd/p2page
```

产物只有一个二进制。`deploy/` 中的 systemd 单元假定安装到 `/usr/local/bin/p2page`。

### 11.3 本地运行

项目没有独立的开发模式。手动运行时仍需要：

- 一个可解析的域名和 `*.domain`；
- HTTPS 反向代理或测试用 TLS 终止；
- 首次启动的 `P2PAGE_ADMIN_PASSWORD`；
- 一个可访问上游的 Client 配置。

本地验证可优先使用单元测试和 `httptest`；真实浏览器行为使用下面的集成测试。

## 12. 测试

### 12.1 单元测试与静态检查

```sh
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

`gofmt -l .` 应无输出。测试覆盖：

- `internal/client/cookies_test.go`：Cookie 路径、`HttpOnly`、可见状态、会话能力。
- `internal/client/stream_test.go`：路径校验、HTML 注入、表单 Content-Length、Origin/Referer 与头部清理、Location/Cookie 路径改写。
- `internal/client/tls_test.go`：`ca_file`、`insecure_skip_verify`、配置校验。
- `internal/server/server_test.go`：邀请码并发消费、管理面 CSRF、私有应用授权与票据重放、ICE 校验。

### 12.2 浏览器集成测试

需要 `chromium` 和 Node.js 22+：

```sh
P2PAGE_BROWSER_TEST=1 go test -count=1 -run '^TestBrowser$' -v ./internal/server
```

测试会：

- 用 `httptest` 启动 TLS 上游和边缘代理；
- 创建 Server、Client token、应用，并启动真实 `client.Run`；
- 用 Node 驱动 Chromium 访问应用；
- 验证 Cookie 不泄漏到公网、路径不泄漏、顶层 POST 只提交一次、深链、下载、上传、SSE、WebSocket 和 Service Worker 首次导航。

修改代理行为、Cookie、Service Worker、顶层表单或下载逻辑时，应运行该测试。它默认跳过，不会在普通 `go test ./...` 中执行。

### 12.3 测试数据与注意事项

- 浏览器测试通过 `SSL_CERT_FILE` 注入测试 CA；不要修改生产信任库。
- `internal/server/testdata/browser.mjs` 是测试驱动，不是生产代码。
- 测试中使用真实 `client.Run`，因此修改 Client 启动逻辑会影响集成测试。
- 竞态测试可能较慢，但涉及并发 peer、Cookie 和流控的改动应至少跑一次 `-race`。

## 13. 编码约定

- 优先使用标准库；新增依赖需有明确理由，避免引入前端框架或构建链。
- 配置解码保持严格 JSON（`DisallowUnknownFields`），错误信息清晰。
- HTTP 处理函数用 `bad(w, code, msg)` 返回错误；不要向客户端泄露内部细节。
- 共享锁内不要执行阻塞 I/O 或网络写入；信令写入使用 `writeMu` 和截止时间。
- DataChannel 写入统一走 `wire.Writer`，不要绕过帧大小和写截止时间。
- 所有上游请求必须经过 `destination` 和 origin 白名单；不要直接拼接用户输入 URL。
- 所有响应头必须经过 `wire.CleanHeaders`；不要转发 hop-by-hop 头。
- 前端保持无构建、原生 ES 模块；不要引入 CDN 资源。
- 注释解释“为什么”，尤其是安全边界和浏览器兼容工作区。
- 文档修改保持中英文同步。

## 14. 修改检查清单

提交前确认：

- [ ] `go test ./...`、`go test -race ./...`、`go vet ./...` 通过。
- [ ] `gofmt -l .` 无输出。
- [ ] 涉及浏览器行为时运行 `P2PAGE_BROWSER_TEST=1 ...`。
- [ ] 修改协议帧/版本时同步 `wire.go`、`shell.js`、Client 和 Server。
- [ ] 修改配置项时更新 `USAGE.md` / `USAGE.en.md` 和示例 JSON。
- [ ] 修改构建/测试/约束时更新 `docs/DEVELOPMENT*` 和 `AGENTS.md`。
- [ ] 没有提交 `p2page` 二进制、`server.json`、`client.json`、数据库或证书。
- [ ] 安全不变量未被放宽；新增日志不包含秘密。
- [ ] 新增或修改的代码有对应测试。

## 15. 调试

- **Server 日志**：应用查找、配置写入、信令断开、维护检查。
- **Client 日志**：`connected to signaling server`、WebRTC 协商失败原因、上游请求失败。
- **浏览器**：DevTools 的 Network/WebSocket、Application → Service Workers、`chrome://webrtc-internals`。
- **悬浮球**：显示 `Direct P2P` / `TURN relay`、候选类型和 ICE RTT；失败时可重连。
- **数据库**：用 `sqlite3 p2page.db` 只读查询用户、Client、应用和邀请状态；不要在生产库上直接改数据。
- **协议问题**：先确认 Server、Client、`shell.js` 的协议版本一致，再检查帧类型和大小限制。
- **Service Worker 问题**：清除站点数据或使用新的浏览器配置；注意 `worker.js` 的更新和首次导航握手。

## 16. 部署产物

- `deploy/Caddyfile`：TLS 终止、Host 保留、`X-Real-IP` 覆盖、WebSocket 转发。
- `deploy/server.example.json`：Server 配置样例。
- `deploy/client.example.json`：Client 配置样例，含 `ca_file` 和 `allowed_ice_urls`。
- `deploy/p2page-server.service`：Server systemd 单元，含 `StateDirectory`、`EnvironmentFile` 和加固项。
- `deploy/p2page-client.service`：Client systemd 单元，`RestrictAddressFamilies` 必须包含 `AF_NETLINK`。

部署相关的运行、备份、升级和排障见 [USAGE.md](USAGE.md)。
