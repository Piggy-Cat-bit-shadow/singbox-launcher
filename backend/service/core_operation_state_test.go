package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"

	"singbox-launcher/core/events"
)

// The operation state machine, tested as a state machine.
//
// The tests this file replaces asserted the wrong thing. One of them
// (`TestStartDoesNotReportStoppedWhilePending`) explicitly required the wire
// state to be `stopped` for "an accepted start whose runtime transition is still
// pending" — which IS the `starting` phase, and which is why the bug it claimed
// to guard against could come back through the door it left open. The other
// (`TestRapidDoubleClickStartsOnce`) asserted only that a record was overwritten,
// so it passed even though the superseded start kept running.
//
// These tests therefore check the things that actually matter: that a stop
// reaches a terminal state, that `stopping` is visible while the core is alive,
// and that a superseded operation's result changes nothing.

// errStaleStart stands in for a real start failure in the staleness tests.
var errStaleStart = errors.New("start failed after being superseded")

// --- C: a stop operation always reaches a terminal state --------------------

// TestStopOperationTerminatesAtStopped is claim C.
//
// `runCoreOpFireAndForget` registered the stop operation, published `stopping`,
// ran the teardown and returned — WITHOUT ever calling finishOp. The record was
// therefore never cleared: the state stayed `stopping` forever, and only an
// unrelated operation happening to overwrite the record could end it.
//
// The completion is now driven by the runtime transition, which is the only
// event that can honestly end a stop.
func TestStopOperationTerminatesAtStopped(t *testing.T) {
	b := backendWithConfig(t)

	// The core is up.
	b.ac.RunningState.Set(true)
	if got := b.coreLifecycleState(); got != protocol.CoreStateRunning {
		t.Fatalf("precondition: state = %q, want running", got)
	}

	// The user presses Stop. The request is registered and `stopping` published.
	if err := b.StopCore(); err != nil {
		t.Fatalf("StopCore: %v", err)
	}
	if op := b.ops.snapshotOp(); op == nil || op.kind != "stop" {
		t.Fatalf("a stop must be recorded as in flight; got %v", op)
	}
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopping {
		t.Fatalf("state after a stop request = %q, want stopping", got)
	}

	// The runtime confirms the core is gone, having been taken down by the user's
	// stop. The transition must say so: a bare `false` is deliberately not enough
	// to end a stop operation, because a restart's teardown also flips this flag
	// and would otherwise be reported as a completed stop.
	b.ac.RunningState.SetStopped(events.TeardownUserStop)

	if op := b.ops.snapshotOp(); op != nil {
		t.Fatalf("the stop operation is still registered as %s after the core went "+
			"down; a stop must reach a terminal state, or the UI sits in "+
			"'stopping' forever", op.kind)
	}
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopped {
		t.Fatalf("state after the core went down = %q, want stopped", got)
	}
}

// TestStoppingVisibleWhileRuntimeStillAlive is claim D.
//
// During a stop the core is usually STILL ALIVE, so the running flag is
// legitimately true. Reporting `running` for that whole window tells the UI
// nothing is happening and the user watches a screen that never acknowledges the
// stop they requested. The accepted stop must outrank the running flag while its
// operation still owns the world — process truth and lifecycle phase are
// different questions.
func TestStoppingVisibleWhileRuntimeStillAlive(t *testing.T) {
	b := backendWithConfig(t)

	b.ac.RunningState.Set(true)
	if err := b.StopCore(); err != nil {
		t.Fatalf("StopCore: %v", err)
	}

	// The core is deliberately still up: this is the teardown window.
	if !b.ac.RunningState.IsRunning() {
		t.Fatal("precondition: the core should still be alive during teardown")
	}
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopping {
		t.Fatalf("state = %q while a stop is in flight and the core is still alive; "+
			"want stopping", got)
	}
	if got := b.coreState().State; got != protocol.CoreStateStopping {
		t.Fatalf("DTO state = %q, want stopping", got)
	}
}

// TestStopCompletionIsNotDrivenByABooleanAlone is the crash/stop distinction.
//
// A crash ALSO moves the running flag from true to false. If the stop operation
// were settled by the boolean alone, a crash during a start would be able to end
// a stop operation that had nothing to do with it — or worse, a crash while a
// stop was pending would be reported as a completed stop.
func TestStopCompletionIsNotDrivenByABooleanAlone(t *testing.T) {
	b := backendWithConfig(t)

	// A CRASH: the flag drops with no teardown reason. It must not conjure a
	// completion for a stop that was never requested.
	b.ac.RunningState.Set(true)
	b.ac.RunningState.Set(false)
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopped {
		t.Fatalf("state after a crash with no stop in flight = %q, want stopped", got)
	}

	// And completeStop is a no-op when the current operation is not a stop.
	if _, ok := b.ops.beginOp("start"); !ok {
		t.Fatal("beginOp refused the start")
	}
	if b.completeStop("test") {
		t.Fatal("completeStop ended an operation that was not a stop")
	}
	if op := b.ops.snapshotOp(); op == nil || op.kind != "start" {
		t.Fatal("completeStop must not clear a non-stop operation")
	}
}

// TestStopAfterStopIsIdempotent — a second StopCore while one is in flight is a
// duplicate, and completing twice must not resurrect or double-clear anything.
func TestStopAfterStopIsIdempotent(t *testing.T) {
	b := backendWithConfig(t)

	// Drive the stop to completion through the REAL chain, with only the process
	// operation faked. The runtime transition is emitted by the fake, so the test
	// does not depend on the package-global RunningState singleton's value left
	// over from another test — a dependency that made this test order-sensitive
	// and let a stop appear "never completed" when the flag happened to be false
	// already (a no-op Set publishes no event).
	installStoppingLegacy(t, b)

	if err := b.StopCore(); err != nil {
		t.Fatalf("first StopCore: %v", err)
	}
	if err := b.StopCore(); err != nil {
		t.Fatalf("second StopCore must be accepted as a duplicate, got %v", err)
	}

	waitUntil(t, "the stop operation to settle", func() bool {
		return b.ops.snapshotOp() == nil
	})

	if b.completeStop(string(events.TeardownUserStop)) {
		t.Fatal("completing an already-settled stop must do nothing")
	}
}

// installStoppingLegacy makes the classic stop path complete deterministically:
// the process operation succeeds and announces a user-stop teardown, which is
// exactly what the real teardown does.
func installStoppingLegacy(t *testing.T, b *Backend) {
	t.Helper()
	lb := core.NewLegacyBackend(b.ac)
	core.SetLegacyOpsForTest(lb, &core.LegacyOpsForTest{
		Stop: func() {
			// The real Stop() notes the reason before flipping the flag; the fake
			// does the same, so the transition carries the same information.
			b.ac.RunningState.SetStopped(events.TeardownUserStop)
		},
	})
	b.ac.SetBackendForTest(lb)
}

// --- F: superseding actually cancels -----------------------------------------

// TestSupersedingStopCancelsStart is claim F.
//
// `beginOp` overwrote the record and the comment said "the newer intent wins",
// but the old start's context and work were never cancelled. Both intents ran to
// completion and the start could finish LAST and commit after the stop — which is
// the opposite of winning.
//
// The cancellation is asserted through the operation's own context, because that
// is the handle the real start body receives and selects on. This test cannot
// pass on bookkeeping alone: if superseding does not cancel, the body's
// ctx.Done() never fires.
func TestSupersedingStopCancelsStart(t *testing.T) {
	b := backendWithConfig(t)

	var startCtx context.Context
	reached := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- b.runCoreOp("start", 30*time.Second, func(ctx context.Context) error {
			startCtx = ctx
			close(reached)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
				// Superseding did NOT cancel the running work.
				return nil
			}
		})
	}()

	<-reached
	waitForOp(t, b, "start")
	if startCtx == nil {
		t.Fatal("the start body never received a context")
	}

	// A stop supersedes the start. Through beginOp this must CANCEL the start,
	// not merely relabel the record.
	stopOp, accepted := b.ops.beginOp("stop")
	if !accepted {
		t.Fatal("a stop during a start must be accepted")
	}
	if stopOp.kind != "stop" {
		t.Fatalf("the stop must own the record, got %q", stopOp.kind)
	}

	select {
	case <-startCtx.Done():
		// The start's own context was cancelled: supersede is a real action,
		// and the start body observes it at its next checkpoint.
	case <-time.After(5 * time.Second):
		t.Fatal("the superseded start's context was never cancelled; 'newer intent " +
			"wins' is only overwriting a record, and the old work keeps running")
	}

	select {
	case <-done:
		// runCoreOp unwound; its result is discarded as stale.
	case <-time.After(5 * time.Second):
		t.Fatal("runCoreOp never returned after its operation was superseded")
	}

	// The superseded start must NOT have disturbed the stop's record.
	if op := b.ops.snapshotOp(); op == nil || op.id != stopOp.id {
		t.Fatalf("the superseded start disturbed the stop operation: %v", op)
	}
}

// TestStaleStartFailureCannotPolluteNewStop is claim G.
//
// finishOp cleared the record only when the kind matched, but recorded the error
// UNCONDITIONALLY. A start that was superseded by a stop and failed later
// therefore still overwrote the error, and the user saw a START failure reported
// against the STOP they had just requested.
//
// The ID check makes a stale result a complete no-op: no record change, no
// error, no state change.
func TestStaleStartFailureCannotPolluteNewStop(t *testing.T) {
	b := backendWithConfig(t)

	startOp, started := b.ops.beginOp("start")
	if !started {
		t.Fatal("beginOp refused the start")
	}
	stopOp, started := b.ops.beginOp("stop")
	if !started {
		t.Fatal("a stop must supersede the start")
	}

	// The superseded start now fails, after the stop already owns the world.
	failure := core.NewStartFailure(core.StartErrSpawnFailed, errStaleStart)
	if got := b.ops.finishOp(startOp, failure); got != settleStale {
		t.Fatalf("a superseded start's result must be rejected as stale, got %v", got)
	}

	// NOTHING was written.
	if _, lastErr := b.ops.snapshot(); lastErr != nil {
		t.Fatalf("a stale start failure was recorded as %q and will be shown against "+
			"the stop the user actually requested", lastErr.Detail)
	}
	if op := b.ops.snapshotOp(); op == nil || op.id != stopOp.id {
		t.Fatalf("the stale start cleared or replaced the stop operation: %v", op)
	}
	if code, msg := b.coreErrorInfo(); code != "" || msg != "" {
		t.Fatalf("the wire error fields carry a stale start failure: %q/%q", code, msg)
	}

	// The current operation can still settle normally.
	if got := b.ops.finishOp(stopOp, nil); got != settleCommitted {
		t.Fatalf("the current stop must settle normally, got %v", got)
	}
	if op := b.ops.snapshotOp(); op != nil {
		t.Fatalf("the stop did not clear its own record: %v", op)
	}
}

// TestStaleResultCannotClearANewerOperationOfTheSameKind — the sharpest form of
// the identity problem. Kinds repeat, so a kind-based check cannot tell these
// apart: start A, supersede with a stop, start B, then let A finish.
func TestStaleResultCannotClearANewerOperationOfTheSameKind(t *testing.T) {
	b := backendWithConfig(t)

	opA, _ := b.ops.beginOp("start")
	_, _ = b.ops.beginOp("stop")
	opB, started := b.ops.beginOp("start")
	if !started {
		t.Fatal("the second start must be accepted")
	}
	if opA.id == opB.id {
		t.Fatal("operation IDs must be unique; identity is the whole mechanism")
	}

	// The FIRST start finally returns, long after the world moved on twice.
	if got := b.ops.finishOp(opA, nil); got != settleStale {
		t.Fatalf("start A's result must be stale, got %v", got)
	}
	// B must still own the record: a kind-based check would have cleared it.
	if op := b.ops.snapshotOp(); op == nil || op.id != opB.id {
		t.Fatalf("start A's late completion cleared start B's record; identity " +
			"must be by ID, never by kind")
	}
}

// TestSupersededOperationIsMarked — the superseded channel is the signal that
// work which cannot be cancelled by context can still observe.
func TestSupersededOperationIsMarked(t *testing.T) {
	b := backendWithConfig(t)

	opA, _ := b.ops.beginOp("start")
	if opA.isSuperseded() {
		t.Fatal("a fresh operation must not be marked superseded")
	}
	_, _ = b.ops.beginOp("stop")
	if !opA.isSuperseded() {
		t.Fatal("superseding must mark the outgoing operation")
	}
}

// TestDuplicateOperationReturnsTheRunningOne — a double click must be refused,
// and the caller must be given the operation that IS running.
func TestDuplicateOperationReturnsTheRunningOne(t *testing.T) {
	b := backendWithConfig(t)

	first, started := b.ops.beginOp("start")
	if !started {
		t.Fatal("the first start was refused")
	}
	second, started := b.ops.beginOp("start")
	if started {
		t.Fatal("a second start was accepted while the first was in flight")
	}
	if second == nil || second.id != first.id {
		t.Fatalf("a duplicate must return the running operation, got %v want id=%d",
			second, first.id)
	}
}

// waitForOp blocks until the named operation is the current one.
func waitForOp(t *testing.T, b *Backend, kind string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if op := b.ops.snapshotOp(); op != nil && op.kind == kind {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s operation became current within the deadline", kind)
}
