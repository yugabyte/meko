#!/bin/sh
# Says which components' CI jobs a change needs, for .github/workflows/ci.yml:
#
#   git diff --name-only --no-renames <base> <head> > changed
#   scripts/ci-scope.sh < changed
#   scripts/ci-scope.sh --all
#
# Not through a pipe: without pipefail a failed diff would read as no changes.
# Reads changed paths, one per line, and prints `agentk=true|false`,
# `backend=true|false` and `presenter=true|false`, ready for $GITHUB_OUTPUT. --all
# prints all true (pushes to main check everything). A path no rule claims runs
# them all, so a new root file can't skip a component; root Markdown and .claude/
# run none. Nor does Markdown only people read: a component's README, CONTRIBUTING,
# AGENTS, CLAUDE and REVIEW files and its docs/. Markdown a test reads or a release
# ships runs its component like code: Meko's docs (backend/docs/docs_test.go checks
# them), test fixtures, and AgentK's session tools skill (src/tools/skill/SKILL.md).
# AgentK's tests read root files under scripts/ and .github/, so those run AgentK,
# and Meko's resolution cases (backend/internal/paths/testdata/resolve-cases.json),
# which AgentK's and Presenter's tests read too, run all three
# (extensions/agentk/test/unit/ci-scope.test.ts checks that every such read, and
# every Markdown file a backend test globs or AgentK's release packs, is covered).
# Exits 2 on bad usage.
set -eu

# Markdown only people read: a README, CONTRIBUTING, AGENTS, CLAUDE or REVIEW file, or docs/.
# Asked only of paths in a component, so the same names elsewhere fall to the rules for them.
doc() {
  case $1 in
    */AGENTS.md | */CLAUDE.md | */CONTRIBUTING.md | */README.md | */REVIEW.md | */docs/*.md) return 0 ;;
  esac
  return 1
}

agentk=false
backend=false
presenter=false

if [ "${1:-}" = "--all" ] && [ $# -eq 1 ]; then
  agentk=true
  backend=true
  presenter=true
elif [ $# -eq 0 ]; then
  while IFS= read -r path || [ -n "$path" ]; do
    case $path in
      '') ;;
      # The CI workflow and this script decide what runs: check every component.
      .github/workflows/ci.yml | scripts/ci-scope.sh) agentk=true backend=true presenter=true ;;
      # Where Meko's data folder is: Meko's paths.Resolve, and AgentK's and Presenter's mekoHome, are tested against it.
      backend/internal/paths/testdata/resolve-cases.json) agentk=true backend=true presenter=true ;;
      # Markdown a test reads: Meko's docs and test fixtures. These match before a component's docs.
      backend/docs/* | backend/*/test/* | backend/*/testdata/*) backend=true ;;
      extensions/agentk/test/*) agentk=true ;;
      extensions/presenter/test/*) presenter=true ;;
      # A component's docs check nothing; its other Markdown (such as a skill a release ships) runs it.
      extensions/presenter/*) doc "$path" || presenter=true ;;
      extensions/agentk/*) doc "$path" || agentk=true ;;
      scripts/* | .github/*) agentk=true ;;
      backend/*) doc "$path" || backend=true ;;
      .claude/*) ;;
      */*) agentk=true backend=true presenter=true ;;
      *.md) ;;
      *) agentk=true backend=true presenter=true ;;
    esac
  done
else
  echo "usage: $0 [--all] < paths" >&2
  exit 2
fi

printf 'agentk=%s\nbackend=%s\npresenter=%s\n' "$agentk" "$backend" "$presenter"
