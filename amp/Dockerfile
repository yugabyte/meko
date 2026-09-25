# syntax=docker/dockerfile:1
### Creates a storage Docker image with postgres, pageserver and safekeeper binaries.
### The image itself is mainly used as a container for the binaries and for starting e2e tests with custom parameters.
### By default, the binaries inside the image have some mock parameters and can start, but are not intended to be used
### inside this image in the real deployments.
ARG REPOSITORY=quay.io/yugabyte
ARG IMAGE=amp-build-tools
ARG TAG=v2
ARG DEBIAN_VERSION=bookworm
ARG DEBIAN_FLAVOR=${DEBIAN_VERSION}-slim

# Here are the INDEX DIGESTS for the images we use.
# You can get them following next steps for now:
# 1. Get an authentication token from DockerHub:
#    TOKEN=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/debian:pull" | jq -r .token)
# 2. Using that token, query index for the given tag:
#    curl -s -H "Authorization: Bearer $TOKEN" \
#       -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
#       "https://registry.hub.docker.com/v2/library/debian/manifests/bullseye-slim" \
#       -I | grep -i docker-content-digest
# 3. As a next step, TODO(fedordikarev): create script and schedule workflow to run these checks
#    and updates on regular bases and in automated way.
ARG BOOKWORM_SLIM_SHA=sha256:40b107342c492725bc7aacbe93a49945445191ae364184a6d24fedb28172f6f7
ARG BULLSEYE_SLIM_SHA=sha256:e831d9a884d63734fe3dd9c491ed9a5a3d4c6a6d32c5b14f2067357c49b0b7e1

# Here we use ${var/search/replace} syntax, to check
# if base image is one of the images, we pin image index for.
# If var will match one the known images, we will replace it with the known sha.
# If no match, than value will be unaffected, and will process with no-pinned image.
ARG BASE_IMAGE_SHA=debian:${DEBIAN_FLAVOR}
ARG BASE_IMAGE_SHA=${BASE_IMAGE_SHA/debian:bookworm-slim/debian@$BOOKWORM_SLIM_SHA}
ARG BASE_IMAGE_SHA=${BASE_IMAGE_SHA/debian:bullseye-slim/debian@$BULLSEYE_SLIM_SHA}

# Naive way:
#
# 1. COPY . .
# 1. make yb-amp-pg-ext
# 2. cargo build <storage binaries>
#
# But to enable docker to cache intermediate layers, we perform a few preparatory steps:
#
# - Build all postgres versions, depending on just the contents of compute/vendor/
# - Use cargo chef to build all rust dependencies

# 1. Build all postgres versions
FROM $REPOSITORY/$IMAGE:$TAG AS pg-build
WORKDIR /home/nonroot

# amp ships only the v18 postgres submodule (POSTGRES_VERSIONS = v18).
COPY --chown=nonroot compute/vendor/postgres-v18 compute/vendor/postgres-v18
COPY --chown=nonroot Makefile Makefile
COPY --chown=nonroot postgres.mk postgres.mk
COPY --chown=nonroot scripts/ninstall.sh scripts/ninstall.sh

# BUILD_TYPE picks the cargo profile AND the PostgreSQL build. Stated rather
# than left to the Makefile's default (debug, assertions on), because an image
# whose build type comes from a default it never named is exactly how #1019
# shipped a cassert build.
ENV BUILD_TYPE=release
RUN set -e \
    && mold -run make -j $(nproc) -s postgres

# 2. Prepare cargo-chef recipe
FROM $REPOSITORY/$IMAGE:$TAG AS plan
WORKDIR /home/nonroot

# The download-checksum manifest and its checker are in the context only for the
# yb-thin-client stage. Excluded here, a pin edit (a chart, an extension) does not
# invalidate this layer and rebuild everything after it.
COPY --chown=nonroot --exclude=downloads_checksum.txt --exclude=scripts/safe_curl.sh . .

RUN set -e \
    && cargo chef prepare --recipe-path recipe.json

# Prebuilt yb_thin_client C++ shim (public, versioned per-arch download,
# pinned to the vendored ABI). Staged at a fixed path; the final image puts
# that path on LD_LIBRARY_PATH so the loader finds it (the binaries carry no
# rpath for it).
FROM $REPOSITORY/$IMAGE:$TAG AS yb-thin-client
WORKDIR /home/nonroot
COPY --chown=nonroot scripts/fetch-yb-thin-client.sh scripts/fetch-yb-thin-client.sh
COPY --chown=nonroot scripts/safe_curl.sh scripts/safe_curl.sh
COPY --chown=nonroot downloads_checksum.txt downloads_checksum.txt
COPY --chown=nonroot compute/libs/yb_thin_client/shim/VERSION compute/libs/yb_thin_client/shim/VERSION
COPY --chown=nonroot compute/libs/yb_thin_client/shim/COMMIT compute/libs/yb_thin_client/shim/COMMIT
# Stage under $HOME (the build-tools image runs as nonroot, which can't write
# /opt); the downstream COPY --from runs as root and places it at /opt.
RUN set -eux \
    && src="$(bash scripts/fetch-yb-thin-client.sh)" \
    && mkdir -p /home/nonroot/yb_thin_client/lib \
    && cp "$src/libyb_thin_client.so" /home/nonroot/yb_thin_client/lib/ \
    && cp "$src/../version.txt" /home/nonroot/yb_thin_client/version.txt

# Main build image
FROM $REPOSITORY/$IMAGE:$TAG AS build
WORKDIR /home/nonroot
ARG GIT_VERSION=local
ARG BUILD_TAG
ARG ADDITIONAL_RUSTFLAGS=""
ARG IO_ALIGNMENT=512
ENV CARGO_FEATURES="default"

# The storage binaries link the yb_thin_client shim; stage it before cargo
# runs so both `cargo chef cook` and the main build find it (its build.rs
# reads YB_THIN_CLIENT_LIB_DIR and links -lyb_thin_client). Runtime resolution
# is handled in the final image via LD_LIBRARY_PATH.
COPY --from=yb-thin-client /home/nonroot/yb_thin_client /opt/yb_thin_client
ENV YB_THIN_CLIENT_LIB_DIR=/opt/yb_thin_client/lib

# 3. Build cargo dependencies. Note that this step doesn't depend on anything else than
# `recipe.json`, so the layer can be reused as long as none of the dependencies change.
COPY --from=plan     /home/nonroot/recipe.json                              recipe.json
RUN set -e \
    && RUSTFLAGS="-Clinker=clang -Clink-arg=-fuse-ld=mold -Clink-arg=-Wl,--no-rosegment -Cforce-frame-pointers=yes --cfg tokio_unstable --cfg tokio_taskdump ${ADDITIONAL_RUSTFLAGS}" cargo chef cook --locked --release --recipe-path recipe.json

# Perform the main build. We reuse the Postgres build artifacts from the intermediate 'pg-build'
# layer, and the cargo dependencies built in the previous step.
COPY --chown=nonroot --from=pg-build /home/nonroot/pg_install-release/ pg_install-release
# The download-checksum manifest and its checker are in the context only for the
# yb-thin-client stage. Excluded here, a pin edit (a chart, an extension) does not
# invalidate this layer and rebuild everything after it.
COPY --chown=nonroot --exclude=downloads_checksum.txt --exclude=scripts/safe_curl.sh . .
COPY --chown=nonroot --from=plan     /home/nonroot/Cargo.lock               Cargo.lock

# Use the Postgres copied from `pg-build`; do not build another one.
#
# `yb-amp-pg-ext` depends on `postgres-install-v18`, and the configure stamp that
# target keys on lives in BUILD_DIR — which is NOT copied from `pg-build`, only
# `pg_install-release` is. Left to itself, make therefore re-configures and reinstalls
# all of Postgres over the binaries that were copied. PG_INSTALL_CACHED stubs
# those targets, so the extension builds against the copied install's pg_config
# and nothing overwrites it — which is what the "reuse the Postgres build
# artifacts" comment above has always claimed, and makes a source-only change
# stop paying a full Postgres compile in a stage keyed on `COPY . .`.
# This stage runs cargo directly, not through build.sh, so postgres_ffi's build
# script gets no POSTGRES_INSTALL_DIR from make and would fall back to
# <root>/pg_install-debug — the debug tree, which this image never builds.
ENV POSTGRES_INSTALL_DIR=/home/nonroot/pg_install-release
ENV PG_INSTALL_CACHED=1
# BUILD_TYPE picks POSTGRES_INSTALL_DIR, so it has to agree with the stage the
# install was copied from — the extension builds against that tree's pg_config.
# It also picks CARGO_PROFILE, and `yb-amp-pg-ext` drives a cargo build of
# libcommunicator.a that is linked into the yb_amp extension this image ships;
# unset, that library is a `--profile=dev` build.
ENV BUILD_TYPE=release

# RUSTFLAGS is exported rather than prefixed onto the cargo command: the `make`
# after the `&&` drives its own cargo build into the same target/release, and a
# different RUSTFLAGS there makes the two invalidate each other's artifacts on
# every build.
RUN  set -e \
    && if [ "$IO_ALIGNMENT" = "4k" ]; then \
        export CARGO_FEATURES="${CARGO_FEATURES},io-align-4k"; \
    elif [ "$IO_ALIGNMENT" = "512" ]; then \
        export CARGO_FEATURES="${CARGO_FEATURES},io-align-512"; \
    fi \
    && export RUSTFLAGS="-Clinker=clang -Clink-arg=-fuse-ld=mold -Clink-arg=-Wl,--no-rosegment -Cforce-frame-pointers=yes --cfg tokio_unstable --cfg tokio_taskdump ${ADDITIONAL_RUSTFLAGS}" \
    && cargo auditable build \
      --features $CARGO_FEATURES \
      --bin pageserver  \
      --bin safekeeper  \
      --bin dump-pages \
      --bin dump-wals \
      --bin reset-consistent-lsn \
      --locked --release \
    && mold -run make -j $(nproc) -s yb-amp-pg-ext

# Assemble the final image
FROM $BASE_IMAGE_SHA
WORKDIR /data

# A versioned awscli, not the floating awscli-exe-linux-<arch>.zip: only a URL
# that always serves the same bytes can be pinned in downloads_checksum.txt.
# The manifest and its checker are bind-mounted, so neither lands in the image.
ARG AWSCLI_VERSION=2.37.1
RUN --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    set -e \
    && echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-retries \
    && apt update \
    && apt install -y \
        libreadline-dev \
        libseccomp-dev \
        ca-certificates \
        openssl \
        unzip \
        curl \
    && ARCH=$(uname -m) \
    && case "$ARCH" in x86_64|aarch64) ;; *) echo "Unsupported architecture: $ARCH"; exit 1 ;; esac \
    && sh /safe_curl/scripts/safe_curl.sh fetch \
        "https://awscli.amazonaws.com/awscli-exe-linux-${ARCH}-${AWSCLI_VERSION}.zip" awscliv2.zip \
    && unzip awscliv2.zip \
    && ./aws/install \
    && rm -rf aws awscliv2.zip \
    && rm -f /etc/apt/apt.conf.d/80-retries \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* \
    && useradd -d /data yb_amp \
    && chown -R yb_amp:yb_amp /data

COPY --from=build --chown=yb_amp:yb_amp /home/nonroot/target/release/pageserver           /usr/local/bin
COPY --from=build --chown=yb_amp:yb_amp /home/nonroot/target/release/safekeeper           /usr/local/bin
# Operator diagnostic tools (the tools crate): dump the page store / WAL, reset the consistent LSN.
COPY --from=build --chown=yb_amp:yb_amp /home/nonroot/target/release/dump-pages           /usr/local/bin
COPY --from=build --chown=yb_amp:yb_amp /home/nonroot/target/release/dump-wals            /usr/local/bin
COPY --from=build --chown=yb_amp:yb_amp /home/nonroot/target/release/reset-consistent-lsn /usr/local/bin
# The storage binaries link libyb_thin_client.so (a DT_NEEDED) but carry no
# rpath for it, so ship it at a fixed path and put that on the loader search
# path via LD_LIBRARY_PATH (distro-agnostic — the base's default lib dir
# varies, e.g. /usr/lib vs /usr/lib64).
COPY --from=yb-thin-client /home/nonroot/yb_thin_client/lib/libyb_thin_client.so /opt/yb_thin_client/lib/libyb_thin_client.so
ENV LD_LIBRARY_PATH=/opt/yb_thin_client/lib
COPY --from=build /home/nonroot/pg_install-release/v18 /usr/local/v18/

# Deprecated: Old deployment scripts use this tarball which contains all the Postgres binaries.
# That's obsolete, since all the same files are also present under /usr/local/v*. But to keep the
# old scripts working for now, create the tarball.
RUN tar -C /usr/local -cvzf /data/postgres_install.tar.gz v18

# By default, pageserver uses `.yb_amp/` working directory in WORKDIR, so create one and fill it with the dummy config.
# Now, when `docker run ... pageserver` is run, it can start without errors, yet will have some default dummy values.
RUN mkdir -p /data/.yb_amp/ && \
  echo "id=1234" > "/data/.yb_amp/identity.toml" && \
  echo "broker_endpoint='http://storage_broker:50051'\n" \
       "pg_distrib_dir='/usr/local/'\n" \
       "listen_pg_addr='0.0.0.0:6400'\n" \
       "listen_http_addr='0.0.0.0:9898'\n" \
       "availability_zone='local'\n" \
  > /data/.yb_amp/pageserver.toml && \
  chown -R yb_amp:yb_amp /data/.yb_amp

VOLUME ["/data"]
USER yb_amp
EXPOSE 6400
EXPOSE 9898

CMD ["/usr/local/bin/pageserver", "-D", "/data/.yb_amp"]
