# Build a self-contained PostgreSQL-capable Voyager image from a remote branch.
#
# Run from the amp repository root:
#   docker build \
#     --build-arg VOYAGER_BRANCH=ameya/amp-voyager-poc \
#     --build-arg VOYAGER_COMMIT="$(git ls-remote \
#         https://github.com/yugabyte/yb-voyager.git ameya/amp-voyager-poc | cut -f1)" \
#     -t yb-voyager:ameya-amp-voyager-poc \
#     -f deploy/voyager/Dockerfile .
#
# VOYAGER_COMMIT is optional but recommended: it both busts Docker's layer cache
# when the branch advances (a same-named branch alone would otherwise reuse a
# stale cached clone) and pins/verifies the exact revision that gets built.

# yb-voyager only ships amd64 artifacts, so always build for linux/amd64.
# --platform (rather than the amd64/ubuntu mirror repo, which has no arm64 index)
# lets this build on arm64 hosts (e.g. Apple Silicon) via emulation.
FROM --platform=linux/amd64 ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG VOYAGER_REPOSITORY=https://github.com/yugabyte/yb-voyager.git
ARG VOYAGER_BRANCH=main
ARG VOYAGER_COMMIT=unpinned

ENV TZ=UTC \
    LANG=en_US.UTF-8 \
    LANGUAGE=en_US:en \
    LC_ALL=en_US.UTF-8

# The Voyager installer expects these tools before it installs the PostgreSQL
# client, Go, and remaining runtime dependencies.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        binutils \
        ca-certificates \
        curl \
        gcc \
        git \
        gnupg \
        jq \
        libc6-dev \
        locales \
        make \
        openjdk-17-jre-headless \
        rsync \
        sudo \
        tzdata \
        wget && \
    locale-gen en_US.UTF-8 && \
    ln -snf /usr/share/zoneinfo/${TZ} /etc/localtime && \
    echo "${TZ}" >/etc/timezone

# Clone into /tmp: it is only build-time scratch for the installer (which installs
# yb-voyager into /usr/local/bin and its own state under /opt) and is removed once
# the build completes. Passing VOYAGER_COMMIT changes this layer's inputs so a
# branch that advanced is re-cloned instead of reusing a stale cached layer, and
# the clone's HEAD is verified against it when it is pinned.
RUN git clone --depth 1 --branch "${VOYAGER_BRANCH}" \
        "${VOYAGER_REPOSITORY}" /tmp/yb-voyager && \
    head="$(git -C /tmp/yb-voyager rev-parse HEAD)" && \
    echo "yb-voyager ${VOYAGER_BRANCH}@${head}" && \
    if [ "${VOYAGER_COMMIT}" != "unpinned" ] && [ "${VOYAGER_COMMIT}" != "${head}" ]; then \
        echo "ERROR: VOYAGER_COMMIT=${VOYAGER_COMMIT} != branch head ${head}" >&2; \
        exit 1; \
    fi

LABEL org.opencontainers.image.revision="${VOYAGER_COMMIT}"

WORKDIR /tmp/yb-voyager/installer_scripts

# Build Voyager from the cloned branch with PostgreSQL support. Debezium is
# intentionally omitted because this image is used for offline migrations. The
# clone is scratch, so drop it (and the Go build caches) in the same layer.
RUN (yes | ./install-yb-voyager \
        --install-from-local-source \
        --only-pg-support \
        --skip-debezium) || \
        { cat /tmp/install-yb-voyager.log; exit 1; } && \
    rm -rf /tmp/yb-voyager /root/go /root/.cache /tmp/go && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Run migrations unprivileged, matching the other AMP images (the endpoint runs
# as `yugabyte`). yb-voyager lives in /usr/local/bin and its config in
# /etc/yb-voyager, both world-readable, so a non-root user can still run it. The
# controller mounts the export volume at /voyager/export. Creating it here lets
# Docker initialize a new named volume with this ownership. A Kubernetes PVC
# hides this image directory instead, so the runner sets the pod's matching
# fsGroup to grant `yugabyte` write access — see voyager_kubernetes.go.
RUN groupadd --gid 1000 yugabyte && \
    useradd --uid 1000 --gid 1000 --create-home --home-dir /home/yugabyte yugabyte && \
    mkdir -p /voyager/export && \
    chown -R yugabyte:yugabyte /voyager

USER yugabyte
WORKDIR /home/yugabyte

CMD ["sleep", "infinity"]
