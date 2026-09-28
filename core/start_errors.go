// Structured start failures.
//
// A failed start used to be reported only through ShowStartupError, which ends
// in showErrorUI: it logs, and then does nothing when uiPort is nil. In the
// headless backend — the one the macOS app actually uses — a start failure was
// therefore written to a log file and nowhere else. The button returned to
// "Start" and the user was told nothing.
//
// These types give a start failure a MACHINE-READABLE identity so it can cross
// IPC and be explained in the user's language, while the technical detail stays
// available for the logs and the help tooltip. A raw Go error string is not a
// user-facing message: it names internal functions and cannot be translated.

package core

import (
	"errors"
	"fmt"
	"strings"
)

// StartErrorCode is a stable token identifying why a start failed.
//
// Stable because the frontend maps it to localized copy: renaming one silently
// degrades the explanation to a generic message, so these are treated as wire
// values rather than as internal identifiers.
type StartErrorCode string

const (
	// StartErrConfigRebuildFailed — the pre-start rebuild could not produce a
	// config, so the core was deliberately not started.
	StartErrConfigRebuildFailed StartErrorCode = "config_rebuild_failed"
	// StartErrSpawnFailed — the process/service could not be launched.
	StartErrSpawnFailed StartErrorCode = "core_start_failed"
	// StartErrDaemonUnreachable — the daemon control channel did not answer.
	StartErrDaemonUnreachable StartErrorCode = "daemon_unreachable"
	// StartErrDaemonApplyFailed — the daemon rejected or failed to apply config.
	StartErrDaemonApplyFailed StartErrorCode = "daemon_apply_failed"
	// StartErrConfigCheckFailed — the core refuses the config it was given.
	StartErrConfigCheckFailed StartErrorCode = "config_check_failed"
	// StartErrClashAPIPortInUse — the Clash API controller port is already taken.
	StartErrClashAPIPortInUse StartErrorCode = "clash_api_port_in_use"
	// StartErrCancelled — the start was superseded or the user stopped it.
	StartErrCancelled StartErrorCode = "cancelled"

	// The PRECONDITION codes below report a start that was declined before it
	// began, because something the launcher needs was not in place.
	//
	// They exist so the reason and its remedy can cross IPC. Every one of these
	// situations previously ended in `ErrStartAborted`, which the service layer
	// suppressed on the theory that "the precondition already explained itself" —
	// true only on the GUI path, where a Fyne dialog had been shown. Headless,
	// the user was told nothing at all.
	//
	// StartErrTunElevationRequired — the core needs administrator authorization.
	StartErrTunElevationRequired StartErrorCode = "tun_elevation_required"
	// StartErrPrivilegesRequired — the core lacks the capabilities it needs.
	StartErrPrivilegesRequired StartErrorCode = "privileges_required"
	// StartErrPrivilegedCopyUnavailable — the protected core copy is missing or
	// stale, so the elevated launch cannot proceed safely.
	StartErrPrivilegedCopyUnavailable StartErrorCode = "privileged_copy_unavailable"
	// StartErrForeignCoreRunning — another sing-box already owns the machine, so
	// starting a second one would fight it for the TUN device.
	StartErrForeignCoreRunning StartErrorCode = "foreign_core_running"
)

// StartFailure carries a code plus the underlying cause.
//
// Implements error so it flows through ordinary Go error handling, and keeps the
// cause wrapped (rather than flattened into a string) so the technical detail is
// still available to logs and to `errors.Is`/`errors.As`.
type StartFailure struct {
	Code   StartErrorCode
	Detail string
	cause  error
}

func (e *StartFailure) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail != "" {
		return e.Detail
	}
	return string(e.Code)
}

func (e *StartFailure) Unwrap() error { return e.cause }

// NewStartFailure builds a StartFailure wrapping cause.
func NewStartFailure(code StartErrorCode, cause error) *StartFailure {
	f := &StartFailure{Code: code, cause: cause}
	if cause != nil {
		f.Detail = cause.Error()
	}
	return f
}

// NewStartFailuref builds a StartFailure from a format string.
func NewStartFailuref(code StartErrorCode, format string, args ...any) *StartFailure {
	return &StartFailure{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// ErrStartAborted reports that a start did not proceed because a precondition
// declined it (elevation dialog, capabilities prompt, already-running warning).
//
// Distinct from a FAULT: nothing broke, the core simply was not started. Callers
// report it as a cancelled operation rather than as an error to investigate,
// which is why it maps to StartErrCancelled instead of a failure code.
var ErrStartAborted = errors.New("start aborted before it began")

// StartErrorCodeOf extracts the code from an error chain, or "" when the error
// did not come from the start path.
func StartErrorCodeOf(err error) StartErrorCode {
	if err == nil {
		return ""
	}
	var f *StartFailure
	if errors.As(err, &f) {
		return f.Code
	}
	if errors.Is(err, ErrStartAborted) {
		return StartErrCancelled
	}
	return ""
}

// ClassifyStartErrorText upgrades a start error to a more specific code when its
// text proves what went wrong.
//
// Reuses the SAME patterns the exit-reason classifier uses rather than
// re-detecting port collisions here: two independent regexes for "address
// already in use" would eventually disagree, and the exit-reason path is the one
// already tested against real core output.
//
// Only ever narrows a code that is already a start failure. An error we cannot
// classify keeps the code it came in with — guessing would put a wrong
// explanation in front of the user, which is worse than a generic one.
func ClassifyStartErrorText(code StartErrorCode, text string) StartErrorCode {
	if code != StartErrSpawnFailed && code != StartErrDaemonApplyFailed {
		return code
	}
	low := strings.ToLower(text)
	if rePortInUse.MatchString(low) {
		return StartErrClashAPIPortInUse
	}
	if reUnknownField.MatchString(low) || reDecodeConfig.MatchString(low) {
		return StartErrConfigCheckFailed
	}
	return code
}

// NewClassifiedStartFailure builds a StartFailure whose code is refined by the
// cause's text.
func NewClassifiedStartFailure(code StartErrorCode, cause error) *StartFailure {
	f := NewStartFailure(code, cause)
	if cause != nil {
		f.Code = ClassifyStartErrorText(code, cause.Error())
	}
	return f
}

// PreconditionRefusal reports that a start was declined because a business
// precondition was not met — and, crucially, whether the user was TOLD.
//
// WHY THIS TYPE EXISTS. `ErrStartAborted` conflated two different statements:
// "the user cancelled" and "a precondition declined, and it already explained
// itself". The explanation was always a Fyne dialog, which is true on the GUI
// path and false on the IPC path: with no `uiPort`, a foreign core already
// running, a missing privileged copy, absent Linux capabilities or a TUN
// elevation prompt produced NO dialog — and the service layer then suppressed
// the error. The user pressed Start, nothing happened, and nothing said why.
//
// A refusal therefore carries its own reason, a stable code, and `Silent`, which
// means "nobody has been told". Only a silent refusal is escalated to the
// frontend, so a precondition that did show its own dialog does not produce a
// second, contradictory message.
type PreconditionRefusal struct {
	// Code is the stable token the frontend localizes.
	Code StartErrorCode
	// Message is the human-readable reason.
	Message string
	// Recoverable reports whether acting on the reason can make a retry work.
	Recoverable bool
	// Silent is true when nothing was shown to the user, because there was no UI
	// to show it in.
	Silent bool
	// cause is the underlying error, for logs.
	cause error
}

// Error implements error, so a refusal travels as an ordinary error value and
// keeps the wrapping/propagation behaviour callers already rely on.
func (r *PreconditionRefusal) Error() string {
	if r == nil {
		return ""
	}
	return r.Message
}

// Unwrap exposes the cause to errors.Is/As, so a refusal does not hide the
// underlying problem from a caller that wants to inspect it.
func (r *PreconditionRefusal) Unwrap() error {
	if r == nil {
		return nil
	}
	return r.cause
}

// NewPreconditionRefusal builds a refusal that wraps cause.
func NewPreconditionRefusal(code StartErrorCode, message string, recoverable, silent bool, cause error) *PreconditionRefusal {
	return &PreconditionRefusal{
		Code:        code,
		Message:     message,
		Recoverable: recoverable,
		Silent:      silent,
		cause:       cause,
	}
}

// AsPreconditionRefusal extracts a refusal from an error chain, if present.
func AsPreconditionRefusal(err error) (*PreconditionRefusal, bool) {
	var r *PreconditionRefusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}
