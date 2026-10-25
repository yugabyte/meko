#!/bin/sh
# Release notes for one product from commit subjects (<type>(<component>[/<area>])[!]: <summary>,
# see scripts/check-title.sh; older ones may have no component) since its previous release tag.
# Only the product's own components count (product.sh): AgentK's notes skip subjects naming only
# meko or presenter, and Meko's take every subject naming meko, alone or in a list
# (feat(agentk,meko): ...), so such a subject is in both.
#
#   scripts/release/release-notes.sh [--product agentk|meko] [<from-ref>]
#                                    # default product agentk, from its latest tag
#
# Also used to decide whether a push is worth releasing: exits 3 when there
# are no feat/fix/perf/deps commits for the product in the range. The other types don't
# release, not even with a breaking-change marker.
set -eu
. "$(dirname "$0")/product.sh"

# The types that release; each one has a section below.
RELEASE='feat|fix|perf|deps'

from=${1:-$(newest_tag)}
range=${from:+$from..}HEAD

# The subjects on stdin (one per line) of types $1 (a regex) that release this product.
own() {
  grep -E "^($1)$scope!?: " | { if [ -n "$exclude" ]; then grep -vE "^($1)$exclude!?: "; else cat; fi; } || true
}

section() {
  # $1 = heading, $2 = type regex
  lines=$(git log --no-merges --format='%s (%h)' "$range" | own "$2")
  [ -n "$lines" ] || return 0
  printf '### %s\n\n' "$1"
  printf '%s\n' "$lines" | sed -E "s/^($2)(\(([^)]*)\))?!?: /- \3: /; s/^- : /- /"
  printf '\n'
}

notes=$(
  # A `!` subject or a BREAKING CHANGE: footer, from releasing commits only.
  breaking=$(
    for h in $(git log --no-merges --format=%H "$range"); do
      s=$(git log -1 --format=%s "$h")
      [ -n "$(printf '%s\n' "$s" | own "$RELEASE")" ] || continue
      printf '%s\n' "$s" | grep -E "^($RELEASE)(\([^)]*\))?!: " || true
      git log -1 --format=%b "$h" | grep -E '^BREAKING CHANGE: ' || true
    done
  )
  if [ -n "$breaking" ]; then
    printf '### Breaking changes\n\n'
    printf '%s\n' "$breaking" | sed 's/^/- /'
    printf '\n'
  fi
  section 'Features' 'feat'
  section 'Fixes' 'fix'
  section 'Performance' 'perf'
  section 'Dependencies' 'deps'
)

if [ -z "$notes" ]; then
  echo "No user-facing changes since ${from:-the first commit}." >&2
  exit 3
fi
printf '%s\n' "$notes"
if [ -n "$from" ]; then
  printf '\n**Changes since %s:** %s\n' "$from" "$(git log --no-merges --oneline "$range" | wc -l | tr -d ' ') commits"
fi
