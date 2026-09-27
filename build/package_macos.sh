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
#   build/package_macos.sh [arm64] [--install] [--dmg]
#
#   --install  дополнительно установить в /Applications (обновляет только
#              исполняемый файл, если приложение уже стоит).
#
set -e

cd "$(dirname "$0")/.."

# Временные каталоги и промежуточные бинарники удаляются на любом выходе:
# успех, ошибка, Ctrl-C. Без trap проба hdiutil оставляла каталог на каждый
# запуск, а прерывание сборки — ещё и STAGE/DMG_TMP с копией bundle.
TMP_PATHS=""
register_tmp() { TMP_PATHS="$TMP_PATHS $1"; }
cleanup_tmp() {
    for d in $TMP_PATHS; do
        [ -n "$d" ] && rm -rf "$d"
    done
    rm -f "${BINARY_NAME}_arm64" 2>/dev/null || true
}
trap cleanup_tmp EXIT INT TERM

# Продукт один: JiejieBox для Apple Silicon (SPEC 149). universal/catalina
# удалены — Intel-срез не нужен ни CI, ни пользователю этой машины, а держать
# его означало собирать вдвое больше на каждый прогон.
DO_INSTALL=false
MAKE_DMG=false
for arg in "$@"; do
    case "$arg" in
        arm64) ;; # единственный профиль; принимаем для совместимости вызовов
        --install) DO_INSTALL=true ;;
        --dmg) MAKE_DMG=true ;;
        -h|--help)
            echo "Usage: $0 [arm64] [--install] [--dmg]"
            echo ""
            echo "  arm64       Apple Silicon build (the only supported profile)"
            echo "  --install   also install to /Applications"
            echo "  --dmg       additionally build a .dmg (off by default: CI never"
            echo "              uses it, and probing hdiutil costs time for nothing)"
            exit 0
            ;;
        universal|catalina)
            echo "ERROR: '$arg' builds are no longer supported; this fork ships arm64 only." >&2
            echo "       Use: $0 arm64" >&2
            exit 1
            ;;
        *) echo "ERROR: unknown argument: $arg" >&2; exit 1 ;;
    esac
done
BUILD_TYPE="arm64"

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

# Версия приложения (SPEC 149).
#
# Раньше здесь стояли `git describe` и `git rev-list --count HEAD`, которым
# нужна ВСЯ история — из-за этого CI клонировал репозиторий с fetch-depth: 0
# на каждом прогоне. Теперь версию передаёт вызывающий:
#
#   APP_VERSION       — человекочитаемая строка (CI: dev-<shortsha> или тег)
#   APP_BUILD_NUMBER  — числовая, монотонная (CI: GITHUB_RUN_NUMBER)
#   TEMPLATE_REF      — коммит для RequiredTemplateRef
#
# Локальный запуск без переменных по-прежнему работает: короткий sha берётся
# из git (для него хватает одного коммита), а номер сборки — из времени.
if [ -n "${APP_VERSION:-}" ]; then
    VERSION="$APP_VERSION"
else
    SHORT_SHA="$(git rev-parse --short=7 HEAD 2>/dev/null || echo "local")"
    VERSION="dev-${SHORT_SHA}"
fi
VERSION="${VERSION}-jiejiebox"

# Санитайзинг версии для имени файла: имя тега может содержать слэш
# (`archive/macos-...`), и тогда «архив» превращается в каталог dist/archive/
# с zip внутри — CI ждёт файл и не находит его. Слэши, пробелы и всё, что не
# буква/цифра/точка/дефис/подчёркивание, заменяем на дефис.
FILE_VERSION="$(printf '%s' "$VERSION" | sed -E 's|[/ ]+|-|g; s/[^A-Za-z0-9._-]+/-/g; s/^-+//; s/-+$//')"
if [ -z "$FILE_VERSION" ]; then
    FILE_VERSION="dev"
fi
TEMPLATE_REF="${TEMPLATE_REF:-$(git rev-parse HEAD 2>/dev/null || echo "")}"

# CFBundleVersion по требованиям macOS — числовая строка (цифры и точки),
# монотонная между сборками. CFBundleShortVersionString остаётся
# человекочитаемым и может содержать буквы и дефисы.
if [ -n "${APP_BUILD_NUMBER:-}" ]; then
    BUILD_NUMBER="$APP_BUILD_NUMBER"
else
    # Локально: секунды эпохи — монотонно и не требует истории git.
    BUILD_NUMBER="$(date +%s)"
fi
case "$BUILD_NUMBER" in
    ''|*[!0-9]*) echo "ERROR: APP_BUILD_NUMBER must be numeric, got '$BUILD_NUMBER'" >&2; exit 1 ;;
esac
# База X.Y.Z из версии, если она там есть (тег v1.2.3 → 1.2.3).
BASE_SEMVER="$(printf '%s' "$VERSION" | sed -nE 's/^v?([0-9]+(\.[0-9]+)*).*/\1/p')"
if [ -z "$BASE_SEMVER" ]; then
    BASE_SEMVER="0.0.0"
fi
BASE_SEMVER="$(printf '%s' "$BASE_SEMVER" | awk -F. '{printf "%d.%d.%d", $1, ($2==""?0:$2), ($3==""?0:$3)}')"
CF_BUNDLE_VERSION="${BASE_SEMVER}.${BUILD_NUMBER}"
echo "Version:      $VERSION"
echo "Bundle ID:    $APP_BUNDLE_ID"
echo "CFBundleVer:  $CF_BUNDLE_VERSION"
echo "Template ref: $TEMPLATE_REF"

export CGO_ENABLED=1
export GOOS=darwin
export SDKROOT="$SDK_PATH"
export CGO_CFLAGS="-mmacosx-version-min=$MIN_MACOS_VERSION"
export CGO_LDFLAGS="-mmacosx-version-min=$MIN_MACOS_VERSION"

# -trimpath убирает абсолютные пути рабочего каталога из бинарника; без него
# в строках остаётся /Users/<имя>/... и артефакт «протекает» сборочной машиной.
GO_BUILD_FLAGS="-trimpath -buildvcs=false"
LDFLAGS="-s -w"
LDFLAGS="$LDFLAGS -X singbox-launcher/internal/constants.AppVersion=$VERSION"
LDFLAGS="$LDFLAGS -X singbox-launcher/internal/constants.RequiredTemplateRef=$TEMPLATE_REF"
# Внешний линкер: внутренний линкер Go игнорирует -mmacosx-version-min и
# штампует minos по SDK, из-за чего бинарь требует macOS новее заявленного.
LDFLAGS="$LDFLAGS -linkmode=external -extldflags=-mmacosx-version-min=$MIN_MACOS_VERSION"

rm -rf "$APP_NAME.app"
# Do NOT wipe dist/ wholesale: only this run's own outputs are removed, so a
# caller that builds twice keeps both results.
mkdir -p "$DIST_DIR"

echo ""
echo "=== Building ${BUILD_TYPE} ==="
GOARCH=arm64 go build $GO_BUILD_FLAGS -ldflags="$LDFLAGS" -o "$BINARY_NAME"
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
    echo "    <string>$FILE_VERSION</string>"
    echo '    <key>CFBundleVersion</key>'
    echo "    <string>$CF_BUNDLE_VERSION</string>"
    echo '    <key>LSMinimumSystemVersion</key>'
    echo "    <string>$MIN_MACOS_VERSION</string>"
    echo '    <key>LSArchitecturePriority</key>'
    echo '    <array><string>arm64</string></array>'
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
ZIP_NAME="${APP_NAME}-${FILE_VERSION}-macos-${BUILD_TYPE}.zip"
DMG_NAME="${APP_NAME}-${FILE_VERSION}-macos-${BUILD_TYPE}.dmg"

# zip: ditto сохраняет права и расширенные атрибуты бандла.
ditto -c -k --sequesterRsrc --keepParent "$APP_NAME.app" "$DIST_DIR/$ZIP_NAME"

# dmg с ярлыком Applications — обычная drag&drop установка.
#
# DMG — только по явному --dmg (SPEC 149).
#
# Раньше образ собирался всегда, а CI следом делал `rm -f dist/*.dmg`:
# он заведомо не нужен, но время на пробу hdiutil и сжатие уже тратилось.
# Теперь по умолчанию только ZIP — то, что уезжает пользователю.
if [ "$MAKE_DMG" = true ]; then
    PROBE_DIR="$(mktemp -d)"
    register_tmp "$PROBE_DIR"
    if hdiutil create -size 1m -fs HFS+ -volname Probe "$PROBE_DIR/probe.dmg" >/dev/null 2>&1; then
        STAGE="$(mktemp -d)"
        DMG_TMP="$(mktemp -d)"
        register_tmp "$STAGE"
        register_tmp "$DMG_TMP"
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
        DMG_NAME=""
    fi
else
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
    SRC_APP="$(pwd)/$APP_NAME.app"

    # Установка = замена ВСЕГО bundle, а не только исполняемого файла.
    # Раньше копировались лишь бинарь и Info.plist: если менялись иконка,
    # Resources, локализация или встроенный helper, в /Applications
    # оставались файлы прошлой версии — смесь двух сборок.
    #
    # Порядок безопасный: сначала новый bundle проверяется и подписывается,
    # затем копируется во временное место РЯДОМ с целью (тот же том, поэтому
    # переименование атомарно), и только потом подменяет установленный.
    # Провал на любом шаге оставляет прежнюю установку рабочей.

    echo "Verifying the new bundle before touching the installed one..."
    if ! codesign --verify "$SRC_APP" 2>/dev/null; then
        echo "ERROR: the freshly built bundle does not verify; nothing was installed." >&2
        exit 1
    fi
    for f in "$SRC_APP/Contents/Info.plist" "$SRC_APP/Contents/MacOS/$BINARY_NAME"; do
        if [ ! -e "$f" ]; then
            echo "ERROR: the new bundle is incomplete ($f is missing); nothing was installed." >&2
            exit 1
        fi
    done

    # Приложение не должно работать во время подмены: macOS держит открытый
    # образ, и подмена под запущенным процессом даёт «Code Signature Invalid».
    if [ -d "$DEST_APP" ]; then
        echo "Quitting the running app (if any)..."
        osascript -e "tell application \"$APP_NAME\"" -e "quit" -e "end tell" >/dev/null 2>&1 || true
        sleep 2
    fi

    STAGE="$(mktemp -d /Applications/.jiejiebox-install.XXXXXX)"
    register_tmp "$STAGE"
    echo "Staging to $STAGE ..."
    ditto "$SRC_APP" "$STAGE/$APP_NAME.app"
    if ! codesign --verify "$STAGE/$APP_NAME.app" 2>/dev/null; then
        echo "ERROR: the staged copy failed verification; the installed app is untouched." >&2
        exit 1
    fi

    if [ -d "$DEST_APP" ]; then
        # Старая версия уезжает в сторону, а не удаляется: если что-то пойдёт
        # не так, её можно вернуть.
        BACKUP="/Applications/.$APP_NAME.previous.$$"
        mv "$DEST_APP" "$BACKUP"
        if mv "$STAGE/$APP_NAME.app" "$DEST_APP"; then
            rm -rf "$BACKUP"
            echo "Replaced the full bundle."
        else
            echo "ERROR: swap failed; restoring the previous installation." >&2
            mv "$BACKUP" "$DEST_APP"
            exit 1
        fi
    else
        echo "No existing install — installing the full bundle."
        mv "$STAGE/$APP_NAME.app" "$DEST_APP"
    fi
    rmdir "$STAGE" 2>/dev/null || true

    if codesign --verify "$DEST_APP" 2>/dev/null; then
        echo "Installed and verified: $DEST_APP"
        echo "Open it with: open \"$DEST_APP\""
    else
        echo "WARNING: the installed bundle does not verify; re-run this script." >&2
        exit 1
    fi
fi
