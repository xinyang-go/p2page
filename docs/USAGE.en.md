# p2page Usage and Configuration

[简体中文](USAGE.md) | English

This document is for deployers and users. It covers all server and client configuration options, account and dashboard operations, ICE/TLS, access control, compatibility, security boundaries, and common troubleshooting. For development and protocol details, see [DEVELOPMENT.en.md](DEVELOPMENT.en.md).

## 1. Runtime Model

```text
Browser ──HTTPS──> public reverse proxy ──HTTP──> p2page server
   │                                               │
   │                                       signaling WebSocket
   │                                               │
   └──── WebRTC DataChannel ────> p2page client ──HTTP(S)──> intranet app
         (direct; own TURN when direct fails)
```

- **Server**: manages accounts, invitations, client tokens, application configuration, and subdomain routing. It relays WebRTC SDP between the browser and the client. It does not relay application HTTP/WebSocket data.
- **Client**: runs on the intranet and connects outbound to the server's signaling endpoint. It creates a WebRTC PeerConnection for each visitor, accesses upstreams through the local allowlist, and keeps application cookies in memory.
- **Browser**: loads the application shell from the server, runs the upstream application in an iframe, and proxies same-origin requests through a Service Worker and DataChannels.
- **TURN (optional)**: when direct connectivity fails, a TURN service operated by the deployer relays encrypted WebRTC traffic. The server does not provide TURN.

The server is a single-instance design: SQLite holds persistent data, while live signaling and connection state are kept in memory. Do not start multiple server instances against the same database.

## 2. Server Configuration

Start the server with `p2page server -config <path>`. `-config` is required; there are no other command-line overrides.

### 2.1 Configuration Fields

The configuration file is strict JSON: unknown fields cause startup to fail.

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `listen` | No | `127.0.0.1:8080` | HTTP listen address. Keep it on loopback and terminate TLS at the reverse proxy. Do not expose it directly to the internet. |
| `domain` | Yes | — | Base domain for management and applications. Must be a lowercase DNS name containing a dot, without a scheme, port, or wildcard. Maximum 180 characters; each label must be 1–63 characters. |
| `database` | Yes | — | SQLite database file path. The parent directory must already exist; a missing file is created with mode `0600`. |
| `admin_username` | Yes | — | Initial administrator username, restricted to `[a-z0-9]{1,30}`. Created on first start; on later starts it must match the existing administrator or startup fails. |
| `trusted_proxy_cidr` | No | Empty | One CIDR. `X-Real-IP` is trusted as the client address only when the request comes from this network. Do not configure it unless the proxy overwrites that header. |

Example:

```json
{
  "listen": "127.0.0.1:8080",
  "domain": "example.com",
  "database": "/var/lib/p2page/p2page.db",
  "admin_username": "admin",
  "trusted_proxy_cidr": "127.0.0.1/32"
}
```

### 2.2 First Start

The first start requires an environment variable:

```sh
export P2PAGE_ADMIN_PASSWORD='replace-with-a-long-random-passphrase'
./p2page server -config server.json
```

- The password must be 12–72 bytes of valid UTF-8. There is no default.
- After the administrator is created, later starts do not need the variable. Remove it from the environment, shell history, and service configuration.
- Passwords are stored as bcrypt hashes.
- There is only one administrator and no built-in password-reset flow. Keep the administrator password safe.
- The first start also generates a database signing key used for sessions and authorization tickets. Do not delete or replace `settings.signing_key`.

### 2.3 DNS, TLS, and Reverse Proxy

1. Point `example.com` and `*.example.com` at the reverse proxy.
2. Configure HTTPS for both names. Wildcard certificates usually require DNS-01 validation.
3. The reverse proxy must preserve `Host`, support WebSocket Upgrade, and forward requests to the `listen` address.
4. Configure `trusted_proxy_cidr` only when the proxy definitely overwrites `X-Real-IP`; otherwise clients can spoof the header to bypass IP-based rate limiting.
5. Configure request-body limits, timeouts, rate limiting, log redaction, and certificate rotation at the proxy.

See `deploy/Caddyfile`:

```caddyfile
example.com, *.example.com {
	tls /etc/p2page/tls/fullchain.pem /etc/p2page/tls/privkey.pem
	reverse_proxy 127.0.0.1:8080 {
		header_up X-Real-IP {remote_host}
	}
}
```

The server itself returns HSTS, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, `Origin-Agent-Cluster`, and related headers, but TLS must be provided by the proxy.

### 2.4 systemd and File Permissions

`deploy/p2page-server.service` is an example unit. Key points:

- use a dedicated `p2page` system user and group;
- `StateDirectory=p2page` creates `/var/lib/p2page` with mode `0700`;
- `EnvironmentFile=-/etc/p2page/bootstrap.env` can provide `P2PAGE_ADMIN_PASSWORD` on the first start;
- `UMask=0077`, `ProtectSystem=strict`, `ProtectHome=true`, and similar hardening options.

After first initialization, remove the password from `bootstrap.env` and restart the service. Configuration files and the database should be readable only by the service user.

### 2.5 Database and Backups

- SQLite uses WAL mode, foreign keys, and a 5-second busy timeout; the server uses a single database connection.
- Persistent data includes users, password hashes, invitation digests, session digests, client-token digests, and application configuration.
- For backups, use SQLite online backup or a consistent snapshot that includes `-wal` and `-shm`. Do not copy only the `.db` file while the server is running and ignore the WAL.
- Restarting the server drops live data channels. Clients reconnect their signaling, and browsers can reconnect using unexpired logins and cookies still held in client memory.

## 3. Client Configuration

Start the client with `p2page client -config <path>`. Its configuration file is also strict JSON; unknown fields cause startup to fail.

### 3.1 Configuration Fields

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `server` | Yes | — | HTTPS address of the server, for example `https://example.com`. Must be a bare origin: no non-root path, query, fragment, or user information. |
| `token` | Yes | — | The 43-character token shown when the client is created in the dashboard. It stops working immediately when the client is deleted. |
| `upstreams` | Yes | — | At least one upstream allowlist entry. Entries accept only HTTP(S) origins without a non-root path, query, or fragment (a root `/` may be omitted); duplicate origins are rejected. |
| `allowed_ice_urls` | No | Omitted | List of ICE URLs the server is allowed to deliver to the browser. Omitted means trust the server; `[]` rejects all external ICE URLs; a non-empty array is compared by exact string match. |

Example:

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

### 3.2 Upstream Entry Fields

| Field | Required | Default | Description |
| --- | --- | --- | --- |
| `url` | Yes | — | Upstream HTTP(S) origin, for example `http://router.lan` or `https://nas.lan:8443`. No path, query, fragment, or user information. |
| `ca_file` | No | System trust store | Extra trusted PEM certificates, such as a private CA or a trusted self-signed certificate. Relative paths resolve from the directory containing `client.json`. HTTPS only; certificate-chain and hostname/IP verification still apply. |
| `insecure_skip_verify` | No | `false` | Skip certificate-chain and hostname verification for this HTTPS upstream only. Cannot be combined with `ca_file`. Affects both HTTP and WSS upstream connections. |
| `public_origin_headers` | No | `false` | Make the upstream receive the public application's `Host`, same-origin `Origin`, and same-origin `Referer` that the browser is allowed to send. Does not change the dial address or TLS verification name. |

Notes:

- **Allowlist matching**: the client looks up an upstream by scheme/host/port. An application target sent by the server must match one of the `upstreams` origins, or the connection is rejected.
- **Application base path**: an application's upstream URL in the dashboard may include a base path (for example `https://nas.lan:8443/app`). A client `upstreams` entry may not include a non-root path; it only provides origin-level allowlisting and TLS configuration.
- **Address canonicalization**: after a match, the client dials using the authority from its local configuration, preventing DNS or spelling differences from being abused.
- **Self-signed certificates**: prefer `ca_file`. In the example, `nas-ca.pem` is relative to `client.json`.
- **Risk of skipping verification**: `insecure_skip_verify` still encrypts traffic but cannot prevent a man-in-the-middle from impersonating the upstream. Use it only on a trusted intranet when a CA cannot be configured. It does not affect TLS verification when the client connects to the public server.

### 3.3 The Three States of `allowed_ice_urls`

| Configuration | Behavior |
| --- | --- |
| `allowed_ice_urls` omitted | Trust all ICE URLs delivered by the server for the application. |
| `"allowed_ice_urls": []` | Reject every external ICE URL. Local candidates are used only when the server delivers no external ICE URLs; suitable for pure LAN direct connections. |
| Non-empty array | Every ICE URL delivered by the server must exactly match a string in the array, or the entire PeerConnection is rejected. |

Do not treat `allowed_ice_urls` as TURN credential protection: it restricts URLs, not secrets. Credentials are delivered to the browser with the ICE configuration.

### 3.4 Startup and Reconnection

```sh
./p2page client -config client.json
```

- A successful connection logs `connected to signaling server`.
- After signaling disconnects, the client retries with exponential backoff from 1 second to 30 seconds. The backoff resets after more than a minute of stable operation.
- Only one active signaling connection is allowed per token. If a duplicate starts, the new connection is rejected and the old one remains.
- Restarting the client clears upstream application cookies in memory, so users must sign in to the application again.
- `deploy/p2page-client.service` is a systemd example. If you harden it yourself, `RestrictAddressFamilies` must include `AF_NETLINK`; otherwise Pion cannot enumerate local interfaces and WebRTC negotiation fails with `netlinkrib: address family not supported by protocol`. After changing the unit, run `systemctl daemon-reload` and restart.

## 4. Accounts and Dashboard

### 4.1 First Login and Invitations

- Management domain: `https://<domain>`.
- First use `admin_username` and `P2PAGE_ADMIN_PASSWORD` to sign in.
- Regular users must register with a one-time invitation created by the administrator:
  - invitations expire after seven days by default;
  - each invitation can be used once;
  - unused, unexpired invitations can be revoked manually;
  - used or expired invitations no longer show a revoke action in the dashboard.
- Usernames allow 1–30 lowercase ASCII letters or digits. Display names may be up to 256 bytes and can contain Chinese and other characters.
- Passwords must be 12–72 bytes.
- Administrators can disable or re-enable regular users. Disabling deletes that user's sessions, disconnects their client signaling, and makes their applications inaccessible. Re-enabling allows them to sign in again.

### 4.2 Client Management

In the **Clients** section:

- **Add**: enter a client label (up to 128 bytes). The token is shown only once; save it immediately.
- **Delete**: immediately invalidates the token, cascades deletion to all applications owned by that client, and disconnects its signaling.
- **Online status**: shows Online / Offline based on whether the signaling connection exists.
- There is currently no client rename action; to rename, add a new client and migrate applications.
- One client can host multiple applications under the same account. One token allows only one active connection.

### 4.3 Application Management

Add or edit applications in the **Applications** section:

| Field | Description |
| --- | --- |
| `App name` | Application subdomain prefix, 1–30 lowercase letters or digits; unique within the account. Changing it changes the application URL. |
| `Display name` | Display name used in the dashboard and title, up to 256 bytes. May contain Chinese. |
| `Upstream URL` | Intranet upstream HTTP(S) URL, up to 2048 bytes. May include a base path; no query, fragment, or user information. |
| `Client` | Client that hosts this application. The application target origin must be present in that client's `upstreams`. |
| `ICE mode` | `STUN only / direct` or `TURN fallback`. |
| `ICE servers` | JSON array, see Section 5. |
| `Public` | When checked, anyone with the URL can access the application; otherwise it is private. |

The application URL is:

```text
https://<appname>-<username>.<domain>
```

- **Public**: accessible without signing in to the management domain. Suitable for services intentionally made public.
- **Private**: only the owning account can access it after signing in on the management domain. Administrators and other users do not automatically gain access.
- Private application grants are bound to the application and browser session and are issued through a short-lived, single-use handoff ticket; they last seven days by default.
- After an application or its client is deleted, existing connections are dropped by the server's periodic check (roughly every 20 seconds).
- Changing an application's target, public state, ICE configuration, or client also invalidates existing connections during the periodic check.
- The application name and username determine the subdomain, so do not rely on the subdomain to hide sensitive information.

## 5. ICE and TURN

### 5.1 Two Modes

- **STUN only / direct**: tries only direct candidates. The server removes `relay` candidates from the SDP; configuring a TURN URL fails validation. `[]` means local candidates only; cross-network access usually needs at least one STUN server.
- **TURN fallback**: allows direct connections and TURN relay; requires at least one TURN URL with a username and credential. STUN can be configured alongside TURN.

Common STUN configuration:

```json
[
  {"urls":["stun:stun.example.net:3478"]}
]
```

STUN + TURN:

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

### 5.2 Validation Rules

The server validates ICE JSON when saving an application:

- JSON must be at most 10 KiB and contain no unknown fields;
- every record must have a non-empty `urls`;
- at most 8 URLs total, each at most 256 characters;
- only `stun:`, `stuns:`, `turn:`, and `turns:` are allowed;
- TURN URL query parameters allow only `transport=tcp` or `transport=udp`;
- STUN URLs cannot have query parameters;
- `username` is at most 256 characters and `credential` at most 512;
- TURN mode requires at least one TURN URL, and every TURN record must have `username` and `credential`;
- STUN mode rejects TURN URLs.

When establishing a connection, the client also checks `allowed_ice_urls` and applies its local TLS and allowlist configuration.

### 5.3 TURN Credential Exposure

The browser must receive TURN credentials to use TURN. **Any visitor of a Public application can read those credentials from the page.**

Recommendations:

- use a dedicated TURN account, never a key that can administer the TURN service;
- use short-lived credentials issued and rotated outside this project;
- configure quotas, peer-address restrictions, and abuse monitoring;
- use `allowed_ice_urls` to pin the ICE addresses permitted by operations, so the client cannot accept arbitrary relays;
- do not treat TURN credentials as an access-control mechanism.

## 6. Proxy Behavior and Compatibility

### 6.1 Supported Application Traffic

- same-origin iframe navigation, Fetch/XHR, EventSource, HTTP Range video requests, uploads, and downloads;
- same-origin window WebSocket, including text, binary, and subprotocols, with a maximum message size of 16 MiB;
- URL-encoded forms, multipart forms, and keepalive requests (including `sendBeacon`);
- a compatibility layer for application cookies, including `HttpOnly`;
- same-origin SPA History API routing and address-bar synchronization (path, query, and `#` route).

Even when the browser and client are on the same LAN, application startup still requires signaling through the public entry point; only the data channel takes the local path. `Direct P2P` in the orb means TURN is not used; it does not guarantee that both ends are on the same subnet. The orb can be dragged, its position is stored per application in browser `localStorage`, and it offers reconnect after a failure.

### 6.2 Request and Response Details

- `CONNECT`, `TRACE`, and `TRACK` methods are not allowed.
- Uploads use bounded chunks and backpressure. URL-encoded forms, multipart forms, and keepalive requests up to 64 KiB carry the actual `Content-Length`; other uploads are streamed and the upstream must accept HTTP/1.1 chunked or HTTP/2 request bodies.
- HTML responses get an injected same-origin `/__p2page/socket.js` compatibility script (except `attachment`, `HEAD`, 204, 205, 206, 304, and responses with `Content-Encoding`).
- Fetch/XHR HTML responses keep their original content and validators. Only page-navigation injection adjusts a known `Content-Length` and removes no-longer-applicable `ETag`, `Content-Md5`, `Digest`, `Content-Digest`, and `Accept-Ranges`.
- Responses do not automatically enter the browser's HTTP cache. There is also no transparent compression between the browser and the upstream.
- Refreshes and new tabs establish new WebRTC connections with independent setup costs.
- Same-origin upstream `Location` headers are rewritten under the mapped base path. External OAuth, cross-origin requests, and frame-denial policies must be validated per application.
- By default, the upstream receives the intranet `Host`, and same-origin `Origin` and `Referer` are mapped to intranet addresses. With `public_origin_headers`, it receives the public `Host`, `Origin`, and same-origin `Referer` that the browser is allowed to send.
- p2page does not forward browser-declared `X-Forwarded-For` or `X-Real-IP`, and strips `Forwarded`, `X-Forwarded-*`, `X-P2Page-*`, and related headers. It does not fabricate visitor IPs.
- `/__p2page/` is reserved and cannot be claimed by an upstream.

### 6.3 Cookie Behavior

- Application cookies are kept in client memory, isolated by application, upstream, and browser session.
- The browser stores only a random session capability in the application subdomain's `localStorage` and hands it to the client over WebRTC; refreshes can continue using it. Do not share that session capability.
- Restarting the client, clearing site storage, seven days of session idle time, or capacity eviction requires signing in to the application again.
- Same-origin HTML gets a `document.cookie` compatibility layer, but scripts cannot see `HttpOnly` cookies; scripts also cannot overwrite or delete `HttpOnly` entries.
- Cookies do not cross subdomains; `Domain` attributes are discarded.
- The native Cookie Store API and real-time multi-tab synchronization are not guaranteed to match browser-native cookies.
- `SameSite` compatibility is approximate: cross-site top-level navigation identifiable from the Referer, and its same-origin HTTP redirects, do not receive `SameSite=Strict` cookies. Entries without a Referer cannot be reliably distinguished.
- **Do not rely on p2page's `SameSite` compatibility as the only CSRF defense.** Upstreams should validate `Origin` or use a CSRF token, and avoid performing writes with GET.

### 6.4 Service Worker, Top-Level Forms, and Downloads

- The application shell registers a Service Worker, takes over same-origin requests under the application subdomain, and sends them to the client over DataChannels.
- A same-origin application already controlled by the p2page Service Worker can submit POST through `target="_top"` or `target="_blank"`; the submitting browser must send a same-origin `Origin`. The new shell submits it once after connecting, supports ordinary responses and redirects such as `303` that switch to GET, and does not replay `307`/`308` responses that require preserving POST.
- The compatibility script allows ordinary and scripted clicks on same-origin `<a download>` links to use streaming downloads. The browser's right-click "Save link as" is out of scope.
- On first visit or a hard refresh that bypasses the Service Worker, the public entry point still receives the URL path and query. Once the Service Worker takes control, a normal refresh requests only the fixed shell entry point.
- When a private application is reauthorized, the current path is temporarily stored in that tab's `sessionStorage`.

### 6.5 Unsupported Scenarios

- Opening an application URL directly returns the browser shell. It is **not** an HTTP reverse proxy for `curl`, native API clients, or crawlers.
- Hard-coded intranet absolute URLs in HTML/JS are not generically rewritten. Upstreams should prefer relative URLs or allow configuring external URLs.
- Upstream Service Worker registration is not supported.
- WebSocket from inside a Web Worker is not bridged.
- Upstream CSP is not removed. If CSP blocks the same-origin `/__p2page/socket.js`, both WebSocket bridging and the `document.cookie` compatibility layer are unavailable. The upstream must return injectable HTML (usually UTF-8).
- If the upstream returns `X-Frame-Options: DENY` or a strict `frame-ancestors`, the application cannot be displayed in the iframe.
- There is no generic compression passthrough or browser HTTP caching between the browser and the upstream.

### 6.6 Browser Requirements

Use a modern browser with:

- Service Worker;
- WebRTC and SCTP DataChannel;
- same-origin iframe support;
- HTTPS (both WebRTC and Service Worker require a secure context).

Validate the target browser, STUN/TURN, upstream CSP/iframe policy, and cookie behavior on the real network.

## 7. Security Model

### 7.1 Control Plane and Data Plane

The server only handles the control plane: accounts, authorization, application lookup, SDP relay, and revocation checks. Application HTTP/WebSocket data, application cookies, and file contents travel only between the browser and the client. When TURN relays traffic, it passes through the TURN service chosen by the deployer, but remains DTLS-encrypted WebRTC traffic.

### 7.2 Identity and Secrets

- Passwords use bcrypt; client tokens and invitation codes are stored only as SHA-256 digests.
- Session cookies and private-application grant cookies use separate host-only `__Host-` cookies.
- Management forms validate the exact Origin; private-application login uses a short-lived, single-use ticket.
- Server login sessions last seven days by default; private-application grants also last seven days.
- Do not log cookies, Authorization headers, invitation codes, tokens, or management POST forms.

### 7.3 Upstream Boundary

- The `upstreams` allowlist matches by scheme/host/port; different paths on the same origin are not separate security boundaries.
- Upstream redirects are not followed automatically, and environment HTTP proxies are not inherited.
- Put the client on a controlled network segment and allow access only to the origins you actually intend to publish and to trusted DNS.
- Before publishing a Public application, review its content, write operations, built-in authentication, and the client's network permissions.
- Upstream applications should still validate CSRF and Origin, and prefer `__Host-` cookies for their own sensitive sessions.

### 7.4 Isolation and Limits

- Application subdomain isolation is not the same as cross-site isolation: all applications still belong to the same browser site. Do not place untrusted application publishers or scripts in the same deployment as highly sensitive applications.
- The server has basic rate limiting: login by IP and username, configuration writes by user, signaling and Offer by IP/host. Production deployments should add rate limiting and monitoring at the public proxy.
- The server periodically checks private connections and configuration revocations, roughly every 20 seconds.
- This is a single-instance design without cross-node high availability or shared sessions.

## 8. Capacity and Default Limits

These are current implementation defaults and may change between versions:

| Item | Limit |
| --- | --- |
| Login session | 7 days |
| Private application grant | 7 days |
| Invitation | 7 days, single use |
| Single WebRTC connection | Up to about 2 hours |
| Pending Offers per client on the server | 16 |
| Concurrent peers per client on server/client | 128 |
| Global concurrent data streams per client | 256 |
| Concurrent data channels per peer | 64 |
| Concurrent browser HTTP requests | 32 (plus a queue limit of 256) |
| Concurrent browser SSE | 8 |
| Concurrent browser WebSocket | 16 |
| Single WebSocket message | 16 MiB |
| Browser sessions in client memory | 512 |
| Cookies per application | 64 |
| Cookie storage budget per application | About 8 KiB |
| Idle client session eviction | 7 days |

Exceeding limits usually appears as reconnects, `429 Too many connections`, `Client is busy`, or failed frontend requests. Capacity-test hot applications.

## 9. Deployment and Operations

### 9.1 Recommended Layout and Permissions

```text
/usr/local/bin/p2page                 # binary
/etc/p2page/server.json               # 0600
/etc/p2page/client.json               # 0600
/etc/p2page/bootstrap.env             # temporary for the first start
/var/lib/p2page/p2page.db             # readable/writable by the service user
/etc/p2page/tls/                      # certificates, readable only by the proxy
```

- Use separate system users for the server and client, or at least ensure the client can read only its own configuration.
- `client.json` contains the token; the `server.json` directory should not contain a plaintext administrator password.
- Grant the reverse proxy's private key only to the proxy process.

### 9.2 Upgrades

- **Upgrade the server and client together.** If protocol versions differ, Offers return `409 Protocol changed` or the client rejects negotiation.
- Back up the database and configuration before upgrading.
- When upgrading from an older version that synchronized cookies through the public domain, update and restart both server and client together. Before upgrading, clear old cookies for the application subdomains in the browser, or use a fresh browser profile; otherwise the browser may automatically send old cookies on the first visit to the application domain. Old application sessions are not migrated and require signing in to the application again. The old static-directory optimization configuration is no longer needed.
- Force-refresh browser pages after an upgrade so the shell, Service Worker, and client use the same protocol version.

### 9.3 Backup and Restore

- Use `sqlite3 p2page.db ".backup backup.db"` or an equivalent consistent snapshot.
- To restore, stop the server, replace the database and its WAL/SHM files, then start it again.
- Backups should not contain `P2PAGE_ADMIN_PASSWORD` or plaintext client tokens. If you back up the configuration directory, remember that `client.json` is sensitive.

### 9.4 Logging and Monitoring

- Server logs mainly record application lookup errors, configuration-write errors, and connection failures. Client logs record signaling connections and negotiation failures.
- At the proxy, record status codes, latency, and byte counts, but redact cookies, Authorization headers, invitation codes, tokens, and management forms.
- Monitor client Online state, signaling reconnects, ICE failure rates, and TURN quotas.
- Alert on server database health, certificate expiry, and TURN credential rotation.

## 10. Troubleshooting

| Symptom | Possible Cause | Action |
| --- | --- | --- |
| Dashboard shows Client Offline | Client not running, network failure, invalid token, proxy lacks WebSocket support | Check client logs, whether the token was deleted, and the proxy Upgrade configuration |
| `Client is offline` | The application's client has no active signaling connection | Start or reconnect the client |
| `504 Client WebRTC negotiation timed out` | ICE negotiation failure, unreachable STUN/TURN, firewall blocking UDP/TCP | Check ICE configuration, TURN credentials, network, and firewall |
| `502 Client rejected the WebRTC offer` | Protocol version mismatch, application target not allowlisted, ICE URL not in `allowed_ice_urls` | Upgrade both sides together; verify `upstreams` and `allowed_ice_urls` |
| Orb shows `Direct P2P failed (STUN-only...)` | STUN mode has no usable direct path | Add STUN, or switch to TURN mode and configure TURN |
| `Application frame unavailable` | Upstream `X-Frame-Options`/`frame-ancestors` rejects the iframe, or the page left the application origin | Adjust upstream CSP/X-Frame-Options, or verify application behavior |
| `401 Application login required` | Private application not authorized or login expired | Sign in to the management domain as the owning account and open the application again |
| `403 Login handoff expired` / already used | Handoff ticket older than one minute or replayed | Open the application again from the management domain |
| Application asks for login again after refresh | Client restarted, site storage cleared, seven days idle, or session evicted | Sign in to the application again; avoid clearing the application subdomain's storage |
| WebSocket does not work | Upstream CSP blocks `/__p2page/socket.js`, or the WebSocket runs inside a Web Worker | Adjust CSP; Worker WebSocket is outside the bridge |
| Cookie behavior is unexpected | Cross-subdomain, native Cookie Store API, multi-tab sync, or approximate SameSite semantics | Use same-origin cookies and validate the application's CSRF policy |
| `netlinkrib: address family not supported` | systemd unit lacks `AF_NETLINK` | Add `AF_NETLINK` to `RestrictAddressFamilies`, run `daemon-reload`, and restart |
| Client reports upstream TLS errors | Self-signed certificate, private CA, or hostname mismatch | Configure `ca_file`; consider `insecure_skip_verify` only on a trusted intranet |
| `configured target is not on the upstream allowlist` | The application target origin is absent from the client's `upstreams` | Add the origin to the client configuration |
| `ICE URL is not on the client allowlist` | `allowed_ice_urls` does not exactly match the URL delivered by the server | Match the full URL exactly, or omit the setting |
| `client already connected` / new connection rejected | The same token already has an active connection | Stop the duplicate process, or create a different client for each host |
| `409 Protocol changed` | Server, client, or browser shell versions differ | Upgrade together and force-refresh the page |
| `429 Too many connections` / `Client is busy` | Rate limit or concurrency limit reached | Reduce request rates, inspect abnormal traffic, and scale if needed |
| Startup reports `admin_username does not match` | Changed `admin_username` on an existing deployment | Restore the original administrator username |
| Startup reports `first run requires P2PAGE_ADMIN_PASSWORD` | First start without the environment variable, or password outside 12–72 bytes | Set a valid password and restart |
| Startup reports `invalid domain` | Domain includes a scheme/port, lacks a dot, or has an invalid label | Use a plain lowercase DNS name such as `example.com` |

## 11. Pre-Launch Checklist

At minimum, confirm:

- [ ] `example.com` and `*.example.com` resolve correctly and the certificate covers both names.
- [ ] The server listens only on loopback and its port is not exposed directly to the internet.
- [ ] The reverse proxy preserves Host, supports WebSocket, and correctly overwrites `X-Real-IP`.
- [ ] `P2PAGE_ADMIN_PASSWORD` has been removed from the runtime environment and the database is mode `0600`.
- [ ] The client configuration is mode `0600`, the token is not leaked, and `upstreams` is minimal.
- [ ] Private applications are accessible only to their owning account; Public applications have had content and write operations reviewed.
- [ ] STUN/TURN works on the real network; TURN credentials are short-lived and quota-limited.
- [ ] Browser tests cover login, cookies, uploads/downloads, SSE, WebSocket, and top-level forms.
- [ ] Database backups, certificate rotation, log redaction, and monitoring are configured.
