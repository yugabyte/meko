#!/bin/sh
# Deletes one product's GitHub releases beyond the newest $KEEP (default 5) in each lane. Other
# products' releases are never touched.
#
# A lane is the tag's prefix plus the version's first prerelease identifier (v0.1.2-beta.3 → beta),
# "prerelease" for a numeric one or a prerelease without one, else "stable". Drafts are left alone.
# Tags are kept, so release-notes.sh and next-version.sh still see every release. The newest stable
# release under a prefix the product no longer tags with (AgentK's v…) is never deleted: AgentKs
# installed before the switch update through it to the new tags.
#
#   scripts/release/prune-releases.sh [--product agentk|meko] [--dry-run]
#
# Environment: GITHUB_REPOSITORY (owner/name), KEEP, GH_TOKEN.
set -eu
. "$(dirname "$0")/product.sh"
repo=${GITHUB_REPOSITORY:?set GITHUB_REPOSITORY to owner/name}
keep=${KEEP:-5}
dry=false
[ "${1:-}" = --dry-run ] && dry=true

# The product's own tags: one of its prefixes, then a version.
prefixes=$(printf '%s' "$tag_prefixes" | sed 's/ /|/g')
# shellcheck disable=SC2016 # the single-quoted text is jq, not shell
gh release list --repo "$repo" --limit 1000 --exclude-drafts --json tagName,isPrerelease,createdAt --jq '
  sort_by(.createdAt) | reverse | .[]
  | select(.tagName | test("^('"$prefixes"')[0-9]"))
  | (.tagName | capture("^(?<prefix>'"$prefixes"')(?<version>[0-9]+\\.[0-9]+\\.[0-9]+.*)$")) as $t
  | ($t.version | sub("\\+.*$"; "") | capture("^[0-9]+\\.[0-9]+\\.[0-9]+(-(?<pre>[^.]+))?").pre) as $pre
  | [.tagName, $t.prefix + ":" + (if $pre and ($pre | test("^[0-9]+$") | not) then $pre elif $pre or .isPrerelease then "prerelease" else "stable" end)]
  | @tsv' |
  awk -F '\t' -v keep="$keep" -v current="$tag_prefix" '
    { split($2, lane, ":") }
    ++n[$2] > keep && !(lane[1] != current && lane[2] == "stable" && n[$2] == 1) { print $1 }' |
  while read -r tag; do
    if $dry; then
      echo "would delete $tag"
    else
      echo "deleting $tag"
      gh release delete "$tag" --repo "$repo" --yes
    fi
  done
