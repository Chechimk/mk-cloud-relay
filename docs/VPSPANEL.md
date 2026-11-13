# Using MK Cloud Relay with VPSPanel

VPSPanel needs **no changes**. The relay talks to the panel's Xray (and SSH
WebSocket) services over ordinary HTTP.

## Xray inbounds
In VPSPanel → Xray manager → Add inbound:

| Wizard step | Choose |
|---|---|
| Protocol | VLESS (or VMess/Trojan) |
| Transport | XHTTP, gRPC, WebSocket or HTTPUpgrade |
| Security | **none** (Cloud Run provides TLS) |
| Port | a free TCP port, open in your VPS/cloud firewall |
| Server address in links | your Cloud Run domain |

Then add a route with the inbound's path (or `/<serviceName>/` for gRPC):
```
ROUTES=/grpcsvc/=http://VPS_IP:80; /xh=http://VPS_IP:8080
```
Several inbounds on different ports can share one relay through different paths.

## Fixing the links VPSPanel prints
VPSPanel prints links for a direct connection. Change them like this:

| Printed | Change to |
|---|---|
| `:PORT?` | `:443?` |
| `security=none` | `security=tls&sni=SERVICE.run.app&fp=chrome&alpn=…` |

Use `alpn=h2` for gRPC and XHTTP stream-one/stream-up, and `alpn=http/1.1` for
WebSocket and HTTPUpgrade. Keep everything else, including `encryption=`.

A link setting in VPSPanel for a CDN or front domain would make this automatic.
That is a separate VPSPanel feature; it is not part of this repository.

## SSH over WebSocket
VPSPanel's WebSocket proxy for SSH works through a route such as
`/ssh=http://VPS_IP:80` (HTTP Injector-style payloads use the Cloud Run host).
For SSH without a WebSocket proxy, use a tunnel:
`/t/ssh=tunnel://VPS_IP:22` and `mk-cloud-relay client -listen 127.0.0.1:2222 …`.
