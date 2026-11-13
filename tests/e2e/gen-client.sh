#!/bin/sh
# TEST-ONLY: writes /tmp/client.json, an Xray client with one SOCKS port per
# transport case, all pointing at the relay.
#   gen-client.sh ADDRESS PORT TLS(0|1)
# E2E_UUID and E2E_SECRET must match the server.
set -eu
ADDR=$1 PORT=$2 TLS=$3
: "${E2E_UUID:?}" "${E2E_SECRET:?}"

sec() { # $1 = alpn
    if [ "$TLS" = 1 ]; then
        printf '"security":"tls","tlsSettings":{"serverName":"%s","alpn":["%s"],"fingerprint":"chrome"}' "$ADDR" "$1"
    else
        printf '"security":"none"'
    fi
}
user() { # $1 = protocol
    case $1 in
        vless)  printf '"vnext":[{"address":"%s","port":%s,"users":[{"id":"%s","encryption":"none"}]}]' "$ADDR" "$PORT" "$E2E_UUID" ;;
        vmess)  printf '"vnext":[{"address":"%s","port":%s,"users":[{"id":"%s","security":"auto"}]}]' "$ADDR" "$PORT" "$E2E_UUID" ;;
        trojan) printf '"servers":[{"address":"%s","port":%s,"password":"%s"}]' "$ADDR" "$PORT" "$E2E_SECRET" ;;
    esac
}
# port name protocol network-json alpn
CASES='20001 vless-grpc-gun          vless  "network":"grpc","grpcSettings":{"serviceName":"vg"} h2
20002 vless-grpc-multi        vless  "network":"grpc","grpcSettings":{"serviceName":"vg","multiMode":true} h2
20003 vmess-grpc              vmess  "network":"grpc","grpcSettings":{"serviceName":"mg"} h2
20004 trojan-grpc             trojan "network":"grpc","grpcSettings":{"serviceName":"tg"} h2
20005 vless-xhttp-stream-one  vless  "network":"xhttp","xhttpSettings":{"path":"/vx","mode":"stream-one"} h2
20006 vless-xhttp-stream-up   vless  "network":"xhttp","xhttpSettings":{"path":"/vx","mode":"stream-up"} h2
20007 vless-xhttp-packet-h2   vless  "network":"xhttp","xhttpSettings":{"path":"/vx","mode":"packet-up"} h2
20008 vless-xhttp-packet-h1   vless  "network":"xhttp","xhttpSettings":{"path":"/vx","mode":"packet-up"} http/1.1
20009 vless-xhttp-one-h1      vless  "network":"xhttp","xhttpSettings":{"path":"/vx","mode":"stream-one"} http/1.1
20010 vless-ws                vless  "network":"ws","wsSettings":{"path":"/vw"} http/1.1
20011 vmess-ws                vmess  "network":"ws","wsSettings":{"path":"/mw"} http/1.1
20012 trojan-ws               trojan "network":"ws","wsSettings":{"path":"/tw"} http/1.1
20013 vless-httpupgrade       vless  "network":"httpupgrade","httpupgradeSettings":{"path":"/vh"} http/1.1'

ins='' outs='' rules=''
echo "$CASES" > /tmp/cases
while read -r port name proto net alpn; do
    ins="$ins{\"tag\":\"in-$port\",\"listen\":\"127.0.0.1\",\"port\":$port,\"protocol\":\"socks\",\"settings\":{\"udp\":false}},"
    outs="$outs{\"tag\":\"out-$port\",\"protocol\":\"$proto\",\"settings\":{$(user "$proto")},\"streamSettings\":{$net,$(sec "$alpn")}},"
    rules="$rules{\"type\":\"field\",\"inboundTag\":[\"in-$port\"],\"outboundTag\":\"out-$port\"},"
done < /tmp/cases
cat > /tmp/client.json <<EOF
{"log":{"loglevel":"warning"},
 "inbounds":[${ins%,}],
 "outbounds":[${outs%,}],
 "routing":{"rules":[${rules%,}]}}
EOF
xray run -test -c /tmp/client.json >/dev/null
