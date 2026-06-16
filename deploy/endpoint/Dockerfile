# yb-light single-container endpoint image.
#
# Ships, in one image, every process that makes up a yb-light endpoint:
#   - yb-pageserver        (page service, backed by YugabyteDB)
#   - safekeeper           (WAL, backed by YugabyteDB)
#   - endpoint_storage     (LFC scratch blobs, backed by YugabyteDB)
#   - compute_ctl + postgres v17 + the `neon` extension (the compute)
# plus a small Python supervisor (PID 1 under tini) that renders each
# service's config from environment variables, brings the stack up in
# order, and monitors/restarts the children.
#
# The storage_controller is NOT in this image: it is a shared,
# cluster-level control plane that lives in the AMP layer. The endpoint
# assumes the `yb_storage` schema already exists (AMP/SC provisions it
# via diesel migrations). For standalone testing, the supervisor can
# provision the schema itself (DEV_PROVISION_SCHEMA=true) using the
# bundled yb_light_local + storage_controller binaries.
#
# Build (from the repo root):
#   docker build -t yb-light/neon-endpoint:dev -f deploy/endpoint/Dockerfile .
#
# yb-light is Postgres v17 only (vendor/postgres-v17, POSTGRES_VERSIONS=v17).

ARG REPOSITORY=ghcr.io/neondatabase
ARG IMAGE=build-tools
ARG TAG=pinned
ARG DEBIAN_VERSION=bookworm
ARG DEBIAN_FLAVOR=${DEBIAN_VERSION}-slim

# 1. Build patched Postgres v17 -> pg_install/v17.
FROM $REPOSITORY/$IMAGE:$TAG AS pg-build
WORKDIR /home/nonroot
COPY --chown=nonroot vendor/postgres-v17 vendor/postgres-v17
COPY --chown=nonroot Makefile Makefile
COPY --chown=nonroot postgres.mk postgres.mk
COPY --chown=nonroot scripts/ninstall.sh scripts/ninstall.sh
ENV BUILD_TYPE=release
RUN set -e \
    && mold -run make -j "$(nproc)" -s postgres

# 2. Build the Rust binaries + the `neon` Postgres extension.
FROM $REPOSITORY/$IMAGE:$TAG AS build
WORKDIR /home/nonroot
ARG GIT_VERSION=local
ARG BUILD_TAG

# Reuse the Postgres build artifacts from the pg-build stage.
COPY --chown=nonroot --from=pg-build /home/nonroot/pg_install/ pg_install
COPY --chown=nonroot . .

# `io-align-512` is the default pageserver feature, so a plain release
# build is correct. We only need the endpoint's own services plus the
# control-plane tools used for (optional) schema provisioning.
#
# The cargo target dir + registry are BuildKit cache mounts (uid/gid match
# the build image's `nonroot` user), so unrelated context changes (e.g.
# editing supervisor.py, which busts the `COPY . .` layer) trigger only an
# incremental recompile rather than a full one. Cache mounts are not part of
# the image layer, so the built binaries are copied out to /home/nonroot/out
# (a real dir) before the mount is unmounted; the final stage copies from there.
RUN --mount=type=cache,target=/home/nonroot/target,uid=1000,gid=1000 \
    --mount=type=cache,target=/home/nonroot/.cargo/registry,uid=1000,gid=1000 \
    set -e \
    && RUSTFLAGS="-Clinker=clang -Clink-arg=-fuse-ld=mold -Clink-arg=-Wl,--no-rosegment -Cforce-frame-pointers=yes" \
       cargo build --locked --release \
         --bin yb-pageserver \
         --bin safekeeper \
         --bin endpoint_storage \
         --bin compute_ctl \
         --bin storage_controller \
         --bin yb_light_local \
    && mkdir -p /home/nonroot/out \
    && cp target/release/yb-pageserver \
          target/release/safekeeper \
          target/release/endpoint_storage \
          target/release/compute_ctl \
          target/release/storage_controller \
          target/release/yb_light_local \
          /home/nonroot/out/ \
    && mold -run make -j "$(nproc)" -s neon-pg-ext

# 3. Assemble the runtime image.
FROM debian:${DEBIAN_FLAVOR} AS endpoint
WORKDIR /data

RUN set -e \
    && echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-retries \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        openssl \
        libcurl4 \
        libreadline8 \
        libseccomp2 \
        libicu72 \
        zlib1g \
        liblz4-1 \
        libzstd1 \
        libossp-uuid16 \
        tini \
        python3 \
        netcat-openbsd \
        procps \
    && rm -f /etc/apt/apt.conf.d/80-retries \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* \
    && useradd -d /data -m yugabyte \
    && chown -R yugabyte:yugabyte /data

# Storage + control-plane binaries (copied out of the cache-mounted target dir
# into /home/nonroot/out by the build stage).
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/yb-pageserver      /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/safekeeper          /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/endpoint_storage    /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/compute_ctl         /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/storage_controller  /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/yb_light_local      /usr/local/bin/

# Postgres v17 install (binaries + the `neon` extension installed by neon-pg-ext).
COPY --from=build /home/nonroot/pg_install/v17 /usr/local/v17/

# Schema migrations, for the optional DEV_PROVISION_SCHEMA path.
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/storage_controller/migrations /usr/local/share/yb-light/migrations

# The supervisor.
COPY --chown=yugabyte:yugabyte deploy/endpoint/supervisor.py /usr/local/bin/yb-endpoint-supervisor
RUN chmod +x /usr/local/bin/yb-endpoint-supervisor

ENV PG_DISTRIB_DIR=/usr/local \
    YB_LIGHT_MIGRATIONS_DIR=/usr/local/share/yb-light/migrations \
    LD_LIBRARY_PATH=/usr/local/v17/lib \
    PATH=/usr/local/bin:/usr/local/v17/bin:$PATH

USER yugabyte

# Compute Postgres port (overridable via COMPUTE_PORT / chart values).
EXPOSE 55433
# pageserver / safekeeper / endpoint_storage HTTP.
EXPOSE 9898 7676 9993

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/yb-endpoint-supervisor"]
