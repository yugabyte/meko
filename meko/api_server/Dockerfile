# Cross-compiles: the builder runs on the BUILD platform and Go emits a binary for
# the TARGET one, so `--platform linux/amd64` from an arm64 laptop compiles natively
# rather than running the whole toolchain under emulation. This image is hand-built
# (nothing in CI publishes it), and an amd64 build is what a GKE/EKS cluster needs.
# Where the two platforms match — any amd64 runner — this renders as it did before.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -o /build/latest/meko-api-server .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY --from=builder /build/latest/meko-api-server /usr/local/bin/meko-api-server

EXPOSE 25433

ENTRYPOINT ["/usr/local/bin/meko-api-server"]
