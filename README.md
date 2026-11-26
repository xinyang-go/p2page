# p2page

简体中文 | [English](README.en.md)

> **浏览器直连内网应用，数据不经过公网服务器。**

p2page 让你用普通浏览器访问运行在内网（家里、办公室或实验室）的 Web 应用，例如 NAS 管理页、路由器后台、开发工具或自托管服务，不需要公网 IP、端口映射，也不用安装插件。

端口映射风险大，VPN 配置麻烦，传统内网穿透又会让所有流量经过公网服务器。p2page 把“牵线”和“数据传输”分开：公网服务器只负责登录、授权和帮浏览器与内网设备“互相认识”；连接建立后，浏览器直接与内网设备通信。页面内容、应用内的登录状态和上传下载文件都不经过公网服务器，也不占用它的带宽。你仍然需要一台小公网服务器做入口，但它不承载应用流量。

## 主要优势

- **数据不经过公网服务器**：页面、登录状态、上传下载都只在浏览器和内网设备之间传输，公网服务器看不到，也不承担带宽。
- **像访问普通网站一样使用**：用现代浏览器打开链接即可，不需要安装插件或客户端 App。
- **不暴露内网端口**：内网设备主动向外建立连接，不需要在路由器上开放端口。
- **默认私有，分享可控**：注册需要邀请码；私有应用只有所属账户能访问；公开应用必须由所有者主动勾选；删除授权即可失效。
- **直连优先**：能直连就不绕路；直连不通时，才使用你自己准备的中继服务器。
- **可自托管**：不依赖外部 SaaS，账户数据、证书和中继都由部署方自己掌握。

理念很简单：能直连就不绕路，默认不公开，自己的数据自己管。

> p2page 面向真实浏览器中的交互式网页，不是给命令行工具或爬虫使用的通用反向代理。

## Quick Start

以下步骤用一个公网域名 `example.com` 演示。需要准备：

- Go 1.24 或更高版本；
- 可解析的 `example.com` 与 `*.example.com` DNS；
- 覆盖这两个名字的 HTTPS 证书（泛域名证书通常需要 DNS-01 验证）；
- 一个能访问内网上游应用的主机，用于运行 Client；
- 一个公网反向代理（示例使用 Caddy）。

### 1. 编译

```sh
go build -trimpath -o p2page ./cmd/p2page
```

产物只有一个 `p2page` 二进制。Server 和 Client 使用同一份程序，通过子命令区分。

### 2. 配置 DNS、证书与反向代理

把 `example.com` 和 `*.example.com` 指向反向代理，并为两个名字配置 HTTPS。反向代理应满足以下要求：

- 保留原始 `Host`；
- 支持 WebSocket Upgrade；
- 建议覆盖写入 `X-Real-IP`，并且只有确认代理会覆盖时才配置 `trusted_proxy_cidr`；
- 将请求转发到 Server 的回环监听地址，例如 `127.0.0.1:8080`。

`deploy/Caddyfile` 是一个可直接修改的示例：

```caddyfile
example.com, *.example.com {
	tls /etc/p2page/tls/fullchain.pem /etc/p2page/tls/privkey.pem
	reverse_proxy 127.0.0.1:8080 {
		header_up X-Real-IP {remote_host}
	}
}
```

**不要把 Server 的监听端口直接暴露到公网。**

### 3. 启动 Server

从示例创建 `server.json`，把 `domain` 改成实际域名，并准备一个仅服务用户可读写的数据库目录：

```json
{
  "listen": "127.0.0.1:8080",
  "domain": "example.com",
  "database": "/var/lib/p2page/p2page.db",
  "admin_username": "admin",
  "trusted_proxy_cidr": "127.0.0.1/32"
}
```

首次启动必须通过环境变量设置管理员密码（12–72 字节，没有默认密码）：

```sh
export P2PAGE_ADMIN_PASSWORD='replace-with-a-long-random-passphrase'
./p2page server -config server.json
```

管理员创建成功后，从环境或服务配置中删除该密码。`deploy/` 提供了 systemd 单元示例；生产环境建议创建独立的 `p2page` 系统用户，并把配置权限收紧到 `0600`。

### 4. 创建 Client 与应用

1. 打开 `https://example.com`，用 `admin_username` 和刚设置的密码登录。
2. 在 **Clients** 中新增 Client，保存只显示一次的 token。
3. 在 **Applications** 中新增应用：选择 Client，填写上游 URL、应用名，按需勾选 Public，并选择 STUN 或 TURN 模式。
4. 应用地址形如 `https://<appname>-<username>.example.com`。

需要邀请其他用户时，管理员在 **Invitations** 中生成一次性邀请码，默认七天到期。

### 5. 启动 Client

在内网主机上创建 `client.json`，填入 Server 地址、token 和允许访问的上游 origin：

```json
{
  "server": "https://example.com",
  "token": "TOKEN_FROM_THE_DASHBOARD",
  "upstreams": [
    {"url": "http://192.168.1.10:8080"}
  ],
  "allowed_ice_urls": [
    "stun:stun.example.net:3478"
  ]
}
```

然后运行：

```sh
./p2page client -config client.json
```

内网使用自签 HTTPS 时，推荐通过 `ca_file` 指定私有 CA 或受信证书；也可以单独为某个上游设置 `insecure_skip_verify`，但这会跳过证书链和主机名校验，仅建议在可信内网使用。

### 6. 访问应用

在浏览器打开 `https://<appname>-<username>.example.com`。右下角悬浮球可以拖动，每个应用会独立记住摆放位置；它可以显示连接状态、直连或 TURN 中继、ICE RTT，并在失败时重连。

- **Public** 应用：任何拿到 URL 的人都可以访问。
- **Private** 应用：浏览器必须先在管理域 `https://example.com` 以应用所属账户登录，再由 p2page 跳转授权。管理员和其他用户不会自动获得访问权。

应用运行在浏览器外壳的同源 iframe 中；请使用启用了 Service Worker 和 WebRTC 的现代浏览器。直接访问应用 URL 得到的是浏览器外壳，不是可供 `curl` 使用的 HTTP 反向代理。

## 文档

| 文档 | 内容 |
| --- | --- |
| [docs/USAGE.md](docs/USAGE.md) / [English](docs/USAGE.en.md) | 完整配置项、账户与面板操作、ICE/TLS、兼容范围、安全与运维排障 |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) / [English](docs/DEVELOPMENT.en.md) | 架构、代码结构、信令与数据通道协议、构建测试、开发约束 |
| [AGENTS.md](AGENTS.md) | 面向自动化代理和贡献者的编译、测试与修改约束（English only） |

## 安全提示

- 发布 Public 应用前，请检查其内容、写操作、内置鉴权以及 Client 所在网段的访问权限。
- TURN 凭据会下发给应用页面，Public 应用的访客可以读取。请使用专用、短有效期、带配额的 TURN 账户。
- 上游白名单只按 scheme/host/port 匹配；同一 origin 下的不同路径不是独立安全边界。
- 公网代理侧建议增加限流、监控和日志脱敏，不记录请求 Cookie、Authorization 或管理 POST 表单。
