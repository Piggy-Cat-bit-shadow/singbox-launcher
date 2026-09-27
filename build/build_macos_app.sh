#!/usr/bin/env bash
# Build the SwiftUI menu-bar frontend and place it in the app bundle.
#
# Split from package_macos.sh on purpose: the Swift build is the slow part and
# fails for reasons (toolchain, SwiftUI macros) that have nothing to do with
# signing or bundling. Keeping them separate makes a failure legible.
#
# SwiftPM, not an Xcode project: the build machines here have Command Line
# Tools only, where xcodebuild refuses to run.
set -euo pipefail
cd "$(dirname "$0")/.."

APP_NAME="JiejieBox"
CONFIG="${CONFIG:-release}"
APP_MACOS="$APP_NAME.app/Contents/MacOS"

if ! command -v swift >/dev/null 2>&1; then
    echo "ERROR: swift not found; install Xcode or Command Line Tools" >&2
    exit 1
fi

echo "=== Building SwiftUI frontend ($CONFIG, arm64) ==="
cd macos
swift build -c "$CONFIG" --arch arm64 2>&1 | tail -5
cd ..

# SwiftPM puts the product under .build/out/Products/<Config>/ on this
# toolchain; fall back to the conventional path for other layouts.
BIN=""
for candidate in \
    "macos/.build/out/Products/$CONFIG/JiejieBox" \
    "macos/.build/$CONFIG/JiejieBox" \
    "macos/.build/arm64-apple-macosx/$CONFIG/JiejieBox"
do
    if [ -x "$candidate" ]; then BIN="$candidate"; break; fi
done

if [ -z "$BIN" ]; then
    echo "ERROR: the Swift build produced no executable" >&2
    find macos/.build -name JiejieBox -type f 2>/dev/null | head >&2 || true
    exit 1
fi

mkdir -p "$APP_MACOS"
cp "$BIN" "$APP_MACOS/JiejieBox"
chmod +x "$APP_MACOS/JiejieBox"
echo "frontend: $APP_MACOS/JiejieBox"
file "$APP_MACOS/JiejieBox"
