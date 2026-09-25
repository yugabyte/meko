# yb-amp build-tools base image.
#
# Self-built replacement for ghcr.io/neondatabase/build-tools:pinned. It carries
# exactly the toolchain the three amp image builds need to compile PostgreSQL v18
# and the Rust storage/compute binaries -- and nothing else. The three consumers
# (Dockerfile, deploy/endpoint/Dockerfile, deploy/extensions/pg18-build.Dockerfile)
# COPY no other artifact out of this base, so one slim image serves all three.
#
# Kept:
#   * clang / LLVM 20, mold, make + build-essential, bison / flex and the
#     PostgreSQL build dependencies (openssl, seccomp, readline, zlib, ...);
#   * a STATIC ICU 67.1 install under /usr/local/icu, plus the
#     /home/nonroot/.docker_build marker the Makefile gates `--with-icu` on
#     (Makefile L70-76). The ICU major must match the compute's collation
#     library, so it is built from source and pinned here regardless of the
#     base OS's dynamic libicu;
#   * Rust (RUSTC_VERSION) with cargo-chef (the root image caches its dependency
#     layer with it; the endpoint build uses a plain `cargo build`) and
#     cargo-auditable (the root image's `cargo auditable build`);
#   * protoc, for the crates that codegen from .proto.
#
# Deliberately NOT here (were in the upstream Neon build-tools, but no amp image
# consumes them): the pgcopydb stage + postgresql-16, node, docker-ce, aws-cli,
# s5cmd, gdb, sql_exporter, lcov + the CPAN modules, pyenv/CPython (nothing in
# the PostgreSQL autoconf build, the cargo build, the pg-ext make, or the shim
# fetch invokes Python), and the extra cargo tools
# (deny/hakari/hack/nextest/diesel_cli/rustfilt).
#
# bookworm only -- every consumer builds bookworm and ships on debian:bookworm-slim.
#
# Build + publish with deploy/build-tools/build.sh (pushes
# quay.io/yugabyte/amp-build-tools:<tag>); .github/workflows/amp-build-tools.yml
# runs it in CI. The tag is a hand-bumped, immutable pin: change this image, then
# publish the next vN and bump `ARG TAG` in the three consumers in lockstep.
#
# The published repo is PRIVATE: consumers must `docker login quay.io` before
# building `FROM` it (CI already does; locally, use your own Quay login).

ARG DEBIAN_VERSION=bookworm
ARG DEBIAN_FLAVOR=${DEBIAN_VERSION}-slim

# INDEX DIGEST for debian:bookworm-slim. Pinning the index (multi-arch) digest
# keeps the base reproducible. To refresh:
# 1. Get an authentication token from DockerHub:
#    TOKEN=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/debian:pull" | jq -r .token)
# 2. Using that token, query the index for the given tag:
#    curl -s -H "Authorization: Bearer $TOKEN" \
#       -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" \
#       "https://registry.hub.docker.com/v2/library/debian/manifests/bookworm-slim" \
#       -I | grep -i docker-content-digest
ARG BOOKWORM_SLIM_SHA=sha256:40b107342c492725bc7aacbe93a49945445191ae364184a6d24fedb28172f6f7

# ${var/search/replace}: if the base matches the image we pin, swap in the known
# digest; otherwise the value is unaffected and the build proceeds unpinned.
ARG BASE_IMAGE_SHA=debian:${DEBIAN_FLAVOR}
ARG BASE_IMAGE_SHA=${BASE_IMAGE_SHA/debian:bookworm-slim/debian@$BOOKWORM_SLIM_SHA}

FROM $BASE_IMAGE_SHA AS build_tools
ARG DEBIAN_VERSION

# Add nonroot user (uid 1000). The three consumer builds run as this user and
# write under /home/nonroot.
RUN useradd -ms /bin/bash nonroot -b /home

# Use strict mode for bash to catch errors early.
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# Every file this image downloads is verified against its pin in
# downloads_checksum.txt by scripts/safe_curl.sh. Both are bind-mounted into the
# RUN that downloads, so neither lands in the image, and the build context is
# only those two files (Dockerfile.dockerignore beside this file).

# /bin/sh in the debian image treats '\n' literally, but bash's built-in echo
# needs '-e', which the SHELL above gives us.
RUN echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-retries && \
    echo -e "retry_connrefused=on\ntimeout=15\ntries=5\nretry-on-host-error=on\n" > /root/.wgetrc && \
    echo -e "--retry-connrefused\n--connect-timeout 15\n--retry 5\n--max-time 300\n" > /root/.curlrc

# System deps: the PostgreSQL build dependencies plus the general build tooling
# (cmake for the mold build, unzip for protoc, plus git and wget).
RUN set -e \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        autoconf \
        automake \
        bison \
        build-essential \
        ca-certificates \
        cmake \
        curl \
        flex \
        git \
        gnupg \
        gzip \
        jq \
        libcurl4-openssl-dev \
        libreadline-dev \
        libseccomp-dev \
        libssl-dev \
        libtool \
        libxml2-dev \
        libxxhash-dev \
        make \
        pkg-config \
        unzip \
        wget \
        xz-utils \
        zlib1g-dev \
        zstd \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# protobuf-compiler (protoc)
ENV PROTOC_VERSION=25.1
RUN --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    sh /safe_curl/scripts/safe_curl.sh fetch \
        "https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-$(uname -m | sed 's/aarch64/aarch_64/g').zip" protoc.zip \
    && unzip -q protoc.zip -d protoc \
    && mv protoc/bin/protoc /usr/local/bin/protoc \
    && mv protoc/include/google /usr/local/include/google \
    && rm -rf protoc.zip protoc

# LLVM. Install the apt key into a keyring (apt-key is deprecated/removed on
# recent debian) and reference it with signed-by=.
ENV LLVM_VERSION=20
#
# The key is pinned like any other download: it is what every LLVM package is
# verified against, so a swapped key would make apt trust whatever it signed.
RUN --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    sh /safe_curl/scripts/safe_curl.sh fetch 'https://apt.llvm.org/llvm-snapshot.gpg.key' /tmp/llvm.key \
    && gpg --dearmor -o /usr/share/keyrings/llvm.gpg < /tmp/llvm.key \
    && echo "deb [signed-by=/usr/share/keyrings/llvm.gpg] http://apt.llvm.org/${DEBIAN_VERSION}/ llvm-toolchain-${DEBIAN_VERSION}-${LLVM_VERSION} main" > /etc/apt/sources.list.d/llvm.stable.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends clang-${LLVM_VERSION} llvm-${LLVM_VERSION} \
    && bash -c 'for f in /usr/bin/clang*-${LLVM_VERSION} /usr/bin/llvm*-${LLVM_VERSION}; do ln -s "${f}" "${f%-${LLVM_VERSION}}"; done' \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Mold: A Modern Linker. Built with clang++ (installed above), so it must follow
# the LLVM step. From the release's source tarball rather than a clone of the
# tag: a tag can be moved, and the tarball's checksum is pinned.
ENV MOLD_VERSION=v2.37.1
RUN --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    set -e \
    && sh /safe_curl/scripts/safe_curl.sh fetch \
        "https://github.com/rui314/mold/archive/refs/tags/${MOLD_VERSION}.tar.gz" /tmp/mold.tar.gz \
    && mkdir /tmp/mold \
    && tar -xzf /tmp/mold.tar.gz -C /tmp/mold --strip-components=1 \
    && rm /tmp/mold.tar.gz \
    && cmake -S /tmp/mold -B /tmp/mold/build -DCMAKE_BUILD_TYPE=Release -DCMAKE_CXX_COMPILER=clang++ \
    && cmake --build /tmp/mold/build -j "$(nproc)" \
    && cmake --install /tmp/mold/build \
    && rm -rf /tmp/mold

# Use the same version of libicu as the compute nodes so that clusters created
# using initdb on the pageserver can be used by computes. 67.1 is pinned because
# existing compute collations were created with it: the compute's Postgres links
# this static, unpatched ICU (`--with-icu` in the Makefile), not the base OS's.
ENV ICU_VERSION=67.1
ENV ICU_PREFIX=/usr/local/icu

# Download and build static ICU.
RUN --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    sh /safe_curl/scripts/safe_curl.sh fetch \
        "https://github.com/unicode-org/icu/releases/download/release-${ICU_VERSION//./-}/icu4c-${ICU_VERSION//./_}-src.tgz" "/tmp/libicu-${ICU_VERSION}.tgz" && \
    mkdir /tmp/icu && \
    pushd /tmp/icu && \
    tar -xzf /tmp/libicu-${ICU_VERSION}.tgz && \
    pushd icu/source && \
    ./configure --prefix=${ICU_PREFIX}  --enable-static --enable-shared=no CXXFLAGS="-fPIC" CFLAGS="-fPIC" && \
    make -j "$(nproc)" && \
    make install && \
    popd && \
    rm -rf icu && \
    rm -f /tmp/libicu-${ICU_VERSION}.tgz

# Switch to nonroot user
USER nonroot:nonroot
WORKDIR /home/nonroot

RUN echo -e "--retry-connrefused\n--connect-timeout 15\n--retry 5\n--max-time 300\n" > /home/nonroot/.curlrc

# Rust
# Please keep the version of llvm (installed above) in sync with rust llvm (`rustc --version --verbose | grep LLVM`)
ENV RUSTC_VERSION=1.88.0
ENV RUSTUP_HOME="/home/nonroot/.rustup"
ENV PATH="/home/nonroot/.cargo/bin:${PATH}"
ARG CARGO_AUDITABLE_VERSION=0.7.0
ARG CARGO_CHEF_VERSION=0.1.71
# A versioned rustup-init, not the floating rustup/dist one: only a URL that
# always serves the same bytes can be pinned. This is only the installer; the
# toolchain it installs is verified by rustup against the dist manifest.
ARG RUSTUP_VERSION=1.29.1
RUN --mount=type=bind,source=downloads_checksum.txt,target=/safe_curl/downloads_checksum.txt \
    --mount=type=bind,source=scripts/safe_curl.sh,target=/safe_curl/scripts/safe_curl.sh \
    sh /safe_curl/scripts/safe_curl.sh fetch \
        "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/$(uname -m)-unknown-linux-gnu/rustup-init" rustup-init && whoami && \
	chmod +x rustup-init && \
	./rustup-init -y --default-toolchain ${RUSTC_VERSION} && \
	rm rustup-init && \
    export PATH="$HOME/.cargo/bin:$PATH" && \
    . "$HOME/.cargo/env" && \
    cargo --version && rustup --version && \
    rustup component add llvm-tools rustfmt clippy && \
    cargo install cargo-auditable           --locked --version "${CARGO_AUDITABLE_VERSION}" && \
    cargo auditable install cargo-auditable --locked --version "${CARGO_AUDITABLE_VERSION}" --force && \
    cargo auditable install cargo-chef      --locked --version "${CARGO_CHEF_VERSION}" && \
    rm -rf /home/nonroot/.cargo/registry && \
    rm -rf /home/nonroot/.cargo/git

# Show versions
RUN whoami \
    && cargo --version --verbose \
    && rustup --version --verbose \
    && rustc --version --verbose \
    && clang --version

# Set following flag to check in Makefile if its running in Docker
RUN touch /home/nonroot/.docker_build
