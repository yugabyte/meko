# yb-amp single-container endpoint image.
#
# Ships, in one image, every process that makes up a yb-amp endpoint:
#   - pageserver        (page service, backed by YugabyteDB)
#   - safekeeper           (WAL, backed by YugabyteDB)
#   - compute_ctl + postgres v18 + the `yb_amp` extension (the compute)
#   - endpoint-supervisor (PID 1 under tini) that renders each service's
#                          config from environment variables, brings the stack
#                          up in order, and monitors/restarts the children
#
# No control plane runs in this image: it is a shared, cluster-level service
# that lives in the AMP layer. The endpoint requires the `yb_storage` schema to
# already exist; the controller provisions it on startup and is the only thing
# that does.
#
# Build (from the repo root):
#   docker build -t yb-amp/yb-amp-endpoint:dev -f deploy/endpoint/Dockerfile .
#
# yb-amp is Postgres v18 only (compute/vendor/postgres-v18, POSTGRES_VERSIONS=v18).

ARG REPOSITORY=quay.io/yugabyte
ARG IMAGE=amp-build-tools
ARG TAG=v1
ARG DEBIAN_VERSION=bookworm
ARG DEBIAN_FLAVOR=${DEBIAN_VERSION}-slim

# 1. Build patched Postgres v18 -> pg_install/v18.
FROM $REPOSITORY/$IMAGE:$TAG AS pg-build
WORKDIR /home/nonroot
# Postgres is configured --with-libxml (see Makefile) so the server exports the
# XML core the hexabridge extension links; that needs libxml2 headers at build
# time. The build-tools base runs as nonroot and can't apt-install, so add it as
# root here (mirrors deploy/extensions/pg18-build.Dockerfile's ABI base).
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends libxml2-dev \
    && rm -rf /var/lib/apt/lists/*
USER nonroot
COPY --chown=nonroot compute/vendor/postgres-v18 compute/vendor/postgres-v18
COPY --chown=nonroot Makefile Makefile
COPY --chown=nonroot postgres.mk postgres.mk
COPY --chown=nonroot scripts/ninstall.sh scripts/ninstall.sh
ENV BUILD_TYPE=release
RUN set -e \
    && mold -run make -j "$(nproc)" -s postgres

# Prebuilt yb_thin_client C++ shim (public, versioned per-arch download,
# pinned to the vendored ABI). Staged at a fixed path; the final image puts
# that path on LD_LIBRARY_PATH so the loader finds it (the binaries carry no
# rpath for it).
FROM $REPOSITORY/$IMAGE:$TAG AS yb-thin-client
WORKDIR /home/nonroot
COPY --chown=nonroot scripts/fetch-yb-thin-client.sh scripts/fetch-yb-thin-client.sh
COPY --chown=nonroot compute/libs/yb_thin_client/shim/VERSION compute/libs/yb_thin_client/shim/VERSION
COPY --chown=nonroot compute/libs/yb_thin_client/shim/COMMIT compute/libs/yb_thin_client/shim/COMMIT
# Stage under $HOME (the build-tools image runs as nonroot, which can't write
# /opt); the downstream COPY --from runs as root and places it at /opt.
RUN set -eux \
    && src="$(bash scripts/fetch-yb-thin-client.sh)" \
    && mkdir -p /home/nonroot/yb_thin_client/lib \
    && cp "$src/libyb_thin_client.so" /home/nonroot/yb_thin_client/lib/ \
    && cp "$src/../version.txt" /home/nonroot/yb_thin_client/version.txt

# 2. Build the Rust binaries + the `yb_amp` Postgres extension.
FROM $REPOSITORY/$IMAGE:$TAG AS build
WORKDIR /home/nonroot
ARG GIT_VERSION=local
ARG BUILD_TAG

# Reuse the Postgres build artifacts from the pg-build stage.
COPY --chown=nonroot --from=pg-build /home/nonroot/pg_install/ pg_install
COPY --chown=nonroot . .

# The storage binaries link the yb_thin_client shim; stage it and point
# build.rs at it (no stub fallback). Runtime resolution is handled in the
# final image via LD_LIBRARY_PATH.
COPY --from=yb-thin-client /home/nonroot/yb_thin_client /opt/yb_thin_client
ENV YB_THIN_CLIENT_LIB_DIR=/opt/yb_thin_client/lib

# As in the pg image: use the Postgres copied from `pg-build` rather than
# building another. `yb-amp-pg-ext` depends on `postgres-install-v18`, whose
# configure stamp lives in BUILD_DIR — not copied across — so make would
# re-configure and reinstall all of Postgres over the copied binaries, at the
# Makefile's `?= debug` default: `--enable-cassert -O0`, in the Postgres this
# image ships. PG_INSTALL_CACHED stubs those targets so nothing overwrites it.
ENV PG_INSTALL_CACHED=1
# Still needed, and not for Postgres: BUILD_TYPE also picks CARGO_PROFILE, and
# `yb-amp-pg-ext` drives a cargo build of libcommunicator.a that is linked into
# the yb_amp extension this image ships. Unset, that library is `--profile=dev`.
ENV BUILD_TYPE=release

# `io-align-512` is the default pageserver feature, so a plain release
# build is correct. We only need the endpoint's own services plus the
# control-plane tools used for (optional) schema provisioning.
#
# The cargo target dir + registry are BuildKit cache mounts (uid/gid match
# the build image's `nonroot` user), so a context change that busts the
# `COPY . .` layer triggers only an incremental recompile rather than a full
# one. Cache mounts are not part of
# the image layer, so the built binaries are copied out to /home/nonroot/out
# (a real dir) before the mount is unmounted; the final stage copies from there.
# RUSTFLAGS is exported rather than prefixed onto the cargo command: the `make`
# after the `&&` drives its own cargo build into the same target/release, and a
# different RUSTFLAGS there makes the two invalidate each other's artifacts —
# which under the cache mount above means recompiling the release dep graph
# twice per build.
RUN --mount=type=cache,target=/home/nonroot/target,uid=1000,gid=1000 \
    --mount=type=cache,target=/home/nonroot/.cargo/registry,uid=1000,gid=1000 \
    set -e \
    && export RUSTFLAGS="-Clinker=clang -Clink-arg=-fuse-ld=mold -Clink-arg=-Wl,--no-rosegment -Cforce-frame-pointers=yes" \
    && cargo build --locked --release \
         --bin pageserver \
         --bin safekeeper \
         --bin compute-ctl \
         --bin endpoint-supervisor \
         --bin dump-pages \
         --bin dump-wals \
         --bin reset-consistent-lsn \
    && mkdir -p /home/nonroot/out \
    && cp target/release/pageserver \
          target/release/safekeeper \
          target/release/compute-ctl \
          target/release/endpoint-supervisor \
          target/release/dump-pages \
          target/release/dump-wals \
          target/release/reset-consistent-lsn \
          /home/nonroot/out/ \
    && mold -run make -j "$(nproc)" -s yb-amp-pg-ext

# 3. Assemble the runtime image.
FROM debian:${DEBIAN_FLAVOR} AS endpoint
WORKDIR /data

# libxml2 (the runtime .so, not the -dev headers): the forked Postgres is now
# built --with-libxml (PG_CONFIGURE_OPTS in Makefile) so the server links it, and
# the externalized hexabridge extension .so links it too (its xmltype/dbms_xmldom
# packages). This is a fresh stage from a slim base — packages from the build
# stages don't carry over — so it must be installed here for either to load.
# libcurl4/zlib1g/openssl cover hexabridge's other link deps (utl_mail/utl_http,
# utl_compress, dbms_crypto).
RUN set -e \
    && echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-retries \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        openssl \
        libcurl4 \
        libxml2 \
        libreadline8 \
        libseccomp2 \
        libicu72 \
        zlib1g \
        liblz4-1 \
        libzstd1 \
        libossp-uuid16 \
        tini \
        netcat-openbsd \
        procps \
    && rm -f /etc/apt/apt.conf.d/80-retries \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* \
    && useradd -d /data -m yugabyte \
    && chown -R yugabyte:yugabyte /data

# Storage + control-plane binaries (copied out of the cache-mounted target dir
# into /home/nonroot/out by the build stage). The supervisor is copied further
# down, next to the ENTRYPOINT that runs it.
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/pageserver           /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/safekeeper           /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/compute-ctl          /usr/local/bin/

# Operator diagnostic tools (the tools crate): dump the YB-backed page store /
# WAL, reset the consistent LSN. On PATH so an operator can `docker exec` and run them.
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/dump-pages           /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/dump-wals            /usr/local/bin/
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/reset-consistent-lsn /usr/local/bin/
# The storage binaries link libyb_thin_client.so (a DT_NEEDED) but carry no
# rpath for it, so ship it at a fixed path and put that on the loader search
# path via LD_LIBRARY_PATH (distro-agnostic — the base's default lib dir
# varies, e.g. /usr/lib vs /usr/lib64).
COPY --from=yb-thin-client /home/nonroot/yb_thin_client/lib/libyb_thin_client.so /opt/yb_thin_client/lib/libyb_thin_client.so

# Postgres v18 install (binaries + the `yb_amp` extension installed by yb-amp-pg-ext).
COPY --from=build /home/nonroot/pg_install/v18 /usr/local/v18/

# The supervisor — PID 1 under tini (see the ENTRYPOINT below).
COPY --from=build --chown=yugabyte:yugabyte /home/nonroot/out/endpoint-supervisor /usr/local/bin/

# NB: LD_LIBRARY_PATH carries BOTH the Postgres v18 libs and the
# yb_thin_client shim dir (copied above). `ENV` assigns rather than appends, so
# both must be named here — a second `ENV LD_LIBRARY_PATH=...` elsewhere in this
# stage would silently drop whichever path it omits, and the storage binaries
# (DT_NEEDED on libyb_thin_client.so, no rpath) would fail to exec.
ENV PG_DISTRIB_DIR=/usr/local \
    LD_LIBRARY_PATH=/usr/local/v18/lib:/opt/yb_thin_client/lib \
    PATH=/usr/local/bin:/usr/local/v18/bin:$PATH

USER yugabyte

# Compute Postgres port (overridable via COMPUTE_PORT / chart values).
EXPOSE 55433
# pageserver / safekeeper HTTP.
EXPOSE 9898 7676

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/endpoint-supervisor"]
