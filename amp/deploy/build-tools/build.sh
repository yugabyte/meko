#!/usr/bin/env bash
# Build (and optionally push) the yb-amp build-tools base image.
#
# This is the self-built replacement for ghcr.io/neondatabase/build-tools:pinned:
# the slim toolchain the three amp image builds compile against (Dockerfile,
# deploy/endpoint/Dockerfile, deploy/extensions/pg18-build.Dockerfile). Run from
# anywhere; paths resolve to the amp/ tree.
#
# Usage:
#   deploy/build-tools/build.sh [--push] [--registry REG] [--image NAME] [--tag TAG]
#
#   --push         also `docker push` the built image
#   --registry REG registry namespace (default: quay.io/yugabyte)
#   --image NAME   image name (default: amp-build-tools)
#   --tag TAG      tag to build/push (default: v2)
#
# The tag is a hand-bumped, IMMUTABLE pin. When you change build-tools/Dockerfile:
# publish the next vN with this script (or the amp-build-tools.yml workflow), then
# bump `ARG TAG` in the three consumer Dockerfiles in the same lockstep. Never
# re-push an existing tag -- consumers reference it by the committed `ARG TAG`, so
# overwriting a published tag silently changes what old commits build against.
# Immutability is enforced push-side: the amp-build-tools.yml workflow refuses to
# overwrite an already-published tag (no Quay repo-setting is relied on).
#
# The Quay repo is PRIVATE, like every other amp image. A build environment must
# `docker login quay.io` (the QUAY_AMP_TOKEN robot, which has read) before
# building a consumer that pulls this base -- CI already does; locally, use your
# own Quay login.
#
# Requires: docker.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"

push=false
registry="quay.io/yugabyte"
image="amp-build-tools"
tag="v2"
while [[ $# -gt 0 ]]; do
  case "$1" in
  --push)
    push=true
    shift
    ;;
  --registry)
    registry="${2:?--registry needs a value}"
    shift 2
    ;;
  --image)
    image="${2:?--image needs a value}"
    shift 2
    ;;
  --tag)
    tag="${2:?--tag needs a value}"
    shift 2
    ;;
  -h | --help)
    sed -n '2,29p' "$0"
    exit 0
    ;;
  -*)
    echo "unknown flag: $1" >&2
    exit 2
    ;;
  *)
    echo "unexpected argument: $1" >&2
    exit 2
    ;;
  esac
done

ref="${registry}/${image}:${tag}"

# The slimmed build-tools Dockerfile has no COPY from the build context (it
# downloads every input). The context is amp/ only so its downloads can be
# verified against downloads_checksum.txt: build-tools/Dockerfile.dockerignore
# admits that file and scripts/safe_curl.sh and nothing else, keeping the
# context tiny and independent of the (huge) amp/ working tree. That per-
# Dockerfile ignore file is a BuildKit feature, hence DOCKER_BUILDKIT=1.
echo ">> building ${ref}"
DOCKER_BUILDKIT=1 docker build -t "$ref" -f "$repo_root/build-tools/Dockerfile" "$repo_root"

if $push; then
  echo ">> pushing ${ref}"
  docker push "$ref"
fi

echo "done"
