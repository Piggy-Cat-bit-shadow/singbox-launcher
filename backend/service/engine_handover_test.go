package service

import (
	"strings"
	"testing"

	"singbox-launcher/core"
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

// TestRunningStateTransitionCarriesItsReason is audit BUG 3's mechanism.
//
// `Running == false` has several causes that are indistinguishable on the wire —
// a user stop, a restart's teardown, a crash — and they need opposite responses.
// The restart's teardown flips the same flag, so settling a pending stop on any
// false reading reported the user's stop as complete moments before a new core
// came up.
func TestRunningStateTransitionCarriesItsReason(t *testing.T) {
	src := stripGoComments(readSource(t, "core/controller.go"))
	if !contains(src, "Teardown: reason") {
		t.Error("the running-state transition does not carry a teardown reason, so a " +
			"listener cannot tell a user stop from a restart's teardown")
	}

	svc := stripGoComments(readSource(t, "backend/service/core_operation.go"))
	if !contains(svc, "events.TeardownUserStop") {
		t.Error("completeStop does not require a deliberate USER stop; a restart's " +
			"teardown would settle the stop operation as a success")
	}

	// The restart paths must label their teardowns, or they would be read as stops.
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
		body := ps[idx : idx+end]
		if !contains(body, "noteTeardown(events.TeardownRestart)") {
			t.Errorf("%s does not label its teardown as a restart, so a superseding "+
				"stop is settled as a success just before the core comes back", fn)
		}
		if !contains(body, "noteTeardown") {
			continue
		}
		noteAt := indexOf(body, "noteTeardown(events.TeardownRestart)")
		setAt := indexOf(body, "RunningState.Set(false)")
		if noteAt >= 0 && setAt >= 0 && noteAt > setAt {
			t.Errorf("%s flips the running flag BEFORE labelling the teardown, so the "+
				"transition is published unlabelled and read as a user stop", fn)
		}
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
