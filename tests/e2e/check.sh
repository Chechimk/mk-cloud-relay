#!/bin/sh
# TEST-ONLY: runs inside the client container after gen-client.sh.
#   check.sh RELAY_BASE_URL     e.g. http://server:8080 or https://x.run.app
# Every case moves real data and checks SHA-256 hashes in both directions.
set -u
BASE=$1
: "${E2E_SECRET:?}"
WS_BASE=$(echo "$BASE" | sed -e 's#^http://#ws://#' -e 's#^https://#wss://#')
PASS=0 FAIL=0
ok()   { PASS=$((PASS+1)); printf 'PASS  %-26s %s\n' "$1" "$2"; }
bad()  { FAIL=$((FAIL+1)); printf 'FAIL  %-26s %s\n' "$1" "$2"; }

head -c 5000000 /dev/urandom > /tmp/up.bin
UP_SHA=$(sha256sum /tmp/up.bin | cut -d' ' -f1)

# transfer NAME CURL_PROXY_ARGS...: 20 MB download + 5 MB upload, hashes checked.
transfer() {
    name=$1; shift
    want=$(curl -fsS --max-time 60 "$@" http://127.0.0.1:8000/blob.sha256 2>/tmp/err) || { bad "$name" "no connection: $(head -c 150 /tmp/err)"; return; }
    t0=$(date +%s)
    got=$(curl -fsS --max-time 240 "$@" http://127.0.0.1:8000/blob 2>/tmp/err | sha256sum | cut -d' ' -f1)
    t1=$(date +%s)
    up=$(curl -fsS --max-time 240 "$@" --data-binary @/tmp/up.bin -H 'Content-Type: application/octet-stream' \
         http://127.0.0.1:8000/cgi-bin/sha 2>>/tmp/err)
    t2=$(date +%s)
    if [ "$got" = "$want" ] && [ "$up" = "$UP_SHA" ]; then
        ok "$name" "down 20MB $((t1-t0))s, up 5MB $((t2-t1))s"
    else
        bad "$name" "download $( [ "$got" = "$want" ] && echo ok || echo WRONG ), upload $( [ "$up" = "$UP_SHA" ] && echo ok || echo WRONG ) $(head -c 150 /tmp/err)"
    fi
}

echo "== Xray transports through the relay"
xray run -c /tmp/client.json >/tmp/xray-client.log 2>&1 &
XPID=$!
sleep 2
while read -r port name _; do
    transfer "$name" --socks5-hostname "127.0.0.1:$port"
done < /tmp/cases

echo "== Concurrency: all transports at once"
pids=''
while read -r port name _; do
    ( want=$(cat /dev/null; curl -fsS --max-time 60 --socks5-hostname "127.0.0.1:$port" http://127.0.0.1:8000/blob.sha256)
      got=$(curl -fsS --max-time 300 --socks5-hostname "127.0.0.1:$port" http://127.0.0.1:8000/blob | sha256sum | cut -d' ' -f1)
      [ -n "$want" ] && [ "$got" = "$want" ] && echo "ok $name" > "/tmp/par.$port" || echo "fail $name" > "/tmp/par.$port" ) &
    pids="$pids $!"
done < /tmp/cases
for p in $pids; do wait "$p"; done
n_ok=$(cat /tmp/par.* | grep -c '^ok')
n_all=$(ls /tmp/par.* | wc -l)
if [ "$n_ok" = "$n_all" ]; then ok "parallel-$n_all-streams" "all 20MB downloads matched"; else bad "parallel-$n_all-streams" "$(grep fail /tmp/par.* | tr '\n' ' ')"; fi
rm -f /tmp/par.*

echo "== TCP tunnel (WebSocket) to an HTTP proxy: HTTP and HTTPS CONNECT"
mk-cloud-relay client -listen 127.0.0.1:8118 -url "$WS_BASE/t/proxy" -token "$E2E_SECRET" >/tmp/tunnel.log 2>&1 &
TPID=$!
sleep 1
transfer tunnel-http-proxy -x "http://e2e:$E2E_SECRET@127.0.0.1:8118"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 -x "http://e2e:$E2E_SECRET@127.0.0.1:8118" https://www.google.com/generate_204)
[ "$code" = 204 ] && ok tunnel-https-connect "CONNECT to www.google.com:443 -> 204" || bad tunnel-https-connect "got $code"
kill $TPID 2>/dev/null

echo "== Security checks"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$BASE/t/proxy")
[ "$code" = 401 ] && ok tunnel-no-token "401" || bad tunnel-no-token "got $code"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -H "Authorization: Bearer wrong-token-00000000" "$BASE/t/proxy")
[ "$code" = 401 ] && ok tunnel-wrong-token "401" || bad tunnel-wrong-token "got $code"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$BASE/no/such/path")
[ "$code" = 404 ] && ok unknown-path "404" || bad unknown-path "got $code"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$BASE/_mk/health")
[ "$code" = 200 ] && ok health "200" || bad health "got $code"

echo "== Direct HTTP forward proxy (absolute-form and CONNECT) to the relay itself"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 -x "$BASE" -U "tester:wrong-pass-123" http://127.0.0.1:8000/blob.sha256)
echo "INFO  forward-proxy-wrong-pass   HTTP $code (407 expected when the platform passes proxy requests)"
got=$(curl -s --max-time 30 -x "$BASE" -U "tester:$E2E_SECRET" http://127.0.0.1:8000/blob.sha256)
want=$(curl -s --max-time 30 --socks5-hostname 127.0.0.1:20010 http://127.0.0.1:8000/blob.sha256)
[ -n "$want" ] && [ "$got" = "$want" ] && echo "INFO  forward-proxy-http         works" || echo "INFO  forward-proxy-http         does not work here: $(echo "$got" | head -c 80)"
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 -x "$BASE" -U "tester:$E2E_SECRET" https://www.google.com/generate_204)
echo "INFO  forward-proxy-connect      result $code (204 = CONNECT works; Cloud Run documents that it rejects CONNECT)"

kill $XPID 2>/dev/null
echo "RESULT $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
