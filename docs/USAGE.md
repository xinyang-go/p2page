# p2page 使用与配置

简体中文 | [English](USAGE.en.md)

本文档面向部署者和使用者，说明 Server 与 Client 的全部配置项、账户与面板操作、ICE/TLS、访问控制、兼容范围、安全边界和常见故障处理。开发与协议细节见 [DEVELOPMENT.md](DEVELOPMENT.md)。

## 1. 运行模型

```text
浏览器 ──HTTPS──> 公网反向代理 ──HTTP──> p2page Server
   │                                      │
   │                                 信令 WebSocket
   │                                      │
   └──── WebRTC DataChannel ────> p2page Client ──HTTP(S)──> 内网应用
         （直连；失败时可走自备 TURN）
```

- **Server**：管理账户、邀请码、Client token、应用配置和子域名路由；在浏览器与 Client 之间转发 WebRTC SDP；不转发应用 HTTP/WebSocket 数据。
- **Client**：运行在内网，主动连接 Server 的信令端点；对每个访问者建立 WebRTC PeerConnection；按本地白名单访问上游；在内存中保存应用 Cookie。
- **浏览器**：加载 Server 提供的应用外壳，在 iframe 中运行上游应用，用 Service Worker 和 DataChannel 代理同源请求。
- **TURN（可选）**：直连失败时由部署方自备的 TURN 服务中继加密的 WebRTC 流量。Server 不提供 TURN。

Server 是单实例设计：SQLite 保存持久数据，在线信令和连接状态保存在内存中。不要对同一数据库启动多个 Server 实例。

## 2. Server 配置

Server 通过 `p2page server -config <path>` 启动。`-config` 必填；没有其他命令行覆盖项。

### 2.1 配置字段

配置文件是严格 JSON：出现未知字段会导致启动失败。

| 字段 | 必需 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `listen` | 否 | `127.0.0.1:8080` | HTTP 监听地址。应保持回环，由反向代理终止 TLS；不要直接暴露到公网。 |
| `domain` | 是 | — | 管理域和应用的基域名。必须是包含点的小写 DNS 名称，不能带 scheme、端口或通配符；总长度不超过 180，每个标签 1–63 字符。 |
| `database` | 是 | — | SQLite 数据库文件路径。父目录必须预先存在；文件不存在时会以 `0600` 创建。 |
| `admin_username` | 是 | — | 初始管理员用户名，只允许 `[a-z0-9]{1,30}`。首次启动时创建管理员；之后必须与数据库中现有管理员一致，否则拒绝启动。 |
| `trusted_proxy_cidr` | 否 | 空 | 一个 CIDR。仅当请求来自该网段时，才信任 `X-Real-IP` 作为客户端地址。代理未覆盖写入该头时不要配置。 |

示例：

```json
{
  "listen": "127.0.0.1:8080",
  "domain": "example.com",
  "database": "/var/lib/p2page/p2page.db",
  "admin_username": "admin",
  "trusted_proxy_cidr": "127.0.0.1/32"
}
```

### 2.2 首次启动

首次启动必须设置环境变量：

```sh
export P2PAGE_ADMIN_PASSWORD='replace-with-a-long-random-passphrase'
./p2page server -config server.json
```

- 密码长度为 12–72 字节，必须是合法 UTF-8；没有默认密码。
- 管理员创建成功后，后续启动不再需要该变量。请从环境、shell 历史或服务配置中删除它。
- 数据库中的密码使用 bcrypt 哈希。
- 当前只有一个管理员，且没有内置的密码重置入口。请妥善保存管理员密码。
- 首次启动还会生成数据库签名密钥，用于会话和授权票据。不要删除或替换数据库中的 `settings.signing_key`。

### 2.3 DNS、证书与反向代理

1. 将 `example.com` 和 `*.example.com` 解析到反向代理。
2. 为两个名字配置 HTTPS。泛域名证书通常需要 DNS-01 验证。
3. 反向代理必须保留 `Host`，支持 WebSocket Upgrade，并将请求转发到 `listen` 地址。
4. 只应在代理确实覆盖 `X-Real-IP` 时配置 `trusted_proxy_cidr`；否则客户端可通过伪造该头绕过按 IP 的限流。
5. 建议在代理侧配置请求体大小、超时、限流、访问日志脱敏和证书轮换。

Caddy 示例见 `deploy/Caddyfile`：

```caddyfile
example.com, *.example.com {
	tls /etc/p2page/tls/fullchain.pem /etc/p2page/tls/privkey.pem
	reverse_proxy 127.0.0.1:8080 {
		header_up X-Real-IP {remote_host}
	}
}
```

Server 自身返回 HSTS、`X-Content-Type-Options: nosniff`、`Referrer-Policy`、`Origin-Agent-Cluster` 等响应头，但 TLS 必须由代理提供。

### 2.4 systemd 与文件权限

`deploy/p2page-server.service` 提供了示例单元，关键点：

- 使用独立的 `p2page` 系统用户和组；
- `StateDirectory=p2page` 创建 `/var/lib/p2page`，权限 `0700`；
- `EnvironmentFile=-/etc/p2page/bootstrap.env` 可在首次启动时提供 `P2PAGE_ADMIN_PASSWORD`；
- `UMask=0077`、`ProtectSystem=strict`、`ProtectHome=true` 等加固项。

首次初始化完成后，从 `bootstrap.env` 删除密码并重启服务。配置文件和数据库应仅对服务用户可读。

### 2.5 数据库与备份

- SQLite 使用 WAL 模式、外键和 5 秒 busy timeout；Server 只使用一个数据库连接。
- 持久数据包括用户、密码哈希、邀请码摘要、会话摘要、Client token 摘要和应用配置。
- 备份时使用 SQLite 在线备份，或包含 `-wal` / `-shm` 的一致快照。不要在运行中只复制 `.db` 文件而忽略 WAL。
- Server 重启会断开会话的数据通道；Client 会重连信令，浏览器重新连接后仍可使用未过期的登录和 Client 内存中的 Cookie。

## 3. Client 配置

Client 通过 `p2page client -config <path>` 启动。配置文件同样是严格 JSON，未知字段会导致启动失败。

### 3.1 配置字段

| 字段 | 必需 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `server` | 是 | — | Server 的 HTTPS 地址，例如 `https://example.com`。必须是裸 origin：不能带非根路径、查询参数、片段或用户信息。 |
| `token` | 是 | — | 面板中创建 Client 时显示的 token，长度为 43 个字符。删除对应 Client 后立即失效。 |
| `upstreams` | 是 | — | 至少一个上游白名单条目。条目只接受 HTTP(S) origin，不能带非根路径、查询或片段（根路径 `/` 可省略）；重复 origin 会被拒绝。 |
| `allowed_ice_urls` | 否 | 省略 | 允许 Server 下发给浏览器的 ICE URL 列表。省略表示信任 Server；`[]` 表示拒绝所有外部 ICE URL；非空数组按完整字符串精确匹配。 |

示例：

```json
{
  "server": "https://example.com",
  "token": "TOKEN_FROM_THE_DASHBOARD",
  "upstreams": [
    {"url": "http://192.168.1.10:8080"},
    {"url": "https://nas.lan:8443", "ca_file": "nas-ca.pem"}
  ],
  "allowed_ice_urls": [
    "stun:stun.example.net:3478",
    "turn:turn.example.net:3478?transport=udp",
    "turns:turn.example.net:5349?transport=tcp"
  ]
}
```

### 3.2 上游条目字段

| 字段 | 必需 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `url` | 是 | — | 上游 HTTP(S) origin，例如 `http://router.lan` 或 `https://nas.lan:8443`。不能带路径、查询、片段或用户信息。 |
| `ca_file` | 否 | 系统信任库 | 额外信任的 PEM 证书文件，可包含私有 CA 或受信自签证书。相对路径从 `client.json` 所在目录解析。只适用于 HTTPS；仍校验证书链和主机名/IP。 |
| `insecure_skip_verify` | 否 | `false` | 仅对该 HTTPS 上游跳过证书链和主机名校验。不能与 `ca_file` 同时使用。同时影响 HTTP 和 WSS 上游连接。 |
| `public_origin_headers` | 否 | `false` | 让上游收到公有应用的 `Host`、同源 `Origin` 和浏览器允许携带的同源 `Referer`。不会改变实际拨号地址或 TLS 校验名称。 |

说明：

- **白名单匹配**：Client 按 scheme/host/port 查找上游。Server 下发的应用目标必须命中某个 `upstreams` origin，否则拒绝连接。
- **应用基础路径**：应用面板中的上游 URL 可以带基础路径（例如 `https://nas.lan:8443/app`）；Client 的 `upstreams` 条目不能带非根路径，只负责 origin 级白名单和 TLS 配置。
- **地址规范化**：命中白名单后，Client 使用本地配置中的 authority 拨号，避免 DNS 或拼写差异被利用。
- **自签证书**：优先使用 `ca_file`。示例中的 `nas-ca.pem` 相对于 `client.json`。
- **跳过校验的风险**：`insecure_skip_verify` 仍然加密流量，但无法防止中间人冒充上游。只应在可信内网、且无法配置 CA 时使用。它不影响 Client 连接公网 Server 时的 TLS 校验。

### 3.3 `allowed_ice_urls` 的三种状态

| 配置 | 行为 |
| --- | --- |
| 省略 `allowed_ice_urls` | 信任 Server 为应用下发的所有 ICE URL。 |
| `"allowed_ice_urls": []` | Client 拒绝任何外部 ICE URL。只有 Server 未下发外部 ICE URL 时才使用本机候选；适合纯局域网直连。 |
| 非空数组 | Server 下发的每一条 ICE URL 都必须与数组中的字符串完全一致，否则整个 PeerConnection 被拒绝。 |

不要把 `allowed_ice_urls` 当成 TURN 凭据保护机制：它只限制 URL，不隐藏凭据。凭据会随 ICE 配置下发给浏览器。

### 3.4 启动与重连

```sh
./p2page client -config client.json
```

- 成功连接后日志显示 `connected to signaling server`。
- 信令断开后按 1 秒到 30 秒的指数退避重连；稳定运行超过一分钟后退避重置。
- 同一个 token 只允许一个活动信令连接。重复启动时，新连接会被拒绝，旧连接保持。
- Client 重启会清空内存中的上游应用 Cookie，用户需要重新登录应用。
- `deploy/p2page-client.service` 是 systemd 示例。若自行加固，`RestrictAddressFamilies` 必须包含 `AF_NETLINK`，否则 Pion 无法枚举本地网卡，WebRTC 协商会失败并报 `netlinkrib: address family not supported by protocol`。修改单元后执行 `systemctl daemon-reload` 并重启。

## 4. 账户与面板

### 4.1 首次登录与邀请码

- 管理域：`https://<domain>`。
- 首次使用 `admin_username` 和 `P2PAGE_ADMIN_PASSWORD` 登录。
- 普通用户必须使用管理员生成的一次性邀请码注册：
  - 邀请码默认七天到期；
  - 只能使用一次；
  - 未使用且未过期的邀请码可以手动撤销；
  - 已使用或已过期的邀请码在面板中不再显示撤销入口。
- 用户名只允许 1–30 位小写 ASCII 字母和数字；显示名最多 256 字节，可使用中文和其他字符。
- 密码长度 12–72 字节。
- 管理员可以停用或恢复普通用户。停用会删除该用户的会话、断开其 Client 信令，并使其应用不可访问；恢复后可以重新登录。

### 4.2 Client 管理

在 **Clients** 区域：

- **新增**：填写 Client 标签（最多 128 字节）。创建后 token 只显示一次，请立即保存。
- **删除**：立即使 token 失效，级联删除属于该 Client 的全部应用，并断开其信令连接。
- **在线状态**：显示 Online / Offline，取决于信令连接是否存在。
- 当前没有重命名 Client 的入口；需要改名时只能新增并迁移应用。
- 一个 Client 可以承载同一账户下的多个应用；一个 token 只允许一个活动连接。

### 4.3 应用管理

在 **Applications** 区域新增或编辑应用：

| 字段 | 说明 |
| --- | --- |
| `App name` | 应用子域名前缀，只允许 1–30 位小写字母和数字；在账户内唯一。修改后应用 URL 会变化。 |
| `Display name` | 面板和标题使用的显示名，最多 256 字节，可包含中文。 |
| `Upstream URL` | 内网上游的 HTTP(S) URL，最多 2048 字节；可带基础路径，不能带查询、片段或用户信息。 |
| `Client` | 承载该应用的 Client。应用目标 origin 必须在该 Client 的 `upstreams` 中。 |
| `ICE mode` | `STUN only / direct` 或 `TURN fallback`。 |
| `ICE servers` | JSON 数组，见第 5 节。 |
| `Public` | 勾选后任何拿到 URL 的人都能访问；不勾选则为私有应用。 |

应用 URL 为：

```text
https://<appname>-<username>.<domain>
```

- **Public**：无需登录管理域即可访问，适合有意公开的服务。
- **Private**：只有所属账户在管理域登录后才能访问。管理员和其他用户不会自动获得访问权。
- 私有应用的授权票据绑定应用和浏览器会话，经短时、单次跳转票据下发；默认有效七天。
- 删除应用或删除其 Client 后，现有连接会在 Server 的周期检查中（约 20 秒）断开。
- 修改应用的目标、公开状态、ICE 配置或所属 Client，也会使现有连接在周期检查中失效。
- 应用名和用户名共同决定子域名，因此不要依赖子域名隐藏敏感信息。

## 5. ICE 与 TURN

### 5.1 两种模式

- **STUN only / direct**：只尝试直连候选。Server 会从 SDP 中移除 `relay` 候选；如果配置了 TURN URL，验证会失败。`[]` 表示只使用本机候选；跨网访问通常需要至少一个 STUN。
- **TURN fallback**：允许直连，也允许通过 TURN 中继；至少需要一个带用户名和凭据的 TURN URL。可以同时配置 STUN。

常见 STUN 配置：

```json
[
  {"urls":["stun:stun.example.net:3478"]}
]
```

STUN + TURN：

```json
[
  {"urls":["stun:stun.example.net:3478"]},
  {
    "urls":[
      "turn:turn.example.net:3478?transport=udp",
      "turns:turn.example.net:5349?transport=tcp"
    ],
    "username":"user",
    "credential":"secret"
  }
]
```

### 5.2 校验规则

Server 在保存应用配置时校验 ICE JSON：

- JSON 大小不超过 10 KiB，且不能有未知字段；
- 每条记录必须有非空 `urls`；
- 所有 URL 总数最多 8 条，单条不超过 256 字符；
- 只允许 `stun:`、`stuns:`、`turn:`、`turns:`；
- TURN URL 的查询参数只允许 `transport=tcp` 或 `transport=udp`；
- STUN URL 不允许查询参数；
- `username` 最多 256 字符，`credential` 最多 512 字符；
- TURN 模式至少有一个 TURN URL，且每条 TURN 记录必须有 `username` 和 `credential`；
- STUN 模式不接受 TURN URL。

Client 还会在建立连接时再次检查 `allowed_ice_urls`，并使用本地 TLS/白名单配置。

### 5.3 TURN 凭据的暴露

浏览器必须收到 TURN 凭据才能使用 TURN。**Public 应用的任何访客都能从页面读取这些凭据。**

建议：

- 使用专用 TURN 账户，不要使用能管理 TURN 服务的密钥；
- 使用短有效期凭据，并在本项目外定期签发和更新；
- 配置配额、对等地址限制和滥用监控；
- 用 `allowed_ice_urls` 固定运维允许的 ICE 地址，避免 Client 接受任意中继；
- 不要把 TURN 凭据当作访问控制手段。

## 6. 代理行为与兼容范围

### 6.1 支持的应用流量

- 同源 iframe 内的导航、Fetch/XHR、EventSource、HTTP Range 视频请求、上传和下载；
- 同源窗口 WebSocket，包括文本、二进制和子协议，单条消息最大 16 MiB；
- URL 编码表单、multipart 表单和 keepalive 请求（含 `sendBeacon`）；
- 应用 Cookie（包括 `HttpOnly`）的兼容层；
- 同源 SPA 的 History API 路由与地址栏同步（路径、查询参数和 `#` 路由）。

即使浏览器与 Client 在同一局域网，应用启动仍需通过公网入口完成信令；只有数据通道建立后才走本地路径。悬浮球显示 `Direct P2P` 只表示未使用 TURN，不保证两端一定在同一子网。悬浮球可拖动，位置按应用保存在浏览器 `localStorage` 中；连接失败时可以重连。

### 6.2 请求与响应细节

- 不允许 `CONNECT`、`TRACE`、`TRACK` 方法。
- 上传使用有界分块和背压。不超过 64 KiB 的 URL 编码表单、multipart 表单和 keepalive 请求会带上实际 `Content-Length`；其他上传以流方式转交，上游必须接受 HTTP/1.1 chunked 或 HTTP/2 请求体。
- HTML 响应会注入同源 `/__p2page/socket.js` 兼容脚本（`attachment`、`HEAD`、204、205、206、304 和带 `Content-Encoding` 的响应除外）。
- Fetch/XHR 的 HTML 响应保留原始内容和验证器；只有页面导航注入脚本时，才会调整已知的 `Content-Length`，并移除不再适用的 `ETag`、`Content-Md5`、`Digest`、`Content-Digest`、`Accept-Ranges`。
- 响应不会自动进入浏览器的 HTTP 缓存；浏览器与上游之间也不是压缩透传。
- 刷新和新标签页会建立新的 WebRTC 连接，具有独立的建链成本。
- 上游同源 `Location` 在映射的基础路径下会改写；外部 OAuth、跨域请求和拒绝 iframe 的策略需要按应用验证。
- 上游默认收到内网 `Host`，同源 `Origin` 和 `Referer` 也会映射到内网地址。设置 `public_origin_headers` 后改用公有 `Host`、`Origin` 和浏览器允许携带的同源 `Referer`。
- p2page 不转发浏览器声明的 `X-Forwarded-For` 或 `X-Real-IP`，并会清理 `Forwarded`、`X-Forwarded-*`、`X-P2Page-*` 等头，不伪造访问者 IP。
- `/__p2page/` 是保留路径，上游不能占用。

### 6.3 Cookie 行为

- 应用 Cookie 保存在 Client 内存中，按应用、上游和浏览器会话隔离。
- 浏览器只在应用子域的 `localStorage` 中保存随机会话凭据，通过 WebRTC 交给 Client；刷新后可继续使用。不要分享该会话凭据。
- Client 重启、清除站点存储、会话空闲七天或被容量淘汰后，需要重新登录应用。
- 同源 HTML 提供 `document.cookie` 兼容层，但不向脚本暴露 `HttpOnly` Cookie；脚本也不能覆盖或删除 `HttpOnly` 条目。
- Cookie 不跨子域；`Domain` 属性会被丢弃。
- 原生 Cookie Store API 和多标签实时同步不保证与浏览器原生 Cookie 一致。
- `SameSite` 兼容是近似实现：能从 Referer 识别的跨站顶层导航及其同源 HTTP 跳转不发送 `SameSite=Strict` Cookie；没有 Referer 的入口无法可靠区分。
- **不要依赖 p2page 的 `SameSite` 兼容作为唯一 CSRF 防护。** 上游应校验 `Origin` 或使用 CSRF token，并避免用 GET 执行写操作。

### 6.4 Service Worker、顶层表单与下载

- 应用外壳注册 Service Worker，接管应用子域的同源请求，并通过 DataChannel 发给 Client。
- 已受 p2page Service Worker 控制的同源应用可通过 `target="_top"` 或 `target="_blank"` 提交 POST；提交浏览器需携带同源 `Origin`。新外壳建链后只提交一次，支持普通响应和 `303` 等改用 GET 的跳转，不重放需要保留 POST 的 `307`/`308`。
- 兼容脚本允许同源 `<a download>` 的普通点击和脚本点击走流式下载；浏览器右键“链接另存为”不在此范围。
- 首次访问或强制刷新绕过 Service Worker 时，公网入口仍会收到 URL 的路径和查询参数；Service Worker 接管后，普通刷新只请求固定的外壳入口。
- 私有应用重新授权时，当前路径临时保存在该标签页的 `sessionStorage` 中。

### 6.5 不支持的场景

- 直接访问应用 URL 得到的是浏览器外壳，**不是**给 `curl`、原生 API 客户端或爬虫使用的 HTTP 反向代理。
- 不通用重写 HTML/JS 中写死的内网绝对地址。上游应优先使用相对 URL，或允许配置外部 URL。
- 不支持上游自己的 Service Worker 注册。
- Web Worker 内的 WebSocket 不经过同源桥接。
- 上游 CSP 不会被移除。若 CSP 禁止加载同源 `/__p2page/socket.js`，WebSocket 桥接和 `document.cookie` 兼容层都不可用。上游需返回可注入的 HTML（一般为 UTF-8）。
- 上游返回 `X-Frame-Options: DENY` 或严格 `frame-ancestors` 时，应用无法在 iframe 中显示。
- 浏览器和上游之间没有通用的压缩透传，也没有浏览器 HTTP 缓存。

### 6.6 浏览器要求

需要现代浏览器，并启用：

- Service Worker；
- WebRTC 和 SCTP DataChannel；
- 同源 iframe；
- HTTPS（WebRTC 和 Service Worker 都要求安全上下文）。

建议在真实网络验证目标浏览器、STUN/TURN、上游应用的 CSP/iframe 策略和 Cookie 行为。

## 7. 安全模型

### 7.1 控制面与数据面

Server 只处理控制面：账户、授权、应用查找、SDP 转发和撤销检查。应用 HTTP/WebSocket 数据、应用 Cookie 和文件内容只在浏览器与 Client 之间传输。TURN 中继时，流量经过部署方指定的 TURN 服务，但仍是 DTLS 加密的 WebRTC 流量。

### 7.2 身份与秘密

- 密码使用 bcrypt；Client token 和邀请码只保存 SHA-256 摘要。
- 会话 Cookie 和私有应用授权 Cookie 使用独立的 host-only `__Host-` Cookie。
- 管理表单按精确 Origin 校验；私有应用登录使用短时、单次票据。
- Server 的登录会话默认七天；私有应用授权默认七天。
- 不要在日志中记录 Cookie、Authorization、邀请码、token 或管理 POST 表单。

### 7.3 上游边界

- `upstreams` 白名单按 scheme/host/port 匹配；同一 origin 上的不同路径不是独立安全边界。
- 不自动跟随上游重定向；不继承环境的 HTTP 代理。
- 把 Client 放在受控网段，仅允许访问实际要发布的 origin 和受信 DNS。
- 发布 Public 应用前，检查其内容、写操作、内置鉴权以及 Client 的网络权限。
- 上游应用仍应检查 CSRF 和 Origin，并优先用 `__Host-` Cookie 保存自己的敏感会话。

### 7.4 隔离与限制

- 应用子域隔离不等同于不同站点隔离：所有应用仍属于同一个浏览器 site，不宜把不受信的应用发布者或脚本与高敏感应用放在同一部署。
- Server 内置基础限流：登录按 IP 和用户名、配置写入按用户、信令和 Offer 按 IP/主机。生产环境仍应在公网代理侧增加限流和监控。
- 私有连接和配置撤销由 Server 周期检查，约 20 秒一次。
- 这是单实例方案，没有跨节点的高可用或共享会话。

## 8. 容量与默认限制

以下是当前实现的默认值，可能随版本调整：

| 项目 | 限制 |
| --- | --- |
| 登录会话 | 7 天 |
| 私有应用授权 | 7 天 |
| 邀请码 | 7 天，单次使用 |
| 单个 WebRTC 连接 | 最长约 2 小时 |
| Server 每个 Client 的待协商 Offer | 16 |
| Server/Client 每个 Client 的并发 Peer | 128 |
| Client 全局并发数据流 | 256 |
| 单个 Peer 的并发数据通道 | 64 |
| 浏览器并发 HTTP 请求 | 32（另有 256 排队上限） |
| 浏览器并发 SSE | 8 |
| 浏览器并发 WebSocket | 16 |
| WebSocket 单条消息 | 16 MiB |
| Client 内存中的浏览器会话 | 512 个 |
| 单个应用 Cookie 数 | 64 |
| 单个 Cookie 存储预算 | 约 8 KiB |
| Client 会话空闲回收 | 7 天 |

超过限制时通常表现为重连、`429 Too many connections`、`Client is busy` 或前端请求失败。热点应用应做容量测试。

## 9. 部署与运维

### 9.1 推荐目录与权限

```text
/usr/local/bin/p2page                 # 二进制
/etc/p2page/server.json               # 0600
/etc/p2page/client.json               # 0600
/etc/p2page/bootstrap.env             # 首次启动临时使用
/var/lib/p2page/p2page.db             # 服务用户可读写
/etc/p2page/tls/                      # 证书，仅代理可读
```

- Server 与 Client 使用独立的系统用户，或至少确保 Client 只能读取自己的配置。
- `client.json` 包含 token；`server.json` 所在目录不应包含明文管理员密码。
- 反向代理证书私钥只授予代理进程。

### 9.2 升级

- **Server 与 Client 必须一起升级**。协议版本不一致时，Offer 会返回 `409 Protocol changed` 或 Client 拒绝协商。
- 升级前备份数据库和配置。
- 从使用公网 Cookie 同步的旧版本升级时，须同步更新并重启 Server 和 Client；升级前清理浏览器中对应应用子域的旧 Cookie，或改用新的浏览器配置，否则浏览器可能在首次打开应用域名时自动发出旧 Cookie。旧应用会话不迁移，需要在应用内重新登录；旧版的静态目录优化配置不再需要。
- 浏览器页面在升级后应强制刷新，确保外壳、Service Worker 和 Client 使用同一协议版本。

### 9.3 备份与恢复

- 使用 `sqlite3 p2page.db ".backup backup.db"` 或等价的一致快照。
- 恢复时停止 Server，替换数据库及其 WAL/SHM，再启动。
- 备份中不要包含 `P2PAGE_ADMIN_PASSWORD` 或 Client token 明文；如果备份配置目录，注意 `client.json` 的敏感性。

### 9.4 日志与监控

- Server 日志主要记录应用查找错误、配置写入错误和连接异常；Client 日志记录信令连接和协商失败。
- 在代理侧记录状态码、延迟和字节数，但对 Cookie、Authorization、邀请码、token 和管理表单做脱敏。
- 监控 Client Online 状态、信令重连、ICE 失败率和 TURN 配额。
- 为 Server 数据库、证书到期和 TURN 凭据轮换设置告警。

## 10. 常见故障

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| 面板显示 Client Offline | Client 未运行、网络不通、token 失效、代理未支持 WebSocket | 检查 Client 日志、token 是否被删除、反向代理 Upgrade 配置 |
| `Client is offline` | 应用所属 Client 没有活动信令连接 | 启动或重连 Client |
| `504 Client WebRTC negotiation timed out` | ICE 协商失败、STUN/TURN 不可达、防火墙阻断 UDP/TCP | 检查 ICE 配置、TURN 凭据、网络和防火墙 |
| `502 Client rejected the WebRTC offer` | 协议版本不一致、应用目标不在白名单、ICE URL 不在 `allowed_ice_urls` | 同步升级、核对 `upstreams` 和 `allowed_ice_urls` |
| 悬浮球提示 `Direct P2P failed (STUN-only...)` | STUN 模式没有可用直连路径 | 增加 STUN，或改用 TURN 模式并配置 TURN |
| `Application frame unavailable` | 上游 `X-Frame-Options`/`frame-ancestors` 拒绝 iframe，或页面跳出了应用 origin | 调整上游 CSP/X-Frame-Options，或验证应用行为 |
| `401 Application login required` | 私有应用未授权或登录已过期 | 在管理域以所属账户重新登录并打开应用 |
| `403 Login handoff expired` / 已使用 | 跳转票据超过一分钟或被重放 | 从管理域重新打开应用 |
| 应用登录后刷新又要求登录 | Client 重启、站点存储被清除、会话空闲七天或被淘汰 | 重新登录应用；避免清理应用子域存储 |
| WebSocket 不工作 | 上游 CSP 禁止 `/__p2page/socket.js`，或使用 Web Worker 中的 WebSocket | 调整 CSP；Worker WebSocket 不在桥接范围 |
| Cookie 行为异常 | 跨子域、原生 Cookie Store API、多标签同步或 SameSite 近似语义 | 使用同源 Cookie，验证应用 CSRF 策略 |
| `netlinkrib: address family not supported` | systemd 单元缺少 `AF_NETLINK` | 在 `RestrictAddressFamilies` 中加入 `AF_NETLINK`，`daemon-reload` 后重启 |
| Client 启动报上游 TLS 错误 | 自签证书、私有 CA、主机名不匹配 | 配置 `ca_file`；仅在可信内网考虑 `insecure_skip_verify` |
| `configured target is not on the upstream allowlist` | 应用目标 origin 不在 Client 的 `upstreams` 中 | 在 Client 配置中加入对应 origin |
| `ICE URL is not on the client allowlist` | `allowed_ice_urls` 与 Server 下发的 URL 不完全一致 | 精确匹配完整 URL，或省略该配置 |
| `client already connected` / 新连接被拒绝 | 同一 token 已有活动连接 | 停止重复进程，或为不同主机创建不同 Client |
| `409 Protocol changed` | Server、Client 或浏览器外壳版本不一致 | 同步升级并强制刷新页面 |
| `429 Too many connections` / `Client is busy` | 触发限流或并发上限 | 降低请求速率，检查是否有异常流量，必要时扩容 |
| 启动报 `admin_username does not match` | 修改了已有部署的 `admin_username` | 恢复原管理员用户名 |
| 启动报 `first run requires P2PAGE_ADMIN_PASSWORD` | 首次启动未设置环境变量，或密码不在 12–72 字节 | 设置合法密码后重启 |
| 启动报 `invalid domain` | 域名带 scheme/端口、缺少点或标签不合法 | 使用纯小写 DNS 域名，例如 `example.com` |

## 11. 快速检查清单

上线前至少确认：

- [ ] `example.com` 和 `*.example.com` 解析正确，证书覆盖两个名字。
- [ ] Server 只监听回环，端口未直接暴露公网。
- [ ] 反向代理保留 Host、支持 WebSocket、正确覆盖 `X-Real-IP`。
- [ ] `P2PAGE_ADMIN_PASSWORD` 已从运行环境删除，数据库权限为 `0600`。
- [ ] Client 配置权限为 `0600`，token 未泄露，`upstreams` 最小化。
- [ ] 私有应用只对所属账户开放；Public 应用已审查内容和写操作。
- [ ] STUN/TURN 在真实网络可用；TURN 凭据短有效期且带配额。
- [ ] 浏览器测试覆盖登录、Cookie、上传下载、SSE、WebSocket 和顶层表单。
- [ ] 数据库备份、证书轮换、日志脱敏和监控已配置。
