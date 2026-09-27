package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/core"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/platform"
	"singbox-launcher/ui/design"
	"singbox-launcher/ui/icons"
)

// CreateHelpTab creates and returns the content for the "Help" page.
//
// SPEC 144: страница переведена на карточки и векторные иконки.
//
// Раньше это была одна строка с версией и две ссылки в подвале, набранные
// emoji-подписями (📦 💬 🐙). Emoji как UI-глиф ненадёжен: его метрики
// приходят из системного emoji-шрифта и различаются на macOS, Windows и
// Linux, а на системе без такого шрифта глиф просто отсутствует. Ссылки
// теперь идут со штатными themed-иконками Fyne и нашим SVG для Telegram —
// они наследуют цвет темы и одинаково выглядят везде.
//
// v0.9.6: "Open Config Folder" + "Kill Sing-Box" buttons moved to the
// Diagnostics page — they're service/maintenance actions, semantically
// closer to logs/STUN/debug-api там, чем к информации о версии и ссылкам.
func CreateHelpTab(ac *core.AppController) fyne.CanvasObject {
	// Version and links section
	versionLabel := widget.NewLabel(locale.Tf("Version: %s", constants.AppVersion))
	versionLabel.TextStyle = fyne.TextStyle{Bold: true}

	// Self-update status is not shown here anymore (SPEC 147): JiejieBox is a
	// custom build, its version does not track the upstream release stream, and
	// the label plus its 2-second polling loop existed only to display the
	// result of the removed launcher-version check.

	telegramLink := widget.NewHyperlink(locale.T("Telegram Channel"), nil)
	_ = telegramLink.SetURLFromString("https://t.me/singbox_launcher")
	telegramLink.OnTapped = func() {
		if err := platform.OpenURL("https://t.me/singbox_launcher"); err != nil {
			debuglog.ErrorLog("toolsTab: Failed to open Telegram link: %v", err)
			ShowError(ac.UIService.MainWindow, err)
		}
	}

	githubLink := widget.NewHyperlink(locale.T("GitHub Repository"), nil)
	_ = githubLink.SetURLFromString("https://github.com/Leadaxe/singbox-launcher")
	githubLink.OnTapped = func() {
		if err := platform.OpenURL("https://github.com/Leadaxe/singbox-launcher"); err != nil {
			debuglog.ErrorLog("toolsTab: Failed to open GitHub link: %v", err)
			ShowError(ac.UIService.MainWindow, err)
		}
	}

	// Language selector + download-locales button moved to the Settings page
	// (ui/settings_tab.go) so all launcher-wide preferences live together.

	about := design.NewSectionCard(
		locale.T("About"),
		locale.T("Launcher version and support links."),
		container.NewVBox(
			versionLabel,
			design.CaptionWrap(locale.Tf("Bundle identifier: %s", constants.AppBundleName)),
		),
	)

	links := design.NewSectionCard(
		locale.T("Links"),
		"",
		container.NewVBox(
			container.NewHBox(widget.NewIcon(icons.Telegram), telegramLink),
			container.NewHBox(widget.NewIcon(icons.Link), githubLink),
		),
	)

	// Пустое место снизу: карточки не должны растягиваться на всю высоту
	// страницы, иначе они выглядят как пустые панели.
	return container.NewVBox(about.Object(), links.Object())
}
