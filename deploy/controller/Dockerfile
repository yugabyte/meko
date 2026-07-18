# yb-amp controller-layer container: the amp controller (issue #94).
#
# controller (the amp-controller, a Go service) is the cluster control plane the
# endpoint image (deploy/endpoint) deliberately leaves out — it owns generation
# authority, the storage-layer registries, and `yb_storage` provisioning.
#
# This image packages a PREBUILT binary — it does not compile anything, so it
# needs no build toolchain. Build the bundle first, then build this image:
#
#   make controller-bundle BUILD_TYPE=release          # -> target/controller-bundle/controller
#   docker build -t yb-amp/controller:dev -f deploy/controller/Dockerfile .

ARG DEBIAN_VERSION=bookworm
ARG DEBIAN_FLAVOR=${DEBIAN_VERSION}-slim
# Where `make controller-bundle` put the binaries, relative to the build context (the
# repo root). Override with --build-arg if you bundle elsewhere.
ARG BUNDLE_DIR=target/controller-bundle

FROM debian:${DEBIAN_FLAVOR} AS controller
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

# The bundle: the controller binary. We copy the whole bundle directory. The
# controller binary is static (CGO_ENABLED=0), so it runs on a slim base.
ARG BUNDLE_DIR
COPY --chown=yugabyte:yugabyte ${BUNDLE_DIR}/ /opt/yb-amp-amp/

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
COPY --chown=yugabyte:yugabyte deploy/helm/yb-amp-endpoint /opt/yb-amp-amp/chart

ENV PATH=/opt/yb-amp-amp:$PATH \
    AMP_LISTEN=:8080 \
    AMP_DEPLOY_CHART=/opt/yb-amp-amp/chart

USER yugabyte

# amp controller API.
EXPOSE 8080

# controller serves the API + task worker and supervises the bundled storage
# controller. AMP_DATABASE_URL must be provided at run time. tini is PID 1 so
# signals reach controller and any orphaned grandchildren are reaped.
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/yb-amp-amp/controller", "serve"]
