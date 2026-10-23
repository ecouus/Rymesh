#!/bin/sh
# Builds macos/dist.noindex/Rymesh.app (.noindex keeps Spotlight from
# listing it next to the installed copy): the Swift shell plus the Go console, both
# universal (Apple silicon + Intel), ad-hoc signed. No Apple developer account
# is needed; a downloaded copy must be allowed once in System Settings →
# Privacy & Security ("Open Anyway").
#
# Needs the Go darwin builds from `make dist` and Swift (Xcode command line
# tools are enough).
set -eu
cd "$(dirname "$0")"
ROOT=..
VERSION=${VERSION:-$(sed -n 's/^const Version = "\(.*\)"/\1/p' "$ROOT/internal/util/util.go")}
APP=dist.noindex/Rymesh.app

for f in "$ROOT/dist/rymesh_darwin_arm64" "$ROOT/dist/rymesh_darwin_amd64" \
	"$ROOT/dist/rymesh_linux_amd64.gz" "$ROOT/dist/rymesh_linux_arm64.gz"; do
	[ -f "$f" ] || { echo "missing $f: run make dist first" >&2; exit 1; }
done

for arch in arm64 x86_64; do
	swift build -c release --triple "$arch-apple-macosx13.0" --scratch-path ".build/$arch"
done

rm -rf "${APP:?}"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
sed "s/__VERSION__/$VERSION/g" Resources/Info.plist > "$APP/Contents/Info.plist"
lipo -create .build/arm64/release/RymeshMac .build/x86_64/release/RymeshMac \
	-output "$APP/Contents/MacOS/Rymesh"
# Drop debug and local symbols: they name the source and build directories
# (the builder's home) and nothing at run time needs them.
strip -S -x "$APP/Contents/MacOS/Rymesh"
lipo -create "$ROOT/dist/rymesh_darwin_arm64" "$ROOT/dist/rymesh_darwin_amd64" \
	-output "$APP/Contents/Resources/rymesh"
# Node programs the app uploads when it deploys to a Linux server, gzipped
# (nodeBinary looks in dist/ next to the console binary).
mkdir -p "$APP/Contents/Resources/dist"
cp "$ROOT/dist/rymesh_linux_amd64.gz" "$ROOT/dist/rymesh_linux_arm64.gz" "$APP/Contents/Resources/dist/"

# App icon: a 1024 master on Apple's icon grid (transparent margin).
ICON="$ROOT/macos/Resources/AppIcon.png"
ICONSET=.build/AppIcon.iconset
rm -rf "${ICONSET:?}"
mkdir -p "$ICONSET"
for s in 16 32 128 256 512; do
	sips -z $s $s "$ICON" --out "$ICONSET/icon_${s}x${s}.png" >/dev/null
	d=$((s * 2))
	sips -z $d $d "$ICON" --out "$ICONSET/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"

# Ad-hoc signature (inner program first): enough to run on Apple silicon.
codesign --force --sign - "$APP/Contents/Resources/rymesh"
codesign --force --sign - "$APP"
codesign --verify --deep --strict "$APP"
echo "built $APP ($VERSION)"
