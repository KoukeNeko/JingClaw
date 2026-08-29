#!/bin/sh
# Builds the macOS client into a .app bundle, and signs it if a certificate is
# available.
#
# SwiftPM produces an executable, and macOS needs a bundle: a plain binary gets
# no window, no menu bar and no place to put an icon. This assembles the
# smallest bundle that behaves like an application.
set -eu

cd "$(dirname "$0")/../macos"

CONFIGURATION=${CONFIGURATION:-release}
BUILT="$(swift build -c "$CONFIGURATION" --show-bin-path)/JingClaw"

swift build -c "$CONFIGURATION" --product JingClaw

APP="${APP_PATH:-$(cd .. && pwd)/build/JingClaw.app}"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"

cp "$BUILT" "$APP/Contents/MacOS/JingClaw"

cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>JingClaw</string>
	<key>CFBundleDisplayName</key><string>JingClaw</string>
	<key>CFBundleIdentifier</key><string>dev.jingclaw.client</string>
	<key>CFBundleExecutable</key><string>JingClaw</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>0.1.0</string>
	<key>CFBundleVersion</key><string>1</string>
	<key>LSMinimumSystemVersion</key><string>14.0</string>
	<key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST

printf 'built %s\n' "$APP"

# Signed when there is an identity. Unsigned is still runnable locally, so a
# machine without a certificate builds rather than fails.
#
# Selected by fingerprint rather than by name. A keychain commonly holds more
# than one certificate with the same subject — a renewal leaves the old one
# behind — and codesign refuses an ambiguous name rather than choosing.
IDENTITY=${CODESIGN_IDENTITY:-$(security find-identity -v -p codesigning 2>/dev/null |
	awk '/Developer ID Application/ { print $2; exit }')}

if [ -n "$IDENTITY" ]; then
	if codesign --force --options runtime --timestamp --sign "$IDENTITY" "$APP" 2>/dev/null ||
		codesign --force --options runtime --sign "$IDENTITY" "$APP"; then
		printf 'signed with %s\n' "$IDENTITY"
		codesign --verify --deep --strict "$APP" && printf 'signature verifies\n'
	else
		printf 'signing failed; the bundle is unsigned and runs locally only\n'
	fi
else
	printf 'not signed: no Developer ID on this machine\n'
fi

# Notarisation is deliberately not attempted here. It needs an Apple ID and an
# app-specific password, which belong to a person rather than to a script, and
# a build that silently fails to notarise is worse than one that says it did
# not try.
printf '\nTo notarise:\n'
printf '  xcrun notarytool submit --keychain-profile <profile> --wait %s\n' "$APP"
