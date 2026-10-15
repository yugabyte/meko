#!/usr/bin/env bash
#
# Build the foundation image and (optionally) push it to Quay, so a cluster can
# run the deployer without a checkout of this private repo. Wrapped by
# .github/workflows/foundation-image.yml on main; stays runnable locally.
#
# Usage:
#   foundation/build-image.sh                        # build + push custom-<sha>
#   foundation/build-image.sh --no-push              # build only (no credentials)
#   foundation/build-image.sh --release              # also the version tags
#   foundation/build-image.sh --release --floating   # also :latest and :yba-*
#
# Credentials (push only), in precedence order:
#   QUAY_USER + QUAY_TOKEN   — a Quay robot account name and its token
#   QUAY_TOKEN as "user:token"
#   QUAY_AMP_TOKEN           — same as QUAY_TOKEN, alternate name
#
# Overridable via env: IMAGE (default quay.io/yugabyte/meko-foundation),
# PLATFORM (default linux/amd64 — the GKE/EKS clusters are amd64), DOCKER, and
# SHA (default the checkout's short HEAD) for the tag.
#
# Tags:
#
#   custom-<sha>        ALWAYS. Immutable, and what every non-publishing caller
#                       needs — the GKE lane pins it.
#
# --release adds, and ONLY the publishing workflow may pass it:
#
#   <version>           THE tag to pin, and the one the published chart names
#                       through appVersion (scripts/version.sh).
#   <version>-<sha>     The same build, carrying the commit.
#
# --floating adds, and only a build of main may pass it:
#
#   latest              What a checkout install resolves to.
#   yba-<build>         Alias for the newest build qualified against that YBA.
#                       Answers "which image goes with this YBA"; never pin it.
#
# --release is a gate, not a convenience. The version is derived from the
# commit, so ANY caller computes the same string — and the GKE lane builds this
# image on every `/ci amp_deploy_gke_foundation` PR, with push credentials, at
# the default checkout depth of 1. Ungated, that overwrote the <version> a
# published chart names, so installing the chart ran a PR branch's code. The
# tag is only immutable if one workflow may write it, which is what this gate
# means; a registry check below is the belt to its braces.
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${IMAGE:-quay.io/yugabyte/meko-foundation}"
PLATFORM="${PLATFORM:-linux/amd64}"
DOCKER="${DOCKER:-docker}"
PUSH=1
RELEASE=0
FLOATING=0

# Print the whole leading comment block, so growing the header cannot silently
# truncate --help the way a fixed line range did.
usage() { awk 'NR>1 && /^#/ { sub(/^# ?/, ""); print; next } NR>1 { exit }' "$0"; }

for arg in "$@"; do
  case "$arg" in
  --no-push) PUSH=0 ;;
  --release) RELEASE=1 ;;
  --floating) FLOATING=1 ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    echo "unknown argument: $arg" >&2
    usage >&2
    exit 2
    ;;
  esac
done

# Overridable, because a GitHub checkout's HEAD is the merge commit rather
# than the PR head, and a caller that has to REFERENCE this tag later (a
# chart's image.tag) must compute it the same way the push did.
SHA="${SHA:-$(git -C "$HERE" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
TAG_SHA="custom-${SHA}"

[ "$FLOATING" = "1" ] && [ "$RELEASE" = "0" ] && {
  echo "--floating needs --release: latest and yba-* point at a release build" >&2
  exit 2
}

TAGS=("${TAG_SHA}")
if [ "$RELEASE" = "1" ]; then
  # Only ever computed under --release. version.sh counts the day's earlier
  # commits, so it needs real history — a depth-1 checkout returns .0 for every
  # build of the day, and the publishing workflow sets fetch-depth: 0.
  VERSION="$(IMAGE="$IMAGE" "$HERE/scripts/version.sh" version)"
  TAGS+=("${VERSION}" "${VERSION}-${SHA}")
  if [ "$FLOATING" = "1" ]; then
    TAGS+=(latest "yba-$(IMAGE="$IMAGE" "$HERE/scripts/version.sh" yba)")
  fi
fi

echo "==> building ${IMAGE} for ${PLATFORM}, tagged:"
printf '      %s\n' "${TAGS[@]}"

if [ "$PUSH" = "1" ]; then
  token="${QUAY_TOKEN:-${QUAY_AMP_TOKEN:-}}"
  if [ -z "$token" ]; then
    echo "no Quay credentials: set QUAY_USER+QUAY_TOKEN (or QUAY_TOKEN=user:token), or pass --no-push" >&2
    exit 1
  fi
  if [ -n "${QUAY_USER:-}" ]; then
    quay_user="$QUAY_USER"
    quay_pass="$token"
  elif printf '%s' "$token" | grep -q ':'; then
    quay_user="${token%%:*}"
    quay_pass="${token#*:}"
  else
    echo "QUAY_TOKEN has no username: set QUAY_USER, or give QUAY_TOKEN as user:token" >&2
    exit 1
  fi
  echo "==> logging in to quay.io as ${quay_user}"
  printf '%s' "$quay_pass" | "$DOCKER" login quay.io -u "$quay_user" --password-stdin
fi

# The gate above is a convention; this is the check. A re-run, a dispatch, or
# any caller that acquires --release must not silently replace the bytes a
# published chart's appVersion already points at. Absent tooling warns rather
# than blocks, so a runner without either tool cannot wedge the publish.
if [ "$PUSH" = "1" ] && [ "$RELEASE" = "1" ]; then
  exists=""
  if command -v skopeo >/dev/null 2>&1; then
    skopeo inspect --no-tags "docker://${IMAGE}:${VERSION}" >/dev/null 2>&1 && exists=1
  elif command -v "$DOCKER" >/dev/null 2>&1; then
    "$DOCKER" manifest inspect "${IMAGE}:${VERSION}" >/dev/null 2>&1 && exists=1
  else
    echo "==> neither skopeo nor ${DOCKER} available; not checking whether ${VERSION} exists"
  fi
  if [ -n "$exists" ]; then
    echo "refusing to re-push ${IMAGE}:${VERSION}: it is already in the registry, and a" >&2
    echo "published chart's appVersion names it. Delete that tag, or land a new commit." >&2
    exit 1
  fi
fi

build_args=(buildx build --platform "$PLATFORM" -f "$HERE/Dockerfile")
for t in "${TAGS[@]}"; do
  build_args+=(-t "${IMAGE}:${t}")
done
if [ "$PUSH" = "1" ]; then
  build_args+=(--push)
fi
build_args+=("$HERE")

"$DOCKER" "${build_args[@]}"

if [ "$PUSH" = "1" ]; then
  echo "==> pushed ${IMAGE} as: ${TAGS[*]}"
else
  echo "==> built ${IMAGE} as: ${TAGS[*]} (push skipped)"
fi
