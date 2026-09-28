package service

import (
	"strings"
	"testing"

	"singbox-launcher/core"
	"singbox-launcher/core/events"
	"sync"
)

// An engine must not release a process it still owns, and the state reported over
// IPC must be derived from OWNERSHIP rather than from a belief that can be stale.
//
// An adversarial audit of the operation state machine found four ways a live core
// could be handed over to another engine or reported as stopped. These are the
// regressions for them.

// installFakeOwnedProcess records an owned process that does not exist, with the
// state combination the audit found produced "stopped": a settled phase and a false
// running flag, which is exactly what an adopted root core looks like.
//
// Returns the recorded PID so a failure message can name it.
func installFakeOwnedProcess(t *testing.T, b *Backend) int {
	t.Helper()
	const pid = 999999 // documented as non-existent; nothing is signalled
	b.ac.RunningState.Set(false)
	b.ac.SetClassicPhaseForTest(core.ClassicStopped)
	b.ac.SetOwnedProcessForTest(pid, "/nonexistent/sing-box")
	t.Cleanup(func() { b.ac.ClearOwnedProcessForTest() })
	return pid
}

// clearOwnedProcess makes the ownership record empty.
func clearOwnedProcess(b *Backend) {
	b.ac.ClearOwnedProcessForTest()
}

// TestEngineCloseStopsALiveOwnedCore is audit BUG 1 (CRITICAL).
//
// `SwitchBackendMode` required `!RunningState.IsRunning()`, which reads like "no
// core is running". It is not: the crash path clears that flag as soon as the exit
// is OBSERVED, and a process can outlive the observation. `LegacyBackend.Close()`
// only bumped the generation — bookkeeping — so the switch succeeded and left a
// live classic core holding the TUN while the daemon's core started beside it.
//
// The fix is that closing an engine must stop what it still owns. Asserted
// structurally, because the real stop path needs a real process.
func TestEngineCloseStopsALiveOwnedCore(t *testing.T) {
	src := stripGoComments(readSource(t, "core/backend_legacy.go"))

	idx := indexOf(src, "func (b *LegacyBackend) Close()")
	if idx < 0 {
		t.Fatal("LegacyBackend.Close not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	if !contains(body, "ownedProcess()") {
		t.Error("Close does not consult process ownership, so a live core survives an " +
			"engine switch and two engines fight over the TUN")
	}
	if !contains(body, "ForceStopOwnedCore()") {
		t.Error("Close does not stop the core it owns; renewing the generation only " +
			"invalidates the bookkeeping and kills nothing")
	}

	// And the stop must happen BEFORE the generation is renewed, or a start that
	// is mid-flight could be adopted on the way out.
	stopAt := indexOf(body, "ForceStopOwnedCore()")
	renewAt := indexOf(body, "renewGeneration()")
	if stopAt >= 0 && renewAt >= 0 && stopAt > renewAt {
		t.Error("Close renews the generation before stopping the owned core, so a " +
			"concurrent start can still commit into a generation being abandoned")
	}
}

// TestLifecycleStateConsultsOwnership is audit BUG 4.
//
// `coreLifecycleState` decided from the classic PHASE, and `isSettled()` counts the
// empty phase and `ClassicStopped` as "nothing happening". But the phase is not a
// statement about whether a PROCESS exists: an adopted privileged root core records
// its identity without ever passing through a start operation, so the phase reads
// stopped while a root sing-box holds the TUN. The IPC layer then reported
// `stopped`, showed a clean Start button, and the next Start collided with the core
// nobody had admitted was running.
func TestLifecycleStateConsultsOwnership(t *testing.T) {
	b := backendWithConfig(t)

	// A live owned process, with the phases and flags that previously produced
	// "stopped": phase settled, running flag false.
	pid := installFakeOwnedProcess(t, b)

	if got := b.coreLifecycleState(); got != "running" {
		t.Fatalf("with a live owned process (pid=%d) the reported state is %q, want "+
			"\"running\"; the UI would show a clean Start button for a machine that is "+
			"actively routed", pid, got)
	}

	if !b.ownsALiveProcess() {
		t.Fatal("ownsALiveProcess did not see the owned process")
	}
}

// TestLifecycleStateIsStoppedWithNoOwnedProcess — the complement. Consulting
// ownership must not pin the state at running forever.
func TestLifecycleStateIsStoppedWithNoOwnedProcess(t *testing.T) {
	b := backendWithConfig(t)
	clearOwnedProcess(b)

	if b.ownsALiveProcess() {
		t.Fatal("ownsALiveProcess reported a process when none is owned")
	}
	if got := b.coreLifecycleState(); got != "stopped" {
		t.Fatalf("with nothing owned the reported state is %q, want \"stopped\"", got)
	}
}

// TestClassifyCoreExitReasonTakesTheGeneration is audit BUG 7.
//
// The classifier looked up `currentGeneration()` while every other callback in the
// file takes the generation as a PARAMETER, precisely so it cannot act for another
// generation. A monitor that won the race to observe an exit just before a renew
// would classify that exit against its successor's log window — turning a transient
// crash into a reported deterministic config failure, which stops auto-restart.
func TestClassifyCoreExitReasonTakesTheGeneration(t *testing.T) {
	src := stripGoComments(readSource(t, "core/process_service.go"))

	if contains(src, "logOffsetFor(ac.classic.currentGeneration())") {
		t.Error("classifyCoreExitReason looks up the CURRENT generation instead of " +
			"being told which generation exited; a late classification would read " +
			"another generation's log window")
	}
	if !contains(src, "func (ac *AppController) classifyCoreExitReason(gen uint64)") {
		t.Error("classifyCoreExitReason does not take the generation as a parameter")
	}
	if !contains(src, "logOffsetFor(gen)") {
		t.Error("classifyCoreExitReason does not use the generation it was given")
	}
}

// TestDaemonFatalChecksActivityBeforeMutating is audit BUG 2.
//
// `retryAfterCoreFatal` called `retryCoreReject` — which PERSISTS a node as
// disabled — before checking `isActive()`. A status frame that arrived after the
// user switched to the classic engine still removed a node from their working set,
// irreversibly, on behalf of an engine nobody was running.
func TestDaemonFatalChecksActivityBeforeMutating(t *testing.T) {
	src := stripGoComments(readSource(t, "core/backend_daemon.go"))

	idx := indexOf(src, "func (b *DaemonBackend) retryAfterCoreFatal(")
	if idx < 0 {
		t.Fatal("retryAfterCoreFatal not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	guardAt := indexOf(body, "b.isActive()")
	mutateAt := indexOf(body, "retryCoreReject(")
	if guardAt < 0 {
		t.Fatal("retryAfterCoreFatal never checks isActive, so a retired backend still " +
			"repairs the config it no longer owns")
	}
	if mutateAt < 0 {
		t.Fatal("retryCoreReject is no longer called — this test needs updating")
	}
	if guardAt > mutateAt {
		t.Error("the isActive guard comes AFTER the node-disabling mutation; a late " +
			"FATAL frame would still disable a user's node after an engine switch")
	}
}

// TestRunningStateTransitionCarriesItsReason is audit BUG 3's mechanism, in its
// corrected form.
//
// `Running == false` has several causes that are indistinguishable on the wire — a
// user stop, a restart's teardown, a crash — and they need opposite responses. The
// restart's teardown flips the same flag, so settling a pending stop on any false
// reading reported the user's stop as complete moments before a new core came up.
//
// The reason is now a PARAMETER of the state write, not a recorded flag. An earlier
// version stored it in a slot read at flip time, and that design was broken: note
// and flip were two steps on shared mutable state, so a reason could survive a no-op
// write and later label an unrelated CRASH as a completed user stop. Passing it
// makes the two one operation, which is why this test can assert the API shape
// rather than the presence of a pair of calls.
func TestRunningStateTransitionCarriesItsReason(t *testing.T) {
	// The state write must accept a reason.
	ctrl := stripGoComments(readSource(t, "core/controller.go"))
	if !contains(ctrl, "func (r *RunningState) SetStopped(reason events.TeardownReason)") {
		t.Error("there is no way to report WHY the core went down; an unlabelled " +
			"`false` cannot be told apart from a crash")
	}
	if !contains(ctrl, "Teardown: publishReason") {
		t.Error("the running-state transition does not carry the teardown reason")
	}
	// And the old shared-slot design must not come back: a recorded reason read at
	// flip time is exactly what let a crash be reported as a user stop.
	if contains(ctrl, "pendingTeardown()") || contains(ctrl, "teardownReason") {
		t.Error("the state write reads a RECORDED teardown reason instead of being " +
			"given one; note and flip are then two steps and the reason can outlive " +
			"the teardown it describes, mislabelling a later crash as a user stop")
	}

	svc := stripGoComments(readSource(t, "backend/service/core_operation.go"))
	if !contains(svc, "events.TeardownUserStop") {
		t.Error("completeStop does not require a deliberate USER stop; a restart's " +
			"teardown would settle the stop operation as a success")
	}

	// Every deliberate teardown must LABEL ITSELF, and every user-stop teardown must
	// say so — including the ones outside process_service.go, which the previous
	// version of this test could not see at all because it scanned only one file.
	labelled := map[string]string{
		"core/process_service.go": "func (svc *ProcessService) Stop()",
	}
	for file, fn := range labelled {
		ps := stripGoComments(readSource(t, file))
		idx := indexOf(ps, fn)
		if idx < 0 {
			t.Errorf("%s: %s not found", file, fn)
			continue
		}
		end := indexOf(ps[idx:], "\nfunc ")
		if end < 0 {
			end = len(ps) - idx
		}
		if !contains(ps[idx:idx+end], "SetStopped(events.TeardownUserStop)") {
			t.Errorf("%s: %s does not report its teardown as a user stop, so a genuine "+
				"stop is never settled and the UI stays in `stopping`", file, fn)
		}
	}

	// The restart paths must label theirs as a restart, or a superseding stop is
	// settled as a success just before the core comes back.
	ps := stripGoComments(readSource(t, "core/process_service.go"))
	for _, fn := range []string{
		"func (svc *ProcessService) KillForRestart()",
		"func (svc *ProcessService) RestartContext(ctx context.Context) error",
	} {
		idx := indexOf(ps, fn)
		if idx < 0 {
			t.Errorf("%s not found", fn)
			continue
		}
		end := indexOf(ps[idx:], "\nfunc ")
		if end < 0 {
			end = len(ps) - idx
		}
		if !contains(ps[idx:idx+end], "SetStopped(events.TeardownRestart)") {
			t.Errorf("%s does not label its teardown as a restart, so a superseding "+
				"stop is settled as a success just before the core comes back", fn)
		}
	}

	// The daemon's stop-on-exit is a USER stop too. Missing it left the operation
	// unsettled while the app exited — and this file was invisible to the previous
	// version of this test.
	db := stripGoComments(readSource(t, "core/backend_daemon.go"))
	if !contains(db, "SetStopped(events.TeardownUserStop)") {
		t.Error("no daemon stop path is labelled as a user stop; a confirmed " +
			"stop-on-exit never settles its operation")
	}
}

// TestTeardownReasonIsNotStoredInASharedSlot is the regression for the design that
// produced audit finding 1.
//
// The reason used to live in one unguarded slot on the controller, written by a
// bare atomic store and read at flip time. That admitted this sequence: a `user_stop`
// is noted, the following `Set(false)` is a DEDUP NO-OP and leaks the reason, the
// core later comes up and goes down in a CRASH, and the crash reads the leaked
// reason and is reported as a completed user stop.
//
// Two properties together remove the class, and both are asserted here.
func TestTeardownReasonIsNotStoredInASharedSlot(t *testing.T) {
	ctrl := stripGoComments(readSource(t, "core/controller.go"))

	// 1. No slot: the reason is an argument, so it cannot be read by a later,
	//    unrelated transition.
	if contains(ctrl, "teardownReason") {
		t.Error("the controller still holds a shared teardown-reason slot")
	}

	// 2. A no-op write records nothing, because it records nothing at all.
	idx := indexOf(ctrl, "func (r *RunningState) set(")
	if idx < 0 {
		t.Fatal("RunningState.set not found")
	}
	end := indexOf(ctrl[idx:], "\nfunc ")
	if end < 0 {
		end = len(ctrl) - idx
	}
	body := ctrl[idx : idx+end]

	noopAt := indexOf(body, "if r.running == value {")
	if noopAt < 0 {
		t.Fatal("the no-op guard is gone; this test needs updating")
	}
	// The no-op branch must contain the early return and nothing that stores state
	// for later use.
	noopBlock := body[noopAt:]
	if closeAt := indexOf(noopBlock, "}"); closeAt >= 0 {
		noopBlock = noopBlock[:closeAt]
	}
	if !contains(noopBlock, "return") {
		t.Error("the no-op branch does not return early")
	}
	if contains(noopBlock, "Store(") {
		t.Error("the no-op branch records state that a LATER transition would read; " +
			"this is exactly how a leaked reason mislabelled a crash as a user stop")
	}
}

// TestStopOperationCannotDependOnASingleEvent — a stop whose contextual call
// succeeded has completed, whether or not a runtime transition reaches the
// operation record.
//
// `RunningState.Set` dedups a no-op write, so a stop that finishes while the flag
// is already false publishes NO event. The old design depended entirely on that
// event, so the record sat at `stopping` forever. Found by stressing the existing
// stop tests: 13 runs in 30 failed once the reason labelling made the flag's
// previous value matter.
func TestStopOperationCannotDependOnASingleEvent(t *testing.T) {
	src := stripGoComments(readSource(t, "backend/service/core_operation.go"))

	idx := indexOf(src, "func (b *Backend) runCoreOpFireAndForget(")
	if idx < 0 {
		t.Fatal("runCoreOpFireAndForget not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	// The success branch must settle the operation itself.
	if !contains(body, "finishOp(op, nil)") {
		t.Error("a successful contextual stop does not settle its own operation; " +
			"completion depends entirely on a runtime event that a no-op flag write " +
			"never publishes, leaving the UI in `stopping` forever")
	}
}

// TestNoteTeardownPrecedesTheFlagFlip — the reason must be recorded before the
// transition it describes is published, or the transition carries the PREVIOUS
// reason (or none).
func TestNoteTeardownPrecedesTheFlagFlip(t *testing.T) {
	src := stripGoComments(readSource(t, "core/process_service.go"))

	// Every labelled teardown must have its note immediately before the flip.
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "noteTeardown(events.") {
			continue
		}
		// The next few lines should contain the Set(false) this label describes.
		found := false
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			if strings.Contains(lines[j], "RunningState.Set(false)") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("line %d records a teardown reason with no RunningState.Set(false) "+
				"nearby; the reason would label an unrelated transition", i+1)
		}
	}
}

// TestNoOpRunningWriteCannotLeakAStopReason is the behavioural regression for audit
// finding 1, and it is behavioural on purpose: the source-shape assertions above
// cannot distinguish a design that merely LOOKS right from one that behaves right.
//
// The sequence that produced the bug:
//
//  1. a user stops the core, and the reason "user stop" is recorded;
//  2. the following running-state write is a DEDUP NO-OP (the flag is already
//     false), so no transition is published — but the old design left the reason
//     sitting in a shared slot;
//  3. the core later comes up and then dies in a CRASH;
//  4. the crash transition reads the leaked reason and is reported as a completed
//     user stop — settling a stop operation that never happened, and hiding a
//     crash the user needed to see.
//
// The reason is now an argument of the write, so step 2 cannot leave anything
// behind. This test drives the real RunningState and the real subscriber.
func TestNoOpRunningWriteCannotLeakAStopReason(t *testing.T) {
	b := backendWithConfig(t)

	var (
		mu       sync.Mutex
		observed []events.VpnStateChangedPayload
	)
	cancel := b.ac.EventBus.Subscribe(events.VpnStateChanged, func(ev events.Event) {
		p, ok := ev.Payload.(events.VpnStateChangedPayload)
		if !ok {
			return
		}
		mu.Lock()
		observed = append(observed, p)
		mu.Unlock()
	})
	defer cancel()

	// The core is up.
	b.ac.RunningState.Set(true)

	// (1)+(2) The user stops it, and the stop's own write happens when the flag is
	// ALREADY false — a no-op, which is exactly the leak window.
	b.ac.RunningState.SetStopped(events.TeardownUserStop) // the real transition
	b.ac.RunningState.SetStopped(events.TeardownUserStop) // dedup no-op

	// (3) A crash: the core comes up and goes down with no reason.
	b.ac.RunningState.Set(true)
	b.ac.RunningState.Set(false)

	mu.Lock()
	got := append([]events.VpnStateChangedPayload(nil), observed...)
	mu.Unlock()

	if len(got) == 0 {
		t.Fatal("no transitions were observed at all; the test is not exercising anything")
	}
	last := got[len(got)-1]
	if last.Running {
		t.Fatalf("the final transition reports running=%v, want false", last.Running)
	}
	if last.Teardown == events.TeardownUserStop {
		t.Fatal("a CRASH was reported as a completed USER STOP. The reason survived a " +
			"no-op write and labelled an unrelated transition, which settles a stop " +
			"operation that never happened and hides the crash from the user")
	}
	if last.Teardown != events.TeardownNone {
		t.Fatalf("the crash carried teardown=%q, want none", last.Teardown)
	}

	// And the genuine stop must still have been labelled correctly.
	var sawUserStop bool
	for _, p := range got {
		if !p.Running && p.Teardown == events.TeardownUserStop {
			sawUserStop = true
		}
	}
	if !sawUserStop {
		t.Fatal("the real user stop was not labelled as one, so a genuine stop would " +
			"never settle its operation")
	}
}

// TestEveryRealUserStopPathIsLabelled is audit finding 4.
//
// `completeStop` settles a stop operation only for a deliberate user stop, so every
// path where the user really did ask for the core to go down must say so. An
// unlabelled `false` is indistinguishable from a crash, and the operation then sits
// at `stopping` while the core is already gone — the exact "record never reaches a
// terminal state" failure the state machine exists to prevent.
//
// Three paths were unlabelled: the daemon's confirmed stop-on-exit, and the adopted
// core's watcher, which can observe a death DURING a user's Stop because it polls on
// a timer. The third was GracefulExit exiting with an operation still registered.
func TestEveryRealUserStopPathIsLabelled(t *testing.T) {
	// The daemon's stop-on-exit is a user stop.
	db := stripGoComments(readSource(t, "core/backend_daemon.go"))
	if !contains(db, "SetStopped(events.TeardownUserStop)") {
		t.Error("the daemon's confirmed stop-on-exit does not label its teardown as a " +
			"user stop, so the operation never settles")
	}

	// The adopted-core watcher must honour the user's recorded intent.
	ps := stripGoComments(readSource(t, "core/process_service.go"))
	idx := indexOf(ps, "func (svc *ProcessService) watchAdoptedCore(")
	if idx < 0 {
		t.Fatal("watchAdoptedCore not found")
	}
	end := indexOf(ps[idx:], "\nfunc ")
	if end < 0 {
		end = len(ps) - idx
	}
	body := ps[idx : idx+end]
	if !contains(body, "StoppedByUser") {
		t.Error("the adopted-core watcher does not consult whether the user asked for " +
			"the stop, so a death it observes during a Stop is reported as a crash and " +
			"the stop is never settled")
	}

	// And the app must not exit with an operation still in flight.
	if !contains(ps, "watchAdoptedCore") {
		t.Fatal("watchAdoptedCore is gone; this test needs updating")
	}
	ctrl := stripGoComments(readSource(t, "core/controller.go"))
	if !contains(ctrl, "SettleOperationsAtExit()") {
		t.Error("GracefulExit does not settle in-flight operations, so the app can " +
			"exit with a stop record that will never reach a terminal state")
	}
	be := stripGoComments(readSource(t, "backend/service/backend.go"))
	if !contains(be, "RegisterExitSettler") {
		t.Error("the exit settler is never registered, so the call in GracefulExit " +
			"does nothing")
	}
}

// TestExitSettlerActuallyClearsTheRecord is the behavioural half of the above: the
// registration must clear a real in-flight operation, not merely exist.
func TestExitSettlerActuallyClearsTheRecord(t *testing.T) {
	b := backendWithConfig(t)

	// Register the settler the way New() does, then start an operation that never
	// completes, so the record really is in flight at exit.
	b.ac.RegisterExitSettler(b.settleOperationsAtExit)
	if _, started := b.ops.beginOp("stop"); !started {
		t.Fatal("could not start the stop operation")
	}
	if b.ops.snapshotOp() == nil {
		t.Fatal("precondition: the operation should be registered")
	}

	b.ac.SettleOperationsAtExit()

	if op := b.ops.snapshotOp(); op != nil {
		t.Fatalf("the app exited with %s (id=%d) still registered; the wire state would "+
			"show `stopping` forever", op.kind, op.id)
	}
}

// TestRestartFailureCorrectsTheState is audit finding 5.
//
// `RestartContext`'s "could not confirm exit" branch returned WITHOUT a state
// write, even though the termination attempt may have killed the process. The
// running flag was then a lie, and the transition it should have produced was
// missing — so a superseding stop could not be settled either.
func TestRestartFailureCorrectsTheState(t *testing.T) {
	ps := stripGoComments(readSource(t, "core/process_service.go"))
	idx := indexOf(ps, "could not confirm exit")
	if idx < 0 {
		t.Fatal("the restart-confirmation failure branch is gone; this test needs updating")
	}
	// The branch's body, up to the return.
	branch := ps[idx:]
	if end := indexOf(branch, "\n\t\treturn NewStartFailure"); end >= 0 {
		branch = branch[:end]
	}
	if !contains(branch, "SetStopped(events.TeardownRestart)") {
		t.Error("the restart failure branch does not correct the running state; the flag " +
			"can be false after a killed core with no transition published, and a " +
			"superseding stop is then never settled")
	}
}
