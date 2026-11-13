#!/usr/bin/env bash
# Deploy MK Cloud Relay to Google Cloud Run from this source tree.
#
#   deploy/deploy.sh SERVICE REGION ENV_FILE [extra gcloud flags...]
#
# Example (new test service first, never your production service):
#   deploy/deploy.sh mk-relay-test europe-west1 deploy/env.yaml
#
# ENV_FILE is a YAML map of environment variables (see deploy/env.example.yaml).
# Keep secrets (TUNNEL_TOKENS, FORWARD_PROXY_USERS) in Secret Manager and pass
# them with --set-secrets, for example:
#   deploy/deploy.sh mk-relay-test europe-west1 deploy/env.yaml \
#       --set-secrets TUNNEL_TOKENS=mk-relay-tunnel-tokens:latest
set -euo pipefail
SERVICE=${1:?service name}
REGION=${2:?region, e.g. europe-west1}
ENV_FILE=${3:?env file, e.g. deploy/env.yaml}
shift 3
cd "$(dirname "$0")/.."

# Why these flags:
#  --timeout 3600        longest allowed request; tunnels/WebSockets/streams
#                        are requests and end at this limit (clients reconnect)
#  --concurrency 1000    one instance can carry many long-lived streams
#  --no-use-http2        keep HTTP/2 end-to-end OFF: WebSocket, HTTPUpgrade
#                        and tunnels need HTTP/1.1, and Cloud Run still sends
#                        native gRPC to the container over HTTP/2 (h2c)
#  --cpu-boost           faster cold starts
gcloud run deploy "$SERVICE" \
    --source . \
    --region "$REGION" \
    --port 8080 \
    --timeout 3600 \
    --concurrency 1000 \
    --cpu 1 --memory 256Mi \
    --min-instances 0 --max-instances 5 \
    --execution-environment gen2 \
    --cpu-boost \
    --no-use-http2 \
    --allow-unauthenticated \
    --env-vars-file "$ENV_FILE" \
    --labels app=mk-cloud-relay \
    "$@"

gcloud run services describe "$SERVICE" --region "$REGION" --format 'value(status.url)'
