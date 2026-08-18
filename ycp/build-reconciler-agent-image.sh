#!/usr/bin/env bash
#
# Build the reconcile-agent image and (optionally) push it to Quay, so it can be
# deployed on Kubernetes beside an amp controller. Wrapped by
# .github/workflows/ycp-reconciler-agent-image.yml on main; stays runnable
# locally.
#
# Usage:
#   ycp/build-reconciler-agent-image.sh            # build + push to Quay
#   ycp/build-reconciler-agent-image.sh --no-push  # build only (no credentials needed)
#
# Credentials (push only), in precedence order:
#   QUAY_USER + QUAY_TOKEN   — a Quay robot account name and its token
#   QUAY_TOKEN as "user:token"
#   QUAY_AMP_TOKEN           — same as QUAY_TOKEN, alternate name
#
# Overridable via env: IMAGE (default quay.io/yugabyte/ycp-reconciler-agent),
# PLATFORM (default linux/amd64 — the GKE/EKS demo clusters are amd64), DOCKER.
#
# Tags: custom-<short-sha> (immutable, what a deployment should pin) and latest.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${IMAGE:-quay.io/yugabyte/ycp-reconciler-agent}"
PLATFORM="${PLATFORM:-linux/amd64}"
DOCKER="${DOCKER:-docker}"
PUSH=1

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; }

for arg in "$@"; do
  case "$arg" in
    --no-push) PUSH=0 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; usage >&2; exit 2 ;;
  esac
done

SHA="$(git -C "$HERE" rev-parse --short HEAD 2>/dev/null || echo unknown)"
TAG_SHA="custom-${SHA}"

echo "==> building ${IMAGE}:${TAG_SHA} (+ :latest) for ${PLATFORM}"

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

build_args=(buildx build --platform "$PLATFORM" -f "$HERE/Dockerfile.reconcile-agent"
  -t "${IMAGE}:${TAG_SHA}" -t "${IMAGE}:latest")
if [ "$PUSH" = "1" ]; then
  build_args+=(--push)
fi
build_args+=("$HERE")

"$DOCKER" "${build_args[@]}"

if [ "$PUSH" = "1" ]; then
  echo "==> pushed ${IMAGE}:${TAG_SHA} and ${IMAGE}:latest"
else
  echo "==> built ${IMAGE}:${TAG_SHA} (push skipped)"
fi
