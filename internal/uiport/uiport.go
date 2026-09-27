// Package uiport defines the boundary between the headless backend and any GUI.
//
// core must not import a GUI toolkit. Historically AppController held a
// concrete *uiservice.UIService, which dragged fyne.io/fyne and fyne.io/systray
// into the backend's dependency graph even though the backend never
// initialised them.
//
// The port lives in this leaf package so both core and the Fyne presentation
// package can reference it without an import cycle: core depends on uiport,
// uiservice depends on uiport, and neither depends on the other.
//
// A nil Port means "no GUI": the headless backend installs nothing, and every
// call site guards on nil before invoking.
package uiport

import "singbox-launcher/internal/debuglog"

// UIAction is one selectable choice in a ShowActions message.
type UIAction struct {
	// Label is the button caption.
	Label string
	// Command is an optional shell command the action runs.
	Command string
	// OpenTerminal requests running Command in a terminal window.
	OpenTerminal bool
}

// Port is the GUI-facing surface core may call.
type Port interface {
	// RefreshProxyList asks the GUI to re-read the proxy list.
	RefreshProxyList()
	// UpdateCoreStatus asks the GUI to re-render core status.
	UpdateCoreStatus()
	// UpdateConfigStatus asks the GUI to re-render config status.
	UpdateConfigStatus()
	// ResetAPIState asks the GUI to drop cached proxy state.
	ResetAPIState()
	// SetListStatus reports a short line about the proxy list.
	SetListStatus(text string)
	// ReportParserProgress reports subscription parsing progress, 0..1.
	ReportParserProgress(progress float64, status string)
	// ReportSubsResult reports the outcome of a subscription update.
	ReportSubsResult(ok bool, message string)
	// ReportCoreStartAborted reports that a start was aborted before launch.
	ReportCoreStartAborted(reason string)
	// AutoPingAfterConnect triggers the post-connect ping pass.
	AutoPingAfterConnect()
	// QuitApplication asks the GUI to terminate.
	QuitApplication()

	// ShowInfo reports an informational message.
	ShowInfo(title, message string)
	// ShowError reports a failure.
	ShowError(title, message string)
	// ShowCommandNeedsTerminal reports that a command must run in a terminal.
	ShowCommandNeedsTerminal(title, message, command string)
	// ShowActions reports a message with selectable actions.
	ShowActions(title, message string, actions []UIAction, dismissText string)
	// ConfirmKillExistingCore asks the user to confirm killing a running core.
	ConfirmKillExistingCore(onConfirm func())
}

// Headless discards every GUI call, logging the user-visible ones.
//
// Used when core runs without a frontend (the backend helper, tests), so call
// sites need no nil checks for messages.
type Headless struct{}

// Ensure Headless satisfies Port at compile time.
var _ Port = Headless{}

func (Headless) RefreshProxyList()                    {}
func (Headless) UpdateCoreStatus()                    {}
func (Headless) UpdateConfigStatus()                  {}
func (Headless) ResetAPIState()                       {}
func (Headless) SetListStatus(string)                 {}
func (Headless) ReportParserProgress(float64, string) {}
func (Headless) ReportSubsResult(bool, string)        {}
func (Headless) ReportCoreStartAborted(string)        {}
func (Headless) AutoPingAfterConnect()                {}
func (Headless) QuitApplication()                     {}

func (Headless) ShowInfo(title, message string) {
	debuglog.InfoLog("ui(info): %s — %s", title, message)
}

func (Headless) ShowError(title, message string) {
	debuglog.ErrorLog("ui(error): %s — %s", title, message)
}

func (Headless) ShowCommandNeedsTerminal(title, message, command string) {
	debuglog.WarnLog("ui(command): %s — %s: %s", title, message, command)
}

func (Headless) ShowActions(title, message string, _ []UIAction, _ string) {
	debuglog.WarnLog("ui(actions): %s — %s", title, message)
}

// ConfirmKillExistingCore declines: a headless backend must not kill a core
// process on its own initiative.
func (Headless) ConfirmKillExistingCore(func()) {}
