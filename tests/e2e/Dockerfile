# TEST-ONLY image: MK Cloud Relay in front of a real Xray-core server and a
# tiny web server, all inside one container. Used for local Docker tests and
# for a disposable Cloud Run test service. Never use it in production.
#
# Build from the repository root:
#   docker build -f tests/e2e/Dockerfile -t mk-cloud-relay-e2e .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=e2e" -o /out/mk-cloud-relay ./cmd/mk-cloud-relay

FROM alpine:3.22
ARG XRAY_VERSION=v26.10.21
RUN apk add --no-cache curl ca-certificates unzip busybox-extras \
 && curl -fsSLo /tmp/x.zip https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}/Xray-linux-64.zip \
 && curl -fsSLo /tmp/x.dgst https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}/Xray-linux-64.zip.dgst \
 && test "$(sha256sum /tmp/x.zip | cut -d' ' -f1)" = "$(sed -n 's/^SHA2-256= *//p' /tmp/x.dgst | head -1)" \
 && unzip -o /tmp/x.zip xray -d /usr/local/bin && rm /tmp/x.* \
 && mkdir -p /www/cgi-bin \
 && head -c 20000000 /dev/urandom > /www/blob \
 && sha256sum /www/blob | cut -d' ' -f1 > /www/blob.sha256
COPY --from=build /out/mk-cloud-relay /usr/local/bin/mk-cloud-relay
COPY tests/e2e/server.json /etc/xray/server.json
COPY tests/e2e/sha.cgi /www/cgi-bin/sha
COPY tests/e2e/start.sh /start.sh
COPY tests/e2e/gen-client.sh tests/e2e/check.sh /usr/local/bin/
RUN chmod +x /start.sh /www/cgi-bin/sha /usr/local/bin/gen-client.sh /usr/local/bin/check.sh
ENV ROUTES="/vg/=http://127.0.0.1:10001;/mg/=h2c://127.0.0.1:10002;/vx=http://127.0.0.1:10003;/vw=http://127.0.0.1:10004;/mw=http://127.0.0.1:10005;/tw=http://127.0.0.1:10006;/vh=http://127.0.0.1:10007;/tg/=http://127.0.0.1:10008;/t/proxy=tunnel://127.0.0.1:3128" \
    HEALTH_PATH="/_mk/health" \
    LOG_REQUESTS="true"
ENTRYPOINT ["/start.sh"]
