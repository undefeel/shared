#!/bin/bash
# Собирает bin/sharedir.app: SwiftUI-приложение меню-бара с демоном внутри.
set -euo pipefail
cd "$(dirname "$0")/.."

APP=bin/sharedir.app
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp macos/Info.plist "$APP/Contents/Info.plist"
cp -R macos/Resources/*.lproj macos/Resources/AppIcon.icns "$APP/Contents/Resources/"

swiftc -O -swift-version 5 -parse-as-library \
  -target "$(uname -m)-apple-macos14.0" \
  macos/SharedirBar/*.swift \
  -o "$APP/Contents/MacOS/SharedirBar"

go build -trimpath -o "$APP/Contents/MacOS/sharedir" ./cmd/sharedir

# Ad-hoc подпись: без неё не работают SMAppService и стабильные права в TCC.
codesign --force --deep --sign - "$APP"
echo "Готово: $APP"
