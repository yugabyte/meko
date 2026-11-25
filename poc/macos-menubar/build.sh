#!/bin/sh
# Builds Meko Menu Bar.app (POC) with the Command Line Tools' swiftc: no Xcode project needed.
set -eu
cd "$(dirname "$0")"
APP="build/Meko Menu Bar.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
swiftc -O -parse-as-library -target arm64-apple-macos13 \
  -o "$APP/Contents/MacOS/MekoMenuBar" Sources/MekoMenuBar.swift
cp Sources/m.svg "$APP/Contents/Resources/m.svg"
cat > "$APP/Contents/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>ai.mekodata.menubar</string>
  <key>CFBundleName</key><string>Meko Menu Bar</string>
  <key>CFBundleExecutable</key><string>MekoMenuBar</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.0.1</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSUIElement</key><true/>
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
EOF
codesign --force --sign - "$APP"
echo "Built $APP"
