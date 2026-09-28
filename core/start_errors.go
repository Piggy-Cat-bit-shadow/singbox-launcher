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
