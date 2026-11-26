# AGENTS.md

This file constrains automation agents, contributors, and reviewers working in this repository. Read [README.en.md](README.en.md), [USAGE](docs/USAGE.en.md), and [DEVELOPMENT](docs/DEVELOPMENT.en.md) before making changes.

## Project Positioning

p2page publishes explicitly authorized intranet web applications to the public internet through browser WebRTC DataChannels. The public server handles only accounts, routing, authorization, and signaling; application data does not pass through the server. Reject any change that makes the server carry application traffic, weakens access control, or breaks browser isolation.

## Build and Test

Environment:

- Go 1.24 or newer (`go.mod` declares `go 1.24.0`).
- No frontend build step; `internal/server/web/*` is compiled into the binary through `go:embed`.
- Node.js 22+ and `chromium` are needed only for the optional browser integration test.

Required commands:

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

Build:

```sh
go build -trimpath -o p2page ./cmd/p2page
# optional: static build
CGO_ENABLED=0 go build -trimpath -o p2page ./cmd/p2page
```

Run this as well for browser-behavior changes:

```sh
P2PAGE_BROWSER_TEST=1 go test -count=1 -run '^TestBrowser$' -v ./internal/server
```

Requirements:

- `gofmt -l .` must produce no output. Run `gofmt -w` or `go fmt ./...` before submitting.
- Do not commit the `p2page` binary, `server.json`, `client.json`, databases, WAL/SHM files, or certificates; they are already in `.gitignore`.
- Do not skip failing tests, weaken assertions, or remove security checks just to make a build pass.

## Key Files

| Path | Purpose |
| --- | --- |
| `cmd/p2page/main.go` | Subcommands and process entry point. |
| `internal/wire/wire.go` | Protocol version, frame types, constants, JSON types, header cleanup. |
| `internal/server/server.go` | Server routing, signaling, SDP validation, maintenance. |
| `internal/server/manage.go` | Schema, authentication, dashboard, invitations, mutations. |
| `internal/server/web/` | Management pages, application shell, Service Worker, WebSocket/cookie bridges. |
| `internal/client/client.go` | Client config, signaling, PeerConnection, upstream allowlist. |
| `internal/client/stream.go` | HTTP/WebSocket proxy, response rewriting, backpressure. |
| `internal/client/cookies.go` | Application cookie store and control channel. |
| `deploy/` | Caddy, systemd, and sample configurations. |
| `docs/` | Bilingual usage and development documentation. |

## Required Constraints

### 1. Protocol Synchronization

`internal/wire/wire.go` and `internal/server/web/shell.js` share the protocol:

- `wire.Version` must equal `protocolVersion` in `shell.js`.
- Frame type values must match the `P` object in `shell.js`.
- Changes to `Chunk`, `Frame`, `Window`, `MaxWS`, `Prefix`, or similar constants must be checked in the server, client, and browser.
- When changing an incompatible frame format or semantic, bump the version and update tests, `docs/DEVELOPMENT*`, and `docs/USAGE*`.

### 2. Data-Plane Isolation

- The server must not proxy, cache, or log application HTTP/WebSocket content, application cookies, uploads, or downloads.
- The server only relays SDP signaling and revocation notices.
- Do not route application traffic through the server for debugging convenience.
- TURN is an optional relay operated by the deployer; do not assume the server provides TURN.

### 3. Security Invariants

Do not relax:

- exact Origin validation for management POSTs (`sameOrigin`);
- binding of private-application grants to the owning account, application, and session;
- host-only `__Host-` login and grant cookies;
- bcrypt passwords and digest-only storage for client tokens, invitation codes, and sessions;
- application cookie isolation: discard `Domain`, protect `HttpOnly`, and isolate by application, upstream, and browser session;
- upstream origin allowlists (scheme/host/port); do not relax by path, follow redirects automatically, or inherit environment proxies;
- SDP allowing exactly one `m=application`; remove relay candidates in STUN mode;
- stripping `Forwarded`, `X-Forwarded-*`, and `X-P2Page-*`; never fabricate visitor IPs; read `X-Real-IP` only from trusted proxy networks;
- leaving upstream CSP in place; do not generically rewrite upstream HTML/JS;
- keeping cookies, Authorization headers, tokens, invitation codes, and management forms out of logs.

### 4. Configuration and Compatibility

- Keep configuration decoding strict (`DisallowUnknownFields`); update examples and documentation when adding fields.
- Do not casually change `admin_username` on an existing deployment; startup will fail, and that is intentional protection.
- The database schema currently uses `CREATE TABLE IF NOT EXISTS` with no migration framework. Schema changes must remain compatible with old databases; do not assume old tables update automatically.
- Do not break foreign-key cascades: user → client → application.
- When changing the server/client protocol, upgrade both sides together; browser pages must also be force-refreshed.

### 5. Dependencies and Frontend

- Prefer the standard library. New dependencies need a clear justification.
- Do not introduce frontend frameworks, bundlers, CDN assets, or runtime Node.js dependencies.
- After editing frontend files, rebuild with `go build`; do not add a separate build directory.
- Do not commit generated assets or minified output.

## Workflow

1. **Read the code and docs first**: determine whether the change affects the server, client, wire protocol, browser shell, or deployment files.
2. **Make the minimal change**: do not refactor unrelated code or overwrite the user's uncommitted work.
3. **Add tests**: protocol, security, cookie, flow-control, and configuration-validation changes need tests.
4. **Run the verification matrix**:
   - Documentation only: check Chinese/English correspondence, links, and configuration examples.
   - Go code: `gofmt`, `go vet`, `go test`, and `-race` when needed.
   - Browser behavior: run `P2PAGE_BROWSER_TEST=1`.
   - Protocol: confirm `wire.Version`, `shell.js`, and client/server validation agree.
   - Configuration: update `deploy/*.example.json` and `docs/USAGE*`.
   - systemd/Caddy: verify the examples in `deploy/`; when available, run `systemd-analyze verify deploy/*.service`.
5. **Keep documentation in sync**: update Chinese and English together; never change only one side. `AGENTS.md` is English-only.
6. **Report results**: state what changed, which tests ran, and any known limitations.

## Prohibited

- Do not expose the server's listening port directly to the public internet or remove the assumption that a reverse proxy terminates TLS.
- Do not weaken Origin/CSRF checks, cookie constraints, TLS verification, or upstream allowlists.
- Do not log or echo plaintext passwords, tokens, invitation codes, cookies, or Authorization headers.
- Do not leak application data in logs, error responses, or test snapshots.
- Do not generically strip upstream CSP, rewrite upstream HTML/JS, or inject third-party scripts.
- Do not add telemetry, external SaaS dependencies, or undocumented network requests.
- Do not delete or skip security-related tests to "fix" the build.
- Do not commit binaries, databases, certificates, real configurations, or any secrets.

## Definition of Done

A change is complete only when:

- [ ] The code compiles and relevant tests and static checks pass.
- [ ] The security invariants in Section 3 are intact.
- [ ] Protocol/configuration/deployment changes are reflected in the corresponding documentation and examples.
- [ ] Chinese and English documentation are updated as a pair, except `AGENTS.md`, which is English-only.
- [ ] No generated artifacts or secrets are committed.
- [ ] The change description includes verification commands and results.
