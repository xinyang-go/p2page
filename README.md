# p2page

用浏览器 WebRTC DataChannel 把明确授权的内网 Web 应用发布到公网。Go 二进制包含 `server` 和 `client` 两个子命令。Server 提供账户管理、子域名入口和 WebRTC 信令；连接建立后的应用 HTTP 请求和响应（包括应用 Cookie）、SSE 和 WebSocket 不经过 Server。启用 TURN 后，无法直连时，数据可以通过自备 TURN 中继；STUN-only 没有中继，协商超时直接报错。

## 部署

1. 使用 Go 1.24 或更高版本运行 `go build -trimpath -o p2page ./cmd/p2page`。
2. 将 `example.com` 和 `*.example.com` 的 DNS 指向公网反向代理，为两个名字配置 HTTPS（泛域名证书通常需要 DNS-01）。用 `deploy/server.example.json` 创建 `server.json`，把 `domain` 改为实际域名；准备可写且仅服务用户可读的数据库目录。Server 默认监听回环 HTTP，**不要把这个端口直接暴露到公网**。代理必须保留 Host、支持 WebSocket Upgrade，并覆盖 `X-Real-IP`；如果不满足后一个条件，就不要配置 `trusted_proxy_cidr`。见 `deploy/Caddyfile`。
3. 首次启动前设置高强度 `P2PAGE_ADMIN_PASSWORD`（12–72 字节，无默认密码），运行 `./p2page server -config server.json`。用 `admin_username` 登录，生成一次性邀请码。管理员初始化后删除启动环境里的密码。邀请码默认七天到期，也可手动失效；管理员可以停用/恢复普通账户。`deploy/` 有 systemd 单元示例，需创建 `p2page` 系统用户，并限制配置文件权限。
4. 注册普通账号，登录配置页，新增 Client 并保存只显示一次的 token。在内网按 `deploy/client.example.json` 创建 `client.json`，填入 HTTPS Server 地址、token 和允许的 HTTP(S) 上游 **origin**；运行 `./p2page client -config client.json`。内网自签 HTTPS 推荐使用 `ca_file`，可填私有 CA 或受信自签证书的 PEM，相对路径从配置文件目录解析，仍校验证书域名/IP。删除 Client 会使 token 失效，并级联删除其应用。
5. 在配置页选择 Client，填写上游 URL 和应用名，按需勾选 Public。管理域名为 `https://example.com`，应用域名为 `https://<appname>-<username>.example.com`。应用名和用户名仅允许 1–30 位小写 ASCII 字母和数字；显示名可使用中文和其他字符。Private 应用只能由所属账户通过管理域登录后访问；其他用户和管理员不会自动获得其访问权。

### 跳过内网上游 TLS 校验

确实无法配置 CA 时，可在 Client 的 `upstreams` 中为单个 HTTPS origin 显式设置：

```json
{"url": "https://nas.lan:8443", "insecure_skip_verify": true}
```

默认关闭，且不能与该条目的 `ca_file` 同时启用。该选项同时跳过 HTTPS 和 WSS 上游的证书链及主机名校验，仍使用 TLS 加密，但无法防止中间人冒充上游；仅建议在可信内网使用。它不影响 Client 连接公网 Server 时的 TLS 校验。

### ICE 配置

配置页支持逐应用修改模式和 ICE 列表。`[]` 表示只尝试可用的直连候选。在常见跨网场景应添加 STUN：

```json
[{"urls":["stun:stun.example.net:3478"]}]
```

TURN 模式至少要有一个带凭据的 TURN URL，也可同时配置 STUN：

```json
[
  {"urls":["stun:stun.example.net:3478"]},
  {"urls":["turn:turn.example.net:3478?transport=udp","turns:turn.example.net:5349?transport=tcp"],"username":"user","credential":"secret"}
]
```

浏览器必须收到所用 TURN 凭据；**Public 应用的访客可读取它们**。使用专用 TURN 账户、短有效期凭据（需在本项目外签发和更新）、配额和对等地址限制；不要填写可用来管理 TURN 的密钥。可在 Client 的 `allowed_ice_urls` 固定由运维允许的 ICE 地址。省略该项意味着信任 Server 下发的 ICE 地址；`[]` 仅允许不配置外部 ICE。

### 支持范围

- 应用运行在全屏同源 iframe 中；悬浮球可拖动，可查看直连或 TURN 状态，连接失败时可重连。使用现代、启用 Service Worker 与 WebRTC 的浏览器。直接访问应用 URL 得到浏览器外壳，**不是**给 `curl`、原生 API 客户端或爬虫使用的 HTTP 反向代理。
- 同源 iframe 导航、Fetch/XHR、EventSource、HTTP Range 视频请求及上传/下载走数据通道。HTTP 流采用有界分块和背压；不超过 64 KiB 的 URL 编码表单会带上实际 `Content-Length`，兼容依赖该字段的 LuCI 等 CGI。其他上传仍以流方式转交，上游必须接受 HTTP/1.1 chunked 或 HTTP/2 请求体。连接最长两小时；超大传输的实测吞吐取决于浏览器、SCTP、网络和 TURN。
- 同源窗口 WebSocket 通过注入脚本桥接，支持文本、二进制和子协议，单条消息最大 16 MiB。上游需返回可注入的 HTML（一般为 UTF-8），且 CSP 要允许加载同源 `/__p2page/socket.js`；项目不移除上游 CSP。Web Worker 内的 WebSocket 不经过该桥接。
- 应用 Cookie（包括 `HttpOnly`）保存在内网 Client 的内存中，按应用、上游和浏览器会话隔离。浏览器只在应用子域的 `localStorage` 中保存随机会话凭据，通过 WebRTC 交给 Client，刷新后可继续使用。Client 重启、清除站点存储、会话空闲七天或被容量淘汰后，需要重新登录应用。不要分享该会话凭据。
- 同源 HTML 提供 `document.cookie` 兼容层，不向脚本提供 `HttpOnly` Cookie。若上游 CSP 禁止加载同源兼容脚本，这个兼容层也不可用。Cookie 不跨子域；原生 Cookie Store API、跨站行为和多标签实时同步不保证与浏览器原生 Cookie 完全一致。
- 地址栏直接显示应用的路径、查询参数和应用自己的 `#` 路由；正常的 iframe 导航及同源 SPA 跳转会同步。Service Worker 接管后，普通刷新只向 Server 请求固定的外壳入口，应用路径仍通过 WebRTC 发给 Client。私有应用重新授权时，当前路径临时保存在该标签页。首次访问或强制刷新绕过 Service Worker 时，公网入口仍会收到 URL 的路径和查询参数。
- 推荐上游支持相对 URL 或允许配置外部 URL；不会通用重写 HTML/JS 中写死的内网绝对地址。上游同源 `Location` 在映射的 base path 下会改写；外部 OAuth、跨域请求和拒绝 iframe 的策略需要按应用验证。不支持上游自己的 Service Worker 注册。`/__p2page/` 为保留路径。

### 运行与安全

- 局域网直连时，应用启动仍需经过公网入口进行信令，但加载资源不再为 Cookie 产生公网往返。悬浮球可查看选中的候选类型及 ICE RTT；`Direct P2P` 表示未使用 TURN，不保证一定是同子网。
- 浏览器端会排队处理超过并发预算的资源请求，为 SSE 和 WebSocket 保留独立配额，避免大型前端同时加载大量 JS/CSS 时耗尽 WebRTC 数据通道。
- 从使用公网 Cookie 同步的旧版本升级时，须同步更新并重启 Server 和 Client。升级前清理浏览器中对应应用子域的旧 Cookie，或改用新浏览器配置；否则浏览器可能在首次打开应用域名时自动发出旧 Cookie。旧应用会话不迁移，需在应用内重新登录；原静态目录优化配置不再需要。
- 如果自行加固 systemd Client 单元，`RestrictAddressFamilies` 必须保留 `AF_NETLINK`，否则 Pion 无法枚举本地网卡，Client 虽然能连接信令 Server，WebRTC 协商仍会报 `netlinkrib: address family not supported by protocol`。修改单元后执行 `systemctl daemon-reload`，并重启 Client。
- 这是单实例方案：SQLite 保存用户、邀请码摘要、会话和应用配置；在线信令和连接状态在内存中。备份数据库时使用 SQLite 在线备份或包含 WAL 的一致快照。Server 重启会断开会话的数据通道，浏览器重连后可以继续使用仍有效的登录和 Cookie。
- 上游 allowlist 按 scheme/host/port 匹配，不自动跟随上游重定向，不继承环境的 HTTP 代理。把 Client 放在受控网段，仅允许访问实际要发布的 origin 和受信 DNS；同一 origin 上的各个 HTTP 路径不是独立的安全边界。发布 Public 应用前，仍要检查其内容、写操作、内置鉴权以及客户端网络权限。
- p2page 自身的登录 Cookie 和私有应用授权 Cookie 仍使用独立的 host-only Cookie；它们不是上游应用 Cookie。表单按精确 Origin 校验，应用登录跳转使用短时单次票据；密码用 bcrypt，Client token 和邀请码只保存摘要。现有 P2P 私有连接的会话或配置撤销由 Server 周期检查（约 20 秒检查一次）；禁用账户或删除 Client 会断开其信令。建议在公网代理侧再加限流、监控和日志脱敏，不记录请求 Cookie、Authorization 或管理 POST 表单。
- 子域隔离并不等同于不同站点隔离：应用仍同属一个浏览器 site，不宜把不受信的应用发布者或脚本与高敏感应用放在同一部署。内网应用本身仍应检查 CSRF 和 Origin，并优先用 `__Host-` Cookie 存放自己的敏感会话。
- 上线前在真实网络验证 STUN/外部 TURN、目标浏览器和应用，并对热点应用做容量测试。公网代理、证书轮换、数据库备份及 TURN 配置由部署方管理。

```sh
go test ./...
go test -race ./...
go vet ./...
```

可选的真实浏览器集成测试（需要 `chromium` 和 Node.js 22+）：

```sh
P2PAGE_BROWSER_TEST=1 go test -count=1 -run '^TestBrowser$' -v ./internal/server
```
