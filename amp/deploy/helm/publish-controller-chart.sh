#!/usr/bin/env bash
#
# Publish the yb-amp-controller chart to oci://quay.io/yugabyte/charts/yb-amp-controller,
# the reference the reconcile-agent defaults to (ycp/cmd/reconcile-agent/helm.go).
#
# Every run publishes a prerelease on the NEXT patch, <major>.<minor>.<patch+1>-<UTC
# yyyymmddHHMMSS>.g<sha>, because most chart commits do not bump the version. Next
# patch, not the current one: semver sorts a prerelease below its own release, so
# <version>-<ts> would sit under the plain <version> forever. When Chart.yaml's
# version has no tag yet, the plain version is published too. That is all "latest"
# needs: with no --version helm resolves the highest release, and with --devel the
# newest commit.
#
# OCI tags are mutable, so an existing version is never pushed again — a chart
# version must mean one set of bytes. That is also why a push needs a clean tree
# on main: a branch's bytes under a plain version would outlive the branch.
#
# Usage:
#   QUAY_TOKEN=user:token amp/deploy/helm/publish-controller-chart.sh   # from a clean main
#   amp/deploy/helm/publish-controller-chart.sh --dry-run                # package only, anywhere
#
# Needs: helm, yq (v4), git.
set -euo pipefail

REGISTRY="${REGISTRY:-oci://quay.io/yugabyte/charts}"
CHART=yb-amp-controller
DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART_DIR="${SCRIPT_DIR}/${CHART}"
WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

base="$(yq '.version' "${CHART_DIR}/Chart.yaml")"
[[ "${base}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] ||
  {
    echo "Chart.yaml version ${base} is not a plain major.minor.patch" >&2
    exit 1
  }
next="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((BASH_REMATCH[3] + 1))"
sha="$(git -C "${SCRIPT_DIR}" rev-parse --short=12 HEAD)"
pre="${next}-$(date -u +%Y%m%d%H%M%S).g${sha}"

if ((!DRY_RUN)); then
  [[ -z "$(git -C "${SCRIPT_DIR}" status --porcelain)" ]] ||
    {
      echo "refusing to publish a dirty tree" >&2
      exit 1
    }
  # CI checks out main detached, so GITHUB_REF is what names the branch there.
  branch="${GITHUB_REF:-refs/heads/$(git -C "${SCRIPT_DIR}" branch --show-current)}"
  [[ "${branch}" == refs/heads/main ]] ||
    {
      echo "refusing to publish from ${branch}; only main publishes" >&2
      exit 1
    }
fi

token="${QUAY_TOKEN:-${QUAY_AMP_TOKEN:-}}"
if ((!DRY_RUN)) && [[ -n "${token}" ]]; then
  [[ "${token}" == *:* ]] || {
    echo "QUAY_TOKEN must be user:token" >&2
    exit 1
  }
  printf '%s' "${token#*:}" | helm registry login quay.io -u "${token%%:*}" --password-stdin
fi

# 0 if the tag exists, 1 if the registry says it does not. Anything else (Quay
# answers 401 for both "no access" and "no such repo") aborts, since treating it
# as absent would re-push a published version.
tag_exists() {
  local out
  if out="$(helm show chart "${REGISTRY}/${CHART}" --version "$1" 2>&1)"; then return 0; fi
  grep -qiE 'not found|manifest unknown' <<<"${out}" && return 1
  echo "cannot tell whether ${CHART}:$1 exists: ${out}" >&2
  exit 1
}

versions=("${pre}")
if ((DRY_RUN)); then
  versions+=("${base}")
elif ! tag_exists "${base}"; then
  versions+=("${base}")
fi

for v in "${versions[@]}"; do
  helm package "${CHART_DIR}" --version "${v}" -d "${WORK}" >/dev/null
  if ((DRY_RUN)); then
    echo "dry-run: would push ${CHART}-${v}.tgz to ${REGISTRY}"
  else
    helm push "${WORK}/${CHART}-${v}.tgz" "${REGISTRY}"
  fi
done

echo "published ${versions[*]} to ${REGISTRY}/${CHART}"
