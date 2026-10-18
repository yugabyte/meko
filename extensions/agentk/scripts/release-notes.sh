#!/bin/sh
# Release notes from commit subjects (<type>[(<component>)][!]: <summary>, see
# scripts/check-title.sh) since the previous release tag.
#
#   scripts/release-notes.sh [<from-ref>]      # default: the latest v0.* tag
#
# Also used to decide whether a push is worth releasing: exits 3 when there
# are no feat/fix/perf/deps commits in the range. The other types don't
# release, not even with a breaking-change marker.
set -eu

# The types that release; each one has a section below.
RELEASE='feat|fix|perf|deps'

from=${1:-$(git tag -l 'v0.*' --sort=-v:refname | head -n 1)}
range=${from:+$from..}HEAD

section() {
  # $1 = heading, $2 = type regex
  lines=$(git log --no-merges --format='%s (%h)' "$range" | grep -E "^($2)(\([^)]*\))?!?: " || true)
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
      printf '%s\n' "$s" | grep -qE "^($RELEASE)(\([^)]*\))?!?: " || continue
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
