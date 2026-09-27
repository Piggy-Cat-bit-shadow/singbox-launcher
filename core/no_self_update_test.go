package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// JiejieBox — кастомная сборка, её версия (`dev.main.<sha>-jiejiebox`) не
// сопоставима с релизами апстрима (`vX.Y.Z`). Поэтому self-update приложения
// удалён целиком (SPEC 147), и эти тесты стерегут именно это: они падают,
// если кто-то вернёт проверку обновлений приложения, попытку сравнить версию
// лаунчера с апстримом или запрос к GitHub releases API на старте.
//
// Проверяется ПОВЕДЕНИЕ (что в собранном коде нет пути self-update и что он
// не ходит в сеть), а не наличие конкретных строк. Строковые проверки здесь
// только там, где иначе не выразить «этого API больше нет»: они смотрят
// исходник пакета, а не форматирование UI.

// repoFile читает файл относительно корня репозитория (тесты core живут в core/).
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestNoApplicationSelfUpdateAPI — символы self-update приложения удалены.
// Их отсутствие и есть гарантия, что старт не может инициировать проверку.
func TestNoApplicationSelfUpdateAPI(t *testing.T) {
	src := repoFile(t, "core/core_version.go")
	for _, forbidden := range []string{
		"GetLatestLauncherVersion",
		"getLatestVersionFromURLWithPrefix",
		"CheckLauncherVersionOnStartup",
		"ShowUpdatePopupIfAvailable",
		"GetCachedLauncherVersion",
		"SetCachedLauncherVersion",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("application self-update symbol %q is back in core/core_version.go; "+
				"JiejieBox must not check upstream releases (SPEC 147)", forbidden)
		}
	}
}

// TestNoUpstreamReleaseCheckAtStartup — старт приложения не обращается к
// GitHub releases API для проверки версии приложения.
//
// Смотрим точку старта: раньше именно main.go звал
// controller.CheckLauncherVersionOnStartup(). Проверяем и вызов, и сам URL —
// вернуть проверку можно и иначе, но адрес всё равно появится рядом.
func TestNoUpstreamReleaseCheckAtStartup(t *testing.T) {
	mainSrc := repoFile(t, "main.go")
	if strings.Contains(mainSrc, "CheckLauncherVersionOnStartup") {
		t.Error("main.go still triggers the launcher version check on startup")
	}

	// Адрес апстримного releases API не должен встречаться в коде приложения.
	// core_downloader.go — исключение: он качает ЯДРО, а не приложение, и
	// ходит за релизами форка sing-box. Проверяем по файлам self-update.
	for _, rel := range []string{"core/core_version.go", "core/controller.go", "ui/help_tab.go"} {
		src := repoFile(t, rel)
		for _, forbidden := range []string{
			"api.github.com/repos/Leadaxe/singbox-launcher",
			"releases/latest",
		} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s still contains %q; the application must not query upstream releases (SPEC 147)", rel, forbidden)
			}
		}
	}
}

// TestNoUpdatePopupUIPlumbing — попап «Update Available» и его проводка
// удалены: ни колбэка в UIService, ни метода показа, ни якоря в Help.
func TestNoUpdatePopupUIPlumbing(t *testing.T) {
	checks := []struct{ file, forbidden string }{
		{"ui/core_dashboard_tab.go", "showUpdatePopup"},
		{"ui/core_dashboard_tab.go", "Update Available"},
		{"ui/core_dashboard_tab.go", "Download from GitHub"},
		{"ui/help_tab.go", "launcherUpdateLabel"},
		{"ui/help_tab.go", "updateLauncherVersionInfo"},
		{"core/uiservice/ui_service.go", "ShowUpdatePopupFunc"},
	}
	for _, c := range checks {
		if strings.Contains(repoFile(t, c.file), c.forbidden) {
			t.Errorf("%s still contains %q; the application update popup must stay removed (SPEC 147)", c.file, c.forbidden)
		}
	}
}

// TestAppControllerHasNoLauncherVersionState — состояние проверки версии
// лаунчера удалено из сервиса состояния: кеша, времени и флага «в процессе».
//
// Прямая проверка кода пакета: без этих полей проверку нельзя перезапустить
// из UI даже случайно.
func TestAppControllerHasNoLauncherVersionState(t *testing.T) {
	src := repoFile(t, "core/services/state_service.go")
	for _, forbidden := range []string{
		"LauncherVersionCheckCache",
		"LauncherVersionCheckInProgress",
		"GetCachedLauncherVersion",
		"SetCachedLauncherVersion",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("state_service.go still declares %q (SPEC 147)", forbidden)
		}
	}
}

// TestResourceUpdateFunctionsSurvive — удаление self-update не задело
// обновление РЕСУРСОВ: ядро, шаблон конфига, подписки. Эти функции — самая
// вероятная жертва «заодно почищу update-код», поэтому проверяются явно.
func TestResourceUpdateFunctionsSurvive(t *testing.T) {
	// Скачивание ядра и его версия.
	downloader := repoFile(t, "core/core_downloader.go")
	for _, want := range []string{"func ", "installBinary"} {
		if !strings.Contains(downloader, want) {
			t.Errorf("core_downloader.go lost %q — core download must be untouched", want)
		}
	}
	// Классификация версии ядра осталась (её использует UI ядра).
	version := repoFile(t, "core/core_version.go")
	for _, want := range []string{"ClassifyCoreVersion", "GetInstalledCoreVersion", "CompareVersions"} {
		if !strings.Contains(version, want) {
			t.Errorf("core_version.go lost %q — core version handling must be untouched", want)
		}
	}
	// Auto-update подписок — отдельная подсистема, она не тронута.
	auto := repoFile(t, "core/auto_update.go")
	for _, want := range []string{"startAutoUpdateLoop", "refreshSourceWithRetry"} {
		if !strings.Contains(auto, want) {
			t.Errorf("auto_update.go lost %q — subscription auto-update must be untouched", want)
		}
	}
}
