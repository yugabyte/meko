#!/bin/sh
# Prints a product's next release version: <major>.<UTC date YYYYMMDD>.<n>, where major is the
# product's (product.sh: 1 for Kode and Meko, 0 for Present) and n counts the product's releases
# of that major already tagged today (0 for the first release of the day). The tag is the
# product's prefix plus this version (see product.sh).
#
#   scripts/release/next-version.sh [--product agentk|meko|present]   # default agentk
#   RELEASE_DATE=20260924 scripts/release/next-version.sh --product meko
#
# Needs the repo's tags (fetch-depth: 0 in CI).
set -eu
. "$(dirname "$0")/product.sh"

day=${RELEASE_DATE:-$(date -u +%Y%m%d)}
case $day in
  [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]) ;;
  *) echo "bad date: $day" >&2; exit 2 ;;
esac

n=0
for p in $tag_prefixes; do
  for t in $(git tag -l "${p}$major.$day.*"); do
    i=${t##*.}
    case $i in
      ''|*[!0-9]*) continue ;;
    esac
    [ "$i" -ge "$n" ] && n=$((i + 1))
  done
done
echo "$major.$day.$n"
