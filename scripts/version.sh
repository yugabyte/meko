#!/usr/bin/env bash
#
# version.sh — single source of truth for the amp controller's version.
#
# The version is a hybrid CalVer/SemVer: "MAJOR.YYMMDD.PATCH" (e.g. "0.260714.0")
#   - MAJOR  — kept at 0 for now; bumped only for a deliberate epoch change.
#   - YYMMDD — the release date (the CalVer component; occupies the SemVer minor
#              slot, so it still parses/sorts as a valid SemVer version).
#   - PATCH  — a same-day counter: 0 for the day's first release, +1 per re-cut.
#
# The repo-root VERSION file (e.g. "0.260714.0") drives everything:
#   - the controller binary (stamped via -ldflags; see the Makefile),
#   - the container image tag ("<VERSION>-<short-commit>"),
#   - the yb-amp-controller Helm chart (version + appVersion).
#
# The VERSION file holds only the base MAJOR.YYMMDD.PATCH; the binary version
# and image tag append "-<short-commit>" (plus "-dirty" for an unclean local
# tree) so every build is uniquely identifiable and traceable to its exact
# commit — e.g. 0.260722.0-1bc8b3d. RELEASE=1 asserts a clean tree (omits the
# "-dirty" marker; the commit id is always present).
#
# Usage:
#   scripts/version.sh                 # base version from VERSION (e.g. 0.260722.0)
#   scripts/version.sh binary-version  # -ldflags version, e.g. 0.260722.0-1bc8b3d
#   scripts/version.sh image-tag       # image tag, e.g. 0.260722.0-1bc8b3d
#   scripts/version.sh sync            # stamp Chart.yaml from VERSION
#   scripts/version.sh bump <part>     # bump date|patch|major in VERSION, then sync
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION_FILE="${ROOT}/VERSION"
CHART="${ROOT}/deploy/helm/yb-amp-controller/Chart.yaml"

read_version() {
  local v
  v="$(tr -d '[:space:]' < "${VERSION_FILE}")"
  # MAJOR.YYMMDD.PATCH — still a valid three-part SemVer; the middle field is a
  # CalVer date rather than a monotonic minor.
  [[ "${v}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "VERSION is not MAJOR.YYMMDD.PATCH: '${v}'" >&2; exit 1; }
  printf '%s' "${v}"
}

# sync Chart.yaml version + appVersion to the VERSION file (in-place, portable sed).
sync_chart() {
  local v="$1" tmp
  tmp="$(mktemp)"
  awk -v v="${v}" '
    /^version:/       { print "version: " v; next }
    /^appVersion:/    { print "appVersion: \"" v "\""; next }
    { print }
  ' "${CHART}" > "${tmp}" && mv "${tmp}" "${CHART}"
  echo ">> Chart.yaml version/appVersion set to ${v}"
}

bump() {
  local part="$1" v major minor patch today
  v="$(read_version)"
  IFS='.' read -r major minor patch <<<"${v}"
  today="$(date +%y%m%d)"   # CalVer date component: YYMMDD (local date).
  case "${part}" in
    # "date" rolls the CalVer field to today and resets the same-day patch
    # counter — the normal way to cut a release. If VERSION is already dated
    # today, roll the patch instead so the version always moves forward.
    # "minor" is accepted as an alias since the date lives in the minor slot.
    date|minor) if [ "${minor}" = "${today}" ]; then patch=$((patch+1)); else minor="${today}"; patch=0; fi ;;
    # "patch" re-cuts the same date (e.g. a hotfix on top of an existing release).
    patch)      patch=$((patch+1)) ;;
    # "major" bumps the epoch and re-stamps the date; reserved for a deliberate reset.
    major)      major=$((major+1)); minor="${today}"; patch=0 ;;
    *) echo "bump part must be date|patch|major (minor is an alias for date)" >&2; exit 1 ;;
  esac
  local new="${major}.${minor}.${patch}"
  printf '%s\n' "${new}" > "${VERSION_FILE}"
  echo ">> VERSION ${v} -> ${new}"
  sync_chart "${new}"
}

# --- build identity ---------------------------------------------------------
# Every build appends "-<short-commit>" to the VERSION-file base, so the CI
# release version is "<VERSION>-<short-commit>" (e.g. 0.260722.0-1bc8b3d): the
# VERSION file carries only the base MAJOR.YYMMDD.PATCH, and the commit id
# (stamped here / by the Makefile) identifies the exact tree — dashes only, so
# it drops straight into an image tag / URL. A "-dirty" marker is added for an
# unclean local tree; RELEASE=1 asserts a clean build and omits it (CI trees
# are clean, so it is a no-op there).
_git_commit() { git -C "${ROOT}" rev-parse --short HEAD 2>/dev/null || echo unknown; }
_dirty()      { [ -n "$(git -C "${ROOT}" status --porcelain 2>/dev/null)" ] && printf -- '-dirty' || true; }
_commit_suffix() {
  local dirty=""
  [ -z "${RELEASE:-}" ] && dirty="$(_dirty)"
  printf -- '-%s%s' "$(_git_commit)" "${dirty}"
}
# lenient read (no strict-semver validation) for the tag/binary forms.
_base() { local v; v="$( { tr -d '[:space:]' < "${VERSION_FILE}"; } 2>/dev/null || true)"; printf '%s' "${v}"; }

# binary_version: "<VERSION>-<commit>[-dirty]" (no "v"), for -ldflags. e.g. 0.260722.0-1bc8b3d
binary_version() { local v; v="$(_base)"; [ -n "${v}" ] || v="0.0.0"; printf '%s%s' "${v}" "$(_commit_suffix)"; }
# image_tag: "<VERSION>-<commit>[-dirty]", or "latest" when VERSION is absent. e.g. 0.260722.0-1bc8b3d
image_tag() { local v; v="$(_base)"; [ -n "${v}" ] || { printf 'latest'; return; }; printf '%s%s' "${v}" "$(_commit_suffix)"; }

case "${1:-print}" in
  print)          read_version; echo ;;
  binary-version) binary_version; echo ;;
  image-tag)      image_tag; echo ;;
  sync)           sync_chart "$(read_version)" ;;
  bump)           bump "${2:-patch}" ;;
  *) echo "usage: version.sh [print|binary-version|image-tag|sync|bump <date|patch|major>]  (set RELEASE=1 for a clean release version)" >&2; exit 1 ;;
esac
