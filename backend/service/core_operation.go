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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/debuglog"
)

// coreOperation is the in-flight start/stop request, if any.
//
// An operation carries an IDENTITY, not just a kind. The kind alone cannot
// answer the question every late callback has to ask — "is the world still the
// one I was started for?" — because kinds repeat: start, stop, start again. With
// only a kind, a finishing operation could not tell whether the record it was
// clearing belonged to itself or to the newer request that had replaced it, and
// a stale result could write its outcome into a world it no longer owned.
//
// The ID is monotonic per backend and never reused, so "op 7 finished" is
// unambiguous even after ops 8 and 9 have come and gone.
type coreOperation struct {
	// id is unique and monotonically increasing. Never reused.
	id uint64
	// kind is "start", "restart" or "stop".
	kind string
	// startedAt bounds how long the UI will wait before giving up on us.
	startedAt time.Time
	// cancel aborts this operation's work. It is called when a NEWER operation
	// supersedes this one, which is what makes "newer intent wins" a statement
	// about behaviour rather than about a bookkeeping field.
	//
	// nil for a stop, which is a teardown that must run to completion: aborting
	// it halfway would leave the core in whatever state the half-finished
	// teardown produced. A stop is never cancelled — it supersedes others.
	cancel context.CancelFunc
	// superseded is closed by the state machine when this operation loses its
	// claim to the record. Work that cannot be cancelled by context alone can
	// select on it, and every result is rejected by ID anyway.
	superseded chan struct{}
	// supersededOnce guards the close, since supersede can race with settle.
	supersededOnce sync.Once
}

// markSuperseded signals that this operation no longer owns the world.
func (o *coreOperation) markSuperseded() {
	if o == nil {
		return
	}
	o.supersededOnce.Do(func() { close(o.superseded) })
}

// isSuperseded reports whether this operation has lost ownership.
func (o *coreOperation) isSuperseded() bool {
	if o == nil {
		return true
	}
	select {
	case <-o.superseded:
		return true
	default:
		return false
	}
}

// adoptOnceState makes the legacy-adoption attempt run at most once per backend.
//
// See adoptLegacyConfig: a repeated failed attempt would cost a config build on
// every snapshot.
type adoptOnceState struct {
	once      sync.Once
	succeeded bool
}

// coreOpState is the lifecycle state machine for start/stop requests.
//
// It answers three questions that the old single "running" boolean collapsed
// into one, and it answers them for a specific operation GENERATION:
//
//  1. Is a start/stop REQUEST in flight, and which one?  (identity, for the UI)
//  2. What is the RUNTIME doing?                          (starting → running)
//  3. Did the last attempt FAIL, and why?                 (the message on Home)
//
// THE ONE INVARIANT THAT MATTERS: an operation's result is committed only if
// that operation still owns the record. Every entry point takes the ID and
// compares it. A superseded operation is a complete no-op — it cannot clear the
// record, set an error, or publish a state, no matter how late it returns.
//
// A dedicated mutex rather than the backend's main one: these fields are written
// from the IPC handler goroutine and read from the state-emit goroutine, and
// holding the snapshot mutex across a start (which can block for tens of seconds
// on a daemon apply) would stall every other IPC call.
type coreOpState struct {
	mu sync.Mutex
	// op is the in-flight operation, nil when idle.
	op *coreOperation
	// nextID allocates operation identities.
	nextID uint64
	// lastErr is the most recent start failure, cleared by a successful start.
	//
	// Kept so the state can report `error` with a reason AFTER the attempt has
	// finished. Without it, a failure and a never-attempted start look identical
	// from the outside, which is exactly why the user saw a bare "Start" button.
	lastErr *core.StartFailure
	// stopping is set while a stop has been accepted but not yet confirmed by
	// the engine that owns the core. It exists because the daemon engine has no
	// classic phase, so without it the wire state had no way to say "a stop is
	// in progress" and reported `running` for the whole teardown.
	stopping atomic.Bool
}

// beginOp records a start/stop request as in flight and returns its identity.
//
// Returns (op, false) when a request of the same kind is already running, which
// is how a rapid double click becomes ONE operation instead of two racing
// starts. The returned op is the EXISTING one in that case, so a caller that
// wants to report on the running operation can.
//
// A request of a DIFFERENT kind supersedes the current one, and superseding is
// an ACTION, not a field assignment: the outgoing operation's cancel func is
// invoked, so the old work actually stops. Previously this only overwrote the
// record and the old goroutine kept running to completion — the comment claimed
// "newer intent wins" while the behaviour was "both intents run, and the loser
// may finish last and win anyway".
func (s *coreOpState) beginOp(kind string) (*coreOperation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.op != nil && s.op.kind == kind {
		debuglog.InfoLog("core op: %s already in flight (id=%d) — ignoring duplicate request", kind, s.op.id)
		return s.op, false
	}

	prev := s.op
	s.nextID++
	op := &coreOperation{
		id:         s.nextID,
		kind:       kind,
		startedAt:  time.Now(),
		superseded: make(chan struct{}),
	}
	if kind == "start" || kind == "restart" {
		// A new start is the thing that can be cancelled; a stop is a teardown
		// that must run to completion.
		op.cancel = nil // set by the caller that owns the context
		// A new attempt clears the previous failure: the message must not
		// outlive the condition it described.
		s.lastErr = nil
	}
	s.op = op

	// Supersede the outgoing operation OUTSIDE the meaning of the record, but
	// still under the lock: the flag and the cancel must both be visible before
	// the new record is, so the old work cannot observe a half-updated world.
	if prev != nil {
		debuglog.InfoLog("core op: %s (id=%d) superseded by %s (id=%d)", prev.kind, prev.id, kind, op.id)
		prev.markSuperseded()
		if prev.cancel != nil {
			prev.cancel()
		}
	}
	return op, true
}

// attachCancel gives an operation its cancellation handle, for callers that
// build the context after registering the operation.
//
// Refuses if the operation has already been superseded: installing a cancel func
// on a dead operation would let a future supersede call it, which is harmless,
// but it would also imply the operation is live when it is not. Reporting the
// refusal keeps that honest.
func (s *coreOpState) attachCancel(op *coreOperation, cancel context.CancelFunc) bool {
	if op == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if op.isSuperseded() {
		if cancel != nil {
			cancel()
		}
		return false
	}
	op.cancel = cancel
	return true
}

// settleResult reports what happened when an operation tried to finish.
type settleResult int

const (
	// settleCommitted: this operation still owned the record; the result is in.
	settleCommitted settleResult = iota
	// settleStale: a newer operation owns the record. NOTHING was written —
	// not the record, not the error, not the state. A stale result is a no-op.
	settleStale
	// settleUnknown: the operation was not the current one and the record is
	// now held by something else (or nothing). Treated exactly like stale.
	settleUnknown
)

// finishOp commits an operation's outcome, but ONLY if that operation still owns
// the record.
//
// The ID check is the whole point. The previous version cleared the record only
// when the kind matched, but recorded the error UNCONDITIONALLY — so a start
// that was superseded by a stop and then failed later would still overwrite the
// new operation's error, and the user would see a start failure reported against
// a stop they had just requested.
//
// A nil err on a start/restart means the start was ACCEPTED (process spawned, or
// the daemon applied the config) — not that the core is already running. The
// running transition arrives separately from the runtime, which is the only
// component that can truthfully report it.
func (s *coreOpState) finishOp(op *coreOperation, err error) settleResult {
	if op == nil {
		return settleUnknown
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.op == nil || s.op.id != op.id {
		// A newer operation owns the world. This result describes a world that
		// no longer exists, so it must change nothing at all.
		debuglog.InfoLog("core op: ignoring stale result for %s (id=%d); current is %s",
			op.kind, op.id, describeOp(s.op))
		return settleStale
	}

	s.op = nil
	op.markSuperseded()

	if err == nil {
		return settleCommitted
	}
	if s.commitErrorLocked(err) {
		return settleCommitted
	}
	return settleCommitted
}

// commitErrorLocked records a failure, reporting whether it was stored.
//
// Callers must hold s.mu and must have already established that the error
// belongs to the current world.
func (s *coreOpState) commitErrorLocked(err error) bool {
	var f *core.StartFailure
	if asStartFailure(err, &f) {
		s.lastErr = f
		return true
	}
	if isStartAborted(err) {
		// Not a fault: a precondition declined the start. Recording it as an
		// error would put a failure message in front of the user for something
		// that did not go wrong.
		//
		// NOTE: this deliberately does NOT mean "the user was already told".
		// A precondition that only knows how to explain itself through a GUI
		// dialog explains nothing on the headless IPC path; those preconditions
		// must carry a structured reason instead of relying on this branch.
		// See preconditionRefusal.
		return false
	}
	// An unclassified error still deserves to be shown; keep its text.
	s.lastErr = core.NewStartFailure(core.StartErrSpawnFailed, err)
	return true
}

// snapshotOp returns the current operation, for a caller that needs its
// identity (to attach a cancel, or to await its settlement).
func (s *coreOpState) snapshotOp() *coreOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.op
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

// describeOp renders an operation for a log line, tolerating nil.
func describeOp(op *coreOperation) string {
	if op == nil {
		return "none"
	}
	return fmt.Sprintf("%s(id=%d)", op.kind, op.id)
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

	// An ACCEPTED STOP outranks everything except a live runtime that has not
	// been told to stop yet.
	//
	// This is the "stopping" case, and it is not cosmetic: while a stop is in
	// flight the core is usually STILL ALIVE, so RunningState is legitimately
	// true. Reporting "running" then would tell the UI there is nothing in
	// progress, and the user would watch a screen that never acknowledges the
	// stop they just requested. The daemon path additionally marks the stop
	// explicitly, because its engine has no classic phase to carry it.
	//
	// A stop is a terminal intent: once accepted it stays visible until the
	// runtime confirms the core is gone.
	if kind == "stop" || b.ops.stopping.Load() || b.daemonStopping() {
		return protocol.CoreStateStopping
	}

	// The classic runtime's PHASE is authoritative when it exists, because it is
	// the only thing that knows the difference between "a start was accepted and
	// is still authorizing" and "nothing is happening".
	//
	// It is consulted AFTER the stop, not before: a stop that has been accepted
	// outranks a phase that still says running, which is the whole point of
	// reporting `stopping` while the core is alive.
	if phase, ok := b.classicPhase(); ok {
		if state := phaseToWireState(phase); state != "" {
			// A settled phase must not contradict an ACCEPTED START that the
			// runtime has not observed yet.
			//
			// This is the window between the request being accepted and the
			// runtime recording that it is starting, and it is not empty: the
			// classic phase only becomes `starting` deep inside the start body,
			// after the template refresh and the build. For that whole window the
			// phase still reads "stopped", and reporting it would show an idle
			// core for a start that is demonstrably under way — the reported
			// symptom, where a successful click appeared to revert instantly.
			//
			// An accepted start with no runtime transition yet is STARTING. The
			// request record is the only component that knows about this window,
			// so it is the one that must speak for it.
			acceptedStartPending := (kind == "start" || kind == "restart") &&
				(state == protocol.CoreStateStopped || state == protocol.CoreStateError)
			if !acceptedStartPending {
				// A recorded failure is reported as an error even though the
				// phase says stopped: the user needs the reason, not just the
				// fact that nothing runs.
				if state == protocol.CoreStateStopped && b.hasLifecycleError() {
					return protocol.CoreStateError
				}
				return state
			}
		}
	}

	switch kind {
	case "start", "restart":
		if b.ac != nil && b.ac.RunningState != nil && b.ac.RunningState.IsRunning() {
			// The start already committed and the core is up: a lingering
			// bookkeeping entry must not hide a working VPN behind "starting".
			return protocol.CoreStateRunning
		}
		return protocol.CoreStateStarting
	}
	if b.ac != nil && b.ac.RunningState != nil && b.ac.RunningState.IsRunning() {
		return protocol.CoreStateRunning
	}
	if lastErr != nil {
		return protocol.CoreStateError
	}
	if b.hasLifecycleError() {
		return protocol.CoreStateError
	}
	return protocol.CoreStateStopped
}

// daemonStopping reports whether the daemon engine has an unconfirmed stop in
// flight. The daemon has no classic phase, so this is its only way to say
// "stopping" on the wire.
func (b *Backend) daemonStopping() bool {
	if b == nil || b.ac == nil {
		return false
	}
	if s, ok := b.ac.Backend().(interface{ Stopping() bool }); ok {
		return s.Stopping()
	}
	return false
}

// classicPhase reads the classic runtime phase, if the runtime is available.
func (b *Backend) classicPhase() (core.ClassicPhase, bool) {
	if b.ac == nil {
		return "", false
	}
	return b.ac.ClassicPhase(), true
}

// phaseToWireState maps a runtime phase onto the wire state.
//
// Returns "" for phases that carry no wire meaning on their own, letting the
// caller fall through to the operation record.
func phaseToWireState(p core.ClassicPhase) string {
	switch p {
	case core.ClassicStarting:
		return protocol.CoreStateStarting
	case core.ClassicRunning:
		return protocol.CoreStateRunning
	case core.ClassicStopping, core.ClassicRestarting:
		return protocol.CoreStateStopping
	case core.ClassicFailed:
		return protocol.CoreStateError
	case core.ClassicStopped:
		return protocol.CoreStateStopped
	}
	return ""
}

// hasLifecycleError reports whether the unified error store holds a failure.
func (b *Backend) hasLifecycleError() bool {
	return b.ac != nil && b.ac.HasLifecycleError()
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

// preconditionRefusal describes a start that was declined because a business
// precondition was not met, in a form the headless frontend can act on.
//
// WHY THIS EXISTS. `core.ErrStartAborted` used to mean "a precondition declined
// the start, and it has already explained itself" — and the explanation was
// always a Fyne dialog. That is true on the GUI path and false on the IPC path:
// with no `uiPort`, a missing privileged copy, a foreign core already running,
// or absent Linux capabilities produced NO dialog and then `ErrStartAborted`,
// which this layer deliberately suppressed. The user pressed Start, nothing
// happened, and nothing said why. "Aborted" was being read as "already
// explained" when on this path it meant "explained to nobody".
//
// So a precondition now carries its own reason, its stable code, and whether
// anything was actually shown to a human. Silent means "nobody has been told",
// and the refusal must travel to the frontend as a real error.
type preconditionRefusal struct {
	// Code is the stable token the frontend localizes.
	Code core.StartErrorCode
	// Message is the human-readable reason.
	Message string
	// Recoverable reports whether acting on the reason can make a retry work.
	Recoverable bool
	// Silent is true when the precondition did NOT show anything to the user,
	// because there was no UI to show it in. Only a silent refusal is escalated
	// to the frontend; one that already showed a dialog would otherwise produce
	// a second, contradictory message.
	Silent bool
}

// Error lets a refusal travel as an ordinary error.
func (r *preconditionRefusal) Error() string { return r.Message }

// asPreconditionRefusal extracts a structured refusal, if the error carries one.
func asPreconditionRefusal(err error) (*preconditionRefusal, bool) {
	var r *preconditionRefusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
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
	op, started := b.ops.beginOp(kind)
	if !started {
		// Already running the same operation: report success for the user's
		// intent rather than an error, but do not start a second one.
		b.EmitCoreState()
		return nil
	}
	b.EmitCoreState()

	// The context is CANCELLABLE BY SUPERSESSION, not only by its own timeout.
	//
	// This is what makes "newer intent wins" true. A stop arriving mid-start
	// cancels this context through the operation's cancel func, so the start's
	// work observes the cancellation at its next checkpoint rather than running
	// happily to completion and committing after the stop.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !b.ops.attachCancel(op, cancel) {
		// Superseded before we could even install the cancel: the work must not
		// begin, because its result could never be committed.
		debuglog.InfoLog("core op: %s (id=%d) was superseded before it began", kind, op.id)
		return nil
	}

	err := fn(ctx)

	settled := b.ops.finishOp(op, err)
	if settled == settleStale {
		// A newer operation owns the world. Its state is already published and
		// this result must not touch it — including the error it would carry.
		debuglog.InfoLog("core op: %s (id=%d) finished after being superseded; result discarded", kind, op.id)
		return nil
	}
	b.EmitCoreState()

	if err == nil {
		return nil
	}
	if refusal, ok := asPreconditionRefusal(err); ok {
		// A precondition declined the start. On the GUI path the precondition
		// showed its own dialog; on the headless path nothing was shown at all,
		// so the refusal must travel as a structured error rather than being
		// silently swallowed. See preconditionRefusal for why "aborted" is not
		// the same statement as "the user has been told".
		if refusal.Silent {
			return nil
		}
		return &protocol.Error{
			Code:        string(refusal.Code),
			Message:     refusal.Message,
			Recoverable: refusal.Recoverable,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		debuglog.WarnLog("core op: %s did not commit within %s — the UI stops waiting, the runtime state is authoritative", kind, timeout)
		return nil
	}
	if errors.Is(err, context.Canceled) {
		// Cancelled because a newer operation superseded this one. That is not a
		// failure of the user's intent; the newer operation is already running
		// and reporting for itself.
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
//
// A stop is not awaited because it tears down processes and the TUN device,
// which can take a while. But "not awaited" is not the same as "never finishes":
// the operation MUST reach a terminal state, or the record leaks and the UI sits
// in `stopping` forever.
//
// The completion is therefore driven by the RUNTIME, not by this function
// returning. `completeStop` is called when the engine that owns the core reports
// that the core is actually gone — the only event that can honestly end a stop.
// Nothing else clears the record.
func (b *Backend) runCoreOpFireAndForget(kind string, fn func()) error {
	if _, started := b.ops.beginOp(kind); !started {
		b.EmitCoreState()
		return nil
	}
	b.EmitCoreState()
	fn()
	return nil
}

// completeStop settles a stop operation once the runtime confirms the core is
// gone, and reports whether it did anything.
//
// Called from the runtime transition (RunningState true → false) and from the
// engines' confirmed-stop paths. It is idempotent and safe to call from any
// goroutine: the clearing is keyed on the operation ID, so a late call after a
// newer start has been registered does nothing.
//
// WHY THIS IS NOT DERIVED FROM THE BOOLEAN ALONE: a stop and a crash both move
// RunningState from true to false. Only the deliberate stop may end the stop
// operation; a crash must be classified by the crash path, which has its own
// decision to make about restarting. Callers therefore pass how the transition
// happened, and only a deliberate stop settles the operation.
func (b *Backend) completeStop(reason string) bool {
	op := b.ops.snapshotOp()
	if op == nil || op.kind != "stop" {
		return false
	}
	settled := b.ops.finishOp(op, nil)
	if settled == settleCommitted {
		debuglog.InfoLog("core op: stop (id=%d) completed (%s)", op.id, reason)
		b.EmitCoreState()
		return true
	}
	return false
}
