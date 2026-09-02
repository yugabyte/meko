#!/usr/bin/env bash
#
# Build the meko api-server image and (optionally) push it to Quay, so the YCP
# control plane — api_server run with --control-plane-mode — can be deployed on
# Kubernetes. Wrapped by .github/workflows/meko-control-plane-image.yml on main
# and workflow_dispatch; stays runnable locally.
#
# Usage:
#   meko/api_server/build-image.sh            # build + push to Quay
#   meko/api_server/build-image.sh --no-push  # build only (no credentials needed)
#   meko/api_server/build-image.sh --no-latest  # tag/push custom-<sha> only
#
# Credentials (push only), in precedence order:
#   QUAY_USER + QUAY_TOKEN   — a Quay robot account name and its token
#   QUAY_TOKEN as "user:token"
#   QUAY_AMP_TOKEN           — same as QUAY_TOKEN, alternate name
#
# Overridable via env: IMAGE (default quay.io/yugabyte/ycp-api-server),
# PLATFORM (default linux/amd64 — the GKE/EKS demo clusters are amd64), DOCKER.
#
# Tags: custom-<short-sha> (immutable, what a deployment should pin), plus
# latest unless --no-latest. CI passes that off main, so only a main build
# moves the floating tag a no-TAG deploy resolves.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${IMAGE:-quay.io/yugabyte/ycp-api-server}"
PLATFORM="${PLATFORM:-linux/amd64}"
DOCKER="${DOCKER:-docker}"
PUSH=1
LATEST=1

# Print the whole leading comment block, so growing the header cannot silently
# truncate --help the way a fixed line range did.
usage() { awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; }

for arg in "$@"; do
  case "$arg" in
    --no-push) PUSH=0 ;;
    --no-latest) LATEST=0 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; usage >&2; exit 2 ;;
  esac
done

SHA="$(git -C "$HERE" rev-parse --short HEAD 2>/dev/null || echo unknown)"
TAG_SHA="custom-${SHA}"

if [ "$LATEST" = "1" ]; then
  echo "==> building ${IMAGE}:${TAG_SHA} (+ :latest) for ${PLATFORM}"
else
  echo "==> building ${IMAGE}:${TAG_SHA} for ${PLATFORM}"
fi

if [ "$PUSH" = "1" ]; then
  token="${QUAY_TOKEN:-${QUAY_AMP_TOKEN:-}}"
  if [ -z "$token" ]; then
    echo "no Quay credentials: set QUAY_USER+QUAY_TOKEN (or QUAY_TOKEN=user:token), or pass --no-push" >&2
    exit 1
  fi
  if [ -n "${QUAY_USER:-}" ]; then
    quay_user="$QUAY_USER"; quay_pass="$token"
  elif printf '%s' "$token" | grep -q ':'; then
    quay_user="${token%%:*}"; quay_pass="${token#*:}"
  else
    echo "QUAY_TOKEN has no username: set QUAY_USER, or give QUAY_TOKEN as user:token" >&2
    exit 1
  fi
  echo "==> logging in to quay.io as ${quay_user}"
  printf '%s' "$quay_pass" | "$DOCKER" login quay.io -u "$quay_user" --password-stdin
fi

build_args=(buildx build --platform "$PLATFORM" -f "$HERE/Dockerfile"
  -t "${IMAGE}:${TAG_SHA}")
# `latest` is a floating tag deployments resolve by default
# (amp/deploy/up-gke.sh: TAG="${TAG:-latest}"), so only a main build may move it.
if [ "$LATEST" = "1" ]; then
  build_args+=(-t "${IMAGE}:latest")
fi
if [ "$PUSH" = "1" ]; then
  build_args+=(--push)
fi
build_args+=("$HERE")

"$DOCKER" "${build_args[@]}"

if [ "$PUSH" = "1" ]; then
  if [ "$LATEST" = "1" ]; then
    echo "==> pushed ${IMAGE}:${TAG_SHA} and ${IMAGE}:latest"
  else
    echo "==> pushed ${IMAGE}:${TAG_SHA}"
  fi
else
  echo "==> built ${IMAGE}:${TAG_SHA} (push skipped)"
fi
