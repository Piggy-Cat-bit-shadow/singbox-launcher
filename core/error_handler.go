package core

import (
	"fmt"

	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
)

// Длинные тексты локализации: ключ = английский текст (SPEC 111).
const (
	parserText  = "Parser failed:\n\n%s\n\nPlease check:\n1. Subscription URL is valid\n2. Network connection\n3. Check parser.log for details"
	startupText = "Failed to start sing-box:\n\n%s\n\nPlease check:\n1. config.json is valid\n2. sing-box executable exists\n3. Check logs for details"
	rebuildText = "Failed to rebuild config.json:\n\n%s\n\nsing-box was not started with the previous config.json: it may not match your current settings. Fix the cause and try again."
)

// showErrorUI logs the error and shows it in the UI if available.
// category is used as a log prefix (e.g. "StartupError", "ParserError").
// showErrorUI reports a user-facing error through every available channel.
//
// THIS IS THE HEADLESS FIX. It used to be log-only when no Fyne UI was attached,
// and the macOS app talks to a HEADLESS backend: there is no uiPort, so every
// error routed through here — startup failure, rebuild failure, parser failure —
// existed only in a log file the user does not read. The SwiftUI client was
// structurally incapable of learning that anything had gone wrong.
//
// Recording into the unified lifecycle store makes each existing caller reach the
// frontend without being rewritten, which is why this is done here rather than at
// the dozen call sites: one seam, every path covered.
func (ac *AppController) showErrorUI(category string, err error) {
	debuglog.ErrorLog("%s: %v", category, err)
	if ac == nil {
		return
	}
	// Only core lifecycle failures belong in the lifecycle store: it is read as
	// "why is my VPN not working", and a parser warning would answer a different
	// question.
	if code, ok := lifecycleCodeForCategory(category); ok {
		ac.RecordLifecycleError(code, lifecycleOperationForCategory(category),
			err.Error(), "", true)
	}
	if ac.hasUI() {
		ac.ui().ShowError(locale.T("Error"), err.Error())
	}
}

// lifecycleCodeForCategory maps a reporting category onto a lifecycle code.
//
// Returns false for categories that are not core lifecycle problems, so the
// store stays meaningful rather than becoming a general error log.
func lifecycleCodeForCategory(category string) (LifecycleErrorCode, bool) {
	switch category {
	case "StartupError":
		return LifecycleErrCoreStart, true
	case "RebuildError":
		return LifecycleErrConfigRebuild, true
	case "StopError":
		return LifecycleErrStopFailed, true
	}
	return "", false
}

// lifecycleOperationForCategory names the operation a category belongs to.
func lifecycleOperationForCategory(category string) string {
	switch category {
	case "StartupError":
		return "start"
	case "RebuildError":
		return "rebuild"
	case "StopError":
		return "stop"
	}
	return ""
}

// ShowStartupError shows an error when sing-box fails to start.
func (ac *AppController) ShowStartupError(err error) {
	ac.showErrorUI("StartupError", fmt.Errorf("%s", locale.Tf(startupText, err.Error())))
}

// ShowRebuildError shows why config.json could not be rebuilt before a core
// start. The start is abandoned rather than carried out on the previous
// config.json (rebuildConfigBeforeStart).
func (ac *AppController) ShowRebuildError(err error) {
	ac.showErrorUI("RebuildError", fmt.Errorf("%s", locale.Tf(rebuildText, err.Error())))
}

// ShowParserError shows an error when parser fails.
func (ac *AppController) ShowParserError(err error) {
	ac.showErrorUI("ParserError", fmt.Errorf("%s", locale.Tf(parserText, err.Error())))
}
