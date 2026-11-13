#!/bin/sh
# TEST-ONLY entrypoint: web server + Xray + relay in one container.
# E2E_UUID and E2E_SECRET are required so a deployed test service never uses
# credentials that are published in this repository.
set -e
: "${E2E_UUID:?set E2E_UUID}" "${E2E_SECRET:?set E2E_SECRET (16+ characters)}"
httpd -p 127.0.0.1:8000 -h /www
if [ "${E2E_ROLE:-server}" != server ]; then
    exec sleep infinity
fi
sed -e "s/__UUID__/$E2E_UUID/g" -e "s/__SECRET__/$E2E_SECRET/g" /etc/xray/server.json > /tmp/server.json
xray run -c /tmp/server.json >/tmp/xray.log 2>&1 &
export TUNNEL_TOKENS="$E2E_SECRET"
export FORWARD_PROXY_UPSTREAM="http://e2e:$E2E_SECRET@127.0.0.1:3128"
export FORWARD_PROXY_USERS="tester:$E2E_SECRET"
exec mk-cloud-relay
