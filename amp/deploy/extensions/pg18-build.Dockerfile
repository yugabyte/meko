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
       libpcre2-dev \
    && rm -rf /var/lib/apt/lists/*

# documentdb links three libraries beyond the PostgreSQL build's own. Two of
# them, libbson and BID, bookworm carries too old, so they are built here
# rather than apt-installed; the third, pcre2, comes from apt above. Versions
# are the ones documentdb pins in scripts/setup_versions.sh.
#
#   libbson  — bookworm ships 1.23; documentdb pins 1.28.0. Built with
#              ENABLE_MONGOC=OFF: the extension needs the BSON library alone,
#              not the MongoDB client driver.
#   BID      — Intel's decimal floating-point library (documentdb's
#              src/types/decimal128.c includes bid_conf.h and bid_functions.h).
#              See the block below for where it comes from and why.
#
# pcre2 comes from apt above: bookworm's 10.42 satisfies the 10.40 pin.
ARG LIBBSON_VERSION=1.28.0
ARG LIBBSON_SHA256=fc8ef2d081d9388b9016d74826f4a229d213a2813708d26063d771ab12e457cb
RUN set -eux; \
    curl -fsSL -o /tmp/bson.tar.gz \
      "https://github.com/mongodb/mongo-c-driver/archive/refs/tags/${LIBBSON_VERSION}.tar.gz"; \
    echo "${LIBBSON_SHA256}  /tmp/bson.tar.gz" | sha256sum -c -; \
    mkdir -p /tmp/bson && tar xzf /tmp/bson.tar.gz -C /tmp/bson --strip-components=1; \
    cmake -S /tmp/bson -B /tmp/bson/build \
      -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local \
      -DENABLE_MONGOC=OFF -DENABLE_STATIC=ON -DENABLE_TESTS=OFF -DCMAKE_POSITION_INDEPENDENT_CODE=ON; \
    cmake --build /tmp/bson/build --target install -j "$(nproc)"; \
    rm -rf /tmp/bson /tmp/bson.tar.gz

# BID is Intel RDFP Math Library 2.0 Update 3 with Debian's patch series
# applied — exactly what documentdb pins (INTEL_DECIMAL_MATH_LIB_VERSION=
# applied/2.0u3-1, fetched upstream from the git-ubuntu import at
# git.launchpad.net/ubuntu/+source/intelrdfpmath). Built rather than
# apt-installed because bookworm's libintelrdfpmath-dev is 2.0u2. It is taken as
# the source package rather than that git tag so both halves are sha256-pinned;
# orig + debian/patches/series reproduces the applied/2.0u3-1 tree byte for byte.
#
# The URLs are Launchpad's +sourcefiles, not archive.ubuntu.com: the archive pool
# drops a version once no supported series publishes it, and 2.0u3-1 is published
# only in questing (an interim release), with 2.0u4-3 already beside it.
# Launchpad keeps every uploaded source file indefinitely.
#
# CALL_BY_REF/GLOBAL_RND/GLOBAL_FLAGS=0 is the calling convention documentdb
# compiles against; these are the flags its own install script passes.
ARG BID_VERSION=2.0u3
ARG BID_DEBIAN_REVISION=1
ARG BID_ORIG_SHA256=13f6924b2ed71df9b137a7df98706a0dcc3b43c283a0e32f8b6eadca4305136a
ARG BID_DEBIAN_SHA256=289a777dd489ffd933a813c1053677f06ffeea2418d862c11e20b638e07ae9c3
RUN set -eux; \
    src="https://launchpad.net/ubuntu/+archive/primary/+sourcefiles/intelrdfpmath/${BID_VERSION}-${BID_DEBIAN_REVISION}"; \
    curl -fsSL -o /tmp/bid.orig.tar.gz "${src}/intelrdfpmath_${BID_VERSION}.orig.tar.gz"; \
    curl -fsSL -o /tmp/bid.debian.tar.xz "${src}/intelrdfpmath_${BID_VERSION}-${BID_DEBIAN_REVISION}.debian.tar.xz"; \
    echo "${BID_ORIG_SHA256}  /tmp/bid.orig.tar.gz" | sha256sum -c -; \
    echo "${BID_DEBIAN_SHA256}  /tmp/bid.debian.tar.xz" | sha256sum -c -; \
    mkdir -p /tmp/bid; \
    tar xzf /tmp/bid.orig.tar.gz -C /tmp/bid; \
    tar xJf /tmp/bid.debian.tar.xz -C /tmp/bid; \
    while read -r p; do \
      [ -n "$p" ] && patch -d /tmp/bid -p1 --batch --fuzz=0 < "/tmp/bid/debian/patches/$p"; \
    done < /tmp/bid/debian/patches/series; \
    make -C /tmp/bid/LIBRARY -j "$(nproc)" \
      CC=gcc CALL_BY_REF=0 GLOBAL_RND=0 GLOBAL_FLAGS=0 UNCHANGED_BINARY_FLAGS=0 _CFLAGS_OPT=-fPIC; \
    install -m 644 /tmp/bid/LIBRARY/libbid.a /usr/local/lib/libbid.a; \
    mkdir -p /usr/local/include/bid; \
    cp /tmp/bid/LIBRARY/src/*.h /usr/local/include/bid/; \
    rm -rf /tmp/bid /tmp/bid.orig.tar.gz /tmp/bid.debian.tar.xz
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
ENV PG_CONFIG=/home/nonroot/pg_install-release/v18/bin/pg_config
ENV PATH=/home/nonroot/pg_install-release/v18/bin:$PATH
