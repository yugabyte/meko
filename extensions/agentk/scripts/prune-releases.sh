#!/bin/sh
# Deletes GitHub releases beyond the newest $KEEP (default 5) in each lane.
#
# A lane is the version's first prerelease identifier (v0.1.2-beta.3 → beta), "prerelease"
# for a numeric one or a prerelease without one, else "stable". Drafts are left alone. Tags are
# kept, so release-notes.sh and next-version.sh still see every release.
#
#   scripts/prune-releases.sh [--dry-run]
#
# Environment: GITHUB_REPOSITORY (owner/name), KEEP, GH_TOKEN.
set -eu
repo=${GITHUB_REPOSITORY:?set GITHUB_REPOSITORY to owner/name}
keep=${KEEP:-5}
dry=false
[ "${1:-}" = --dry-run ] && dry=true

# shellcheck disable=SC2016 # the single-quoted text is jq, not shell
gh release list --repo "$repo" --limit 1000 --exclude-drafts --json tagName,isPrerelease,createdAt --jq '
  sort_by(.createdAt) | reverse | .[]
  | (.tagName | sub("\\+.*$"; "") | capture("^v?[0-9]+\\.[0-9]+\\.[0-9]+(-(?<pre>[^.]+))?").pre) as $pre
  | [.tagName, (if $pre and ($pre | test("^[0-9]+$") | not) then $pre elif $pre or .isPrerelease then "prerelease" else "stable" end)]
  | @tsv' |
  awk -F '\t' -v keep="$keep" '++n[$2] > keep { print $1 }' |
  while read -r tag; do
    if $dry; then
      echo "would delete $tag"
    else
      echo "deleting $tag"
      gh release delete "$tag" --repo "$repo" --yes
    fi
  done
