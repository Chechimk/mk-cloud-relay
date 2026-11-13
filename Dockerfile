# MK Cloud Relay - production image.
# Static Go binary on a distroless, non-root base (includes CA certificates).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/mk-cloud-relay ./cmd/mk-cloud-relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mk-cloud-relay /mk-cloud-relay
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/mk-cloud-relay"]
