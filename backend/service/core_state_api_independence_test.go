package service

import (
	"testing"

	"singbox-launcher/backend/protocol"
)

// TestRunningCoreReportsRunningWithoutAnyAPI is CASE 11.
//
// `Clash API ready` and `core process running` are DIFFERENT FACTS, and the
// launcher must not derive one from the other. A config can legitimately disable
// the Clash API entirely, and the API can be slow, restarting, or pointed at a
// port that is still binding — in every one of those cases the core IS running.
//
// The reported defect is the reverse of this test's name: a running core being
// reported as stopped. This pins the property that makes that impossible at the
// backend layer, so the mistake can only be made by a consumer that ignores the
// wire state.
func TestRunningCoreReportsRunningWithoutAnyAPI(t *testing.T) {
	b := backendWithConfig(t)

	// The core is up. Nothing has been loaded from the Clash API — no proxies, no
	// groups, no selected group — which is exactly the state during startup.
	b.ac.RunningState.Set(true)

	state := b.coreState()
	if state.State != protocol.CoreStateRunning {
		t.Fatalf("with the core running and no API data, coreState() = %q, want %q.\n"+
			"A core whose API has not answered is still a running core; deriving "+
			"liveness from API readiness is what let a working VPN be shown as stopped.",
			state.State, protocol.CoreStateRunning)
	}
}

// TestStoppedCoreIsNotRescuedByCachedAPIData — the converse, and the reason the
// two facts must not be conflated in EITHER direction.
//
// A stale proxy list left over from a previous run must not keep a stopped core
// looking alive. This is the mirror of the reported bug and is just as damaging:
// the user presses Start on a core the UI already believes is running.
func TestStoppedCoreIsNotRescuedByCachedAPIData(t *testing.T) {
	b := backendWithConfig(t)

	b.ac.RunningState.Set(false)
	// Leftovers from an earlier session: the core is gone but the API data it
	// produced is still in memory.
	b.ac.SetProxiesList(nil)

	state := b.coreState()
	if state.State == protocol.CoreStateRunning {
		t.Errorf("coreState() = %q with the core stopped; cached API data must not "+
			"keep a dead core looking alive", state.State)
	}
}

// TestCoreStateIsNeverPermanentlyStarting — CASE 13's backend half.
//
// A start that failed must reach a settled state. `starting` that never resolves
// is the reported symptom in its purest form: the user watches a spinner with no
// way forward, because nothing distinguishes "still working" from "already over".
func TestCoreStateIsNeverPermanentlyStarting(t *testing.T) {
	b := backendWithConfig(t)

	// Begin a start, then settle it with a failure exactly as runCoreOp does.
	op, started := b.ops.beginOp("start")
	if !started {
		t.Fatal("the start was refused")
	}
	if got := b.coreLifecycleState(); got != protocol.CoreStateStarting {
		t.Fatalf("an in-flight start must report %q, got %q", protocol.CoreStateStarting, got)
	}

	b.ops.finishOp(op, errStaleStart)
	got := b.coreLifecycleState()
	if got == protocol.CoreStateStarting {
		t.Error("a SETTLED failed start still reports `starting`; the operation ended " +
			"and the state must say so — a permanent `starting` is a UI with no way out")
	}
	if got != protocol.CoreStateError && got != protocol.CoreStateStopped {
		t.Errorf("a failed start settled to %q, want %q or %q",
			got, protocol.CoreStateError, protocol.CoreStateStopped)
	}
}
