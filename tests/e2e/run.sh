#!/usr/bin/env bash
# End-to-end tests with real Xray-core traffic.
#
#   tests/e2e/run.sh local
#       Builds the test image, starts a server container (relay + Xray +
#       web server) and a client container on a private Docker network.
#
#   E2E_UUID=... E2E_SECRET=... tests/e2e/run.sh cloud https://SERVICE.run.app
#       Runs only the client container locally against a deployed copy of the
#       test image (see docs/TESTING.md). Use the same E2E_UUID/E2E_SECRET
#       that the service was deployed with.
set -euo pipefail
cd "$(dirname "$0")/../.."
IMAGE=mk-cloud-relay-e2e
NET=mkrelay-e2e
mode=${1:-local}

docker build -q -f tests/e2e/Dockerfile -t "$IMAGE" . >/dev/null
cleanup() { docker rm -f mkrelay-server mkrelay-client >/dev/null 2>&1 || true; docker network rm "$NET" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

if [ "$mode" = local ]; then
    E2E_UUID=${E2E_UUID:-$(cat /proc/sys/kernel/random/uuid 2>/dev/null || python -c 'import uuid;print(uuid.uuid4())')}
    E2E_SECRET=${E2E_SECRET:-local-$(date +%s)-secret-value}
    export E2E_UUID E2E_SECRET
    docker network create "$NET" >/dev/null
    docker run -d --name mkrelay-server --network "$NET" -e E2E_UUID -e E2E_SECRET "$IMAGE" >/dev/null
    docker run -d --name mkrelay-client --network "$NET" -e E2E_UUID -e E2E_SECRET -e E2E_ROLE=client "$IMAGE" >/dev/null
    sleep 2
    docker exec mkrelay-client gen-client.sh mkrelay-server 8080 0
    status=0
    docker exec mkrelay-client check.sh http://mkrelay-server:8080 || status=1

    echo "== Backend failure: stop Xray, expect clean errors, then recover"
    docker exec mkrelay-server pkill xray || true
    sleep 1
    code=$(docker exec mkrelay-client curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/grpc' --http2-prior-knowledge http://mkrelay-server:8080/vg/Tun || true)
    grpc=$(docker exec mkrelay-client curl -s -D - -o /dev/null -X POST -H 'Content-Type: application/grpc' --http2-prior-knowledge http://mkrelay-server:8080/vg/Tun | tr -d '\r' | sed -n 's/^grpc-status: //p' || true)
    ws=$(docker exec mkrelay-client curl -s -o /dev/null -w '%{http_code}' -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' http://mkrelay-server:8080/vw || true)
    echo "gRPC with backend down: HTTP $code grpc-status $grpc (want 200/14); WebSocket: HTTP $ws (want 502)"
    [ "$grpc" = 14 ] && [ "$ws" = 502 ] || status=1
    docker exec -d mkrelay-server sh -c 'xray run -c /tmp/server.json >/tmp/xray.log 2>&1'
    sleep 2
    want=$(docker exec mkrelay-client sh -c 'xray run -c /tmp/client.json >/dev/null 2>&1 & sleep 2; curl -fsS --max-time 60 --socks5-hostname 127.0.0.1:20001 http://127.0.0.1:8000/blob.sha256' || true)
    [ -n "$want" ] && echo "recovered after backend restart" || { echo "did not recover"; status=1; }
    exit $status
fi

if [ "$mode" = cloud ]; then
    url=${2:?usage: run.sh cloud https://SERVICE.run.app}
    : "${E2E_UUID:?}" "${E2E_SECRET:?}"
    host=${url#https://}; host=${host%%/*}
    docker run -d --name mkrelay-client -e E2E_UUID -e E2E_SECRET -e E2E_ROLE=client "$IMAGE" >/dev/null
    sleep 1
    docker exec mkrelay-client gen-client.sh "$host" 443 1
    docker exec mkrelay-client check.sh "https://$host"
    exit $?
fi
echo "usage: $0 local | cloud URL" >&2
exit 2
