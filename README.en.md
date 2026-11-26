# p2page

[简体中文](README.md) | English

> **Your browser connects directly to the intranet app; data does not pass through the public server.**

p2page lets you open web applications running on your intranet (at home, in the office, or in a lab) with an ordinary browser — for example a NAS admin page, router console, developer tool, or self-hosted service — without a public IP, port forwarding, or plugins.

Port forwarding carries risk, VPNs are cumbersome, and traditional tunneling sends all traffic through a public server. p2page separates "introducing the two sides" from "carrying the data": the public server only handles sign-in, authorization, and introducing the browser to the intranet device. Once connected, the browser talks directly to the intranet device. Pages, signed-in state inside the application, and uploaded/downloaded files do not pass through the public server, and it does not carry their bandwidth. You still need a small public server as the entry point, but it does not carry application traffic.

## Main Advantages

- **Application data bypasses the public server**: pages, signed-in state, uploads, and downloads travel only between the browser and the intranet device. The public server cannot see them and does not carry their bandwidth.
- **Works like an ordinary website**: open the link in a modern browser. No plugin or client app is required.
- **No intranet ports exposed**: the intranet device connects outward, so you do not need to open ports on the router.
- **Private by default, sharing under control**: registration requires an invitation; private applications are accessible only to their owning account; public access must be explicitly enabled by the owner; deleting authorization revokes access.
- **Direct connection first**: traffic takes a direct path when possible, and uses a relay you provide only when direct connectivity fails.
- **Self-hosted**: no external SaaS dependency. Account data, certificates, and relays remain under the deployer's control.

The philosophy is simple: take the direct path when possible, keep applications private by default, and keep your own data under your control.

> p2page is for interactive pages in a real browser, not a general reverse proxy for command-line tools or crawlers.

## Quick Start

The following example uses a public domain `example.com`. You need:

- Go 1.24 or newer;
- DNS records for `example.com` and `*.example.com`;
- an HTTPS certificate covering both names (wildcard certificates usually require DNS-01 validation);
- a host that can reach the intranet upstream application, used to run the client;
- a public reverse proxy (the example uses Caddy).

### 1. Build

```sh
go build -trimpath -o p2page ./cmd/p2page
```

The result is a single `p2page` binary. The server and client are the same program, selected by subcommand.

### 2. Configure DNS, TLS, and the reverse proxy

Point `example.com` and `*.example.com` at the reverse proxy and configure HTTPS for both names. The reverse proxy should meet these requirements:

- preserve the original `Host` header;
- support WebSocket Upgrade;
- preferably overwrite `X-Real-IP`, and configure `trusted_proxy_cidr` only when you are sure the proxy overwrites it;
- forward requests to the server's loopback listener, for example `127.0.0.1:8080`.

`deploy/Caddyfile` is a ready-to-edit example:

```caddyfile
example.com, *.example.com {
	tls /etc/p2page/tls/fullchain.pem /etc/p2page/tls/privkey.pem
	reverse_proxy 127.0.0.1:8080 {
		header_up X-Real-IP {remote_host}
	}
}
```

**Never expose the server's listening port directly to the internet.**

### 3. Start the server

Create `server.json` from the example, change `domain` to your real domain, and prepare a database directory readable and writable only by the service user:

```json
{
  "listen": "127.0.0.1:8080",
  "domain": "example.com",
  "database": "/var/lib/p2page/p2page.db",
  "admin_username": "admin",
  "trusted_proxy_cidr": "127.0.0.1/32"
}
```

The first start requires the administrator password through an environment variable (12–72 bytes; there is no default):

```sh
export P2PAGE_ADMIN_PASSWORD='replace-with-a-long-random-passphrase'
./p2page server -config server.json
```

After the administrator is created, remove the password from the environment or service configuration. `deploy/` contains systemd unit examples. In production, create a dedicated `p2page` system user and restrict configuration files to `0600`.

### 4. Create a client and an application

1. Open `https://example.com` and sign in with `admin_username` and the password you just set.
2. Under **Clients**, add a client and save the token, which is shown only once.
3. Under **Applications**, add an application: select the client, enter the upstream URL and application name, check Public if needed, and choose STUN or TURN mode.
4. The application URL looks like `https://<appname>-<username>.example.com`.

To invite other users, an administrator creates a one-time invitation under **Invitations**. Invitations expire after seven days by default.

### 5. Start the client

On the intranet host, create `client.json` with the server address, token, and allowed upstream origins:

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

Then run:

```sh
./p2page client -config client.json
```

For a self-signed HTTPS upstream, prefer `ca_file` with a private CA or the trusted certificate. You can also set `insecure_skip_verify` for one specific upstream, but this skips certificate-chain and hostname verification and should be used only on a trusted intranet.

### 6. Open the application

Open `https://<appname>-<username>.example.com` in a browser. The floating orb in the bottom-right corner can be dragged, and each application remembers its position. It shows the connection state, whether the path is direct or relayed through TURN, and the ICE RTT. It also offers a reconnect action after a failure.

- **Public** applications: anyone with the URL can access them.
- **Private** applications: the browser must first sign in to the management domain `https://example.com` as the owning account; p2page then redirects and grants access. Administrators and other users do not automatically gain access.

Applications run in a same-origin iframe inside the browser shell. Use a modern browser with Service Worker and WebRTC support. Opening the application URL directly returns the browser shell; it is not an HTTP reverse proxy for `curl`.

## Documentation

| Document | Contents |
| --- | --- |
| [USAGE](docs/USAGE.en.md) / [中文](docs/USAGE.md) | Full configuration reference, account and dashboard operations, ICE/TLS, compatibility, security, and operations troubleshooting |
| [DEVELOPMENT](docs/DEVELOPMENT.en.md) / [中文](docs/DEVELOPMENT.md) | Architecture, code layout, signaling and data-channel protocol, build and test, development constraints |
| [AGENTS.md](AGENTS.md) | Build, test, and change constraints for automation agents and contributors (English only) |

## Security Notes

- Before publishing a Public application, review its content, write operations, built-in authentication, and the network access available to the client.
- TURN credentials are delivered to the application page and can be read by visitors of a Public application. Use a dedicated TURN account with short-lived credentials and quotas.
- The upstream allowlist matches by scheme/host/port only. Different paths on the same origin are not separate security boundaries.
- Add rate limiting, monitoring, and log redaction at the public proxy. Do not log request cookies, Authorization headers, or management POST forms.
