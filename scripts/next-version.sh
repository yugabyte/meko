#!/bin/sh
# Prints the next release version: 0.<UTC date YYYYMMDD>.<n>, where n counts
# the releases already tagged today (0 for the first release of the day).
#
#   scripts/next-version.sh            # uses today's UTC date
#   RELEASE_DATE=20260924 scripts/next-version.sh
#
# Needs the repo's tags (fetch-depth: 0 in CI).
set -eu

day=${RELEASE_DATE:-$(date -u +%Y%m%d)}
case $day in
  [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]) ;;
  *) echo "bad date: $day" >&2; exit 2 ;;
esac

n=0
for t in $(git tag -l "v0.$day.*"); do
  p=${t##*.}
  case $p in
    ''|*[!0-9]*) continue ;;
  esac
  [ "$p" -ge "$n" ] && n=$((p + 1))
done
echo "0.$day.$n"
