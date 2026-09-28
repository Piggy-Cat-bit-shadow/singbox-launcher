package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/platform"
)

// GetInstalledCoreVersion получает установленную версию sing-box.
// После первой успешной проверки в этой сессии возвращает закешированное
// значение без повторного запуска `sing-box version`.
func (ac *AppController) GetInstalledCoreVersion() (string, error) {
	ac.installedCoreVersionCacheMu.Lock()
	defer ac.installedCoreVersionCacheMu.Unlock()
	if ac.installedCoreVersionCache != "" {
		return ac.installedCoreVersionCache, nil
	}

	v, err := coreVersionAt(ac.FileService.CoreBinaryPath())
	if err != nil {
		return "", err
	}
	ac.installedCoreVersionCache = v
	return v, nil
}

// coreVersionAt запускает `<path> version` и разбирает версию. Без кэша:
// кроме выбранного ядра, так спрашивают и затенённое (SPEC 135 §3.3).
func coreVersionAt(path string) (string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", fmt.Errorf("sing-box not found at %s", path)
	}

	cmd := exec.Command(path, "version")
	platform.PrepareCommand(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		debuglog.WarnLog("GetInstalledCoreVersion: command failed: %v, output: %q", err, string(output))
		return "", fmt.Errorf("failed to get version: %w", err)
	}

	version := ParseCoreVersionOutput(string(output))
	if version == "" {
		debuglog.WarnLog("GetInstalledCoreVersion: unable to parse version from output: %q", output)
		return "", fmt.Errorf("unable to parse version from output: %s", strings.TrimSpace(string(output)))
	}
	return version, nil
}

// coreVersionRegexp recognizes the `sing-box version X` line.
//
// One pattern for the whole project: the version is printed by the core and read
// in more than one place (the installed core, and a candidate during import), and
// two parsers would eventually disagree about what a build calls itself.
var coreVersionRegexp = regexp.MustCompile(`sing-box version\s+(\S+)`)

// ParseCoreVersionOutput extracts the version from `sing-box version` output.
//
// Returns "" when the output does not look like a sing-box version banner, which
// is how a non-core binary gets rejected.
func ParseCoreVersionOutput(output string) string {
	matches := coreVersionRegexp.FindStringSubmatch(strings.TrimSpace(output))
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

// GetCoreBinaryPath возвращает путь к бинарнику sing-box для отображения.
func (ac *AppController) GetCoreBinaryPath() string {
	p := ac.FileService.CoreBinaryPath()
	rel, err := filepath.Rel(string(ac.FileService.Layout.Data), p)
	if err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// CompareVersions сравнивает две версии (формат X.Y.Z или X.Y.Z-N-hash или X.Y.Z-dev.branch-hash).
// Возвращает: -1 если v1 < v2, 0 если v1 == v2, 1 если v1 > v2.
// Реализация — constants.CompareVersions (её зовёт и core/template).
func CompareVersions(v1, v2 string) int {
	return constants.CompareVersions(v1, v2)
}

// CoreVersionRelation — как установленное ядро соотносится с закреплённой
// версией лаунчера (SPEC 143).
type CoreVersionRelation string

const (
	// CoreVersionSame — в точности закреплённая версия.
	CoreVersionSame CoreVersionRelation = "same"
	// CoreVersionNewer — ядро новее закреплённого: кастомная или более
	// поздняя сборка. Предлагать «Reinstall» нельзя — это откат рабочего ядра.
	CoreVersionNewer CoreVersionRelation = "newer"
	// CoreVersionOlder — ядро старее закреплённого: обновление уместно.
	CoreVersionOlder CoreVersionRelation = "older"
	// CoreVersionUnknown — версия не разобралась (пусто, мусор).
	CoreVersionUnknown CoreVersionRelation = "unknown"
)

// ClassifyCoreVersion сравнивает установленную версию ядра с закреплённой.
//
// Раньше UI сравнивал строки на точное равенство (`installedVersion != required`)
// и на любой кастомной сборке показывал «Reinstall v<закреплённая>»: ядро
// 1.15.0-jiejie-masquerade.5 считалось «другой версией» наравне со старой, и
// пользователя подталкивали заменить рабочее ядро официальным. Здесь версии
// сравниваются по базе X.Y.Z, поэтому более новое кастомное ядро не выглядит
// как подлежащее замене.
func ClassifyCoreVersion(installed, required string) CoreVersionRelation {
	inst := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(installed), "v"))
	req := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(required), "v"))
	if inst == "" {
		return CoreVersionUnknown
	}
	if inst == req {
		return CoreVersionSame
	}
	if !isParseableCoreVersion(inst) || !isParseableCoreVersion(req) {
		// Разобрать не удалось — сравнивать нечем. «Другая», но не «старая»:
		// безопаснее не предлагать замену неизвестного ядра.
		return CoreVersionUnknown
	}
	switch c := CompareVersions(inst, req); {
	case c > 0:
		return CoreVersionNewer
	case c < 0:
		return CoreVersionOlder
	default:
		// База совпала, но строки разные — например, суффикс сборки.
		return CoreVersionNewer
	}
}

// isParseableCoreVersion — версия начинается с числовой базы X.Y.Z.
func isParseableCoreVersion(v string) bool {
	parts := strings.SplitN(v, "-", 2)[0]
	segments := strings.Split(parts, ".")
	if len(segments) < 2 {
		return false
	}
	for _, s := range segments {
		if s == "" {
			return false
		}
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
	}
	return true
}

// InvalidateInstalledCoreVersionCache сбрасывает сессионный кэш версии ядра.
// Вызывается после успешной установки/переустановки ядра, иначе Core Dashboard
// до перезапуска лаунчера показывает прежнюю версию.
func (ac *AppController) InvalidateInstalledCoreVersionCache() {
	ac.installedCoreVersionCacheMu.Lock()
	defer ac.installedCoreVersionCacheMu.Unlock()
	ac.installedCoreVersionCache = ""
}

// InvalidateCoreBinaryCaches сбрасывает ВСЕ кэши, выведенные из бинаря ядра.
//
// Единственный владелец инвалидации: любой путь, заменяющий бинарь ядра,
// обязан звать этот метод, а не чистить кэши по одному в своём обработчике —
// иначе новый кэш, добавленный позже, переживёт подмену бинаря и будет
// рассказывать о нём неправду до перезапуска приложения.
//
// Кэши делятся на два вида:
//
//   - версия ядра: сессионный, сам не протухает — сбрасывается здесь;
//   - теги сборки и поддержка chain: ключуются (mtime, size) бинаря и потому
//     сами промахиваются после подмены. Всё равно сбрасываются явно: полагаться
//     на то, что у нового файла гарантированно другие mtime и size, значит
//     зависеть от файловой системы (совпадение размера и секундной точности
//     mtime возможно), а последствие — узел уезжает в конфиг, который ядро
//     отвергает целиком.
func (ac *AppController) InvalidateCoreBinaryCaches() {
	if ac == nil {
		return
	}
	ac.InvalidateInstalledCoreVersionCache()

	ac.coreBuildTagsCacheMu.Lock()
	ac.coreBuildTagsCache = nil
	ac.coreBuildTagsCacheMu.Unlock()

	ac.chainSupportCacheMu.Lock()
	ac.chainSupportCache = nil
	ac.chainSupportCacheMu.Unlock()

	debuglog.InfoLog("CoreBinaryCaches: invalidated after a core binary change")
}
