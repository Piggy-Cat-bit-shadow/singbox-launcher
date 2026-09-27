package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/core"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
	"singbox-launcher/ui/design"
)

// Файл appearance.go — выбор внешнего вида (SPEC 145 fix-wave §3).
//
// **Что это даёт.** Раньше тема была жёстко «системной», и при этом из-за
// ошибки в варианте Fyne светлая система получала тёмную палитру. Теперь
// пользователь может выбрать явно, а по умолчанию остаётся System — именно
// этот режим даёт совпадение нативной шапки окна с содержимым.
//
// **Почему строка, а не число.** Значения — "system"/"light"/"dark". Числовые
// коды темы Fyne (VariantDark == 0, VariantLight == 1) слишком легко
// перепутать; именно такая путаница и была исходным дефектом.
//
// **Про нативную шапку.** В режимах Light/Dark меняется содержимое окна;
// оформление самой системной шапки задаёт ОС, и публичного способа переключить
// его из Fyne нет. Приватные AppKit-вызовы здесь сознательно не используются —
// это отмечено в подсказке под переключателем.

// buildAppearanceSection собирает раздел Appearance.
func buildAppearanceSection(ac *core.AppController) (fyne.CanvasObject, func()) {
	binDir := ac.FileService.Layout.Data.Bin()

	options := []string{
		locale.T("System"),
		locale.T("Light"),
		locale.T("Dark"),
	}
	sel := widget.NewSelect(options, nil)
	sel.Selected = appearanceLabel(locale.LoadSettings(binDir).AppearanceMode)
	sel.OnChanged = func(label string) {
		mode := appearanceModeForLabel(label)
		st := locale.LoadSettings(binDir)
		if st.AppearanceMode == mode {
			return
		}
		st.AppearanceMode = mode
		if err := locale.SaveSettings(binDir, st); err != nil {
			debuglog.WarnLog("settings: save appearance_mode: %v", err)
			return
		}
		ApplyAppearance(ac, mode)
	}

	row := container.NewBorder(nil, nil,
		container.NewVBox(
			widget.NewLabel(locale.T("Appearance")),
			design.CaptionWrap(locale.T("System follows the macOS appearance. Light and Dark change the window contents; the window frame itself stays under the system's control.")),
		), nil, sel)

	return row, func() {
		sel.SetSelected(appearanceLabel(locale.LoadSettings(binDir).AppearanceMode))
	}
}

// appearanceLabel — подпись для текущего режима.
func appearanceLabel(mode string) string {
	switch design.AppearanceMode(mode) {
	case design.AppearanceLight:
		return locale.T("Light")
	case design.AppearanceDark:
		return locale.T("Dark")
	default:
		return locale.T("System")
	}
}

// appearanceModeForLabel — обратное преобразование подписи в значение.
func appearanceModeForLabel(label string) string {
	switch label {
	case locale.T("Light"):
		return string(design.AppearanceLight)
	case locale.T("Dark"):
		return string(design.AppearanceDark)
	default:
		return string(design.AppearanceSystem)
	}
}

// ApplyAppearance применяет режим к теме приложения.
//
// Тема уже установлена в main(); здесь мы лишь сообщаем ей выбранный режим.
// Ничего в бизнес-слое не трогается.
func ApplyAppearance(ac *core.AppController, mode string) {
	if ac == nil || ac.UIService == nil || ac.UIService.Application == nil {
		return
	}
	th, ok := ac.UIService.Application.Settings().Theme().(*design.Theme)
	if !ok || th == nil {
		// Тема не наша (например, приложение собрано без установки темы) —
		// менять нечего, и падать из-за этого нельзя.
		debuglog.WarnLog("appearance: application theme is not design.Theme; ignoring mode %q", mode)
		return
	}
	th.SetMode(design.AppearanceMode(mode))
	debuglog.InfoLog("appearance: mode set to %q", mode)
}
