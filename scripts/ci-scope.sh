#!/bin/sh
# Says which components' CI jobs a change needs, for .github/workflows/ci.yml:
#
#   git diff --name-only --no-renames <base> <head> > changed
#   scripts/ci-scope.sh < changed
#   scripts/ci-scope.sh --all
#
# Not through a pipe: without pipefail a failed diff would read as no changes.
# Reads changed paths, one per line, and prints `agentk=true|false` and
# `backend=true|false`, ready for $GITHUB_OUTPUT. --all prints both true (pushes
# to main check everything). A path no rule claims runs both, so a new root file
# can't skip a component; root Markdown and .claude/ run neither. AgentK's tests
# read root files under scripts/ and .github/, so those run AgentK
# (extensions/agentk/test/unit/ci-scope.test.ts checks that every such read is
# covered). Exits 2 on bad usage.
set -eu

agentk=false
backend=false

if [ "${1:-}" = "--all" ] && [ $# -eq 1 ]; then
  agentk=true
  backend=true
elif [ $# -eq 0 ]; then
  while IFS= read -r path || [ -n "$path" ]; do
    case $path in
      '') ;;
      # The CI workflow and this script decide what runs: check both components.
      .github/workflows/ci.yml | scripts/ci-scope.sh) agentk=true backend=true ;;
      extensions/agentk/* | scripts/* | .github/*) agentk=true ;;
      backend/*) backend=true ;;
      .claude/*) ;;
      */*) agentk=true backend=true ;;
      *.md) ;;
      *) agentk=true backend=true ;;
    esac
  done
else
  echo "usage: $0 [--all] < paths" >&2
  exit 2
fi

printf 'agentk=%s\nbackend=%s\n' "$agentk" "$backend"
