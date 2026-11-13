# Testing

## Unit tests
```bash
go test -race ./...
```
No Go installed? Use Docker:
```bash
docker run --rm -v "$PWD":/src -w /src golang:1.25 go test -race ./...
```

## Local end to end (Docker, real Xray traffic)
```bash
tests/e2e/run.sh local
```
This builds `tests/e2e/Dockerfile`, a **test-only** image containing the relay,
Xray-core (pinned release, verified by its published SHA-256), and a small web
server. It then runs 13 Xray transport cases, the tunnel, the forward proxy,
security checks, a parallel run and a backend-failure check. Each Xray case
downloads 20 MB and uploads 5 MB and compares hashes.

## Cloud Run end to end
The same test image runs as a disposable Cloud Run service. The client side
runs locally in Docker.

1. Pick test credentials (never reuse real ones):
   ```bash
   export E2E_UUID=$(uuidgen) E2E_SECRET=$(openssl rand -hex 16)
   ```
2. Deploy the test image to a **new** service (Cloud Shell or any machine with gcloud):
   ```bash
   cp tests/e2e/Dockerfile Dockerfile      # local copy only; do not commit
   gcloud run deploy mk-relay-e2e --source . --region europe-west1 \
       --port 8080 --timeout 3600 --concurrency 1000 --no-use-http2 \
       --allow-unauthenticated --set-env-vars E2E_UUID=$E2E_UUID,E2E_SECRET=$E2E_SECRET
   git checkout Dockerfile
   ```
3. Run the checks:
   ```bash
   tests/e2e/run.sh cloud https://mk-relay-e2e-XXXX.europe-west1.run.app
   ```
4. Optionally deploy a second copy with `--use-http2` to compare the two modes.
5. Delete the test services:
   `gcloud run services delete mk-relay-e2e --region europe-west1`.

The test service is a working proxy for anyone who knows `E2E_UUID` or
`E2E_SECRET`, so use fresh random values and delete it after testing.
