# Build the foundation image — the CLI that stands up and converges an
# operator-managed YugabyteDB universe (foundation/cmd/foundation). Built and
# pushed as quay.io/yugabyte/meko-foundation by build-image.sh.
#
# It exists because `go run` from a checkout is not a way to ship this: the
# module path `meko/foundation` is not a resolvable URL, so `go install` cannot
# work even internally, and the repo is private.
#
# kubectl and helm ride along because the deployer shells out to both — they are
# not an implementation detail of the image, they are two thirds of what it is.
# Pinning them here is also the only place their versions are chosen: Preflight
# refuses helm below 3.11, and nothing checks the kubectl version at all.
#
# Cross-compiles: the builder runs on the BUILD platform and Go emits a binary
# for the TARGET one, so `--platform linux/amd64` from an arm64 laptop compiles
# natively instead of under emulation.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH

# Both tools are fetched for the TARGET arch and checksum-verified. The arch
# matters: hardcoding amd64 would silently put an amd64 binary in an arm64
# image, which fails at the first exec rather than at build.
ARG HELM_VERSION=3.18.6
RUN set -e \
    && arch="${TARGETARCH:-$(go env GOARCH)}" \
    && url="https://get.helm.sh/helm-v${HELM_VERSION}-linux-${arch}.tar.gz" \
    && wget -qO /tmp/helm.tgz "$url" \
    && wget -qO /tmp/helm.sha256 "${url}.sha256sum" \
    && echo "$(cut -d' ' -f1 /tmp/helm.sha256)  /tmp/helm.tgz" | sha256sum -c - \
    && tar -xzf /tmp/helm.tgz -C /tmp "linux-${arch}/helm" \
    && mv "/tmp/linux-${arch}/helm" /helm

ARG KUBECTL_VERSION=1.31.5
RUN set -e \
    && arch="${TARGETARCH:-$(go env GOARCH)}" \
    && base="https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${arch}/kubectl" \
    && wget -qO /kubectl "$base" \
    && wget -qO /tmp/kubectl.sha256 "${base}.sha256" \
    && echo "$(cat /tmp/kubectl.sha256)  /kubectl" | sha256sum -c - \
    && chmod +x /kubectl

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /foundation ./cmd/foundation

FROM gcr.io/distroless/static:nonroot
COPY --from=build /foundation /foundation
COPY --from=build /helm /usr/local/bin/helm
COPY --from=build /kubectl /usr/local/bin/kubectl
# Both tools write under $HOME, which is /home/nonroot here and not writable by
# UID 65532. /tmp is. kubectl's discovery cache is the one that matters: without
# a writable path it re-fetches the API surface on every call, which `prune`
# makes once per converge.
ENV HELM_CACHE_HOME=/tmp/helm/cache \
    HELM_CONFIG_HOME=/tmp/helm/config \
    HELM_DATA_HOME=/tmp/helm/data \
    KUBECACHEDIR=/tmp/kube/cache
ENTRYPOINT ["/foundation"]
