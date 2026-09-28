//go:build darwin

package core

import (
	"errors"
	"os"
	"strings"

	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/uiport"
)

// Гейт привилегированного старта classic-движка (SPEC 137).
//
// Ядро с TUN стартует под root через AEWP, и root исполняет только
// root-owned копию ядра — ту же, что запускает служба демона (SPEC 136,
// раскладка lx.11). Перед стартом лаунчер без root проверяет цепочку
// владения копии и сверяет её sha256 с ядром лаунчера; не прошло — старта
// с привилегиями нет, пользователь получает одну sudo-команду, которая
// создаёт или обновляет копию. Лаунчер ничего не копирует сам.
//
// Здесь — платформенная часть (общая — classic_privileged.go): тексты
// диалога («root», Terminal, sudo) и сам диалог отказа гейта.

// Длинные тексты локализации: ключ = английский текст (SPEC 111).
const (
	privilegedCopyMissingText     = "TUN mode starts the sing-box core as root. For safety the launcher runs only a root-owned copy of the core that your user account cannot modify, and there is no such copy yet."
	privilegedCopyOutdatedText    = "TUN mode starts the sing-box core as root from a root-owned copy, and the copy is not the launcher's current core (copy %s, launcher core %s) — for example after a core update."
	privilegedCopyUnsafeText      = "TUN mode starts the sing-box core as root only from a root-owned copy, and the copy's location failed the ownership check:\n%s\nIf the command below reports the same problem, fix the ownership of that path."
	privilegedCopyServiceNoteText = "The daemon service is installed on this Mac, so the command is its Install or update command: it refreshes the same copy and restarts the service."
	privilegedCopyInstructionText = "Run this command in Terminal (it asks for your sudo password), then click Retry:"
	daemonCoreUpdatedBodyText     = "The daemon service still runs the previous core. Run this command in Terminal to install the new core into the service and restart it (it asks for your sudo password):"
)

// showPrivilegedCopyDialog — диалог отказа гейта (SPEC 137 §5): причина,
// одна sudo-команда, Copy the command / Run in Terminal / Retry / Close.
// Retry повторяет Start тем же путём, что кнопка Start. command == "" (ядро
// лаунчера копию не умеет) — вместо команды coreHint, кнопка одна: Close.
// showPrivilegedCopyDialog explains why a privileged start cannot proceed and
// what to do about it.
//
// The Fyne dialog was the ONLY outlet, and it returned immediately when no UI was
// attached — which is always, for the headless backend the macOS app uses. So the
// single most actionable failure in this area ("the protected core copy is
// missing/outdated; run this command") reached nobody: the user pressed Connect,
// nothing happened, and the reason sat in a log.
//
// The failure is now recorded with the full instruction text, and because the
// privileged-copy states are distinct codes the frontend can render the right
// wording and offer the command rather than a generic "start failed".
func (ac *AppController) showPrivilegedCopyDialog(c privilegedCopyCheck, command string, viaService bool, coreHint string) {
	var title, reason string
	switch c.State {
	case privilegedCopyMissing:
		title = locale.T("Core copy for privileged start is missing")
		reason = locale.T(privilegedCopyMissingText)
	case privilegedCopyOutdated:
		title = locale.T("Core copy for privileged start is outdated")
		reason = locale.Tf(privilegedCopyOutdatedText, shortSHA(c.CopySHA256), shortSHA(c.LauncherSHA256))
	default:
		title = locale.T("Core copy for privileged start is not protected")
		reason = locale.Tf(privilegedCopyUnsafeText, c.Detail)
	}
	parts := []string{reason}
	if command == "" {
		parts = append(parts, coreHint)
		ac.recordPrivilegedCopyFailure(title, parts)
		if ac.hasUI() {
			ac.ui().ShowCommandNeedsTerminal(title, strings.Join(parts, "\n\n"), "")
		}
		return
	}
	if viaService {
		parts = append(parts, locale.T(privilegedCopyServiceNoteText))
	}
	parts = append(parts, locale.T(privilegedCopyInstructionText))
	ac.recordPrivilegedCopyFailure(title, parts)
	if ac.hasUI() {
		ac.ui().ShowCommandNeedsTerminal(title, strings.Join(parts, "\n\n"), command)
	}
}

// recordPrivilegedCopyFailure stores the privileged-copy problem with its fix.
//
// The command is part of the MESSAGE rather than a separate field on purpose: it
// is a sudo command the user must run themselves, and the launcher must never
// execute it automatically. Sending it as text keeps it advisory — the frontend
// shows it, it does not run it.
func (ac *AppController) recordPrivilegedCopyFailure(title string, parts []string) {
	if ac == nil {
		return
	}
	ac.RecordLifecycleError(LifecycleErrPrivilegedCopy, "start", title, strings.Join(parts, "\n\n"), true)
}

// classicElevatedUsesCopy — на macOS привилегированный старт идёт своим
// путём (TUN → AEWP, startSingBoxPrivileged), не через повышенный токен.
func classicElevatedUsesCopy() bool { return false }

// elevatedClassicStart — только Windows (SPEC 141 §8).
func (ac *AppController) elevatedClassicStart() (string, *os.File, error) {
	return "", nil, errors.New("elevated classic start is Windows-only")
}

// tunInstallServiceAction — диалога «TUN без прав» на macOS нет.
func (ac *AppController) tunInstallServiceAction() (uiport.UIAction, bool) {
	return uiport.UIAction{}, false
}

// showDaemonCoreUpdatedDialog — «Core updated»: sudo-команда install для
// Terminal.
func (ac *AppController) showDaemonCoreUpdatedDialog(command string) {
	ac.ui().ShowCommandNeedsTerminal(locale.T("Core updated — update the daemon service"), locale.T(daemonCoreUpdatedBodyText), command)
}

// ShowDaemonUnsafeNoticeElevated — на macOS модальное предупреждение
// показывает main.go (sudo-команда для Terminal).
func (ac *AppController) ShowDaemonUnsafeNoticeElevated(_, _, _ string) bool {
	return false
}
