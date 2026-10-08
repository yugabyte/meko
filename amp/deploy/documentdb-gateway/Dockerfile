# syntax=docker/dockerfile:1
#
# documentdb-gateway: the MongoDB wire-protocol front end for the documentdb
# extension (compute/architecture/extensions.md). It terminates the wire
# protocol and translates to SQL against the endpoint's Postgres, so it runs as a
# sidecar in the endpoint pod and talks to the compute over the pod's loopback.
#
# Built from the SAME documentdb release the extension image is built from — the
# gateway and the SQL surface it drives are versioned together upstream, so a
# mismatch is not a supported combination. Keep DOCUMENTDB_VERSION in step with
# the documentdb row in deploy/extensions/extensions.yaml; both fetch the same
# tarball, pinned once in downloads_checksum.txt.
#
# The context is amp/, for that manifest, scripts/safe_curl.sh and the patch
# below, and Dockerfile.dockerignore beside this file admits nothing else:
#
#   docker build -t <repo>/amp-documentdb-gateway:<tag> -f deploy/documentdb-gateway/Dockerfile .
#
# Runtime configuration is environment-first (a JSON config file is optional):
#   DOCUMENTDB_LISTEN_ADDR       host:port or :port for the wire-protocol listener
#   DOCUMENTDB_PG_URL_FILE       file holding the PostgreSQL connection URL
#   DOCUMENTDB_TLS_AUTO_GENERATE true to self-sign on first start
#   DOCUMENTDB_TLS_STATE_DIR     where the generated cert is persisted
#   DOCUMENTDB_TLS_CERT_FILE     PEM certificate chain to serve instead, re-read
#   DOCUMENTDB_TLS_KEY_FILE      within a minute of either file's mtime moving
#   DOCUMENTDB_LOG_LEVEL         tracing filter (info, debug, …)
#
# `documentdb-gateway check` probes the backend and verifies the extension is
# loaded, exiting 0/1 — that is what the chart's probes run.

ARG RUST_VERSION=1.95
ARG DEBIAN_VERSION=bookworm
ARG DEBIAN_FLAVOR=${DEBIAN_VERSION}-slim

FROM rust:${RUST_VERSION}-${DEBIAN_VERSION} AS build
WORKDIR /src

# Pinned to the extension's release. A checksum mismatch fails the build rather
# than shipping an unverified tree; the manifest and its checker are
# bind-mounted, not copied in.
#
# The patch makes the gateway serve every certificate in its cert file, not just
# the leaf, as Postgres does: a leaf issued through an intermediate fails at a
# client that trusts only the root.
ARG DOCUMENTDB_VERSION=0.116-0
RUN --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    --mount=type=bind,source=compute/image/patches/documentdb-gateway-cert-chain.patch,target=/patches/documentdb-gateway-cert-chain.patch \
    set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl pkg-config libssl-dev patch; \
    rm -rf /var/lib/apt/lists/*; \
    sh /safe_curl/scripts/safe_curl.sh fetch \
      "https://github.com/documentdb/documentdb/archive/refs/tags/v${DOCUMENTDB_VERSION}.tar.gz" src.tgz; \
    mkdir -p documentdb; \
    tar -xzf src.tgz --strip-components=1 -C documentdb; \
    rm src.tgz; \
    patch --batch --fuzz=0 -p1 -d documentdb < /patches/documentdb-gateway-cert-chain.patch

# Only the gateway workspace is built; the extension half of the tree is built
# separately into its ImageVolume (deploy/extensions).
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/documentdb/pg_documentdb_gw/target \
    set -eux; \
    cd documentdb/pg_documentdb_gw; \
    cargo build --release --locked -p documentdb_gateway; \
    install -m 0755 target/release/documentdb_gateway /usr/local/bin/documentdb-gateway

# --- runtime ---
FROM debian:${DEBIAN_FLAVOR} AS runtime
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates libssl3; \
    rm -rf /var/lib/apt/lists/*; \
    useradd --system --uid 10001 --create-home --home-dir /var/lib/documentdb gateway

COPY --from=build /usr/local/bin/documentdb-gateway /usr/local/bin/documentdb-gateway

# The self-signed cert is written here on first start, so it must be writable by
# the unprivileged user and is a mount point in the pod.
ENV DOCUMENTDB_TLS_STATE_DIR=/var/lib/documentdb/tls \
    DOCUMENTDB_TLS_AUTO_GENERATE=true \
    DOCUMENTDB_LISTEN_ADDR=:10260 \
    DOCUMENTDB_LOG_LEVEL=info
RUN mkdir -p /var/lib/documentdb/tls && chown -R gateway:gateway /var/lib/documentdb

USER gateway
EXPOSE 10260
ENTRYPOINT ["/usr/local/bin/documentdb-gateway"]
CMD ["run"]
