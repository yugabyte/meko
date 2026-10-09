#!/usr/bin/env bash
# `cond && ok ... || bad ...` is safe throughout: ok and bad only echo and
# count, so they cannot fail and fall through to the other branch.
# shellcheck disable=SC2015
#
# Tests for scripts/safe_curl.sh — the paths its guarantee rests on, with a fake
# curl on PATH so nothing touches the network:
#
#   * a pinned download lands, verified, with no .part left behind;
#   * an unpinned URL fails closed, prints the line to add, and leaves nothing;
#   * a mismatch re-fetches, up to three attempts, then fails;
#   * a transient mismatch recovers on a later attempt;
#   * verify / lookup agree with the manifest;
#   * add appends a new pin, is a no-op on an unchanged one, refuses a pin whose
#     file changed, and refuses http;
#   * check rejects a malformed checksum, a non-https URL and a duplicate URL.
#
# safe_curl.sh runs under sh (dash on CI runners), as it does in the Docker
# build stages. Usage: scripts/test_safe_curl.sh
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
checksum="$here/safe_curl.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

pass=0
fail=0
ok() {
  echo "  ✓ $*"
  pass=$((pass + 1))
}
bad() {
  echo "  ✗ $*"
  fail=$((fail + 1))
}

# --- fake curl: serves $work/served/<basename of the URL>. When a file named
# <basename>.first exists, the FIRST request is served that instead (then the
# marker is dropped), which is how a transient mismatch is staged. Every call is
# counted in $work/curl.calls.
mkdir -p "$work/bin" "$work/served"
cat >"$work/bin/curl" <<'STUB'
#!/bin/sh
out="" url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --retry|--retry-delay) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
echo "$url" >> "$WORK/curl.calls"
name=${url##*/}
if [ -f "$WORK/served/$name.first" ]; then
  cp "$WORK/served/$name.first" "$out"; rm -f "$WORK/served/$name.first"; exit 0
fi
[ -f "$WORK/served/$name" ] || exit 22   # curl -f on a 404
cp "$WORK/served/$name" "$out"
STUB
chmod +x "$work/bin/curl"
export WORK="$work" PATH="$work/bin:$PATH"

sha() { sha256sum "$1" | cut -d' ' -f1; }
printf 'good bytes\n' >"$work/served/good.tgz"
printf 'other bytes\n' >"$work/served/changed.tgz"
printf 'tampered\n' >"$work/tampered"
GOOD=https://example.test/dl/good.tgz
CHANGED=https://example.test/dl/changed.tgz
NEW=https://example.test/dl/new.tgz
printf 'new bytes\n' >"$work/served/new.tgz"

manifest="$work/downloads_checksum.txt"
write_manifest() {
  cat >"$manifest" <<EOF
# test manifest
$(sha "$work/served/good.tgz")  $GOOD
$(sha "$work/tampered")  $CHANGED
EOF
}
run() { DOWNLOADS_CHECKSUM_FILE="$manifest" sh "$checksum" "$@"; }
calls() { grep -c . "$work/curl.calls" 2>/dev/null || echo 0; }
reset_calls() { : >"$work/curl.calls"; }

echo "safe_curl.sh:"
write_manifest

# check
run check 2>/dev/null && ok "check: a valid manifest passes" || bad "check: valid manifest rejected"
for broken in "ABC  $GOOD" "$(sha "$work/served/good.tgz")  http://example.test/x" \
  "$(sha "$work/served/good.tgz")  $GOOD"; do
  cp "$manifest" "$work/broken.txt"
  echo "$broken" >>"$work/broken.txt"
  if DOWNLOADS_CHECKSUM_FILE="$work/broken.txt" sh "$checksum" check 2>/dev/null; then
    bad "check: accepted '$broken'"
  else
    ok "check: rejects '${broken:0:40}…'"
  fi
done

# fetch, pinned
reset_calls
if run fetch "$GOOD" "$work/out" 2>/dev/null && cmp -s "$work/out" "$work/served/good.tgz"; then
  ok "fetch: a pinned download lands with the served bytes"
else
  bad "fetch: pinned download failed"
fi
[ ! -e "$work/out.part" ] && ok "fetch: no .part left behind" || bad "fetch: .part left behind"

# fetch, unpinned
rm -f "$work/out"
err=$(run fetch "$NEW" "$work/out" 2>&1) && bad "fetch: an unpinned URL succeeded" || ok "fetch: an unpinned URL fails"
echo "$err" | grep -qF "$(sha "$work/served/new.tgz")  $NEW" &&
  ok "fetch: it prints the exact line to add" || bad "fetch: no line to add in: $err"
[ ! -e "$work/out" ] && [ ! -e "$work/out.part" ] && ok "fetch: nothing left behind for an unpinned URL" ||
  bad "fetch: an unpinned URL left a file"

# fetch, persistent mismatch: three attempts, then fail
reset_calls
run fetch "$CHANGED" "$work/out" 2>/dev/null && bad "fetch: a mismatch succeeded" || ok "fetch: a mismatch fails"
[ "$(calls)" = 3 ] && ok "fetch: it tried three times" || bad "fetch: tried $(calls) times, want 3"
[ ! -e "$work/out" ] && [ ! -e "$work/out.part" ] && ok "fetch: nothing left behind on a mismatch" ||
  bad "fetch: a mismatch left a file"

# fetch, transient mismatch: bad bytes first, then the pinned ones
reset_calls
cp "$work/tampered" "$work/served/good.tgz.first"
run fetch "$GOOD" "$work/out" 2>/dev/null && cmp -s "$work/out" "$work/served/good.tgz" &&
  ok "fetch: a transient mismatch recovers on a later attempt" || bad "fetch: did not recover from a transient mismatch"
[ "$(calls)" = 2 ] && ok "fetch: recovered on the second attempt" || bad "fetch: took $(calls) attempts, want 2"

# verify / lookup
run verify "$GOOD" "$work/served/good.tgz" 2>/dev/null && ok "verify: the pinned bytes pass" || bad "verify: pinned bytes rejected"
run verify "$GOOD" "$work/tampered" 2>/dev/null && bad "verify: other bytes passed" || ok "verify: other bytes fail"
run verify "$NEW" "$work/served/new.tgz" 2>/dev/null && bad "verify: an unpinned URL passed" || ok "verify: an unpinned URL fails"
[ "$(run lookup "$GOOD" 2>/dev/null)" = "$(sha "$work/served/good.tgz")" ] && ok "lookup: prints the pin" || bad "lookup: wrong pin"
run lookup "$NEW" 2>/dev/null && bad "lookup: an unpinned URL succeeded" || ok "lookup: an unpinned URL fails"

# add
before=$(cat "$manifest")
run add "$GOOD" 2>/dev/null && [ "$(cat "$manifest")" = "$before" ] &&
  ok "add: an unchanged pin is a no-op" || bad "add: an unchanged pin changed the manifest"
run add "$CHANGED" 2>/dev/null && bad "add: re-pinned a changed file" || ok "add: refuses a pin whose file changed"
[ "$(cat "$manifest")" = "$before" ] && ok "add: the refused pin left the manifest alone" || bad "add: the manifest changed on refusal"
run add "http://example.test/dl/new.tgz" 2>/dev/null && bad "add: pinned an http URL" || ok "add: refuses http"
run add "$NEW" 2>/dev/null && [ "$(run lookup "$NEW" 2>/dev/null)" = "$(sha "$work/served/new.tgz")" ] &&
  ok "add: a new URL is pinned" || bad "add: a new URL was not pinned"
run check 2>/dev/null && ok "add: the manifest is still valid after it" || bad "add: left an invalid manifest"

echo
if ((fail > 0)); then
  echo "FAIL: $fail of $((pass + fail)) checks failed"
  exit 1
fi
echo "PASS: $pass checks"
