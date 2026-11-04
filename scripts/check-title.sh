#!/bin/sh
# Checks a commit subject or PR title against the repo's title rules:
#
#   <type>(<component>[/<area>][,<component>[/<area>]...])[!]: <summary>
#
#   scripts/check-title.sh [--no-length] "<title>"
#
# PRs are squash-merged with the PR title as the commit subject, and
# scripts/release/release-notes.sh reads the type to decide whether to release, so a
# PR title follows the same rules. --no-length skips the length limit
# (Dependabot's subjects run long). Exits 0 when the title is valid, 1 with
# the reason when it isn't, 2 on bad usage.
set -eu

# The types, and the ones that release (RELEASE in scripts/release/release-notes.sh, kept equal by
# test/unit/check-title.test.ts). Only a releasing type can be marked breaking with `!`.
TYPES='feat|fix|perf|deps|ci|test|docs|nit|chore'
RELEASE='feat|fix|perf|deps'
# The components a title names: agentk (src/extensions/agentk), meko (Meko, in src/meko),
# presenter (src/extensions/presenter) and repo (the shared root), with an optional area after a
# slash. A change that ships in more than one names each, comma-separated (feat(agentk,meko): ...),
# and releases every product it names (scripts/release/product.sh). Each but repo is a component of
# scripts/ci-scope.sh too (test/unit/check-title.test.ts checks).
COMPONENTS='agentk|meko|presenter|repo'
MAX=72

length=true
if [ "${1:-}" = "--no-length" ]; then
  length=false
  shift
fi
if [ $# -ne 1 ]; then
  echo "usage: $0 [--no-length] <title>" >&2
  exit 2
fi
title=$1

list() { printf '%s' "$1" | sed 's/|/, /g'; }

fail() {
  {
    printf 'Bad title: %s\n  %s\n\n' "$title" "$1"
    printf 'Format: <type>(<component>[/<area>])[!]: <summary>\n'
    printf '  or, for a change in more than one: <type>(<component>[/<area>],<component>[/<area>])[!]: <summary>\n'
    printf 'Types: %s (feat, fix, perf and deps release)\n' "$(list "$TYPES")"
    printf 'Components: %s\n' "$(list "$COMPONENTS")"
    printf 'Area: optional, lowercase letters, digits and dashes, e.g. fix(agentk/web): ...\n'
  } >&2
  exit 1
}

case $title in
  *'
'*) fail 'A title is one line.' ;;
esac

printf '%s\n' "$title" | grep -qE '^[^:]+: ' ||
  fail 'Start with "<type>(<component>): " (a colon, then a space).'

head=${title%%: *}
summary=${title#*: }
type=${head%%[(!]*}
printf '%s\n' "$type" | grep -qE "^($TYPES)\$" ||
  fail "Unknown type \"$type\". Types are lowercase and one of the list below."

rest=${head#"$type"}
case $rest in
  '' | '!') fail "Name the component in parentheses after the type: one of $(list "$COMPONENTS")." ;;
esac
printf '%s\n' "$rest" | grep -qE '^\([^()]*\)!?$' ||
  fail "Bad component \"$rest\". Put it in parentheses right after the type, then an optional !."
scope=${rest#(}
scope=${scope%%)*}
case $scope in
  *,*) case $scope in
      ,* | *, | *,,* | *' '*) fail "Bad component list \"$scope\". Separate the components with a comma and no spaces, e.g. feat(agentk,meko): ..." ;;
    esac ;;
esac
# Each comma-separated part is <component>[/<area>], and names a component once.
parts=$scope,
seen=,
while [ -n "$parts" ]; do
  part=${parts%%,*}
  parts=${parts#*,}
  component=${part%%/*}
  printf '%s\n' "$component" | grep -qE "^($COMPONENTS)\$" ||
    fail "Unknown component \"$component\". Use one of the list below, with an optional area after a slash."
  case $seen in
    *",$component,"*) fail "Component \"$component\" is named twice. Name each component once, with at most one area." ;;
  esac
  seen=$seen$component,
  case $part in
    */*) area=${part#*/}
      printf '%s\n' "$area" | grep -qE '^[a-z0-9-]+$' ||
        fail "Bad area \"$area\". Use one level of lowercase letters, digits and dashes." ;;
  esac
done
case $rest in
  *!) printf '%s\n' "$type" | grep -qE "^($RELEASE)\$" ||
    fail "\"$type\" doesn't release, so it can't be breaking (!). Breaking changes ship: use feat, fix, perf or deps." ;;
esac

# The PR number GitHub appends on a squash merge doesn't count.
bare=$(printf '%s\n' "$summary" | sed -E 's/ ?\(#[0-9]+\)$//')
printf '%s\n' "$bare" | grep -qE '^[^[:space:]]' || fail 'The summary is empty or starts with a space.'
case $bare in
  *.) fail 'The summary ends with a period.' ;;
  *[[:space:]]) fail 'The summary ends with a space.' ;;
esac

if $length; then
  # Characters, not bytes, whatever the shell and locale: drop UTF-8 continuation bytes.
  n=$(printf '%s' "$head: $bare" | LC_ALL=C tr -d '\200-\277' | wc -c | tr -d ' ')
  [ "$n" -le "$MAX" ] || fail "The title is $n characters; keep it to $MAX."
fi
