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
