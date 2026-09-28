package core

import (
	"testing"

	"singbox-launcher/core/events"
)

// TestClassicStartTransitionsStoppedToRunning is CASE 1: the end-to-end Classic
// lifecycle the user reported as never completing.
//
// The user's log shows a fully successful start — root-owned copy verified,
// sing-box started, runtime config matched, PID written, Clash API up, four
// groups loaded — while the UI stayed on "正在启动". This walks the state the way
// that start does and asserts the transition the UI reads.
//
// It is deliberately about the STATE, not about spawning a process: the spawn is
// the OS's business and is covered by the privileged tests. What failed was the
// bookkeeping that tells the user what happened.
func TestClassicStartTransitionsStoppedToRunning(t *testing.T) {
	ac := newPhaseProbeController(t)

	// STOPPED: nothing owned, phase settled.
	//
	// The zero phase is the EMPTY string, not `ClassicStopped`: a runtime that has
	// never run an operation has no phase, and `phaseToWireState` returns "" for
	// it so the derivation falls through to RunningState. Both values mean
	// "settled and not running", so both are accepted — asserting one spelling
	// would pin an implementation detail rather than the meaning.
	switch got := ac.classic.currentPhase(); got {
	case ClassicStopped, "":
	default:
		t.Fatalf("a fresh runtime must be settled and stopped (%q or %q), got %q",
			ClassicStopped, "", got)
	}
	if ac.RunningState.IsRunning() {
		t.Fatal("a fresh runtime must not report running")
	}

	// START requested: the phase becomes `starting`, which the wire maps to
	// `starting` — the state the UI shows as 正在启动.
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("beginOperation refused a start on a settled runtime")
	}
	if got, want := ac.classic.currentPhase(), ClassicStarting; got != want {
		t.Fatalf("phase after a start request = %q, want %q", got, want)
	}
	if ac.RunningState.IsRunning() {
		t.Error("the core is not up yet; running must not be true merely because a " +
			"start was requested")
	}

	// The start succeeds: ownership and the phase are committed together. This is
	// where the reported bug lived — the phase stayed `starting` forever.
	svc := NewProcessService(ac)
	if !svc.commitPrivilegedStartLocked(gen, 1111, 2222, "/tmp/x.pid", "/bin/true") {
		t.Fatal("commitPrivilegedStartLocked refused a current generation")
	}

	// RUNNING: both the value AND the phase say so. Either one alone is not
	// enough — the value drives the buttons, the phase drives the wire state, and
	// a start that updates only one of them is the reported defect.
	if !ac.RunningState.IsRunning() {
		t.Error("running must be true after a committed start")
	}
	if got, want := ac.classic.currentPhase(), ClassicRunning; got != want {
		t.Errorf("phase after a committed start = %q, want %q.\n"+
			"phaseToWireState maps %q to `starting`, so the UI would show 正在启动 "+
			"for a core that is demonstrably up.", got, want, got)
	}
}

// TestClassicStopReturnsToStopped — the transition must be reversible, and a
// stop that clears the value without clearing the phase leaves the same
// disagreement in the other direction.
func TestClassicStopReturnsToStopped(t *testing.T) {
	ac := newPhaseProbeController(t)
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("beginOperation refused")
	}
	svc := NewProcessService(ac)
	if !svc.commitPrivilegedStartLocked(gen, 1111, 2222, "/tmp/x.pid", "/bin/true") {
		t.Fatal("commit refused")
	}

	// The owned process is gone: the runtime releases the generation, which
	// resets the phase to stopped.
	ac.classic.clearOwnership(gen)
	if !ac.classic.setPhase(gen, ClassicStopped) {
		t.Fatal("setPhase refused a current generation")
	}
	ac.RunningState.SetStopped(events.TeardownUserStop)

	if ac.RunningState.IsRunning() {
		t.Error("running must be false after a confirmed stop")
	}
	if got := ac.classic.currentPhase(); got == ClassicRunning {
		t.Errorf("phase is still %q after a stop; the wire state would keep reporting "+
			"a running core for a process that is gone", got)
	}
}
