# yb-light AMP-layer container: the amp controller bundled with the storage
# controller it manages (issue #94).
#
# amp (the yblight-controller, a Go service) supervises storage_controller as a
# child process — it spawns it, streams its logs, restarts it on exit, and stops
# it cleanly on shutdown (yblight-controller.md §11.1). The two binaries are
# bundled side by side under /opt/yb-light-amp, so amp resolves the storage
# controller from its own directory with no path configuration.
#
# This image packages PREBUILT binaries — it does not compile anything, so it
# needs no build toolchain. Build the bundle first (in the build-tools image or
# CI, so the binaries link against a compatible glibc), then build this image:
#
#   make amp-bundle BUILD_TYPE=release          # -> target/amp-bundle/{amp,storage_controller}
#   docker build -t yb-light/amp:dev -f deploy/amp/Dockerfile .
#
# The storage_controller is the shared, cluster-level control plane the endpoint
# image (deploy/endpoint) deliberately leaves out — it belongs to this AMP layer.

ARG DEBIAN_VERSION=bookworm
ARG DEBIAN_FLAVOR=${DEBIAN_VERSION}-slim
# Where `make amp-bundle` put the binaries, relative to the build context (the
# repo root). Override with --build-arg if you bundle elsewhere.
ARG BUNDLE_DIR=target/amp-bundle

FROM debian:${DEBIAN_FLAVOR} AS amp
WORKDIR /data

RUN set -e \
    && echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-retries \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        tini \
    && rm -f /etc/apt/apt.conf.d/80-retries \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* \
    && useradd -d /data -m yugabyte \
    && chown -R yugabyte:yugabyte /data

# The bundle: amp and storage_controller side by side. amp resolves the storage
# controller from its own directory (#94), so no bin path needs configuring.
# The amp binary is static (built CGO_ENABLED=0 by `make amp-bundle`); the
# storage controller is dynamically linked against glibc, hence the
# build-it-on-a-compatible-base note above.
ARG BUNDLE_DIR
COPY --chown=yugabyte:yugabyte ${BUNDLE_DIR}/amp                /opt/yb-light-amp/amp
COPY --chown=yugabyte:yugabyte ${BUNDLE_DIR}/storage_controller /opt/yb-light-amp/storage_controller

ENV PATH=/opt/yb-light-amp:$PATH \
    AMP_LISTEN=:8080 \
    AMP_STORAGE_CONTROLLER=true \
    AMP_STORAGE_CONTROLLER_LISTEN=127.0.0.1:50051

USER yugabyte

# amp controller API.
EXPOSE 8080

# amp serves the API + task worker and supervises the bundled storage
# controller. AMP_DATABASE_URL must be provided at run time. tini is PID 1 so
# signals reach amp and any orphaned grandchildren are reaped.
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/yb-light-amp/amp", "serve"]
