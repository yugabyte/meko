#!/usr/bin/env bash
# Formats the repo's tracked source, or with --check, shows what isn't formatted
# and exits 1. The Format workflow runs `--check` with no languages, so a clean
# run here is a clean run in CI.
#
#   .github/scripts/format.sh                   # format everything in place
#   .github/scripts/format.sh go python         # only these languages
#   .github/scripts/format.sh --check [LANG…]   # CI mode: print diffs, change nothing
#
#   go       gofmt from the Go release in meko/api_server/go.mod (what CI's
#            setup-go installs), every tracked .go file
#   rust     cargo fmt on the amp workspace (amp/rust-toolchain.toml pins it)
#   python   ruff format, every tracked .py file (amp/pyproject.toml under amp/,
#            ruff.toml elsewhere; both exclude vendored code)
#
# Exit status: 0 formatted, 1 not formatted, 2 bad usage or a missing tool.
set -euo pipefail

# Formatter output changes between releases, so CI and developers must run the
# same one. Keep RUFF_VERSION in step with amp/poetry.lock.
RUFF_VERSION=0.11.2

usage() {
  echo "usage: $0 [--check] [go|rust|python]..." >&2
  exit 2
}

check=false
langs=()
for arg in "$@"; do
  case $arg in
    --check) check=true ;;
    go | rust | python) langs+=("$arg") ;;
    *) usage ;;
  esac
done
[ ${#langs[@]} -gt 0 ] || langs=(go rust python)

cd "$(git rev-parse --show-toplevel)"

# Tracked files matching the pathspecs that exist on disk: a file deleted but not
# yet staged is still in the index, and every formatter fails on its path.
files() {
  git ls-files -z -- "$@" | while IFS= read -r -d '' f; do
    if [ -e "$f" ]; then printf '%s\0' "$f"; fi
  done
}

# A tool is missing: say how to get it, and fail the run without stopping the
# other languages.
missing() {
  echo "$*" >&2
  return 2
}

# The Go release CI uses: setup-go reads the toolchain directive if there is one,
# else the go directive. A bare "1.N" go line names release 1.N.0.
go_pin() {
  local mod=meko/api_server/go.mod v
  v=$(sed -n 's/^toolchain go//p' "$mod")
  [ -n "$v" ] || v=$(sed -n 's/^go //p' "$mod")
  case $v in *.*.*) ;; *) v=$v.0 ;; esac
  echo "go$v"
}

fmt_go() {
  command -v go >/dev/null || missing "go not found: install Go (https://go.dev/dl/)" || return
  # gofmt's output can change between releases. GOTOOLCHAIN fetches the pinned
  # one once if the local go differs; in CI it is already the installed one.
  local pin goroot
  pin=$(go_pin)
  goroot=$(GOTOOLCHAIN=$pin go env GOROOT 2>/dev/null) ||
    missing "couldn't get $pin's gofmt (GOTOOLCHAIN=$pin go env GOROOT failed; offline?)" || return
  local gofmt=$goroot/bin/gofmt
  if $check; then
    # gofmt -l exits 0 even when it lists files.
    # It also fails on a syntax error, which must not pass as "nothing listed".
    local unformatted
    unformatted=$(files '*.go' | xargs -0r "$gofmt" -l) || return 1
    [ -z "$unformatted" ] && return 0
    echo "$unformatted" | xargs "$gofmt" -d
    return 1
  fi
  files '*.go' | xargs -0r "$gofmt" -w
}

fmt_rust() {
  command -v cargo >/dev/null || missing "cargo not found: install rustup (https://rustup.rs)" || return
  (cd amp && cargo fmt --version) >/dev/null 2>&1 ||
    missing "rustfmt not found for amp's toolchain: (cd amp && rustup component add rustfmt)" || return
  if $check; then
    (cd amp && cargo fmt --all -- --check)
  else
    (cd amp && cargo fmt --all)
  fi
}

# uvx runs the pinned version without installing it; otherwise accept a ruff on
# PATH only if it is that exact version.
ruff_cmd() {
  if command -v uvx >/dev/null; then
    RUFF=(uvx "ruff@$RUFF_VERSION")
  elif [ "$(ruff --version 2>/dev/null)" = "ruff $RUFF_VERSION" ]; then
    RUFF=(ruff)
  else
    missing "need ruff $RUFF_VERSION (found: $(ruff --version 2>/dev/null || echo none)):" \
      "install uv (https://docs.astral.sh/uv/) or run: pip install ruff==$RUFF_VERSION"
  fi
}

fmt_python() {
  ruff_cmd || return
  "${RUFF[@]}" --version >/dev/null 2>&1 ||
    missing "couldn't run ruff $RUFF_VERSION (${RUFF[*]} --version failed; offline?)" || return
  # --force-exclude keeps the configs' excludes in force for the explicit file list.
  if $check; then
    files '*.py' | xargs -0r "${RUFF[@]}" format --diff -q --force-exclude
  else
    files '*.py' | xargs -0r "${RUFF[@]}" format -q --force-exclude
  fi
}

status=0
unformatted_langs=()
for lang in "${langs[@]}"; do
  rc=0
  "fmt_$lang" || rc=$?
  case $rc in
    0) echo "$lang: ok" ;;
    2)
      echo "$lang: tool missing" >&2
      status=2
      ;;
    *)
      # Any other failure (xargs reports one as 123) means not formatted.
      echo "$lang: not formatted" >&2
      unformatted_langs+=("$lang")
      [ $status -eq 2 ] || status=1
      ;;
  esac
done

if $check && [ ${#unformatted_langs[@]} -gt 0 ]; then
  echo "Fix with: .github/scripts/format.sh ${unformatted_langs[*]}" >&2
fi
exit $status
