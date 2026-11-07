#!/bin/sh
# Smoke-test a release tarball on the machine that built it: unpack it, check its version, start
# meko on a fresh data folder (the context engine must load the bundled model, which it reads offline), save
# a memory and find it again, and stop.
#
#   scripts/smoke-release.sh <tarball> <version> [port]
set -eu

[ $# -ge 2 ] || { echo "usage: $0 <tarball> <version> [port]" >&2; exit 2; }
tarball=$1
version=$2
port=${3:-17990}

tmp=$(mktemp -d /tmp/meko-smoke.XXXXXX)
meko="$tmp/meko-$version/bin/meko"
data="$tmp/data"
cleanup() {
  "$meko" stop --data-dir "$data" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

tar -xzf "$tarball" -C "$tmp"
v=$("$meko" version --json | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')
[ "$v" = "$version" ] || { echo "the tarball's meko says it's $v, not $version" >&2; exit 1; }
"$meko" version

# A private HOME and file secrets: nothing of the runner's own is touched. No usage stats: a release
# build would send them to YugabyteDB. The web listener gets the port after the API listener's, never
# its default 7777, which a Kode or a Meko on the machine may hold.
export HOME="$tmp/home" MEKO_SECRETS=file MEKO_CALL_HOME=false MEKO_WEB_PORT=$((port + 1))
mkdir -p "$HOME"
"$meko" start --data-dir "$data" --port "$port"
i=0
until "$meko" status --data-dir "$data" --verbose | grep -q 'Context engine: ready'; do
  i=$((i + 1))
  if [ "$i" -ge 120 ]; then
    "$meko" status --data-dir "$data" --verbose || true
    echo "--- context-engine.log" >&2
    tail -n 40 "$data/logs/context-engine.log" >&2 || true
    echo "the context engine didn't get ready within 2 minutes" >&2
    exit 1
  fi
  sleep 1
done
"$meko" status --data-dir "$data"
# meko-cli is the same binary run by that name, as meko install links it in ~/.local/bin.
meko_cli="$tmp/meko-cli"
ln -s "$meko" "$meko_cli"
"$meko_cli" add_memory "The release smoke test saves a fact about blue herons nesting in March." --data-dir "$data"
out=$("$meko_cli" search "When do herons nest?" --data-dir "$data")
printf '%s\n' "$out"
printf '%s\n' "$out" | grep -q 'blue herons' || { echo "search didn't find the memory" >&2; exit 1; }
# The model came from the tarball: nothing was downloaded into the data folder.
[ -z "$(ls -A "$data/models" 2>/dev/null)" ] || { echo "the context engine downloaded a model instead of using the bundled one" >&2; exit 1; }
# The web UI came from the tarball too. The web listener serves it, at the address meko ui prints
# (which waits for the listener to start); the API listener doesn't. As over SSH, it only prints it.
url=$(SSH_CONNECTION=smoke "$meko" ui --data-dir "$data")
curl -fsS "$url" | grep -q 'id="root"' || { echo "the web listener at $url doesn't serve the web UI" >&2; exit 1; }
"$meko" stop --data-dir "$data"
echo "smoke test passed: meko $version"
