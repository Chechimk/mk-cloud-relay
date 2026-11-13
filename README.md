# MK Cloud Relay

A protocol-agnostic HTTP relay for **Google Cloud Run** (and any host that
runs a container). One small Go binary forwards HTTP/1.1, HTTP/2, gRPC,
WebSocket, HTTPUpgrade and XHTTP traffic to backends you configure, without
reading or changing the payload. Raw TCP services, such as an HTTP proxy with
HTTPS `CONNECT` or SSH, travel through authenticated WebSocket tunnels.

- No license keys, activation, telemetry or "phone home" of any kind.
- No open proxy: every backend comes from your configuration. Tunnels and the
  forward proxy always require credentials.
- Payloads are never logged. Paths are not logged either, because they often
  carry secrets such as gRPC service names.
- Works with Xray/V2Ray servers (VLESS, VMess, Trojan, …), VPSPanel-managed
  servers, ordinary websites and APIs, and HTTP proxies such as Squid.

```
                         Cloud Run (TLS, one domain)
client ──HTTPS / h2──▶  ┌───────────────────────────┐
                        │      MK Cloud Relay       │
                        │ /grpcsvc/  ─▶ h2c   ──────┼──▶ Xray gRPC     VPS:2096
                        │ /xh        ─▶ HTTP/1.1 ───┼──▶ Xray XHTTP    VPS:8080
                        │ /ws        ─▶ WebSocket ──┼──▶ Xray WS / SSH-WS VPS:8880
                        │ /api/      ─▶ https ──────┼──▶ any web backend
                        │ /t/proxy   ─▶ tunnel ─────┼──▶ HTTP proxy    VPS:3128
                        └───────────────────────────┘
```

## Contents
- [What works where](#what-works-where)
- [Quick start on Cloud Run](#quick-start-on-cloud-run)
- [Configuration](#configuration)
- [Recipes](#recipes): [Xray / VPSPanel](#xray-and-vpspanel-backends) · [HTTP proxy](#http-and-https-proxy) · [plain websites](#plain-http-backends)
- [Operations](#operations): logs, updates, rollback
- [Security](#security)
- [Testing](#testing) · [Replacing thomascloud](docs/THOMASCLOUD.md) · [Test results](docs/TEST-RESULTS.md)

## What works where

| Traffic | Relay | Cloud Run | Notes |
|---|---|---|---|
| HTTP/1.1 reverse proxy | yes | yes | streaming, no buffering |
| HTTP/2 from clients | yes | yes | Cloud Run speaks h2 to clients |
| gRPC (unary + bidirectional streaming, trailers) | yes | see [results](docs/TEST-RESULTS.md) | backend over h2c |
| WebSocket (VLESS/VMess/Trojan WS, SSH-WS) | yes | see results | HTTP/2 end-to-end must be **off** |
| HTTPUpgrade (Xray) | yes | see results | same as WebSocket |
| XHTTP packet-up | yes | see results | works over HTTP/1.1 and h2 |
| XHTTP stream-up / stream-one | yes | see results | need streaming uploads from the platform |
| TCP over WebSocket tunnel (`tunnel://`) | yes | see results | carries HTTP proxy CONNECT, SSH, … |
| HTTP forward proxy (`GET http://…` to the relay) | yes | depends on front end | through `FORWARD_PROXY_UPSTREAM` |
| HTTPS `CONNECT` sent directly to the relay | yes | **no** | Cloud Run rejects the CONNECT method; use a tunnel |
| Raw TCP, UDP, QUIC, Hysteria2, SOCKS5 directly | no | no | Cloud Run accepts only HTTP; TCP works via a tunnel, UDP does not |

"Relay: yes" means it is verified by automated tests with real Xray-core
traffic in Docker. The Cloud Run column is filled from live tests in
[docs/TEST-RESULTS.md](docs/TEST-RESULTS.md).

### Cloud Run facts this design relies on
From the official Cloud Run documentation:
- By default Cloud Run downgrades requests to HTTP/1 for the container, **except
  native gRPC traffic, which still arrives as HTTP/2 cleartext (h2c)**. The
  relay accepts both on one port, so one service can carry gRPC and WebSocket
  together.
- With "HTTP/2 end-to-end" on, everything arrives as h2c and **WebSockets are
  not supported**. Keep it off unless a route needs streaming that only works
  over h2.
- Cloud Run does not pass `CONNECT` or `TRACE` requests to containers.
- Every request, WebSocket included, ends at the request timeout (maximum 60
  minutes). Clients must reconnect; Xray and the tunnel client do this.

## Quick start on Cloud Run

Requirements: a Google Cloud project with billing, the `gcloud` CLI (Cloud
Shell has it), and a backend reachable from the internet.

```bash
git clone https://github.com/Chechimk/mk-cloud-relay
cd mk-cloud-relay
cp deploy/env.example.yaml deploy/env.yaml   # edit ROUTES for your backend
deploy/deploy.sh mk-relay-test europe-west1 deploy/env.yaml
```

`deploy.sh` runs `gcloud run deploy --source .` with the recommended settings:
timeout 3600 s, concurrency 1000, HTTP/2 end-to-end off, 256 MiB, unauthenticated
invocations allowed (the relay does its own access control), and up to 5 instances.
Deploy a **new test service** first; never overwrite a working service until the
test service passes.

Console users: Cloud Run → Create service → "Continuously deploy from a
repository" → this repository, build type **Dockerfile** (`/Dockerfile`), container
port 8080, request timeout 3600, and "HTTP/2 end-to-end" **unticked**. Add the
variables below under *Variables & Secrets*.

## Configuration

All settings are environment variables.

### Routes
`ROUTES` holds `PATH_PREFIX=TARGET[,option…]` entries separated by `;` or new
lines. The longest matching prefix wins, paths are **not** rewritten unless you
add `strip`, and unmatched paths get a plain 404.

| Target | Backend protocol |
|---|---|
| `http://host:port` | HTTP/1.1; requests with `Content-Type: application/grpc` use h2c automatically |
| `h2c://host:port` | always HTTP/2 cleartext (prior knowledge) |
| `https://host:port` | TLS; negotiates h2 or HTTP/1.1; verifies the certificate |
| `tunnel://host:port` | authenticated WebSocket → raw TCP |
| `host:port` | same as `http://` |

| Option | Meaning |
|---|---|
| `host=NAME` | Host header sent to the backend (default: the client's Host) |
| `host=backend` | send the backend's address as Host |
| `strip` | remove the prefix before forwarding |
| `xff` | add X-Forwarded-For/Proto/Host (off by default for privacy) |
| `proto=h1\|h2c\|auto` | override the protocol of an `http://` target |
| `sni=NAME` | TLS server name for `https://` targets |
| `insecure` | skip certificate checks for `https://` (self-signed backends) |
| `token=T1\|T2` | tunnel tokens for this route only |

Example:
```
ROUTES=/49qwp74u/=http://34.63.66.185:80; /xh=http://34.63.66.185:8080; /t/proxy=tunnel://34.63.66.185:3128
```

### All variables

| Variable | Default | Purpose |
|---|---|---|
| `ROUTES` | – | route table (above) |
| `DEFAULT_TARGET` | – | a single catch-all backend, same as a `/` route. `PROXY_TARGET` and `PROXY_TARGETS` are accepted as aliases for one `host:port` |
| `PORT` | 8080 | listen port (Cloud Run sets it) |
| `TUNNEL_TOKENS` | – | comma-separated tokens (16+ characters) for `tunnel://` routes; required if any exist |
| `MAX_TUNNELS` | 1000 | open tunnels per instance (0 = no limit) |
| `FORWARD_PROXY_UPSTREAM` | – | `http://[user:pass@]host:port` of the proxy that serves forward-proxy requests |
| `FORWARD_PROXY_USERS` | – | `user:password,…` (passwords 8+ characters); required with the upstream |
| `HEALTH_PATH` | – | path that answers `200 ok` (for example `/_mk/health`) |
| `LOG_LEVEL` | info | debug, info, warn, error |
| `LOG_REQUESTS` | false | one line per request: route, method, protocol, status, bytes, time |
| `DIAL_TIMEOUT` | 10s | backend connect timeout |
| `READ_HEADER_TIMEOUT` | 20s | slow-client header timeout |
| `IDLE_TIMEOUT` | 120s | idle keep-alive connections from Cloud Run |
| `BACKEND_IDLE_TIMEOUT` | 90s | idle pooled backend connections |
| `TCP_KEEPALIVE` | 30s | TCP keepalive toward backends |
| `WS_PING_INTERVAL` | 25s | WebSocket ping on tunnels; keeps mobile/NAT paths alive (0 = off) |
| `MAX_CONCURRENT_STREAMS` | 1000 | HTTP/2 streams per connection |

Durations accept `30`, `30s` or `2m`. Run `mk-cloud-relay check-config` to
validate a configuration and print the route table.

## Recipes

### Xray and VPSPanel backends

On the server, create an inbound **without TLS** (Cloud Run already provides
TLS), on a public port, and route its path through the relay:

| Xray inbound (server) | Route | Client link changes |
|---|---|---|
| VLESS/VMess/Trojan + gRPC, serviceName `abc` | `/abc/=http://VPS:PORT` | `security=tls`, `alpn=h2` |
| VLESS + XHTTP, path `/xh` | `/xh=http://VPS:PORT` | `security=tls`; `alpn=h2` for stream-one/stream-up |
| VLESS/VMess/Trojan + WebSocket, path `/ws` | `/ws=http://VPS:PORT` | `security=tls`, `alpn=http/1.1` |
| VLESS + HTTPUpgrade, path `/hu` | `/hu=http://VPS:PORT` | `security=tls`, `alpn=http/1.1` |

In every client link use the Cloud Run host as the address and SNI, port `443`,
and `fp=chrome`. For example, for XHTTP:
```
vless://UUID@SERVICE.run.app:443?encryption=none&security=tls&sni=SERVICE.run.app&alpn=h2&fp=chrome&type=xhttp&path=%2Fxh&mode=packet-up#relay
```
VLESS Encryption (`encryption=mlkem768x25519plus…`) works unchanged: the relay
never looks inside the payload.

VPSPanel needs no changes. In its Xray manager pick the protocol and transport,
set **security none**, choose a port, and enter the Cloud Run domain as the
address in links. Then change the printed link as in the table above. See
[docs/VPSPANEL.md](docs/VPSPANEL.md).

Xray client JSON (outbound) for gRPC through the relay:
```json
{ "protocol": "vless",
  "settings": { "vnext": [{ "address": "SERVICE.run.app", "port": 443,
                 "users": [{ "id": "UUID", "encryption": "none" }] }] },
  "streamSettings": { "network": "grpc", "grpcSettings": { "serviceName": "abc" },
                      "security": "tls",
                      "tlsSettings": { "serverName": "SERVICE.run.app", "alpn": ["h2"], "fingerprint": "chrome" } } }
```

### HTTP and HTTPS proxy

Cloud Run rejects `CONNECT`, so a browser or app cannot use the Cloud Run URL
directly as an HTTPS proxy. The supported way is a tunnel to a real proxy:

1. Run an HTTP proxy with authentication on the backend (Squid, 3proxy, or an
   Xray `http` inbound) and add `TUNNEL_TOKENS` plus a route
   `/t/proxy=tunnel://VPS:3128`.
2. On the client device run:
   ```bash
   mk-cloud-relay client -listen 127.0.0.1:8118 -url wss://SERVICE.run.app/t/proxy -token YOUR_TOKEN
   ```
3. Use `127.0.0.1:8118` as the HTTP proxy (with the proxy's own user and
   password). Both `http://` and `https://` (CONNECT) work, because CONNECT now
   travels inside the tunnel.

The same tunnel carries any TCP protocol, for example SSH:
`-url wss://SERVICE.run.app/t/ssh` with `/t/ssh=tunnel://VPS:22`.

The relay can also act as a forward proxy itself (`FORWARD_PROXY_UPSTREAM` +
`FORWARD_PROXY_USERS`) when it runs somewhere that passes proxy requests (a VPS,
Kubernetes, or behind a TCP load balancer). It authenticates the client, then
hands each request to the upstream proxy. It never connects to destinations on
its own, so clients cannot use it to reach internal addresses.

### Plain HTTP backends
```
ROUTES=/api/=https://api.internal.example,host=backend; /=http://203.0.113.10:8080
```

## Operations

- **Logs:** Cloud Run → service → Logs. Set `LOG_REQUESTS=true` for per-request
  lines and `LOG_LEVEL=debug` to see tunnel open and close events. Backend failures
  appear as `backend error` warnings with the route and backend address.
- **Health:** set `HEALTH_PATH=/_mk/health`; Cloud Run's default TCP startup
  probe already works without it.
- **Update:** `git pull && deploy/deploy.sh SERVICE REGION deploy/env.yaml`. With
  continuous deployment, push to the connected branch.
- **Rollback:** Cloud Run keeps every revision. Run
  `gcloud run services update-traffic SERVICE --region REGION --to-revisions REVISION=100`,
  or use Revisions → Manage traffic in the console.
- **Change backends without rebuilding:** edit the variables (Edit & deploy new
  revision). It takes effect in seconds, and the previous revision stays
  available for rollback.

## Security

- The relay contacts only the backends in `ROUTES` and `FORWARD_PROXY_UPSTREAM`.
  It makes no other outbound connections: no update checks, telemetry, metadata
  server or license server.
- Tunnel tokens are compared in constant time and must be 16+ characters. Keep
  them in Secret Manager (`--set-secrets TUNNEL_TOKENS=name:latest`).
- The forward proxy refuses to start without users, and never forwards the
  client's `Proxy-Authorization` header.
- X-Forwarded-For is dropped by default, so the backend does not learn client
  IPs unless you add `xff`.
- The container runs as a non-root user on a distroless image with no shell.
- Cloud Run itself logs every request (URL, client IP, user agent) in Cloud
  Logging. That is Google's platform logging, not the relay's; restrict who can
  read your project's logs.

## Testing

```bash
go test -race ./...            # unit tests: routing, streaming, gRPC trailers, WebSocket, tunnels, auth
tests/e2e/run.sh local         # Docker: real Xray-core traffic through the relay
```
See [docs/TESTING.md](docs/TESTING.md) for the Cloud Run end-to-end procedure,
and [docs/TEST-RESULTS.md](docs/TEST-RESULTS.md) for the latest results.

## License
MIT
