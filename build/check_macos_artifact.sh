#!/bin/bash
#
# check_macos_artifact.sh — приёмка готового macOS-пакета (SPEC 146).
#
# Распаковывает ZIP-релиз и проверяет, что это ровно то, что нужно:
# подписанный bundle с исполняемым бинарём, нужной архитектуры, без мусора,
# без пользовательских данных и без путей сборочной машины.
#
# Принцип — ALLOWLIST, а не blacklist: любой файл вне ожидаемого набора
# считается ошибкой. Так новая случайно попавшая в bundle вещь не проедет
# молча; чтобы её добавить, надо осознанно расширить список ниже.
#
# Использование:
#   build/check_macos_artifact.sh <path-to.zip> [arm64]
#
# Код возврата: 0 — приёмка пройдена, 1 — нет.

set -euo pipefail

ZIP="${1:-}"
EXPECTED_ARCH="${2:-}"

if [ -z "$ZIP" ] || [ ! -f "$ZIP" ]; then
    echo "usage: $0 <path-to.zip> [arm64]" >&2
    exit 1
fi

APP_NAME="JiejieBox"
EXPECTED_BUNDLE_ID="com.piggycat.jiejiebox"
FAILED=0

pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; FAILED=1; }
info() { printf '      %s\n' "$1"; }

WORK="$(mktemp -d)"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

echo "=== Artifact acceptance: $(basename "$ZIP") ==="
info "size: $(stat -f%z "$ZIP") bytes"

# --- распаковка тем же инструментом, что у пользователя ---
if ! ditto -x -k "$ZIP" "$WORK" 2>/dev/null; then
    fail "archive cannot be extracted with ditto"
    exit 1
fi
pass "archive extracts with ditto"

APP="$WORK/$APP_NAME.app"
if [ ! -d "$APP" ]; then
    fail "bundle $APP_NAME.app is missing from the archive"
    ls -la "$WORK" || true
    exit 1
fi
pass "bundle $APP_NAME.app is present"

# --- структура bundle ---
EXEC="$APP/Contents/MacOS/$APP_NAME"
[ -f "$EXEC" ] && pass "executable Contents/MacOS/$APP_NAME exists" || fail "executable Contents/MacOS/$APP_NAME is missing"
if [ -f "$EXEC" ]; then
    [ -x "$EXEC" ] && pass "executable bit is set after ZIP round-trip" || fail "executable bit is NOT set after ZIP round-trip"
fi
[ -f "$APP/Contents/Info.plist" ] && pass "Contents/Info.plist exists" || fail "Contents/Info.plist is missing"

# --- Info.plist ---
if [ -f "$APP/Contents/Info.plist" ]; then
    if plutil -lint "$APP/Contents/Info.plist" >/dev/null 2>&1; then
        pass "Info.plist is a valid plist"
    else
        fail "Info.plist is not a valid plist"
    fi
    BID=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$APP/Contents/Info.plist" 2>/dev/null || echo "")
    [ "$BID" = "$EXPECTED_BUNDLE_ID" ] && pass "bundle id is $EXPECTED_BUNDLE_ID" || fail "bundle id is '$BID', expected $EXPECTED_BUNDLE_ID"

    # CFBundleVersion обязан быть числом (точками/цифрами); произвольная
    # git-строка с буквами и дефисами недопустима.
    BVER=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$APP/Contents/Info.plist" 2>/dev/null || echo "")
    if printf '%s' "$BVER" | grep -Eq '^[0-9]+(\.[0-9]+)*$'; then
        pass "CFBundleVersion '$BVER' is numeric"
    else
        fail "CFBundleVersion '$BVER' is not a numeric version (macOS requires digits/dots)"
    fi
    SVER=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$APP/Contents/Info.plist" 2>/dev/null || echo "")
    if [ -z "$SVER" ]; then
        fail "CFBundleShortVersionString is empty"
    elif printf '%s' "$SVER" | grep -qE '[/[:space:]]'; then
        fail "CFBundleShortVersionString '$SVER' contains a slash or space (invalid for macOS)"
    else
        pass "CFBundleShortVersionString '$SVER' is well formed"
    fi
fi

# --- подпись ---
if codesign --verify "$APP" >/dev/null 2>&1; then
    pass "codesign --verify passes"
else
    fail "codesign --verify FAILS (bundle seal broken)"
fi
if [ -d "$APP/Contents/_CodeSignature" ]; then
    pass "Contents/_CodeSignature is present"
else
    fail "Contents/_CodeSignature is missing (bundle is not signed)"
fi

# --- архитектуры ---
if [ -f "$EXEC" ]; then
    ARCHS="$(lipo -archs "$EXEC" 2>/dev/null || echo "")"
    info "architectures: $ARCHS"
    case "$EXPECTED_ARCH" in
        arm64)
            # Этот fork выпускает только Apple Silicon. Сборка «на всякий
            # случай» с x86_64-слоем вдвое тяжелее и на этой машине
            # бесполезна, поэтому наличие чужого слоя — ошибка, а не запас.
            case "$ARCHS" in
                *x86_64*) fail "x86_64 slice must NOT be present in an arm64-only build" ;;
                *) pass "no x86_64 slice" ;;
            esac
            if [ "$ARCHS" = "arm64" ]; then
                pass "architecture is exactly arm64"
            else
                fail "architecture must be exactly 'arm64', got '$ARCHS'"
            fi
            ;;
        universal)
            case "$ARCHS" in
                *arm64*) pass "contains arm64" ;;
                *) fail "arm64 is missing from a universal build" ;;
            esac
            case "$ARCHS" in
                *x86_64*) pass "contains x86_64" ;;
                *) fail "x86_64 is missing from a universal build" ;;
            esac
            ;;
        catalina)
            case "$ARCHS" in
                *x86_64*) pass "contains x86_64" ;;
                *) fail "x86_64 is missing" ;;
            esac
            ;;
        *)
            info "architecture not asserted (no expected type given)"
            ;;
    esac
fi

# --- ALLOWLIST содержимого bundle ---
# Разрешено ровно это; всё прочее — ошибка.
ALLOWED_EXACT="Contents/Info.plist
Contents/PkgInfo
Contents/MacOS/$APP_NAME
Contents/Resources/app.icns"
ALLOWED_PREFIXES="Contents/_CodeSignature/"

echo "--- bundle contents (allowlist check) ---"
while IFS= read -r rel; do
    [ -z "$rel" ] && continue
    ok=0
    while IFS= read -r a; do
        [ -n "$a" ] && [ "$rel" = "$a" ] && ok=1
    done <<< "$ALLOWED_EXACT"
    for p in $ALLOWED_PREFIXES; do
        case "$rel" in "$p"*) ok=1 ;; esac
    done
    if [ "$ok" -eq 1 ]; then
        info "allowed: $rel"
    else
        fail "unexpected file in bundle: $rel (add it to the allowlist only with a documented reason)"
    fi
done < <(cd "$APP" && find . -type f | sed 's|^\./||' | sort)

# --- запрещённое содержимое: пользовательские данные, секреты, мусор ---
echo "--- forbidden content scan ---"
FORBIDDEN_NAMES='config\.json|settings\.json|state\.json|cache\.db|singbox\.pid|\.DS_Store|.*\.(log|tmp|bak|orig|prof|pprof|dmg|zip)$|go\.(mod|sum)$|README|LICENSE|AGENTS\.md|\.git'
FOUND_FORBIDDEN="$(cd "$APP" && find . -type f | sed 's|^\./||' | grep -Ei "$FORBIDDEN_NAMES" || true)"
if [ -z "$FOUND_FORBIDDEN" ]; then
    pass "no config/secrets/archives/dev files inside the bundle"
else
    fail "forbidden files inside the bundle:"
    printf '      %s\n' $FOUND_FORBIDDEN
fi

# Вложенные архивы — признак случайной упаковки артефакта в артефакт.
NESTED="$(cd "$APP" && find . -type f \( -name '*.zip' -o -name '*.dmg' -o -name '*.tar*' \) || true)"
[ -z "$NESTED" ] && pass "no nested archives" || fail "nested archives found: $NESTED"

# --- динамические зависимости и rpath ---
if [ -f "$EXEC" ]; then
    echo "--- dynamic libraries ---"
    DYLIBS="$(otool -L "$EXEC" 2>/dev/null | tail -n +2 | awk '{print $1}')"
    BAD_LIBS="$(printf '%s\n' "$DYLIBS" | grep -E '^(/opt/homebrew|/usr/local|/Users|/tmp|/private/tmp)' || true)"
    if [ -z "$BAD_LIBS" ]; then
        pass "no build-machine or Homebrew library paths"
    else
        fail "unexpected library paths:"
        printf '      %s\n' $BAD_LIBS
    fi

    echo "--- LC_RPATH ---"
    RPATHS="$(otool -l "$EXEC" 2>/dev/null | awk '/LC_RPATH/{getline; getline; print $2}' || true)"
    BAD_RPATHS="$(printf '%s\n' "$RPATHS" | grep -E '^(/opt/homebrew|/usr/local|/Users|/tmp|/private/tmp)' || true)"
    if [ -z "$BAD_RPATHS" ]; then
        pass "no build-machine rpaths"
    else
        fail "unexpected rpaths:"
        printf '      %s\n' $BAD_RPATHS
    fi

    # Пути сборочной машины в строках бинарника: -trimpath должен их убрать.
    echo "--- embedded developer paths ---"
    LEAK="$(strings -a "$EXEC" 2>/dev/null | grep -E '^(/Users/[^/]+/(Desktop|Documents|go/pkg)|/home/runner|/Users/runner)' | sort -u | head -5 || true)"
    if [ -z "$LEAK" ]; then
        pass "no developer/runner home paths embedded"
    else
        fail "embedded developer paths found:"
        printf '      %s\n' $LEAK
    fi
fi

# --- размеры ---
echo "--- sizes ---"
EXEC_SIZE=$(stat -f%z "$EXEC" 2>/dev/null || echo 0)
APP_SIZE=$(du -sk "$APP" | awk '{print $1 * 1024}')
ZIP_SIZE=$(stat -f%z "$ZIP")
info "executable: $EXEC_SIZE bytes ($((EXEC_SIZE / 1048576)) MiB)"
info "app bundle: $APP_SIZE bytes ($((APP_SIZE / 1048576)) MiB)"
info "zip:        $ZIP_SIZE bytes ($((ZIP_SIZE / 1048576)) MiB)"

echo
if [ "$FAILED" -eq 0 ]; then
    echo "ALL CHECKS PASSED"
else
    echo "ARTIFACT ACCEPTANCE FAILED"
fi
exit "$FAILED"
