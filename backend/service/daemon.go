// Daemon service: status, setup and pairing for the persistent-service engine.
//
// The daemon engine is NOT a mode value. It depends on a launchd service being
// installed, a paired mTLS identity, and a reachable control plane — any of
// which can be missing. Treating it as a plain enum is why selecting it looked
// like a freeze: the UI flipped a setting and the backend then tried to
// construct a daemon client against a service that was never installed.
//
// So this file separates two operations that the old single click conflated:
//
//	setup   — install, start, pair; each returns a command for the user to run
//	          or an action the backend performs, and none of them switch mode
//	activate — set_core_mode("daemon"), only offered once status says ready
//
// Nothing here re-implements launchd: every command comes from the existing
// DaemonInstallCommand / DaemonBootstrapCommand / DaemonRepairCommand /
// DaemonUninstallCommand builders, and pairing goes through
// PairDaemonWithInvite.

package service

import (
	"strings"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
)

// DaemonStatus reports everything the Daemon screen needs to decide what to
// show, in one call.
//
// The service state comes from the existing classifier (DaemonUIStatus.Service)
// rather than a re-derived guess, so the menu bar and the backend log never
// disagree about whether the installed service is safe, stale or running.
func (b *Backend) DaemonStatus() (protocol.DaemonStatusDTO, error) {
	if b.ac == nil {
		return protocol.DaemonStatusDTO{
			Supported: false,
			Error:     "backend not initialised",
		}, nil
	}
	if !core.DaemonEngineAvailable() {
		return protocol.DaemonStatusDTO{
			Supported: false,
			Service:   protocol.DaemonServiceNotInstalled,
			Error:     "the daemon engine is not available in this build",
		}, nil
	}

	snap := b.ac.DaemonStatusSnapshot()

	status := protocol.DaemonStatusDTO{
		Supported:       true,
		Service:         string(snap.Service.State),
		ServiceDetail:   snap.Service.Detail,
		Installed:       snap.ServiceInstalled,
		Paired:          snap.Paired,
		Address:         snap.Address,
		Reachable:       snap.Reachable,
		CoreStatus:      snap.CoreStatus,
		DaemonVersion:   snap.DaemonVersion,
		RunningVersion:  snap.Service.RunningVersion,
		LauncherVersion: snap.Service.LauncherVersion,
		CoreSupportsLxd: snap.CoreSupportsLxd,
		NeedsInstall:    snap.Service.NeedsInstall(),
		ProtocolStale:   len(snap.MissingRPCs) > 0,
		MissingRPCs:     snap.MissingRPCs,
		NeedsStart:      snap.Service.NeedsBootstrap(),
		ActiveMode:      b.daemonModeActive(),
	}
	// A fingerprint is a long hex string. The UI only needs to know whether a
	// pin exists and which one, truncated; the full value is not actionable in
	// a menu bar and is a pairing secret-adjacent detail.
	if fp := b.daemonFingerprint(); fp != "" {
		status.Fingerprint = truncateFingerprint(fp)
	}
	if snap.LastError != "" {
		status.Error = snap.LastError
	}
	status.PersistsAfterQuit = !b.daemonStopsOnExit()
	status.Ready = status.Installed && status.Paired && status.Reachable
	return status, nil
}

// daemonModeActive reports whether the selected engine is the daemon.
//
// This is an IDENTITY question, not a preference one, and it used to be answered
// with `CorePersistsAfterAppExit()` — the "keep running after quit" policy. With
// the daemon selected and that option OFF (a user who wants the tunnel torn down
// when the app closes), this reported false: the app believed it was on the
// classic engine while the daemon was actually serving the tunnel.
//
// The damage was not just a wrong label. `DaemonStatus.ActiveMode` gates the
// destructive actions on the daemon screen (`active_mode && core.running`), so
// the protection around unpair/remove-service was lifted precisely when a real
// daemon core was running — the one state those guards exist for. The two axes
// are now separate: Active is identity, PersistsAfterQuit is policy, and both are
// reported independently.
func (b *Backend) daemonModeActive() bool {
	if b.ac == nil {
		return false
	}
	return b.ac.BackendMode() == core.BackendDaemon
}

// daemonFingerprint reads the paired server fingerprint from settings.
func (b *Backend) daemonFingerprint() string {
	if b.ac == nil || b.ac.FileService == nil {
		return ""
	}
	return locale.LoadSettings(b.ac.FileService.Layout.Data.Bin()).DaemonServerFingerprint
}

// daemonStopsOnExit reports the stored stop-on-exit preference.
//
// The setting is negative in storage (DaemonStopVPNOnExit); the DTO exposes the
// positive "keep running" phrasing the UI uses, converted here so Swift never
// has to know about the inverted field.
func (b *Backend) daemonStopsOnExit() bool {
	if b.ac == nil || b.ac.FileService == nil {
		return false
	}
	return locale.LoadSettings(b.ac.FileService.Layout.Data.Bin()).DaemonStopVPNOnExit
}

// SetDaemonKeepRunningAfterQuit stores the exit policy.
//
// keepRunning=true means the VPN survives the app quitting, which is the
// inverse of the stored DaemonStopVPNOnExit flag.
func (b *Backend) SetDaemonKeepRunningAfterQuit(keepRunning bool) error {
	if b.ac == nil || b.ac.FileService == nil {
		return &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}
	return b.updateSettings(func(st *locale.Settings) {
		st.DaemonStopVPNOnExit = !keepRunning
	})
}

// truncateFingerprint shortens a SHA-256 pin for display.
func truncateFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	if len(fp) <= 16 {
		return fp
	}
	return fp[:16] + "…"
}

// DaemonCommandResult is one setup step's outcome.
//
// Command is a shell command for the user to run (install, uninstall, repair,
// start). The frontend owns presenting it — copy, or open a terminal — because
// that is desktop presentation, not business logic. `InTerminal` mirrors the
// core's own report for the platforms where the core opens the terminal itself.
type DaemonCommandResult struct {
	// Operation is the step that produced this command ("install", "start",
	// "repair", "uninstall", "fresh_invite").
	Operation string `json:"operation"`
	// Command is the shell command, empty when the operation is not available.
	Command string `json:"command"`
	// Available is false when the core cannot produce the command (for example
	// a core build without the `lxd` subcommand).
	Available bool `json:"available"`
	// Message explains an unavailable step, or reports what to do next.
	Message string `json:"message"`
	// NeedsAdmin marks steps that require administrator rights, so the UI can
	// say so before opening a terminal.
	NeedsAdmin bool `json:"needs_admin"`
	// FollowUp is the step that becomes available afterwards ("pair", or
	// "refresh"), so the UI can guide the sequence instead of leaving the user
	// at a dead end.
	FollowUp string `json:"follow_up"`
	// Status is the refreshed daemon status after the call, so the page does
	// not need a second round trip.
	Status protocol.DaemonStatusDTO `json:"status"`
}

// DaemonInstall returns the install/update command.
func (b *Backend) DaemonInstall() (DaemonCommandResult, error) {
	if b.ac == nil {
		return DaemonCommandResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	cmd, err := b.ac.DaemonInstallCommand()
	result := DaemonCommandResult{
		Operation:  string(core.DaemonOpInstall),
		Command:    cmd,
		Available:  err == nil && cmd != "",
		NeedsAdmin: true,
		FollowUp:   "pair",
	}
	if err != nil {
		result.Message = err.Error()
	} else if cmd == "" {
		result.Message = "this core build cannot install the daemon service"
	} else {
		result.Message = "Run the command in Terminal, then refresh the status. " +
			"The service prints a pairing invite when it finishes."
	}

	b.emit(protocol.EventDaemonChanged, nil)
	status, _ := b.DaemonStatus()
	result.Status = status
	return result, nil
}

// DaemonStart returns the command that loads the installed service.
func (b *Backend) DaemonStart() (DaemonCommandResult, error) {
	if b.ac == nil {
		return DaemonCommandResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	cmd, err := b.ac.DaemonBootstrapCommand()
	result := DaemonCommandResult{
		Operation:  string(core.DaemonOpStart),
		Command:    cmd,
		Available:  err == nil && cmd != "",
		NeedsAdmin: true,
		FollowUp:   "refresh",
	}
	if err != nil {
		result.Message = err.Error()
	} else if cmd == "" {
		result.Message = "this core build cannot start the daemon service"
	} else {
		result.Message = "Run the command in Terminal, then refresh the status."
	}

	b.emit(protocol.EventDaemonChanged, nil)
	status, _ := b.DaemonStatus()
	result.Status = status
	return result, nil
}

// DaemonRepairCommand returns the re-pairing command.
//
// Re-pairing is needed when the launcher lost its client identity but the
// service is healthy: the command asks the service for a fresh one-time invite.
func (b *Backend) DaemonRepair() (DaemonCommandResult, error) {
	if b.ac == nil {
		return DaemonCommandResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	cmd := b.ac.DaemonRepairCommand()
	result := DaemonCommandResult{
		Operation:  string(core.DaemonOpFreshInvite),
		Command:    cmd,
		Available:  cmd != "",
		NeedsAdmin: true,
		FollowUp:   "pair",
	}
	if cmd == "" {
		result.Message = "this core build cannot create a pairing invite"
	} else {
		result.Message = "Run the command in Terminal; it prints a fresh invite. " +
			"Paste that invite here to pair."
	}

	b.emit(protocol.EventDaemonChanged, nil)
	status, _ := b.DaemonStatus()
	result.Status = status
	return result, nil
}

// DaemonUninstall returns the service-removal command.
func (b *Backend) DaemonUninstall(purge bool) (DaemonCommandResult, error) {
	if b.ac == nil {
		return DaemonCommandResult{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	cmd := b.ac.DaemonUninstallCommand(purge)
	result := DaemonCommandResult{
		Operation:  string(core.DaemonOpUninstall),
		Command:    cmd,
		Available:  cmd != "",
		NeedsAdmin: true,
		FollowUp:   "refresh",
	}
	if purge {
		result.Message = "Run the command in Terminal to remove the service and all daemon data."
	} else {
		result.Message = "Run the command in Terminal to remove the service, keeping the core copy."
	}

	b.emit(protocol.EventDaemonChanged, nil)
	status, _ := b.DaemonStatus()
	result.Status = status
	return result, nil
}

// PairDaemon completes pairing from a pasted invite.
//
// The invite is the only credential the user handles: the backend parses it,
// enrols the client identity and stores the address and fingerprint. The
// long-lived secret never crosses the IPC boundary in either direction.
func (b *Backend) PairDaemon(invite string) (protocol.DaemonStatusDTO, error) {
	if b.ac == nil {
		return protocol.DaemonStatusDTO{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	invite = strings.TrimSpace(invite)
	if invite == "" {
		return protocol.DaemonStatusDTO{}, &protocol.Error{
			Code: "bad_request", Message: "paste the invite printed by the service", Recoverable: false,
		}
	}

	// The invite format is address#fingerprint#code; a malformed one is a paste
	// problem, and saying so beats an opaque parse error from deep inside.
	if strings.Count(invite, "#") < 2 {
		return protocol.DaemonStatusDTO{}, &protocol.Error{
			Code:        "bad_invite",
			Message:     "that does not look like an invite — expected address#fingerprint#code",
			Recoverable: false,
		}
	}

	// The secret accompanies the invite on the platforms where the service
	// generates one; on macOS the invite carries everything needed and an empty
	// secret is correct.
	if err := b.ac.PairDaemonWithInvite(invite, ""); err != nil {
		return protocol.DaemonStatusDTO{}, &protocol.Error{
			Code:        "pair_failed",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: daemon paired")
	b.emit(protocol.EventDaemonChanged, nil)
	return b.DaemonStatus()
}

// UnpairDaemonForget drops the local pairing.
func (b *Backend) UnpairDaemonForget() (protocol.DaemonStatusDTO, error) {
	if b.ac == nil {
		return protocol.DaemonStatusDTO{}, &protocol.Error{
			Code: "not_ready", Message: "backend not initialised", Recoverable: true,
		}
	}

	if err := b.ac.UnpairDaemon(); err != nil {
		return protocol.DaemonStatusDTO{}, &protocol.Error{
			Code:        "unpair_failed",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: daemon unpaired")
	b.emit(protocol.EventDaemonChanged, nil)
	return b.DaemonStatus()
}
