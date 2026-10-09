#!/usr/bin/env bash
#
# Publish the yb-amp-controller chart as packaged .tgz files and prove the agent
# could pull them.
#
# The `controller` desired-state kind helm-upgrades the release the agent runs
# in, which needs a chart reference the agent can actually resolve. The default
# (`oci://quay.io/yugabyte/charts/yb-amp-controller`, published by
# amp/deploy/helm/publish-controller-chart.sh) needs Quay credentials the agent
# does not carry unless that repository is public. This publishes to the dev
# uploads bucket instead, which is served over plain https, and CONTROLLER_CHART
# points the agent at it.
#
# It is a TEST, not a publish script: every step asserts, and the last two
# assertions are the ones that matter — that `helm show chart` can read what was
# uploaded over plain https, and that `helm template` renders it. A .tgz that
# uploads but does not resolve is the failure this exists to catch.
#
# Two versions are published because one proves nothing: the kind's whole job is
# moving a release from one chart version to another, so the test publishes a
# pair and asserts they are independently resolvable.
#
# Usage:
#   AWS_PROFILE=yugadev ycp/test/local/controller-chart-publish.sh
#   CHART_VERSIONS="0.1.0 0.2.0" ycp/test/local/controller-chart-publish.sh
#
# Needs: helm, aws (profile with write access to the bucket), curl.
set -uo pipefail

BUCKET="${BUCKET:-uploads.dev.yugabyte.com}"
PREFIX="${PREFIX:-dshubin/amp-controller-chart}"
BASE_URL="${BASE_URL:-https://${BUCKET}/${PREFIX}}"
# Unique per run, because the bucket is fronted by CloudFront: re-uploading a key
# does NOT change what is served for it, so a chart republished under a version it
# has already used serves the OLD bytes for as long as the cache holds them. An
# agent pulling that reference would deploy the previous chart while every local
# check said it had the new one.
#
# Chart versions are meant to be immutable anyway; this just makes the tooling
# obey that instead of relying on nobody re-running it. Pass CHART_VERSIONS to pin
# them, and pick versions you have not published before.
_v_base="${_v_base:-$(date -u +0.%y%m%d.%H%M)}"
CHART_VERSIONS="${CHART_VERSIONS:-${_v_base}1 ${_v_base}2}"
export AWS_PROFILE="${AWS_PROFILE:-yugadev}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
CHART_DIR="${REPO_ROOT}/amp/deploy/helm/yb-amp-controller"

pass=0 fail=0
ok() {
  printf '  \033[32mok\033[0m   %s\n' "$*"
  pass=$((pass + 1))
}
bad() {
  printf '  \033[31mFAIL\033[0m %s\n' "$*"
  fail=$((fail + 1))
}
hdr() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

hdr "preconditions"
for bin in helm aws curl; do
  command -v "$bin" >/dev/null && ok "$bin present" || {
    bad "$bin missing"
    exit 1
  }
done
[ -f "${CHART_DIR}/Chart.yaml" ] && ok "chart at ${CHART_DIR}" || {
  bad "no chart at ${CHART_DIR}"
  exit 1
}
aws sts get-caller-identity >/dev/null 2>&1 &&
  ok "aws credentials resolve (AWS_PROFILE=${AWS_PROFILE})" ||
  {
    bad "aws credentials do not resolve for AWS_PROFILE=${AWS_PROFILE}"
    exit 1
  }

# helm lint first: publishing a chart that does not render wastes the round trip,
# and a broken chart reaches the agent as an upgrade that fails on the cluster.
hdr "lint"
if helm lint "${CHART_DIR}" >"${WORK}/lint.log" 2>&1; then
  ok "helm lint clean"
else
  bad "helm lint failed"
  sed 's/^/    /' "${WORK}/lint.log"
fi

published=()
for v in ${CHART_VERSIONS}; do
  hdr "package + publish ${v}"
  # --version and --app-version together: the chart version is what `helm upgrade
  # --version` selects, and appVersion is the image tag the chart falls back to,
  # so a package whose two disagree deploys something other than it claims.
  if helm package "${CHART_DIR}" --version "${v}" --app-version "${v}" \
    --destination "${WORK}" >"${WORK}/pkg-${v}.log" 2>&1; then
    ok "packaged ${v}"
  else
    bad "helm package ${v} failed"
    sed 's/^/    /' "${WORK}/pkg-${v}.log"
    continue
  fi

  tgz="${WORK}/yb-amp-controller-${v}.tgz"
  [ -s "${tgz}" ] && ok "$(basename "${tgz}") is non-empty" || {
    bad "no tarball for ${v}"
    continue
  }

  # text/plain would make some clients treat it as text; helm does not care, but a
  # browser download of a mislabelled .tgz is a confusing way to find out.
  if aws s3 cp "${tgz}" "s3://${BUCKET}/${PREFIX}/" \
    --content-type application/gzip >"${WORK}/up-${v}.log" 2>&1; then
    ok "uploaded to s3://${BUCKET}/${PREFIX}/"
  else
    bad "upload of ${v} failed"
    sed 's/^/    /' "${WORK}/up-${v}.log"
    continue
  fi
  published+=("${v}")
done

for v in "${published[@]}"; do
  url="${BASE_URL}/yb-amp-controller-${v}.tgz"
  hdr "resolve ${v} over https"

  code=$(curl -s -o "${WORK}/dl-${v}.tgz" -w '%{http_code}' "${url}")
  if [ "${code}" = 200 ]; then
    ok "GET ${url} -> 200"
  else
    bad "GET ${url} -> ${code}; the bucket path is not publicly readable"
    continue
  fi
  if cmp -s "${WORK}/yb-amp-controller-${v}.tgz" "${WORK}/dl-${v}.tgz"; then
    ok "downloaded bytes match what was packaged"
  else
    # Almost always a cache hit on a version published before: CloudFront keeps
    # serving the first bytes uploaded under a key. Publish a version you have not
    # used rather than waiting it out.
    age=$(curl -sI "${url}" | awk 'tolower($1)=="age:"{print $2}' | tr -d '\r')
    bad "downloaded bytes differ from the package (cloudfront age=${age:-?}s — this version was published before)"
  fi

  # The assertions that matter: helm itself resolving the URL, not just curl.
  if helm show chart "${url}" >"${WORK}/show-${v}.txt" 2>&1; then
    got=$(awk '/^version:/{print $2}' "${WORK}/show-${v}.txt")
    [ "${got}" = "${v}" ] && ok "helm show chart reports version ${got}" ||
      bad "helm show chart reports ${got}, want ${v}"
  else
    bad "helm show chart ${url} failed"
    sed 's/^/    /' "${WORK}/show-${v}.txt"
  fi

  if helm template rel "${url}" --set agent.enabled=true --set agent.envName=e \
    --set agent.ycpUrl=https://ycp.example.com/api/v1 \
    >"${WORK}/tpl-${v}.yaml" 2>&1; then
    ok "helm template renders from the URL"
  else
    bad "helm template from ${url} failed"
    sed 's/^/    /' "${WORK}/tpl-${v}.yaml"
  fi
done

# What the agent is actually configured with. A chart reference that resolves for
# `helm show` but not for `helm upgrade --version` would strand the kind, and the
# two take the reference differently: a packaged .tgz URL is already versioned, so
# the agent must NOT also pass --version.
hdr "the reference the agent would use"
if [ ${#published[@]} -gt 0 ]; then
  first="${published[0]}"
  echo "    CONTROLLER_CHART=${BASE_URL}/yb-amp-controller-${first}.tgz"
  if helm show values "${BASE_URL}/yb-amp-controller-${first}.tgz" >/dev/null 2>&1; then
    ok "a .tgz URL is a usable chart reference on its own"
  else
    bad "the .tgz URL is not usable as a chart reference"
  fi
fi

hdr "result"
printf '  %d passed, %d failed\n' "${pass}" "${fail}"
[ "${fail}" -eq 0 ] || exit 1
