#!/usr/bin/env bash
#
# Publish a foundation chart to oci://quay.io/yugabyte/charts, so a consumer can
# install it without a checkout of this private repo — the same gap the image
# closed for the binary.
#
# Modelled on amp/deploy/helm/publish-controller-chart.sh, with one difference
# that matters. Amp's chart carries its own version and its appVersion is
# allowed to go stale, because every amp deploy passes `--set imageTag`. The
# foundation chart is installed BARE — the image tag falls back to appVersion —
# so the chart and the image have to be one number, published from one run.
# That number is scripts/version.sh, and this script stamps it onto both fields
# rather than reading a version out of Chart.yaml.
#
# It also refuses to publish a chart whose image is not in the registry. A
# published chart that names a missing tag installs cleanly and then sits in
# ImagePullBackOff, which is the exact failure the appVersion comment in
# Chart.yaml records.
#
# Usage:
#   QUAY_TOKEN=user:token publish-chart.sh <chart-dir>      # from a clean main
#   publish-chart.sh --dry-run <chart-dir>                   # package only, anywhere
#
# Needs: helm, git, python3.
set -euo pipefail

REGISTRY="${REGISTRY:-oci://quay.io/yugabyte/charts}"
IMAGE="${IMAGE:-quay.io/yugabyte/meko-foundation}"
DRY_RUN=0

args=()
for a in "$@"; do
  case "$a" in
  --dry-run) DRY_RUN=1 ;;
  *) args+=("$a") ;;
  esac
done
[ "${#args[@]}" -eq 1 ] || {
  echo "usage: publish-chart.sh [--dry-run] <chart-dir>" >&2
  exit 2
}

CHART_DIR="$(cd "${args[0]}" && pwd)"
CHART="$(basename "${CHART_DIR}")"
HELM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERSION_SH="${HELM_DIR}/../../scripts/version.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

version="$("${VERSION_SH}" version)"
yba="$("${VERSION_SH}" yba)"

# The CRD chart is NOT versioned with the operator. It is cluster-scoped and
# shared, so a new version has to mean "the schema changed" — bumping it on
# every operator commit would leave a cluster admin unable to tell a real
# schema change from noise. Its Chart.yaml version is the authority, hand-bumped
# with the schema, and publishing refuses when that version already exists,
# which is what forces the bump.
crd_chart=0
[ "${CHART}" = "meko-foundation-crd" ] && crd_chart=1
if ((crd_chart)); then
  version="$(python3 -c 'import sys,yaml; print(yaml.safe_load(open(sys.argv[1]))["version"])' \
    "${CHART_DIR}/Chart.yaml")"
fi

if ((!DRY_RUN)); then
  [ -z "$(git -C "${HELM_DIR}" status --porcelain)" ] ||
    {
      echo "refusing to publish a dirty tree" >&2
      exit 1
    }
  # CI checks out main detached, so GITHUB_REF is what names the branch there.
  branch="${GITHUB_REF:-refs/heads/$(git -C "${HELM_DIR}" branch --show-current)}"
  [ "${branch}" = "refs/heads/main" ] ||
    {
      echo "refusing to publish from ${branch}; only main publishes" >&2
      exit 1
    }
fi

token="${QUAY_TOKEN:-${QUAY_AMP_TOKEN:-}}"
if ((!DRY_RUN)) && [ -n "${token}" ]; then
  case "${token}" in *:*) ;; *)
    echo "QUAY_TOKEN must be user:token" >&2
    exit 1
    ;;
  esac
  printf '%s' "${token#*:}" | helm registry login quay.io -u "${token%%:*}" --password-stdin
fi

# Exit codes: 0 published, 3 nothing to publish (this version already exists),
# anything else a real failure. See the already-published branch below.
#
# 0 if the tag exists, 1 if the registry says it does not. Anything else aborts:
# Quay answers 401 for both "no access" and "no such repo", so treating a failed
# lookup as absent would re-push a published version — and OCI tags are mutable,
# so a version must mean one set of bytes.
chart_exists() {
  local out
  if out="$(helm show chart "${REGISTRY}/${CHART}" --version "$1" 2>&1)"; then return 0; fi
  grep -qiE 'not found|manifest unknown' <<<"${out}" && return 1
  echo "cannot tell whether ${CHART}:$1 exists: ${out}" >&2
  exit 1
}

# The chart is useless without the image it names, and the failure is silent:
# helm installs happily and the pod never starts.
require_image() {
  if command -v skopeo >/dev/null 2>&1; then
    skopeo inspect --no-tags "docker://${IMAGE}:$1" >/dev/null 2>&1 && return 0
  elif command -v docker >/dev/null 2>&1; then
    docker manifest inspect "${IMAGE}:$1" >/dev/null 2>&1 && return 0
  else
    echo "::warning::neither skopeo nor docker is available; not verifying ${IMAGE}:$1 exists" >&2
    return 0
  fi
  echo "refusing to publish ${CHART} ${version}: its image ${IMAGE}:$1 is not in the registry. " \
    "A chart naming a missing tag installs cleanly and then sits in ImagePullBackOff." >&2
  exit 1
}

if ((DRY_RUN)); then
  echo "dry-run: would publish ${CHART} ${version}"
elif chart_exists "${version}"; then
  # Exit 3 means exactly this and nothing else, so a caller can tolerate "no
  # new version to publish" without also swallowing a missing repository, an
  # ambiguous 401 or a failed push. A bare `|| true` around this script hid all
  # of those behind the same green line.
  echo "${CHART} ${version} is already published; nothing to do"
  exit 3
fi

sets=(--version "${version}")
if ((!crd_chart)); then
  ((DRY_RUN)) || require_image "${version}"
  sets+=(--app-version "${version}")
fi

helm package "${CHART_DIR}" "${sets[@]}" -d "${WORK}" >/dev/null
pkg="${WORK}/${CHART}-${version}.tgz"

if ((DRY_RUN)); then
  echo "dry-run: packaged ${pkg##*/} (tested-with-yba ${yba}); not pushing"
else
  helm push "${pkg}" "${REGISTRY}"
  echo "published ${CHART} ${version} to ${REGISTRY} (tested-with-yba ${yba})"
fi
