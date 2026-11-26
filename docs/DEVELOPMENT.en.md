# p2page Development Guide

[简体中文](DEVELOPMENT.md) | English

This document is for developers changing p2page. It covers the architecture, code layout, signaling and data-channel protocols, build and test procedures, debugging, and change constraints. For deployment and configuration, see [USAGE.en.md](USAGE.en.md). For agent collaboration constraints, see [AGENTS.md](../AGENTS.md).

## 1. Goals and Non-Goals

### Goals

- Access explicitly authorized intranet web applications with an ordinary browser.
- Keep the public server responsible only for accounts, routing, authorization, and WebRTC signaling; it does not carry application data.
- Provide `server` and `client` subcommands from one Go binary.
- Embed frontend assets with no Node.js build step and no external database service.
- Tighten security boundaries by default: loopback listening, host-only cookies, Origin validation, upstream origin allowlists, and secrets stored only as digests.
- Support common applications within browser capabilities: Fetch/XHR, SSE, WebSocket, Range, uploads/downloads, and cookies.

### Non-Goals

- Do not act as a general HTTP reverse proxy or serve `curl`, native clients, or crawlers.
- Do not provide anonymity, censorship resistance, or hide the client's real network location.
- Do not generically rewrite upstream HTML/JS, and do not modify upstream CSP.
- Do not implement a multi-tenant high-availability cluster; the current design is a single-instance SQLite deployment.
- Do not provide a TURN service; only connect to a TURN service operated by the deployer.

## 2. Code Layout

```text
cmd/p2page/main.go             subcommand parsing, signals, server/client entry points
internal/wire/wire.go          shared protocol constants, JSON types, header cleanup, frame writer
internal/server/server.go      server config, HTTP routing, signaling, SDP validation, maintenance
internal/server/manage.go      database schema, authentication, dashboard, mutations, invitations
internal/server/web/           embedded management pages, app shell, Service Worker, bridge scripts
internal/client/client.go      client config, signaling, PeerConnection, upstream allowlist
internal/client/stream.go      HTTP/WebSocket data-channel proxy, response rewriting, upload backpressure
internal/client/cookies.go     application cookie store, session capability, cookie control channel
deploy/                        Caddy and systemd examples, sample configs
internal/server/*_test.go      server and browser integration tests
internal/client/*_test.go      client cookie, stream, and TLS tests
```

There is no standalone `package.json` or frontend build directory. `internal/server/web/*` is compiled into the binary through `//go:embed web/*`. After changing HTML/CSS/JS, rebuild with `go build`.

## 3. Architecture and Lifecycle

### 3.1 Server Startup

`cmd/p2page/main.go` parses `server|client -config path.json`, creates a context that handles `SIGINT`/`SIGTERM`, and calls the corresponding `Run`.

`server.Run`:

1. Decode the config as strict JSON (`DisallowUnknownFields`).
2. Fill the default `listen`; validate `domain`, `database`, and `admin_username`.
3. Open the database with WAL, foreign keys, busy timeout, and a single connection.
4. `init` creates the schema and signing key, and on first start creates the single administrator from `P2PAGE_ADMIN_PASSWORD`.
5. Start the `maintain` periodic task.
6. Start `http.Server` with read/write timeouts and a header-size limit.
7. On cancellation, close all signaling connections and gracefully shut down HTTP.

### 3.2 Application Access Lifecycle

1. The browser requests `https://<app>-<user>.<domain>/...`.
2. The server splits `<app>-<user>` from the Host, validates the names, and looks up the application.
3. Public applications are allowed directly. Private applications validate the host-only grant cookie; otherwise the browser is redirected to the management domain's `/authorize`.
4. After login on the management domain, the server creates a short-lived, single-use `handoff` ticket and POSTs it to the application subdomain's `/__p2page/authorize` to obtain a grant cookie.
5. Authorized requests return the application shell `shell.html`, or embedded static assets.
6. The shell creates an `RTCPeerConnection` and POSTs `/__p2page/offer`; the server forwards the SDP to the matching client.
7. The client validates the target origin, ICE URLs, and protocol version, establishes the PeerConnection, and returns an answer.
8. Once DataChannels open, the browser sends application requests through the Service Worker to the client; the client accesses the upstream and returns the response.

### 3.3 Client Startup

`client.Run`:

1. Decode the config as strict JSON.
2. Validate that `server` is a bare HTTPS origin, token length is 43, and `upstreams` is non-empty.
3. Build an `http.Transport` and WebSocket dialer for each upstream, resolve `ca_file`, `insecure_skip_verify`, and `public_origin_headers`, and create the origin map.
4. Handle the three states of `allowed_ice_urls`.
5. Connect to `wss://<server>/__p2page/agent` with `Authorization: Bearer <token>`.
6. On a signaling Offer, create an independent context for each peer, negotiate the PeerConnection, and handle the two DataChannel types: control and request.
7. Reconnect with exponential backoff when signaling disconnects.

## 4. HTTP Routing and Access Control

### 4.1 Management Domain

The management domain (`r.Host == domain`) is handled by `manage`:

- `/__p2page/agent`: client signaling WebSocket.
- `/__p2page/style.css`, `/__p2page/login.js`: embedded assets.
- `/`, `/login`, `/register`: login and invitation registration.
- `/dashboard`: account, client, application, user, and invitation management; POST requires an exact Origin.
- `/logout`: POST only.
- `/authorize`: private-application login redirect that issues a short-lived handoff ticket.
- Every other path returns 404.

All management POSTs check `sameOrigin`, that is, `Origin == "https://" + r.Host`.

### 4.2 Application Subdomain

The application subdomain is handled by `ServeHTTP`:

- `/__p2page/authorize`: accepts a handoff ticket.
- `/__p2page/reauthorize`: private-application reauthorization entry.
- `/__p2page/shell`: application shell.
- `/__p2page/offer`: accepts a browser SDP Offer.
- `/__p2page/shell.js`, `worker.js`, `socket.js`, `style.css`: embedded assets.
- Any other non-`/__p2page/` path: returns the shell; the Service Worker and application path take over.

`/__p2page/` is reserved. The client's `destination` also rejects the prefix, preventing an upstream from claiming it.

### 4.3 Authentication and Authorization

- Usernames are `[a-z0-9]{1,30}`; passwords are 12–72 bytes; display names are at most 256 bytes.
- Login sessions use a random 32-byte token; the database stores only a SHA-256 digest. The cookie is `__Host-p2page-session`, with `Secure`, `HttpOnly`, `SameSite=Lax`, and a seven-day lifetime.
- Private-application grants use `__Host-p2page-grant`, bound to application ID, session digest, and expiry; seven days.
- Handoff tickets are HMAC-signed and valid for one minute. The `handoffs` table records the nonce and enforces single use.
- Invitation codes are random 32-byte values, stored only as digests, valid for seven days, and consumed atomically in a transaction.
- Passwords use bcrypt. Failed logins still run a dummy hash to reduce timing differences that could enumerate usernames.
- There is exactly one administrator. Disabling a user deletes their sessions and disconnects their client signaling.

## 5. Signaling Protocol

Signaling travels only over the server's `/__p2page/agent` WebSocket and does not carry application data. Messages use `wire.Signal`:

| Field | Description |
| --- | --- |
| `id` | Peer identifier; the client requires length 32. |
| `sdp` | Offer/Answer SDP. |
| `target` | Application upstream URL. |
| `origin` | Browser-side application origin, for example `https://app-user.example.com`. |
| `appId` | Application ID. |
| `version` | Protocol version, currently `wire.Version = 2`. |
| `ice` | ICE configuration delivered by the server to the client. |
| `error` | Reason the client rejected negotiation. |
| `closed` | Notifies the peer that it has closed. |

### 5.1 Client Attachment

`server.attach`:

- Rate limits by source IP (120 per minute).
- Requires `Authorization: Bearer <43-character token>`.
- Requires no `Origin` header.
- Queries `clients` and `users` and requires the user to be enabled.
- Allows only one signaling connection per client; a duplicate is closed with `1008 client already connected`.
- Sets a 256 KiB read limit, 45-second read deadline, and 15-second ping.
- Uses `pending` for offers awaiting responses and `peers` for active peers.

### 5.2 Offer/Answer

`server.offer`:

- Accepts POST only and validates the exact Origin.
- Rate limits by Host + source IP (100 per minute).
- Limits the request body to 220 KiB.
- `applicationOffer` requires SDP starting with `v=0` and exactly one `m=application`; in STUN mode it removes `typ relay` candidates.
- Validates `Version == wire.Version`.
- Allows at most 16 pending offers and 128 active peers per client; exceeding either returns 429.
- Sends the `Signal` to the client and waits up to 25 seconds. The client's answer must have the same version, no error, and not be closed.
- Returns 502/503/504 on failure; returns `{"sdp": "..."}` on success.

### 5.3 Revocation and Maintenance

`server.maintain` runs every 20 seconds:

- Deletes expired sessions and handoffs.
- Cleans up expired rate-limit records.
- Iterates active peers, re-queries the application and user, compares application ID, target, client, public state, mode, and ICE configuration, and validates private-application sessions. On invalidation it sends `closed` to the client and deletes the peer.

As a result, deleting a client/application, disabling a user, or changing application configuration drops existing connections within about 20 seconds.

## 6. DataChannel Protocol

### 6.1 Channels

The client accepts only ordered, reliable channels, that is, channels where `Ordered()` is true and `MaxRetransmits`/`MaxPacketLifeTime` are unset:

- `control`: only one per peer. Carries cookie state and scripted cookie writes.
- `request`: at most 64 concurrent per peer, used for HTTP/WebSocket streams.

The global concurrent stream limit is 256 (`client.streams`). The browser also has independent quotas of 32 HTTP, 8 SSE, and 16 WebSocket streams.

### 6.2 Frame Format

Every DataChannel message is a 1-byte type followed by a payload, with a maximum frame size of 60 KiB (`wire.Frame`). The type definitions must match the `P` object in `internal/server/web/shell.js`:

| Value | Go Constant | Purpose |
| --- | --- | --- |
| 1 | `Start` | Start request, JSON `wire.Request` |
| 2 | `Data` | Request-body or response-body chunk |
| 3 | `End` | End of HTTP request/response body |
| 4 | `Headers` | Response headers, JSON `wire.Response` |
| 5 | `Failure` | Failure reason string |
| 6 | `Credit` | Upload/download window credit |
| 7 | `TextEnd` | End of a WebSocket text message |
| 8 | `BinaryEnd` | End of a WebSocket binary message |
| 9 | `Close` | WebSocket close JSON |
| 10 | `Cookies` | Cookie state on the control channel |

Constants:

- `Chunk = 32 KiB`: maximum request/response chunk.
- `Frame = 60 KiB`: maximum frame size; payload must be smaller.
- `Window = 32`: upload credit window.
- `MaxWS = 16 MiB`: maximum single WebSocket message.
- `Prefix = "/__p2page/"`: reserved path.
- `Version = 2`: protocol version.

> **Synchronization requirement**: when changing frame types, protocol version, or these constants, update `internal/wire/wire.go`, `internal/server/web/shell.js`, `internal/client/*`, and the server's version validation together. Bump `Version` when necessary and update the documentation and browser tests.

### 6.3 HTTP Request Stream

1. The browser opens a `request` DataChannel and sends `Start` plus JSON `wire.Request`.
2. The client reads the Start within 10 seconds and validates the path, method, and target origin.
3. The client immediately fails `CONNECT`, `TRACE`, and `TRACK`.
4. If there is a request body, the client sends `Credit Window` to the browser; the browser sends `Data` chunks according to the credit and finishes with `End`.
5. The client passes the upload stream to `http.Transport` through an `io.Pipe` and returns `Credit` as it consumes data.
6. Upstream response headers are returned via `Headers`; the body is sent as `Data` according to browser credit and finishes with `End`.
7. Failures send a `Failure` string.

Response handling:

- `Set-Cookie` is not forwarded to the public internet. It is written into the client's in-memory cookie jar, and visible cookies are synchronized over the control channel.
- Same-origin `Location` headers are rewritten under the base path.
- HTML responses inject `/__p2page/socket.js`, adjust `Content-Length`, and remove stale validators.
- Browser-declared `X-Forwarded-*`, `X-Real-IP`, `Forwarded`, and `X-P2Page-*` are not forwarded by default; `proxyHeaders` cleans them consistently.

### 6.4 WebSocket Stream

`serveSocket`:

- Dials `ws`/`wss` using the same upstream allowlist and TLS configuration.
- Supports subprotocols, text, and binary messages, up to 16 MiB per message.
- Uplink: browser `Data` chunks followed by `TextEnd`/`BinaryEnd`; downlink works the same way.
- Close: the browser sends `Close` JSON `{code, reason}`; the client validates the code (1000 or 3000–4999) and reason length before forwarding.
- Supports only same-origin WebSocket in the application window; WebSocket inside a Web Worker is not supported.

### 6.5 Flow Control

- **Upload**: the client grants the browser `Window` credits first; each `Data` frame consumes one; the client returns `Credit` as it consumes data.
- **Download**: the browser sends `Credit` according to its own window; each `Data` frame sent by the client consumes one.
- `Writer.Send` serializes writes and sets a 20-second write deadline.
- An oversized frame, invalid type, credit overrun, or closed channel cancels the entire stream.

## 7. Cookie Subsystem

### 7.1 Storage Model

`sessionStore` keeps cookie jars keyed by `{appID, target, SHA-256(browser capability)}`:

- The browser stores a random 32-byte capability (base64url, 43 characters) in the application subdomain's `localStorage` and hands it to the client over the control channel's `open` message.
- The client receives the capability only over DTLS. It cannot use it to select an application or upstream; both are bound by the authenticated Offer.
- Each jar holds at most 64 cookies within a budget of about 8 KiB; there are at most 512 browser sessions globally.
- Sessions are deleted after seven days idle or when capacity evicts them.
- Restarting the client clears memory, so users must sign in to the application again.

### 7.2 Cookie Semantics

- `Domain` is always discarded; cookies do not cross subdomains.
- Paths are normalized into the application's externally visible path space.
- `__Host-`/`__Secure-` constraints are preserved; the `__Host-p2page-` prefix is rejected.
- Scripted writes cannot overwrite or delete `HttpOnly` entries.
- `SameSite` is approximate: `Strict` cookies are withheld when the Referer identifies a cross-site entry; without a Referer the distinction is unreliable.
- Visible cookies are synchronized as `wire.CookieState` with revisions; the `document.cookie` compatibility layer is provided by `shell.js` and `socket.js`.
- The native Cookie Store API and real-time multi-tab synchronization are not guaranteed to match.

### 7.3 Interaction with the Browser Shell

- Control channel: `open` (browser → client, carries the session capability), `ready` (client → browser, carries cookie state), `set` (scripted cookie write), and `ack` (client confirmation with the new state).
- Request channel: `wire.Request.Credentials` indicates whether cookies are included; `CookieRevision` avoids redundant snapshots.
- Response `Set-Cookie` is consumed inside the client and never exposed to the public internet.

## 8. Browser Shell and Service Worker

`internal/server/web/` has no bundling step and is embedded directly by Go:

- `shell.html` / `shell.js`: creates the PeerConnection, control/request channels, floating orb, route mirroring, and cookie compatibility layer.
- `worker.js`: registered at scope `/`, intercepts application requests, converts Fetch events into DataChannel streams, and handles first navigation and top-level POST.
- `socket.js`: injected into upstream HTML to bridge same-origin WebSocket and `document.cookie`.
- `dashboard.html`, `auth.html`, `handoff.html`, `reauthorize.html`: management pages.
- `style.css`: management UI and application shell styling.

Browser-side limits:

- Only ordered, reliable DataChannels are accepted.
- HTTP, SSE, and WebSocket have separate rate-limited queues.
- After a refresh, the Service Worker requests the fixed shell entry point while the application path continues through the DataChannel.
- On first visit or a hard refresh that bypasses the Service Worker, the path and query still reach the server.

## 9. Database

The schema in `manage.go` uses `CREATE TABLE IF NOT EXISTS` and has no separate migration framework:

| Table | Contents |
| --- | --- |
| `settings` | Key-value data such as the signing key. |
| `users` | Username, display name, bcrypt password, admin/disabled flags; unique administrator index. |
| `invites` | Invitation digest, expiry, consumer, and revocation flag. |
| `sessions` | Login session digest, user, and expiry. |
| `handoffs` | Single-use handoff nonce and expiry. |
| `clients` | Client label, token digest, and owning user. |
| `apps` | Application name, display name, target, public, mode, ICE JSON, owning user/client. |

When changing the schema:

- Remain compatible with existing databases; do not assume `CREATE TABLE IF NOT EXISTS` updates old tables;
- provide migration or backward-compatible reads for old data;
- preserve foreign-key cascade semantics: user → client → application;
- never store plaintext tokens, invitation codes, or passwords.

## 10. Security Invariants

Do not break these constraints:

1. **The data plane does not pass through the server**: the server does not proxy application HTTP/WebSocket content or store application cookies.
2. **The public entry point must terminate TLS at the reverse proxy**: the server listens on loopback by default and does not provide TLS.
3. **Management POSTs must validate the exact Origin**: do not relax `sameOrigin`.
4. **Private-application grants must bind application and session**: do not automatically grant access to administrators or other users.
5. **Secrets are stored only as digests**: client tokens, invitation codes, and sessions are stored as digests; never log plaintext.
6. **Cookie isolation**: application cookies do not cross applications, upstreams, or browser sessions; `Domain` must be discarded; `HttpOnly` cannot be overwritten by scripts.
7. **Upstream allowlists are origin-based**: do not relax them by path; do not follow redirects automatically or inherit environment proxies.
8. **Do not fabricate visitor IPs**: strip `Forwarded`, `X-Forwarded-*`, and `X-Real-IP`; read `X-Real-IP` only from trusted proxy networks.
9. **SDP allows DataChannels only**: require a single `m=application`; remove relay candidates in STUN mode.
10. **Protocol versions must match**: the server, client, and browser shell `Version` must stay in sync.
11. **TURN credentials may be public**: Public application pages deliver ICE configuration; never assume credentials are secret.
12. **Redact logs**: do not log cookies, Authorization headers, tokens, invitation codes, or management forms.

## 11. Build and Run

### 11.1 Environment

- Go 1.24 or newer (`go.mod` declares `go 1.24.0`).
- Node.js is not needed for the frontend; Node.js 22+ is needed only for the optional browser integration test.
- SQLite uses `modernc.org/sqlite` and does not require CGO.

### 11.2 Build

```sh
go build -trimpath -o p2page ./cmd/p2page
```

Optional static build:

```sh
CGO_ENABLED=0 go build -trimpath -o p2page ./cmd/p2page
```

The output is a single binary. The systemd units in `deploy/` assume installation at `/usr/local/bin/p2page`.

The container image uses the same static build:

```sh
docker build -t p2page .
```

It is a multi-stage build, runs as UID/GID `10001`, and contains the binary plus CA certificates and time zone data. It does not include an HTTPS reverse proxy.

### 11.3 Local Run

There is no separate development mode. Manual runs still require:

- a resolvable domain and `*.domain`;
- an HTTPS reverse proxy or test TLS termination;
- `P2PAGE_ADMIN_PASSWORD` for the first start;
- a client configuration that can reach an upstream.

Prefer unit tests and `httptest` for local verification; use the integration test below for real browser behavior.

## 12. Testing

### 12.1 Unit Tests and Static Checks

```sh
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

`gofmt -l .` should produce no output. Coverage includes:

- `internal/client/cookies_test.go`: cookie paths, `HttpOnly`, visible state, session capability.
- `internal/client/stream_test.go`: path validation, HTML injection, form Content-Length, Origin/Referer and header cleanup, Location/cookie path rewriting.
- `internal/client/tls_test.go`: `ca_file`, `insecure_skip_verify`, configuration validation.
- `internal/server/server_test.go`: concurrent invitation consumption, management CSRF, private-application grants and ticket replay, ICE validation.

### 12.2 Browser Integration Test

Requires `chromium` and Node.js 22+:

```sh
P2PAGE_BROWSER_TEST=1 go test -count=1 -run '^TestBrowser$' -v ./internal/server
```

The test:

- starts a TLS upstream and edge proxy with `httptest`;
- creates a server, client token, and application, and starts a real `client.Run`;
- drives Chromium through Node to access the application;
- verifies cookies do not leak to the public internet, paths do not leak, top-level POST is submitted exactly once, deep links, downloads, uploads, SSE, WebSocket, and first Service Worker navigation.

Run this test when changing proxy behavior, cookies, the Service Worker, top-level forms, or download logic. It is skipped by default and does not run under a normal `go test ./...`.

### 12.3 Test Data and Caveats

- The browser test injects a test CA through `SSL_CERT_FILE`; do not modify the production trust store.
- `internal/server/testdata/browser.mjs` is a test driver, not production code.
- The test uses a real `client.Run`, so changes to client startup affect the integration test.
- Race tests can be slow, but changes involving concurrent peers, cookies, or flow control should run with `-race` at least once.

## 13. Coding Conventions

- Prefer the standard library. New dependencies need a clear justification; avoid frontend frameworks or build chains.
- Keep configuration decoding strict (`DisallowUnknownFields`) with clear error messages.
- HTTP handlers should use `bad(w, code, msg)` for errors and must not leak internal details.
- Do not perform blocking I/O or network writes while holding shared locks. Signaling writes use `writeMu` and deadlines.
- All DataChannel writes go through `wire.Writer`; do not bypass frame-size or write-deadline checks.
- All upstream requests must go through `destination` and the origin allowlist; never concatenate user input into a URL directly.
- All response headers must pass through `wire.CleanHeaders`; do not forward hop-by-hop headers.
- Keep the frontend build-free and use native ES modules; do not introduce CDN assets.
- Comments should explain "why", especially around security boundaries and browser compatibility workarounds.
- Keep Chinese and English documentation in sync.

## 14. Change Checklist

Before submitting:

- [ ] `go test ./...`, `go test -race ./...`, and `go vet ./...` pass.
- [ ] `gofmt -l .` produces no output.
- [ ] Run `P2PAGE_BROWSER_TEST=1 ...` for browser behavior changes.
- [ ] When changing protocol frames/version, synchronize `wire.go`, `shell.js`, client, and server.
- [ ] When changing configuration, update `USAGE.md` / `USAGE.en.md` and sample JSON.
- [ ] When changing build/test/constraints, update `docs/DEVELOPMENT*` and `AGENTS.md`.
- [ ] Do not commit the `p2page` binary, `server.json`, `client.json`, databases, or certificates.
- [ ] Security invariants are not relaxed, and new logs contain no secrets.
- [ ] New or changed code has corresponding tests.

## 15. Debugging

- **Server logs**: application lookup, configuration writes, signaling disconnects, maintenance checks.
- **Client logs**: `connected to signaling server`, WebRTC negotiation failures, upstream request failures.
- **Browser**: DevTools Network/WebSocket, Application → Service Workers, and `chrome://webrtc-internals`.
- **Floating orb**: shows `Direct P2P` / `TURN relay`, candidate types, and ICE RTT; offers reconnect after failure.
- **Database**: use `sqlite3 p2page.db` for read-only queries about users, clients, applications, and invitations. Do not modify production data directly.
- **Protocol issues**: first confirm the server, client, and `shell.js` protocol versions match, then inspect frame types and size limits.
- **Service Worker issues**: clear site data or use a fresh browser profile; watch `worker.js` updates and the first-navigation handshake.

## 16. Deployment Artifacts

- `deploy/Caddyfile`: TLS termination, Host preservation, `X-Real-IP` overwrite, WebSocket forwarding.
- `deploy/server.example.json`: server configuration sample.
- `deploy/client.example.json`: client configuration sample with `ca_file` and `allowed_ice_urls`.
- `deploy/p2page-server.service`: server systemd unit with `StateDirectory`, `EnvironmentFile`, and hardening.
- `deploy/p2page-client.service`: client systemd unit; `RestrictAddressFamilies` must include `AF_NETLINK`.
- `deploy/docker-compose.example.yml`: server-only Compose example; it assumes an external HTTPS reverse proxy.
- `Dockerfile` / `.dockerignore`: multi-stage static build and non-root runtime image.
- `.github/workflows/ci.yml`: formatting, vet, unit and race tests, plus multi-architecture image builds published to GHCR on non-PR events.

See [USAGE.en.md](USAGE.en.md) for deployment, Docker, backup, upgrade, and troubleshooting.
