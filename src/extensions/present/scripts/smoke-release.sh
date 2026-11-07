#!/bin/sh
# Check a release tarball on this machine: it unpacks, says its version, and its server starts and
# serves Present's page on its socket, with a scratch Meko data folder (no Meko needed).
#
#   scripts/smoke-release.sh <tarball> <version>
set -eu

if [ $# -ne 2 ]; then
  echo "usage: $0 <tarball> <version>" >&2
  exit 2
fi
tarball=$1
version=$2

# Short: the server's socket path has a length limit.
dir=$(mktemp -d /tmp/pres-smoke.XXXXXX)
trap '"$bin" stop >/dev/null 2>&1 || true; rm -rf "$dir"' EXIT INT TERM
tar -xzf "$tarball" -C "$dir"
bin="$dir/present-$version/bin/me_present"
MEKO_HOME="$dir/meko"
export MEKO_HOME

got=$("$bin" version --json)
[ "$got" = "{\"version\":\"$version\"}" ] || { echo "version --json said $got, not $version" >&2; exit 1; }

"$bin" start || { cat "$MEKO_HOME/extensions/present/logs/server.log" >&2; exit 1; }
sock=$("$bin" _handoff --json | "$dir/present-$version/node/bin/me_present" -e 'process.stdin.on("data", (b) => process.stdout.write(JSON.parse(b).socket))')
page=$(curl -fsS --unix-socket "$sock" http://present/)
case $page in
  *'<base href="/ext/present/"'*) ;;
  *) echo "the page has no <base href=\"/ext/present/\">" >&2; exit 1 ;;
esac
case $page in
  *"content=\"$version\""*) ;;
  *) echo "the page doesn't name version $version" >&2; exit 1 ;;
esac
"$bin" stop
echo "present $version: ok"
