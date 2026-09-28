// Lifecycle errors: one place where every runtime failure is recorded, and one
// place the frontend reads it from.
//
// # THE PROBLEM THIS SOLVES
//
// Core failures used to be reported by whichever layer noticed them, each in its
// own way: `ShowStartupError` on the Fyne UI port, `ShowRebuildError` likewise, a
// log line, or — most often — nothing at all. The Fyne paths are log-only when
// no UI port is attached, which is exactly the case for the headless backend the
// macOS SwiftUI app talks to. So the SwiftUI app was structurally unable to learn
// that a rebuild failed, that authorization was cancelled, that the privileged
// copy was missing, or that the core had been rejected: the information existed
// only in a log file the user does not read.
//
// The `ConfigBuilt{OK:false}` event had no backend subscriber at all, so a config
// that failed `sing-box check` — and therefore left the OLD config on disk — was
// invisible to the UI, which happily kept showing "connected".
//
// # THE MODEL
//
// A lifecycle error is `(code, operation, message, detail, recoverable)`. It is
// recorded once, on the controller, and read by `coreState()`. Because it lives
// in the snapshot as well as in the change event, a frontend that was not
// listening (or restarted) still recovers it — which is what makes the failure
// state survive a GUI restart instead of silently disappearing.
//
// Codes are stable tokens, never prose: the frontend localizes them, and a code
// that changes breaks the user-facing message without any compile error.
package core

import (
	"strings"
	"sync"
	"time"

	"singbox-launcher/core/events"
	"singbox-launcher/internal/debuglog"
)

// LifecycleErrorCode names a runtime failure class. Stable wire values.
type LifecycleErrorCode string

const (
	// LifecycleErrConfigRebuild — the pre-start rebuild failed, so nothing was
	// started. The previous config is untouched.
	LifecycleErrConfigRebuild LifecycleErrorCode = "config_rebuild_failed"
	// LifecycleErrConfigCheck — the core rejected the freshly built config.
	LifecycleErrConfigCheck LifecycleErrorCode = "config_check_failed"
	// LifecycleErrConfigReferences — the built config had dangling references.
	LifecycleErrConfigReferences LifecycleErrorCode = "config_references_failed"
	// LifecycleErrCoreStart — spawning the core process failed.
	LifecycleErrCoreStart LifecycleErrorCode = "core_start_failed"
	// LifecycleErrPortInUse — the Clash API port is held by someone else.
	LifecycleErrPortInUse LifecycleErrorCode = "clash_api_port_in_use"
	// LifecycleErrAuthCancelled — the user dismissed the authorization dialog.
	LifecycleErrAuthCancelled LifecycleErrorCode = "authorization_cancelled"
	// LifecycleErrAuthTimeout — authorization did not complete in time. The
	// operation continues in the background; see the late-result handling.
	LifecycleErrAuthTimeout LifecycleErrorCode = "authorization_timeout"
	// LifecycleErrPrivilegedCopy — the protected core copy is missing or stale.
	LifecycleErrPrivilegedCopy LifecycleErrorCode = "privileged_copy_unavailable"
	// LifecycleErrPermission — the OS refused the operation (permissions).
	LifecycleErrPermission LifecycleErrorCode = "permission_denied"
	// LifecycleErrFastExit — the core exited immediately after starting, so it
	// never reached readiness.
	LifecycleErrFastExit LifecycleErrorCode = "core_fast_exit"
	// LifecycleErrRestartExhausted — auto-restart gave up.
	LifecycleErrRestartExhausted LifecycleErrorCode = "restart_exhausted"
	// LifecycleErrStopFailed — stopping did not complete; the process may still
	// be running.
	LifecycleErrStopFailed LifecycleErrorCode = "stop_failed"
	// LifecycleErrDaemonUnreachable — the daemon could not be contacted.
	LifecycleErrDaemonUnreachable LifecycleErrorCode = "daemon_unreachable"
	// LifecycleErrDaemonApply — the daemon refused or failed the apply.
	LifecycleErrDaemonApply LifecycleErrorCode = "daemon_apply_failed"
	// LifecycleErrCancelled — the operation was superseded (mode switch,
	// shutdown) rather than failing.
	LifecycleErrCancelled LifecycleErrorCode = "cancelled"
)

// LifecycleErrorSnapshot is a read-only copy of a recorded failure.
//
// Exported because the backend layer must report it over the wire, and it cannot
// name an unexported type. A snapshot rather than a pointer to the live record:
// callers outside this package have no business mutating it.
type LifecycleErrorSnapshot struct {
	Code        LifecycleErrorCode
	Operation   string
	Message     string
	Detail      string
	Recoverable bool
	ConfigError bool
	At          time.Time
}

// lifecycleError is one recorded failure.
type lifecycleError struct {
	Code        LifecycleErrorCode
	Operation   string
	Message     string
	Detail      string
	Recoverable bool
	// ConfigError marks a failure from building/activating the config rather
	// than from the process. Decides the "your previous config is still in use"
	// reassurance, which is only true for these.
	ConfigError bool
	At          time.Time
}

// lifecycleErrors is the store. One per AppController.
//
// A mutex rather than atomics because the value is a struct with several fields
// and readers must see a consistent set (a code from one failure next to the
// detail of another would be worse than a lock).
type lifecycleErrors struct {
	mu   sync.Mutex
	last *lifecycleError
	// seq increments on every record. Used to let the frontend tell a NEW
	// failure from a re-read of the same one without comparing message text.
	seq uint64
}

// record stores a failure, replacing any previous one.
//
// Deliberately last-write-wins: the newest failure is the one the user needs.
// Keeping a history here would mean deciding which entry the UI shows, and the
// log already has the history.
func (l *lifecycleErrors) record(e lifecycleError) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	l.mu.Lock()
	l.last = &e
	l.seq++
	l.mu.Unlock()

	// Mirrored to the log with the code, so a log reader can grep a class of
	// failure rather than guess at message wording.
	if e.Detail != "" {
		debuglog.WarnLog("lifecycle error [%s] operation=%s: %s (%s)", e.Code, e.Operation, e.Message, e.Detail)
	} else {
		debuglog.WarnLog("lifecycle error [%s] operation=%s: %s", e.Code, e.Operation, e.Message)
	}
}

// snapshot returns the current failure, or nil.
func (l *lifecycleErrors) snapshot() *lifecycleError {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		return nil
	}
	cp := *l.last
	return &cp
}

// clear drops the recorded failure.
//
// Called when an operation genuinely starts, not when it is merely accepted: a
// stale error must not outlive the retry that is already under way, but it also
// must not be cleared so early that a failed start reports "no error".
func (l *lifecycleErrors) clear() {
	l.mu.Lock()
	l.last = nil
	l.seq++
	l.mu.Unlock()
}

// hasError reports whether a failure is currently recorded.
func (l *lifecycleErrors) hasError() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last != nil
}

// RecordLifecycleError is the package-level entry point used by call sites that
// hold an AppController. Safe on a nil controller.
//
// Publishing VpnStateChanged after recording is what carries the failure to the
// frontend LIVE. That event is already bridged to the protocol and already
// triggers an EmitCoreState() in the backend, so a recorded failure reaches
// SwiftUI through a path that is known to work, instead of needing new plumbing
// that could quietly not be wired up. The payload's Running flag reports the
// real running state; the failure itself rides in the CoreState that follows.
func (ac *AppController) RecordLifecycleError(code LifecycleErrorCode, operation, message, detail string, recoverable bool) {
	if ac == nil {
		return
	}
	ac.lifecycleErr.record(lifecycleError{
		Code:        code,
		Operation:   operation,
		Message:     message,
		Detail:      detail,
		Recoverable: recoverable,
		ConfigError: isConfigErrorCode(code),
	})
	ac.publishLifecycleChange()
}

// BeginDaemonStop publishes "a stop has been accepted but is not yet confirmed".
//
// The daemon engine has no classic phase, so this is the only place the stop can
// be recorded. Without it the wire state reported `running` for the whole
// teardown — the core is genuinely still alive at that point, so nothing else in
// the state derivation could tell that a stop was in progress.
//
// It deliberately does NOT touch RunningState: the core is still up, and
// claiming otherwise before the daemon confirms it is the exact lie this seam
// exists to prevent.
func (ac *AppController) BeginDaemonStop() {
	if ac == nil {
		return
	}
	if b, ok := ac.Backend().(stopStateBackend); ok {
		b.SetStopping(true)
	}
	ac.publishLifecycleChange()
}

// EndDaemonStop settles a daemon stop. confirmed=false keeps the "not stopped"
// truth: the failure has already been recorded, and the state must continue to
// show a running core rather than a stopped one.
func (ac *AppController) EndDaemonStop(confirmed bool) {
	if ac == nil {
		return
	}
	if b, ok := ac.Backend().(stopStateBackend); ok {
		b.SetStopping(false)
	}
	if confirmed {
		// The daemon itself said the core is gone, so "stopped" is now an earned
		// statement rather than an assumption. This is the only place the daemon
		// path is allowed to clear the running flag.
		ac.RunningState.Set(false)
	} else {
		// The core may still be up. Re-assert the running truth so a stale
		// "stopped" cannot leak to the UI, and let the recorded error carry the
		// explanation.
		ac.RunningState.Set(true)
	}
	ac.publishLifecycleChange()
}

// EmitCoreStateChange nudges listeners that the core's state changed.
//
// Exported for paths that change the running state WITHOUT going through
// RunningState.Set — the late privileged adoption, which records ownership
// directly. Routing those through here keeps the frontend's view of a
// background adoption as current as a user-initiated start.
func (ac *AppController) EmitCoreStateChange() {
	ac.publishLifecycleChange()
}

// publishLifecycleChange nudges listeners that the lifecycle picture changed.
func (ac *AppController) publishLifecycleChange() {
	if ac == nil || ac.EventBus == nil {
		return
	}
	running := ac.RunningState != nil && ac.RunningState.IsRunning()
	ac.EventBus.Publish(events.Event{
		Kind:    events.VpnStateChanged,
		Payload: events.VpnStateChangedPayload{Running: running},
	})
}

// RecordConfigError records a config-pipeline failure.
func (ac *AppController) RecordConfigError(code LifecycleErrorCode, operation, message, detail string) {
	if ac == nil {
		return
	}
	ac.lifecycleErr.record(lifecycleError{
		Code:        code,
		Operation:   operation,
		Message:     message,
		Detail:      detail,
		Recoverable: true,
		ConfigError: true,
	})
	ac.publishLifecycleChange()
}

// ClearLifecycleError drops any recorded failure.
//
// Only publishes when something was actually cleared: a clean rebuild runs this
// on every successful build, and firing an event each time would turn a routine
// operation into a stream of redundant state pushes.
func (ac *AppController) ClearLifecycleError() {
	if ac == nil {
		return
	}
	if !ac.lifecycleErr.hasError() {
		return
	}
	ac.lifecycleErr.clear()
	ac.publishLifecycleChange()
}

// LifecycleError exposes the recorded failure for tests and for coreState().
func (ac *AppController) LifecycleError() *LifecycleErrorSnapshot {
	if ac == nil {
		return nil
	}
	live := ac.lifecycleErr.snapshot()
	if live == nil {
		return nil
	}
	return &LifecycleErrorSnapshot{
		Code:        live.Code,
		Operation:   live.Operation,
		Message:     live.Message,
		Detail:      live.Detail,
		Recoverable: live.Recoverable,
		ConfigError: live.ConfigError,
		At:          live.At,
	}
}

// HasLifecycleError reports whether a failure is recorded.
func (ac *AppController) HasLifecycleError() bool {
	if ac == nil {
		return false
	}
	return ac.lifecycleErr.hasError()
}

// isConfigErrorCode reports whether a code belongs to the config pipeline.
func isConfigErrorCode(code LifecycleErrorCode) bool {
	switch code {
	case LifecycleErrConfigRebuild, LifecycleErrConfigCheck, LifecycleErrConfigReferences:
		return true
	}
	return false
}

// ClassifyLifecycleError maps a raw error from the start path onto a stable code.
//
// This is the single classifier: the alternative is each call site inventing its
// own `if strings.Contains(err, ...)`, which drifts the moment a message is
// reworded. Text matching happens ONCE, here, over the messages the lower layers
// actually produce.
func ClassifyLifecycleError(err error) (LifecycleErrorCode, bool) {
	if err == nil {
		return "", true
	}
	// A structured StartFailure already carries its own code and has been
	// classified where the real cause was known.
	if code := StartErrorCodeOf(err); code != "" {
		return mapStartCodeToLifecycle(code), startErrorRecoverable(code)
	}
	msg := strings.ToLower(err.Error())
	switch {
	// "address already in use" is the message the OS and the core actually
	// produce; requiring the word "port" next to it missed every real instance
	// and silently downgraded a port conflict to a generic start failure — which
	// is exactly the distinction the user needs (the port is occupied, retrying
	// will not help until it is freed).
	case strings.Contains(msg, "address already in use"),
		strings.Contains(msg, "port is already allocated"),
		strings.Contains(msg, "bind: address already"):
		return LifecycleErrPortInUse, false
	case strings.Contains(msg, "permission") || strings.Contains(msg, "operation not permitted"):
		return LifecycleErrPermission, false
	case strings.Contains(msg, "cancel"):
		return LifecycleErrAuthCancelled, false
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out"):
		return LifecycleErrAuthTimeout, true
	}
	return LifecycleErrCoreStart, true
}

// mapStartCodeToLifecycle converts a core start code to its lifecycle code.
func mapStartCodeToLifecycle(code StartErrorCode) LifecycleErrorCode {
	switch code {
	case StartErrConfigRebuildFailed:
		return LifecycleErrConfigRebuild
	case StartErrConfigCheckFailed:
		return LifecycleErrConfigCheck
	case StartErrSpawnFailed:
		return LifecycleErrCoreStart
	case StartErrDaemonUnreachable:
		return LifecycleErrDaemonUnreachable
	case StartErrDaemonApplyFailed:
		return LifecycleErrDaemonApply
	case StartErrClashAPIPortInUse:
		return LifecycleErrPortInUse
	case StartErrCancelled:
		return LifecycleErrCancelled
	}
	return LifecycleErrCoreStart
}

// startErrorRecoverable reports whether retrying could plausibly help.
//
// A deterministic failure (bad config, occupied port, missing permissions) stays
// a failure however many times it is retried; presenting it as retryable is what
// makes a user click Start forever.
func startErrorRecoverable(code StartErrorCode) bool {
	switch code {
	// A port held by another process does NOT clear itself: retrying the same
	// action produces the same failure until the port is freed, so advertising
	// it as retryable invites the user to click Start in a loop. It is listed
	// here as NOT recoverable deliberately.
	case StartErrConfigRebuildFailed, StartErrConfigCheckFailed,
		StartErrDaemonUnreachable:
		return true
	}
	return false
}
