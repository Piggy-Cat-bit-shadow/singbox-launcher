// Core start/stop operation state.
//
// The frontend needs to distinguish three different things that the old single
// "running" boolean collapsed into one:
//
//	1. Is a start/stop REQUEST in flight?      (the button's pending state)
//	2. What is the RUNTIME doing?              (starting → running)
//	3. Did the last attempt FAIL, and why?     (the message on Home)
//
// Conflating them produced the reported symptom: pressing Start returned
// "success" instantly (the IPC call succeeded — the request was merely
// accepted), the pending flag cleared, and `coreState()` then reported the only
// thing it knew, which was that the core was not running yet. The button snapped
// back to "Start" while the start was still in progress, and if the start then
// failed there was nothing left to show that anything had happened.
//
// So: PENDING is about the request, CoreState is about the runtime, and the two
// are deliberately NOT the same value. `coreState()` never impersonates a
// request state, and a pending request never invents a runtime state.

package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/debuglog"
)

// coreOperation is the in-flight start/stop request, if any.
type coreOperation struct {
	// kind is "start", "restart" or "stop".
	kind string
	// startedAt bounds how long the UI will wait before giving up on us.
	startedAt time.Time
}

// adoptOnceState makes the legacy-adoption attempt run at most once per backend.
//
// See adoptLegacyConfig: a repeated failed attempt would cost a config build on
// every snapshot.
type adoptOnceState struct {
	once      sync.Once
	succeeded bool
}

// coreOpState guards the operation record and the last failure.
//
// A dedicated mutex rather than the backend's main one: these fields are written
// from the IPC handler goroutine and read from the state-emit goroutine, and
// holding the snapshot mutex across a start (which can block for tens of seconds
// on a daemon apply) would stall every other IPC call.
type coreOpState struct {
	mu sync.Mutex
	// op is the in-flight operation, nil when idle.
	op *coreOperation
	// lastErr is the most recent start failure, cleared by a successful start.
	//
	// Kept so the state can report `error` with a reason AFTER the attempt has
	// finished. Without it, a failure and a never-attempted start look identical
	// from the outside, which is exactly why the user saw a bare "Start" button.
	lastErr *core.StartFailure
}

// beginOp records a start/stop request as in flight.
//
// Returns false when an operation of the same kind is already running, which is
// how a rapid double click becomes ONE operation instead of two racing starts.
// A different kind REPLACES the record: pressing Stop while a start is in flight
// is a real state change, and the newer intent wins.
func (s *coreOpState) beginOp(kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.op != nil && s.op.kind == kind {
		debuglog.InfoLog("core op: %s already in flight — ignoring duplicate request", kind)
		return false
	}
	s.op = &coreOperation{kind: kind, startedAt: time.Now()}
	if kind == "start" || kind == "restart" {
		// A new attempt clears the previous failure: the message must not
		// outlive the condition it described.
		s.lastErr = nil
	}
	return true
}

// finishOp clears the in-flight record and records the outcome.
//
// A nil err on a start/restart means the start was ACCEPTED (process spawned, or
// the daemon applied the config) — not that the core is already running. The
// running transition arrives separately from the runtime, which is the only
// component that can truthfully report it.
func (s *coreOpState) finishOp(kind string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.op != nil && s.op.kind == kind {
		s.op = nil
	}
	if err == nil {
		return
	}
	var f *core.StartFailure
	if asStartFailure(err, &f) {
		s.lastErr = f
		return
	}
	if isStartAborted(err) {
		// Not a fault: a precondition declined the start. Recording it as an
		// error would put a failure message in front of the user for something
		// that did not go wrong.
		return
	}
	// An unclassified error still deserves to be shown; keep its text.
	s.lastErr = core.NewStartFailure(core.StartErrSpawnFailed, err)
}

// snapshot returns the in-flight operation kind and the last failure.
func (s *coreOpState) snapshot() (kind string, lastErr *core.StartFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.op != nil {
		kind = s.op.kind
	}
	return kind, s.lastErr
}

// clearError drops the remembered failure, for an explicit dismissal.
func (s *coreOpState) clearError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = nil
}

// coreState resolves the runtime state, honouring the documented priority:
//
//	RunningState true      → running   (the runtime is the only source of truth)
//	else op == start/restart → starting
//	else op == stop          → stopping
//	else lastErr != nil      → error
//	else                     → stopped
//
// Running wins over everything: a start that succeeded must never be reported as
// still starting merely because the request bookkeeping has not caught up. The
// pending states likewise outrank a stale error, so a retry shows "starting"
// rather than the previous failure.
func (b *Backend) coreLifecycleState() string {
	kind, lastErr := b.ops.snapshot()
	if b.ac != nil && b.ac.RunningState != nil && b.ac.RunningState.IsRunning() {
		return protocol.CoreStateRunning
	}
	switch kind {
	case "start", "restart":
		return protocol.CoreStateStarting
	case "stop":
		return protocol.CoreStateStopping
	}
	if lastErr != nil {
		return protocol.CoreStateError
	}
	return protocol.CoreStateStopped
}

// coreErrorInfo reports the last start failure as wire fields.
//
// Separate from the state string so the frontend can localize: the stable code
// selects the translated sentence and the detail goes to the log and the
// tooltip, rather than dumping a Go error string into the UI.
func (b *Backend) coreErrorInfo() (code, message string) {
	_, lastErr := b.ops.snapshot()
	if lastErr == nil {
		return "", ""
	}
	return string(lastErr.Code), lastErr.Detail
}

// asStartFailure and isStartAborted keep core's error taxonomy out of this
// file's call sites.
func asStartFailure(err error, target **core.StartFailure) bool {
	f, ok := err.(*core.StartFailure)
	if ok {
		*target = f
	}
	return ok
}

func isStartAborted(err error) bool {
	return err == core.ErrStartAborted
}

// coreOpTimeout bounds how long an IPC start/restart call may block.
//
// The daemon apply budget is the binding constraint: the daemon restarts its
// in-process instance and re-establishes listeners, which is legitimately slow
// on a machine with many outbounds. 45s is comfortably above the observed apply
// time (well under a second locally) while still returning control to the UI
// instead of hanging forever if the daemon wedges.
//
// Hitting this is NOT reported as a start failure: the operation may well
// succeed a moment later, and claiming failure would be a lie. The request
// simply stops WAITING, and the runtime transition reports the truth when it
// arrives.
const coreOpTimeout = 45 * time.Second

// runCoreOp runs a start/restart as one operation and returns a structured
// error on failure.
//
// The sequence is what makes the button behave:
//
//  1. record the request as pending and publish the transition (starting)
//     BEFORE doing any work, so the UI shows progress immediately;
//  2. await the real outcome (process spawned / daemon applied config);
//  3. clear the pending flag and record the failure, if any;
//  4. publish the resulting state.
//
// Step 1 is what was missing: previously the state was published only after a
// fire-and-forget start, at which point it still said "stopped" because nothing
// had happened yet — which is how a successful click turned into an immediate
// revert.
//
// A duplicate request of the same kind is dropped, so a double click starts ONE
// operation.
func (b *Backend) runCoreOp(kind string, timeout time.Duration, fn func(context.Context) error) error {
	if !b.ops.beginOp(kind) {
		// Already running the same operation: report success for the user's
		// intent rather than an error, but do not start a second one.
		b.EmitCoreState()
		return nil
	}
	b.EmitCoreState()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := fn(ctx)

	b.ops.finishOp(kind, err)
	b.EmitCoreState()

	if err == nil {
		return nil
	}
	if errors.Is(err, core.ErrStartAborted) {
		// Nothing failed; the start was declined by a precondition that has
		// already explained itself. Returning a protocol error would put a
		// second, contradictory message on screen.
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		debuglog.WarnLog("core op: %s did not commit within %s — the UI stops waiting, the runtime state is authoritative", kind, timeout)
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return &protocol.Error{Code: string(core.StartErrCancelled),
			Message: "start cancelled", Recoverable: true}
	}

	code := core.StartErrorCodeOf(err)
	if code == "" {
		code = core.StartErrSpawnFailed
	}
	// Recoverable: the user can retry after acting on the reason, and the
	// daemon/port situations are frequently transient.
	return &protocol.Error{
		Code:        string(code),
		Message:     err.Error(),
		Recoverable: true,
	}
}

// runCoreOpFireAndForget publishes the pending transition for an operation whose
// outcome arrives only from the runtime (Stop).
func (b *Backend) runCoreOpFireAndForget(kind string, fn func()) error {
	if !b.ops.beginOp(kind) {
		b.EmitCoreState()
		return nil
	}
	b.EmitCoreState()
	fn()
	return nil
}
