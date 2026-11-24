#!/bin/sh
# Builds PaceBar.app (the pace menu bar icon) into dist/PaceBar.app.
# Needs the Xcode command line tools (swiftc). Universal (arm64 + x86_64),
# macOS 13 or later, ad-hoc signed.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
out=${1:-$root/dist/PaceBar.app}
version=${PACEBAR_VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for arch in arm64 x86_64; do
	swiftc -O -parse-as-library -target "$arch-apple-macos13.0" -o "$tmp/PaceBar-$arch" "$here/PaceBar.swift"
done
rm -rf "$out"
mkdir -p "$out/Contents/MacOS"
lipo -create -output "$out/Contents/MacOS/PaceBar" "$tmp/PaceBar-arm64" "$tmp/PaceBar-x86_64"
cat >"$out/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key><string>com.pace.bar</string>
	<key>CFBundleName</key><string>PaceBar</string>
	<key>CFBundleDisplayName</key><string>Pace</string>
	<key>CFBundleExecutable</key><string>PaceBar</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$version</string>
	<key>CFBundleVersion</key><string>$version</string>
	<key>LSMinimumSystemVersion</key><string>13.0</string>
	<key>LSUIElement</key><true/>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
EOF
codesign --force --sign - "$out" >/dev/null 2>&1
echo "built $out ($version)"
