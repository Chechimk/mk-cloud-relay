# Replacing thomascloud

The `thomascloud` repository (a fork of "CloudRun-CDN") deploys a single
pre-compiled file, `entrypoint` (about 9.5 MB, a Go program with no source),
on Alpine. These observations come from its README, its Cloud Run logs, and
the readable text inside the binary. The binary was never run.

| Finding | Evidence | Effect |
|---|---|---|
| A plain Go `httputil.ReverseProxy` to one backend | symbols `NewSingleHostReverseProxy`, log `Registrado backend sv1 -> http://IP:80` | HTTP/1.1 only |
| No HTTP/2 cleartext (h2c) server | Cloud Run returned `upstream connect error … reset reason: protocol error` for every request with HTTP/2 end-to-end on | gRPC cannot work: Cloud Run sends gRPC to the container as h2c in both modes |
| HTTP/1.1 to the backend | default `ReverseProxy` transport | even with HTTP/2 end-to-end off, gRPC backends cannot be reached |
| License key in `KEY_TARGETS` | strings `main.lic`, `Error al leer KEY_TARGETS`, `decrypt`; README: "the bot generates a new key" | needs a key from a third party to redeploy |
| Reports to Telegram | strings `api.telegram.org`, `sendMessage`, `chat_id`, `main.botNotificacion` | someone else's bot is notified |
| Reads the Google Cloud project ID | `metadata.google.internal/computeMetadata/v1/project/project-id` | the project ID is likely in that report |

MK Cloud Relay replaces it with readable source code, no key or reporting,
h2c and HTTP/1.1 on the same port (gRPC and WebSocket in one service),
per-path routing to several backends, and authenticated TCP tunnels.

## Migrating
`PROXY_TARGETS=203.0.113.10:80` keeps working as a single catch-all backend.
Remove `KEY_TARGETS`. Then switch the service's source to this repository, or
deploy a new service and move clients over after testing.
