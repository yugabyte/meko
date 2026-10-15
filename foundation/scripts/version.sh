#!/usr/bin/env bash
#
# version.sh — the foundation's version, derived rather than declared.
#
# The version is CalVer: MAJOR.YYMMDD.PATCH, e.g. 0.261014.2
#   - MAJOR  — 0, bumped only for a deliberate epoch change.
#   - YYMMDD — the UTC date of the HEAD commit (occupies the SemVer minor slot,
#              so the whole string still parses and sorts as SemVer 2).
#   - PATCH  — how many commits landed earlier that same UTC day, so the day's
#              first build is .0 and each later one counts up. EVERY commit, not
#              only foundation ones — see head_patch.
#
# There is NO VERSION file and nothing to bump. That is deliberate: amp has the
# same scheme behind amp/VERSION and the file has TWO commits in its whole
# history — it was set in July and never moved. A number that depends on
# somebody remembering to bump it goes stale, and a stale version that names an
# image tag is the bug this scheme exists to prevent.
#
# Everything here is a pure function of the commit, so a re-run of the same
# commit produces the same version — which is what makes "never re-push an
# existing version" a safe rule rather than a flaky one.
#
# Usage:
#   version.sh version      # 0.261014.2 — the chart version AND the image tag
#   version.sh yba          # 2026.1.3.0-b21 — the YBA build the spec is qualified with
#
# Needs: git, python3.
#
# The count needs real history. A depth-1 checkout sees one commit and returns
# .0 for every build of the day, which is why build-image.sh gates the release
# tags behind --release rather than deriving them for any caller.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MAJOR="${MAJOR:-0}"
IMAGE="${IMAGE:-quay.io/yugabyte/meko-foundation}"

# The HEAD commit's own UTC date, not today's: the version then describes the
# commit rather than when CI happened to run it, so a dispatch days later
# reproduces the same number instead of inventing a new one.
head_day() { TZ=UTC git -C "${ROOT}" show -s --date=format-local:%y%m%d --format=%cd HEAD; }

# Commits reachable from HEAD that landed on the same UTC day. HEAD is always
# one of them, so the count is at least 1 and the day's first build gets .0.
#
# Deliberately NOT filtered to foundation/. A filtered log omits a HEAD that
# touches nothing there — and this workflow fires on its own file and on
# workflow_dispatch — so the leading run belonged to some earlier commit and
# HEAD inherited a patch that was already published, under an image tag that
# was then overwritten. Counting every commit makes the patch a strictly
# increasing function of position in the day's history, which is the only
# property it needs.
head_patch() {
  local day
  day="$(head_day)"
  TZ=UTC git -C "${ROOT}" log --date=format-local:%y%m%d --format='%cd' HEAD |
    awk -v d="${day}" '$1 != d { exit } { c++ } END { print (c > 0 ? c - 1 : 0) }'
}

version() { echo "${MAJOR}.$(head_day).$(head_patch)"; }

# The YBA build the examples pin, which is what the chart's tested-with
# annotation and the yba-* image alias report. Read from the example rather
# than declared here, so there is one place to edit when the build moves — and
# TestTheAnnotationMatchesTheExample fails if the two drift.
yba() {
  python3 - "${ROOT}/examples/foundation-cr.yaml" <<'PY'
import sys, yaml
spec = yaml.safe_load(open(sys.argv[1]))["spec"]
print(spec["chartVersion"])
PY
}

case "${1:-version}" in
version) version ;;
yba) yba ;;
*)
  echo "usage: version.sh [version|yba]" >&2
  exit 2
  ;;
esac
