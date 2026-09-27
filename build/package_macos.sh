#!/bin/bash
#
# JiejieBox — сборка устанавливаемого macOS-клиента (SPEC 144).
#
# Отличия от build/build_darwin.sh апстрима:
#   - своё имя приложения и свой CFBundleIdentifier, поэтому сборка стоит
#     рядом с оригинальным Singbox Launcher и не конфликтует с ним;
#   - свой домен ad-hoc подписи;
#   - на выходе .dmg и .zip, готовые к установке двойным щелчком.
#
# Данные при этом ОБЩИЕ с апстримом (~/Library/Application Support/singbox-launcher):
# там уже лежат config.json и кастомное ядро пользователя, и переносить их
# нельзя — см. комментарий к constants.DataDirAppName.
#
# Использование:
#   build/package_macos.sh [arm64|universal] [--install]
#
#   --install  дополнительно установить в /Applications (обновляет только
#              исполняемый файл, если приложение уже стоит).
#
set -e

cd "$(dirname "$0")/.."

BUILD_TYPE="arm64"
DO_INSTALL=false
for arg in "$@"; do
    case "$arg" in
        arm64|universal) BUILD_TYPE="$arg" ;;
        --install) DO_INSTALL=true ;;
        -h|--help)
            echo "Usage: $0 [arm64|universal] [--install]"
            exit 0
            ;;
        *) echo "ERROR: unknown argument: $arg" >&2; exit 1 ;;
    esac
done

APP_NAME="JiejieBox"
APP_BUNDLE_ID="com.piggycat.jiejiebox"
BINARY_NAME="JiejieBox"
MIN_MACOS_VERSION="11.0"
DIST_DIR="dist"

echo ""
echo "========================================"
echo "  Packaging ${APP_NAME} (macOS ${BUILD_TYPE})"
echo "========================================"

if ! command -v xcrun >/dev/null 2>&1; then
    echo "ERROR: xcrun not found. Install Xcode Command Line Tools:" >&2
    echo "  xcode-select --install" >&2
    exit 1
fi

SDK_PATH=$(xcrun --show-sdk-path 2>/dev/null || echo "")
if [ -z "$SDK_PATH" ]; then
    echo "ERROR: cannot find the macOS SDK (xcode-select --install)" >&2
    exit 1
fi
SDK_VERSION=$(xcrun --show-sdk-version 2>/dev/null || echo "unknown")
echo "SDK: $SDK_PATH (${SDK_VERSION})"

# Полный Xcode нужен CGO-части (UTCoreTypes.h отсутствует в CLT).
UTCORETYPES_H="$SDK_PATH/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Headers/UTCoreTypes.h"
if [ ! -f "$UTCORETYPES_H" ]; then
    echo "ERROR: full Xcode is required (missing UTCoreTypes.h in the SDK)." >&2
    echo "  sudo xcode-select --switch /Applications/Xcode.app" >&2
    exit 1
fi

# Версия приложения: собственная, чтобы её было видно в Finder и в UI,
# и чтобы она не совпадала с версией апстрима (иначе не отличить сборки).
VERSION="${APP_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}"
VERSION="${VERSION}-jiejiebox"
TEMPLATE_REF=$(git rev-parse HEAD)
echo "Version:      $VERSION"
echo "Bundle ID:    $APP_BUNDLE_ID"
echo "Template ref: $TEMPLATE_REF"

export CGO_ENABLED=1
export GOOS=darwin
export SDKROOT="$SDK_PATH"
export CGO_CFLAGS="-mmacosx-version-min=$MIN_MACOS_VERSION"
export CGO_LDFLAGS="-mmacosx-version-min=$MIN_MACOS_VERSION"

LDFLAGS="-s -w"
LDFLAGS="$LDFLAGS -X singbox-launcher/internal/constants.AppVersion=$VERSION"
LDFLAGS="$LDFLAGS -X singbox-launcher/internal/constants.RequiredTemplateRef=$TEMPLATE_REF"
# Внешний линкер: внутренний линкер Go игнорирует -mmacosx-version-min и
# штампует minos по SDK, из-за чего бинарь требует macOS новее заявленного.
LDFLAGS="$LDFLAGS -linkmode=external -extldflags=-mmacosx-version-min=$MIN_MACOS_VERSION"

rm -rf "$APP_NAME.app" "$DIST_DIR"
mkdir -p "$DIST_DIR"

echo ""
echo "=== Building ${BUILD_TYPE} ==="
if [ "$BUILD_TYPE" = "universal" ]; then
    GOARCH=arm64 go build -buildvcs=false -ldflags="$LDFLAGS" -o "${BINARY_NAME}_arm64"
    GOARCH=amd64 go build -buildvcs=false -ldflags="$LDFLAGS" -o "${BINARY_NAME}_amd64"
    lipo -create -output "$BINARY_NAME" "${BINARY_NAME}_arm64" "${BINARY_NAME}_amd64"
    rm -f "${BINARY_NAME}_arm64" "${BINARY_NAME}_amd64"
else
    GOARCH=arm64 go build -buildvcs=false -ldflags="$LDFLAGS" -o "$BINARY_NAME"
fi
file "$BINARY_NAME"

echo ""
echo "=== Creating ${APP_NAME}.app ==="
APP_MACOS="$APP_NAME.app/Contents/MacOS"
APP_RESOURCES="$APP_NAME.app/Contents/Resources"
mkdir -p "$APP_MACOS" "$APP_RESOURCES"
mv "$BINARY_NAME" "$APP_MACOS/$BINARY_NAME"
chmod +x "$APP_MACOS/$BINARY_NAME"

HAS_ICON=false
if [ -f "assets/app.icns" ]; then
    cp "assets/app.icns" "$APP_RESOURCES/app.icns"
    HAS_ICON=true
fi

{
    echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
    echo '<plist version="1.0">'
    echo '<dict>'
    echo '    <key>CFBundleExecutable</key>'
    echo "    <string>$BINARY_NAME</string>"
    echo '    <key>CFBundleIdentifier</key>'
    echo "    <string>$APP_BUNDLE_ID</string>"
    echo '    <key>CFBundleName</key>'
    echo "    <string>$APP_NAME</string>"
    echo '    <key>CFBundleDisplayName</key>'
    echo "    <string>$APP_NAME</string>"
    echo '    <key>CFBundlePackageType</key>'
    echo '    <string>APPL</string>'
    if [ "$HAS_ICON" = true ]; then
        echo '    <key>CFBundleIconFile</key>'
        echo '    <string>app</string>'
    fi
    echo '    <key>CFBundleShortVersionString</key>'
    echo "    <string>$VERSION</string>"
    echo '    <key>CFBundleVersion</key>'
    echo "    <string>$VERSION</string>"
    echo '    <key>LSMinimumSystemVersion</key>'
    echo "    <string>$MIN_MACOS_VERSION</string>"
    if [ "$BUILD_TYPE" = "universal" ]; then
        echo '    <key>LSArchitecturePriority</key>'
        echo '    <array><string>arm64</string><string>x86_64</string></array>'
    else
        echo '    <key>LSArchitecturePriority</key>'
        echo '    <array><string>arm64</string></array>'
    fi
    echo '    <key>NSHighResolutionCapable</key>'
    echo '    <true/>'
    echo '    <key>LSUIElement</key>'
    echo '    <false/>'
    echo '</dict>'
    echo '</plist>'
} > "$APP_NAME.app/Contents/Info.plist"

plutil -lint "$APP_NAME.app/Contents/Info.plist" >/dev/null

echo ""
echo "=== Ad-hoc signing ==="
# Ad-hoc подпись: приложение локальное, Developer ID и нотаризации нет.
# Это НЕ notarized-сборка, и Gatekeeper её не пропустит «из интернета» —
# но при установке из локального .dmg карантина нет, и запуск обычный.
codesign --force --sign - --identifier "$APP_BUNDLE_ID" --timestamp=none "$APP_NAME.app"
codesign --verify --verbose=1 "$APP_NAME.app"

echo ""
echo "=== Packaging ==="
ZIP_NAME="${APP_NAME}-${VERSION}-macos-${BUILD_TYPE}.zip"
DMG_NAME="${APP_NAME}-${VERSION}-macos-${BUILD_TYPE}.dmg"

# zip: ditto сохраняет права и расширенные атрибуты бандла.
ditto -c -k --sequesterRsrc --keepParent "$APP_NAME.app" "$DIST_DIR/$ZIP_NAME"

# dmg с ярлыком Applications — обычная drag&drop установка.
#
# hdiutil/diskutil требуют прав на создание образов; в ограниченных средах
# сборки обе операции запрещены ("операция не разрешена"). Это не ошибка
# упаковки приложения: zip уже собран и полностью годится для установки,
# поэтому dmg здесь — необязательное дополнение, и его отсутствие не
# считается провалом сборки.
if hdiutil create -size 1m -fs HFS+ -volname Probe "$(mktemp -d)/probe.dmg" >/dev/null 2>&1; then
    STAGE="$(mktemp -d)"
    DMG_TMP="$(mktemp -d)"
    cp -R "$APP_NAME.app" "$STAGE/"
    ln -s /Applications "$STAGE/Applications"
    if hdiutil create -volname "$APP_NAME" -srcfolder "$STAGE" -ov -format UDZO "$DMG_TMP/$DMG_NAME" >/dev/null 2>&1; then
        mv "$DMG_TMP/$DMG_NAME" "$DIST_DIR/$DMG_NAME"
        echo "Created: $DMG_NAME"
    else
        echo "NOTE: hdiutil could not build the .dmg in this environment; the .zip is the deliverable."
        DMG_NAME=""
    fi
    rm -rf "$STAGE" "$DMG_TMP"
else
    echo "NOTE: disk-image creation is not permitted in this environment."
    echo "      The .zip is the installable deliverable; run this script on a normal"
    echo "      macOS session to also produce the .dmg."
    DMG_NAME=""
fi

echo ""
echo "========================================"
echo "  Done"
echo "========================================"
echo "  .app : $(pwd)/$APP_NAME.app"
echo "  .zip : $(pwd)/$DIST_DIR/$ZIP_NAME"
[ -n "$DMG_NAME" ] && echo "  .dmg : $(pwd)/$DIST_DIR/$DMG_NAME"
# Запись параметров сборки рядом с артефактом: хеш меняется с каждым
# коммитом (в бинарь штампуется RequiredTemplateRef), поэтому храним его
# вместе с архивом, а не в тексте документации.
ZIP_SHA=$(shasum -a 256 "$DIST_DIR/$ZIP_NAME" | awk '{print $1}')
{
    echo "JiejieBox — build record"
    echo "========================"
    echo ""
    echo "Repository : https://github.com/Piggy-Cat-bit-shadow/singbox-launcher"
    echo "Commit     : $TEMPLATE_REF"
    echo "Version    : $VERSION"
    echo "Bundle ID  : $APP_BUNDLE_ID"
    echo "Build type : $BUILD_TYPE (macOS $MIN_MACOS_VERSION+)"
    echo ""
    echo "Archive    : $ZIP_NAME"
    echo "Size       : $(stat -f%z "$DIST_DIR/$ZIP_NAME") bytes"
    echo "SHA256     : $ZIP_SHA"
    if [ -n "$DMG_NAME" ]; then
        echo ""
        echo "DMG        : $DMG_NAME"
        echo "DMG SHA256 : $(shasum -a 256 "$DIST_DIR/$DMG_NAME" | awk '{print $1}')"
    fi
    echo ""
    echo "Signature  : ad-hoc (NOT notarized)"
    echo ""
    echo "Rebuild:"
    echo "  export GITHUB_ACTIONS=true && ./build/package_macos.sh $BUILD_TYPE"
} > "$DIST_DIR/BUILD_INFO.txt"

echo ""
echo "  SHA256:"
shasum -a 256 "$DIST_DIR/$ZIP_NAME" | sed 's/^/    /'
[ -n "$DMG_NAME" ] && shasum -a 256 "$DIST_DIR/$DMG_NAME" | sed 's/^/    /'
echo ""
echo "  Record: $(pwd)/$DIST_DIR/BUILD_INFO.txt"
echo "  Install:  unzip $(pwd)/$DIST_DIR/$ZIP_NAME -d /Applications   (or open the .dmg)"
echo "  Or:       build/package_macos.sh $BUILD_TYPE --install"

if [ "$DO_INSTALL" = true ]; then
    echo ""
    echo "=== Installing to /Applications ==="
    DEST_APP="/Applications/$APP_NAME.app"
    if [ -d "$DEST_APP" ]; then
        echo "Existing $DEST_APP found — replacing the executable only."
        osascript -e "tell application \"$DEST_APP\" to quit" >/dev/null 2>&1 || true
        sleep 2
        cp "$APP_NAME.app/Contents/MacOS/$BINARY_NAME" "$DEST_APP/Contents/MacOS/$BINARY_NAME"
        chmod +x "$DEST_APP/Contents/MacOS/$BINARY_NAME"
        cp "$APP_NAME.app/Contents/Info.plist" "$DEST_APP/Contents/Info.plist"
        codesign --force --sign - --identifier "$APP_BUNDLE_ID" "$DEST_APP"
    else
        echo "No existing install — copying the full bundle."
        cp -R "$APP_NAME.app" /Applications/
    fi
    codesign --verify "$DEST_APP" && echo "Installed and verified: $DEST_APP"
fi
