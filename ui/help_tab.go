package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/core"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/platform"
)

// CreateHelpTab creates and returns the content for the "Help" tab.
//
// v0.9.6: "Open Config Folder" + "Kill Sing-Box" buttons moved to the
// Diagnostics tab (🔍) — they're service/maintenance actions, semantically
// closer to logs/STUN/debug-api там, чем к информации о версии и ссылкам.
func CreateHelpTab(ac *core.AppController) fyne.CanvasObject {
	// Version and links section
	versionLabel := widget.NewLabel(locale.Tf("📦 Version: %s", constants.AppVersion))
	versionLabel.Alignment = fyne.TextAlignCenter

	// Self-update status is not shown here anymore (SPEC 147): JiejieBox is a
	// custom build, its version does not track the upstream release stream, and
	// the label plus its 2-second polling loop existed only to display the
	// result of the removed launcher-version check.

	telegramLink := widget.NewHyperlink(locale.T("💬 Telegram Channel"), nil)
	_ = telegramLink.SetURLFromString("https://t.me/singbox_launcher")
	telegramLink.OnTapped = func() {
		if err := platform.OpenURL("https://t.me/singbox_launcher"); err != nil {
			debuglog.ErrorLog("toolsTab: Failed to open Telegram link: %v", err)
			ShowError(ac.UIService.MainWindow, err)
		}
	}

	githubLink := widget.NewHyperlink(locale.T("🐙 GitHub Repository"), nil)
	_ = githubLink.SetURLFromString("https://github.com/Leadaxe/singbox-launcher")
	githubLink.OnTapped = func() {
		if err := platform.OpenURL("https://github.com/Leadaxe/singbox-launcher"); err != nil {
			debuglog.ErrorLog("toolsTab: Failed to open GitHub link: %v", err)
			ShowError(ac.UIService.MainWindow, err)
		}
	}

	// Language selector + download-locales button moved to the Settings tab
	// (ui/settings_tab.go) so all launcher-wide preferences live together.

	return container.NewVBox(
		versionLabel,
		widget.NewSeparator(),
		container.NewHBox(
			layout.NewSpacer(),
			telegramLink,
			widget.NewLabel(" | "),
			githubLink,
			layout.NewSpacer(),
		),
	)
}
