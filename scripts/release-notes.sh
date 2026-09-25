#!/bin/sh
# Release notes from Conventional Commit subjects since the previous release tag.
#
#   scripts/release-notes.sh [<from-ref>]      # default: the latest v0.* tag
#
# Also used to decide whether a push is worth releasing: exits 3 when there
# are no feat/fix/perf commits in the range.
set -eu

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
  breaking=$(git log --no-merges --format='%s%n%b' "$range" | grep -E '^[a-z]+(\([^)]*\))?!: |^BREAKING CHANGE: ' || true)
  if [ -n "$breaking" ]; then
    printf '### Breaking changes\n\n'
    printf '%s\n' "$breaking" | sed 's/^/- /'
    printf '\n'
  fi
  section 'Features' 'feat'
  section 'Fixes' 'fix'
  section 'Performance' 'perf'
)

if [ -z "$notes" ]; then
  echo "No user-facing changes since ${from:-the first commit}." >&2
  exit 3
fi
printf '%s\n' "$notes"
if [ -n "$from" ]; then
  printf '\n**Changes since %s:** %s\n' "$from" "$(git log --no-merges --oneline "$range" | wc -l | tr -d ' ') commits"
fi
