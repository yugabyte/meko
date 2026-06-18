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

# The Docker CLI, so the controller's Docker deployer can launch endpoint
# containers over a mounted daemon socket. Just the static client binary (no
# daemon, no apt repo). The Kubernetes deployer uses client-go instead and needs
# neither this nor the socket, so this is specific to the Docker runtime.
ARG DOCKER_CLI_VERSION=26.1.3
ADD https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_CLI_VERSION}.tgz /tmp/docker.tgz
RUN set -e \
    && tar -xzf /tmp/docker.tgz -C /tmp docker/docker \
    && mv /tmp/docker/docker /usr/local/bin/docker \
    && chmod +x /usr/local/bin/docker \
    && rm -rf /tmp/docker /tmp/docker.tgz

# The bundle: amp (and, in the full bundle, storage_controller) side by side.
# amp resolves the storage controller from its own directory (#94), so no bin
# path needs configuring. We copy the whole bundle directory so the same image
# works for both layouts: `make amp-bundle` (amp + storage_controller) and
# `make amp-bundle-amp-only` (amp alone — run with AMP_STORAGE_CONTROLLER=false).
# The amp binary is static (CGO_ENABLED=0); the storage controller, when present,
# is dynamically linked against glibc, hence the build-it-on-a-compatible-base note.
ARG BUNDLE_DIR
COPY --chown=yugabyte:yugabyte ${BUNDLE_DIR}/ /opt/yb-light-amp/

# helm + the endpoint chart, for the Kubernetes deploy runtime: the k8s
# provisioner shells out to `helm upgrade --install` (it is not a client-go
# deployer). In-cluster, helm uses the pod's service account, so no kubeconfig is
# needed — just RBAC on that service account to manage the endpoint's objects.
ARG HELM_VERSION=3.18.6
ADD https://get.helm.sh/helm-v${HELM_VERSION}-linux-amd64.tar.gz /tmp/helm.tgz
RUN set -e \
    && tar -xzf /tmp/helm.tgz -C /tmp linux-amd64/helm \
    && mv /tmp/linux-amd64/helm /usr/local/bin/helm \
    && chmod +x /usr/local/bin/helm \
    && rm -rf /tmp/helm.tgz /tmp/linux-amd64
COPY --chown=yugabyte:yugabyte deploy/helm/yb-light-endpoint /opt/yb-light-amp/chart

ENV PATH=/opt/yb-light-amp:$PATH \
    AMP_LISTEN=:8080 \
    AMP_STORAGE_CONTROLLER=true \
    AMP_STORAGE_CONTROLLER_LISTEN=127.0.0.1:50051 \
    AMP_DEPLOY_CHART=/opt/yb-light-amp/chart

USER yugabyte

# amp controller API.
EXPOSE 8080

# amp serves the API + task worker and supervises the bundled storage
# controller. AMP_DATABASE_URL must be provided at run time. tini is PID 1 so
# signals reach amp and any orphaned grandchildren are reaped.
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/yb-light-amp/amp", "serve"]
