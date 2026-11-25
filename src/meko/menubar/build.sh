#!/bin/sh
# Build Meko's menu bar app, Meko.app, into <out-dir> (make menubar: bin/Meko.app, beside bin/meko).
#
#   menubar/build.sh <version> <out-dir>
#
# macOS only. The Command Line Tools' swiftc is enough: no Xcode project. The app is signed ad hoc,
# as meko is unsigned: a browser download of a release would be quarantined either way.
set -eu

if [ $# -ne 2 ]; then
  echo "usage: $0 <version> <out-dir>" >&2
  exit 2
fi
version=$1
out=$2
[ "$(uname -s)" = Darwin ] || { echo "the menu bar app builds only on macOS" >&2; exit 1; }
src=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$out"
app="$(cd "$out" && pwd)/Meko.app"

rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
# MenuBarExtra needs macOS 13; Meko's releases need 14 anyway.
swiftc -O -parse-as-library -target "$(uname -m)-apple-macos13" \
  -o "$app/Contents/MacOS/meko-menubar" "$src/MekoMenuBar.swift"
cp "$src/m.svg" "$app/Contents/Resources/m.svg"
sed "s/@VERSION@/$version/" "$src/Info.plist" > "$app/Contents/Info.plist"
codesign --force --sign - "$app"
