#!/bin/sh
# Builds macos/dist/AgentConsole.app: the Swift shell plus the Go console, both
# universal (Apple silicon + Intel), ad-hoc signed. No Apple developer account
# is needed; a downloaded copy must be allowed once in System Settings →
# Privacy & Security ("Open Anyway").
#
# Needs the Go darwin builds from `make dist` and Swift (Xcode command line
# tools are enough).
set -eu
cd "$(dirname "$0")"
ROOT=..
VERSION=${VERSION:-$(git -C "$ROOT" describe --tags --always 2>/dev/null || echo dev)}
APP=dist/AgentConsole.app

for f in "$ROOT/dist/agentconsole_darwin_arm64" "$ROOT/dist/agentconsole_darwin_amd64"; do
	[ -f "$f" ] || { echo "missing $f: run make dist first" >&2; exit 1; }
done

for arch in arm64 x86_64; do
	swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path ".build/$arch"
done

rm -rf "${APP:?}"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
sed "s/__VERSION__/$VERSION/g" Resources/Info.plist > "$APP/Contents/Info.plist"
lipo -create .build/arm64/release/AgentConsoleMac .build/x86_64/release/AgentConsoleMac \
	-output "$APP/Contents/MacOS/AgentConsole"
lipo -create "$ROOT/dist/agentconsole_darwin_arm64" "$ROOT/dist/agentconsole_darwin_amd64" \
	-output "$APP/Contents/Resources/agentconsole"

# App icon from the web icon.
ICONSET=.build/AppIcon.iconset
rm -rf "${ICONSET:?}"
mkdir -p "$ICONSET"
for s in 16 32 128 256 512; do
	sips -z $s $s "$ROOT/web/icon-512.png" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
	d=$((s * 2))
	[ $d -le 512 ] && sips -z $d $d "$ROOT/web/icon-512.png" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
cp "$ROOT/web/icon-512.png" "$ICONSET/icon_256x256@2x.png"
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"

# Ad-hoc signature (inner program first): enough to run on Apple silicon.
codesign --force --sign - "$APP/Contents/Resources/agentconsole"
codesign --force --sign - "$APP"
codesign --verify --deep --strict "$APP"
echo "built $APP ($VERSION)"
