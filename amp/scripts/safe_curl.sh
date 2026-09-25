#!/bin/sh
# Verify every file amp downloads against amp/downloads_checksum.txt.
#
# The manifest pins the sha256 of each external URL, so a file that changes
# after we pinned it — a compromised mirror, a re-tagged release, a swapped
# object behind the same URL — fails the build instead of being installed. It
# follows the rule yugabyte-db-thirdparty applies to thirdparty_src_checksums.txt:
#
#   * one manifest for everything, one `<sha256>  <url>` line per URL;
#   * a URL with no line FAILS, printing the line that would have to be added —
#     fail closed, so adding a download without pinning it is not possible;
#   * a wrong checksum re-fetches, up to three times, then fails;
#   * a malformed checksum is fatal on load.
#
# Plain POSIX sh: it runs inside Docker build stages whose base images have
# dash, not bash.
#
# Usage:
#   safe_curl.sh fetch  <url> <dest>   download <url> to <dest>, verified
#   safe_curl.sh verify <url> <file>   verify a file already on disk (e.g. from ADD)
#   safe_curl.sh lookup <url>          print <url>'s pinned checksum
#   safe_curl.sh add    <url>          developer only: fetch <url> and append its line
#   safe_curl.sh check                 validate the manifest
#
# DOWNLOADS_CHECKSUM_FILE overrides the manifest path; by default it is
# downloads_checksum.txt one directory above this script: amp/ in the repo, and
# /safe_curl/ where the Dockerfiles bind-mount the two files.
set -eu

self_dir=$(cd "$(dirname "$0")" && pwd)
manifest=${DOWNLOADS_CHECKSUM_FILE:-"$(cd "$self_dir/.." && pwd)/downloads_checksum.txt"}
max_attempts=3

die() { printf 'safe_curl: %s\n' "$*" >&2; exit 1; }

[ -f "$manifest" ] || die "manifest not found: $manifest"

sha256_of() {
	sha256sum "$1" | cut -d' ' -f1
}

# Every non-comment line must be `<64 lowercase hex>  https://...`, each URL once.
check_manifest() {
	awk '
		/^[[:space:]]*(#|$)/ { next }
		{
			if (NF != 2) { printf "line %d: want \"<sha256>  <url>\", got %d fields\n", NR, NF; bad = 1; next }
			if ($1 !~ /^[0-9a-f]+$/ || length($1) != 64) { printf "line %d: %s is not a sha256 (64 lowercase hex)\n", NR, $1; bad = 1 }
			if ($2 !~ /^https:\/\//) { printf "line %d: %s is not an https URL\n", NR, $2; bad = 1 }
			if ($2 in seen) { printf "line %d: %s is already pinned on line %d\n", NR, $2, seen[$2]; bad = 1 }
			seen[$2] = NR
		}
		END { exit bad }
	' "$manifest" >&2 || die "invalid manifest $manifest"
}

lookup() {
	awk -v url="$1" '!/^[[:space:]]*(#|$)/ && $2 == url { print $1; exit }' "$manifest"
}

download() {
	# curl where it exists, wget otherwise: some build stages carry only one.
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "neither curl nor wget is available to fetch $1"
	fi
}

fetch() {
	url=$1 dest=$2
	want=$(lookup "$url")
	attempt=1
	while :; do
		download "$url" "$dest.part"
		got=$(sha256_of "$dest.part")
		if [ -z "$want" ]; then
			rm -f "$dest.part"
			die "no checksum pinned for $url
Add this line to $manifest (after confirming the file is the one you meant):
$got  $url"
		fi
		if [ "$got" = "$want" ]; then
			mv "$dest.part" "$dest"
			return 0
		fi
		rm -f "$dest.part"
		[ "$attempt" -lt "$max_attempts" ] || die "mismatch for $url after $attempt attempts
  pinned:   $want
  received: $got"
		printf 'safe_curl: mismatch for %s (attempt %d of %d), re-fetching\n' "$url" "$attempt" "$max_attempts" >&2
		attempt=$((attempt + 1))
	done
}

verify() {
	url=$1 file=$2
	want=$(lookup "$url")
	got=$(sha256_of "$file")
	[ -n "$want" ] || die "no checksum pinned for $url
Add this line to $manifest (after confirming the file is the one you meant):
$got  $url"
	[ "$got" = "$want" ] || die "mismatch for $url
  pinned:   $want
  received: $got"
}

add() {
	url=$1
	case $url in https://*) ;; *) die "refusing to pin a non-https URL: $url" ;; esac
	existing=$(lookup "$url")
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT
	download "$url" "$tmp"
	got=$(sha256_of "$tmp")
	if [ -n "$existing" ]; then
		# Never overwrite a pin silently: a changed file behind a pinned URL is
		# exactly what this manifest exists to catch.
		[ "$existing" = "$got" ] && { printf 'safe_curl: %s already pinned\n' "$url" >&2; return 0; }
		die "$url is pinned to $existing but now serves $got — investigate before re-pinning"
	fi
	printf '%s  %s\n' "$got" "$url" >> "$manifest"
	printf 'safe_curl: pinned %s  %s\n' "$got" "$url" >&2
}

cmd=${1:-}
check_manifest
case $cmd in
	fetch)  [ $# -eq 3 ] || die "usage: $0 fetch <url> <dest>";  fetch "$2" "$3" ;;
	verify) [ $# -eq 3 ] || die "usage: $0 verify <url> <file>"; verify "$2" "$3" ;;
	lookup) [ $# -eq 2 ] || die "usage: $0 lookup <url>"
	        sum=$(lookup "$2"); [ -n "$sum" ] || die "no checksum pinned for $2"; printf '%s\n' "$sum" ;;
	add)    [ $# -eq 2 ] || die "usage: $0 add <url>"; add "$2" ;;
	check)  [ $# -eq 1 ] || die "usage: $0 check" ;;
	*)      die "usage: $0 {fetch|verify|lookup|add|check} ..." ;;
esac
