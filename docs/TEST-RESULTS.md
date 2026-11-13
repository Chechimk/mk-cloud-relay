# Test results

Every result here moved real data and checked SHA-256 hashes in both
directions: a 20 MB download and a 5 MB upload per case. A "connected" status
alone never counts as a pass.

Tested on 2026-11-13 with Xray-core v26.10.21.

## 1. Unit tests: `go test -race ./...` (Go 1.25)

All 21 tests pass under the race detector:

| Test | What it proves |
|---|---|
| ReverseProxyPreservesRequest | exact path (including `%2F`), query and Host are kept; `strip`, `host=`, `xff` work; X-Forwarded-For is dropped by default |
| AutoRouteProtocolSelection | `http://` routes send gRPC as h2c and everything else as HTTP/1.1 |
| GRPCBidirectionalStreamingAndTrailers | 20 messages, each reply received before the next message is sent (no buffering); `grpc-status` trailers delivered; `http://` and `h2c://` routes |
| StreamingResponseIsNotBuffered | the first chunk reaches the client while the backend is still sending |
| HTTP1FullDuplex | the response is read while the request body is still uploading (XHTTP stream-one over HTTP/1.1) |
| WebSocketReverseProxy | 2 MB binary messages echo intact; clean close |
| HTTPUpgradeRawStream | 101 response then raw bytes (Xray HTTPUpgrade) |
| UpgradeEndDeliversEverythingToSlowClient | the backend finishes while the client is still uploading and has not read anything yet: all 8 MB still arrive, followed by a clean end |
| BackendResetAfterDataEndsCleanly | a backend that resets its stream right after the last byte still gives the client every byte and a clean end (HTTP/1.1 and h2) |
| ResponseEndsWhileClientStillSending | the backend finishes while the client's HTTP/2 request is still open: the response ends within `STREAM_LINGER` |
| Tunnel | 16 parallel 1 MB echoes through `mk-cloud-relay client`; 401 without a token; 400 without upgrade; a wrong token never opens a tunnel |
| TunnelBackendClosesFirst | the backend sends 8 MB and closes at once: the tunnel client receives all of it and a clean end |
| WSFrameTracker | WebSocket close frames are recognised at every chunk size; raw HTTPUpgrade bytes are not mistaken for frames |
| ForwardProxy | HTTPS through CONNECT and plain HTTP through an authenticated upstream; 407 for a wrong password; client credentials are not passed on |
| ConnectRefusedWithoutForwardProxy | CONNECT gets 405 when no forward proxy is configured |
| BackendDownAndUnknownPaths | 502 for HTTP, trailers-only `grpc-status 14` for gRPC, 404 for unknown paths, health path |
| ConcurrentStreams | 200 parallel 256 KB responses over HTTP/1.1 and h2c |
| config tests (4) | route syntax, rejected unsafe settings (open tunnel, open forward proxy, short tokens/passwords, duplicates) |

## 2. Local Docker end to end: `tests/e2e/run.sh local`

Setup: Xray server (no TLS) → relay → Xray client, on a Docker network. The
server container also runs a web server for hashed downloads and an upload
checker.

```
PASS  vless-grpc-gun             down 20MB 3s, up 5MB 32s *
PASS  vless-grpc-multi           down 20MB 1s, up 5MB 32s *
PASS  vmess-grpc                 down 20MB 0s, up 5MB 2s
PASS  trojan-grpc                down 20MB 0s, up 5MB 33s *
PASS  vless-xhttp-stream-one     down 20MB 1s, up 5MB 240s **
PASS  vless-xhttp-stream-up      down 20MB 0s, up 5MB 3s
PASS  vless-xhttp-packet-h2      down 20MB 1s, up 5MB 3s
PASS  vless-xhttp-packet-h1      down 20MB 1s, up 5MB 3s
PASS  vless-xhttp-one-h1         down 20MB 0s, up 5MB 240s **
PASS  vless-ws                   down 20MB 1s, up 5MB 1s
PASS  vmess-ws                   down 20MB 0s, up 5MB 3s
PASS  trojan-ws                  down 20MB 0s, up 5MB 3s
PASS  vless-httpupgrade          down 20MB 0s, up 5MB 34s *
PASS  parallel-13-streams        all 20MB downloads matched
PASS  tunnel-http-proxy          down 20MB 0s, up 5MB 1s
PASS  tunnel-https-connect       CONNECT to www.google.com:443 -> 204
PASS  tunnel-no-token            401
PASS  tunnel-wrong-token         401
PASS  unknown-path               404
PASS  health                     200
INFO  forward-proxy-wrong-pass   HTTP 407
INFO  forward-proxy-http         works
INFO  forward-proxy-connect      result 204
RESULT 20 passed, 0 failed
Backend failure: gRPC -> grpc-status 14, WebSocket -> 502; recovered after backend restart
```

The upload timings measure until the connection *closes*. The upload checker
replies without a length and ends its reply by closing, and Xray's client
only closes after the server side has ended. So:

\* The connection closes after `STREAM_LINGER` (30 s): the relay waits that
long for the client before ending the stream, see the Cloud Run section. The
upload itself and the reply arrive in 2-3 s.

\*\* XHTTP stream-one: the same stall happens with the relay removed (Xray
client connected straight to the Xray server, 4 of 4 runs), so it is Xray
behaviour with this endpoint. Hashes match.

## 3. Google Cloud Run

A disposable service `mk-relay-e2e` in europe-west1, built from
`Dockerfile.e2e` (relay and Xray in one container), 1 instance, timeout
3600 s, concurrency 1000, HTTP/2 end-to-end off. The client ran in Docker on
a PC, connecting over TLS to `*.run.app:443`.

Final run (commit 51f0e99):

```
PASS  vless-grpc-gun             down 20MB 4s, up 5MB 34s
PASS  vless-grpc-multi           down 20MB 4s, up 5MB 33s
PASS  vmess-grpc                 down 20MB 5s, up 5MB 4s
PASS  trojan-grpc                down 20MB 41s, up 5MB 34s
PASS  vless-xhttp-stream-one     down 20MB 4s, up 5MB 36s
PASS  vless-xhttp-stream-up      down 20MB 4s, up 5MB 6s
PASS  vless-xhttp-packet-h2      down 20MB 4s, up 5MB 5s
PASS  vless-ws                   down 20MB 7s, up 5MB 3s
PASS  vmess-ws                   down 20MB 4s, up 5MB 4s
PASS  trojan-ws                  down 20MB 5s, up 5MB 4s
PASS  vless-httpupgrade          down 20MB 4s, up 5MB 36s
PASS  parallel-11-streams        all 20MB downloads matched
PASS  tunnel-http-proxy          down 20MB 5s, up 5MB 12s
PASS  tunnel-https-connect       CONNECT to www.google.com:443 -> 204
PASS  tunnel-no-token            401
PASS  tunnel-wrong-token         401
PASS  unknown-path               404
PASS  health                     200
INFO  forward-proxy-wrong-pass   HTTP 404
INFO  forward-proxy-http         does not work here (Google's own 404 page)
INFO  forward-proxy-connect      result 000
RESULT 18 passed, 0 failed
```

Long-lived connections: two rate-limited 20 MB downloads (95 KB/s) ran at the
same time over VLESS+WebSocket and VLESS+gRPC. Each kept one connection open
for 205 s, and both hashes matched.

### What the first Cloud Run runs showed, and the fixes
The first runs failed 10 of 20, with downloads cut at 13-17 MB. The relay's
own debug log (byte counts and how each stream ended) showed it had sent all
20,009,973 bytes with a clean end. A minimal VLESS-over-WebSocket client
without Xray then received only 12.9-14.6 MB followed by an abrupt EOF. So
**Cloud Run's front end drops data it has buffered but not yet delivered when
the container ends a connection or stream first.**

The fixes, in order:
1. WebSocket/HTTPUpgrade are proxied by the relay itself, ending with a
   half-close rather than an abrupt close.
2. The relay keeps the client side open until the client closes. With that,
   the minimal client received all 20,000,197 bytes in 3 of 3 runs.
3. HTTP/2 streams: the handler waits for the client to finish its request
   instead of returning, because Go then resets the stream. The wait no
   longer blocks the transport's request-body reader.
4. Before closing, wait until the front end has acknowledged every byte
   (TIOCOUTQ), so no late packet can reset a socket with queued data.
5. WebSocket close frames are detected (headers only), and the relay waits
   for the client's close up to 5 minutes. Tunnels send an end marker and
   wait for the client. This fixed the parallel case, where the front end
   holds several MB per stream for longer.

### Not working on Cloud Run
- **XHTTP over HTTP/1.1**: the Xray client sends `alpn=http/1.1`, but its
  XHTTP code still negotiates HTTP/2 with Cloud Run and fails
  (`malformed HTTP response … http2_handshake_failed`). Removing the TLS
  fingerprint did not help. XHTTP over h2 works in all three modes. The same
  HTTP/1.1 cases pass locally without TLS.
- **Forward proxy sent straight to the relay**: `CONNECT` and absolute-form
  requests (`GET http://127.0.0.1:8000/...`) get Google's own HTML 404. The
  front end routes by host and never hands them to the container. Use a
  `tunnel://` route.

### Not verified
- HTTP/2 end-to-end mode on Cloud Run (Google documents that WebSockets do
  not work in it).
- Real phone apps (v2rayNG, NekoBox, …), and networks in Eritrea.
- A backend on a separate VPS through Cloud Run. In the test, Xray ran inside
  the same container; the backend leg was tested locally over Docker
  networking.
- More than one instance with XHTTP: harmless with a shared backend, but
  with the backend inside each test instance the instances had separate
  sessions, so the test service was limited to one instance.
