# Cross-compiles like api_server's: the builder runs on the BUILD platform and Go emits a binary for the
# TARGET one, so `--platform linux/amd64` from an arm64 laptop compiles natively.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /build/meko-datapack-metrics-consumer .

FROM alpine:3.21

# ca-certificates for TLS to the database; a fixed non-root uid so a pod security policy can pin it.
RUN apk add --no-cache ca-certificates && adduser -S -u 10001 app

COPY --from=builder /build/meko-datapack-metrics-consumer /usr/local/bin/meko-datapack-metrics-consumer

USER 10001

# /healthz and /metrics (MEKO_EVENTS_CONSUMER_HTTP_PORT).
EXPOSE 8080

# docker run --rm -e YUGABYTEDB_SYSTEM_URL="host=... dbname=... user=... password=..." meko-datapack-metrics-consumer
ENTRYPOINT ["/usr/local/bin/meko-datapack-metrics-consumer"]
