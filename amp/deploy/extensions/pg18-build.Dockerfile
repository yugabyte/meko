# yb-amp extension ABI-anchor base image.
#
# Builds the exact forked PostgreSQL v18 (compute/vendor/postgres-v18) that the
# yb-amp endpoint ships, and keeps the full build toolchain (clang/mold/make +
# the PG headers and pg_config) in the image. Extension images are built FROM
# this so they compile and link against the SAME PostgreSQL — same headers, same
# ABI, same NEON_SMGR hooks (storage/smgr.h #defines it, so the pgvector/rum
# unlogged-build hooks light up automatically). Building an extension against
# stock PG18 would risk a subtly incompatible binary; anchoring here forbids it.
#
# It is a build-time base only (never shipped to a pod): the extension.Dockerfile
# final stage is FROM scratch and copies just the built artifacts out.
#
# Build (from the repo root):
#   docker build -t amp-pg18-build:local -f deploy/extensions/pg18-build.Dockerfile .

ARG REPOSITORY=quay.io/yugabyte
ARG IMAGE=amp-build-tools
ARG TAG=v1

FROM $REPOSITORY/$IMAGE:$TAG
WORKDIR /home/nonroot

# System -dev headers some extensions link against. HexaBridge (amp-ext-hexabridge)
# links libcurl (utl_mail/utl_http), libxml2 (dbms_xmldom), OpenSSL (dbms_crypto)
# and zlib (utl_compress), so its compile needs these headers here — extensions
# build as the unprivileged `nonroot` user and can't install packages themselves.
# Idempotent (a no-op for anything the base already carries) and build-time only:
# this image is never shipped to a pod, so it adds no runtime bloat.
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       libcurl4-openssl-dev libxml2-dev libssl-dev zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
USER nonroot

# Same inputs as the endpoint image's pg-build stage, so the PG we compile
# extensions against is byte-for-byte the PG the endpoint runs.
COPY --chown=nonroot compute/vendor/postgres-v18 compute/vendor/postgres-v18
COPY --chown=nonroot Makefile Makefile
COPY --chown=nonroot postgres.mk postgres.mk
COPY --chown=nonroot scripts/ninstall.sh scripts/ninstall.sh
ENV BUILD_TYPE=release
RUN set -e \
    && mold -run make -j "$(nproc)" -s postgres

# The one path extension builds key off. Absolute so the generic
# extension.Dockerfile needs no knowledge of the layout.
ENV PG_CONFIG=/home/nonroot/pg_install/v18/bin/pg_config
ENV PATH=/home/nonroot/pg_install/v18/bin:$PATH
