package core

import (
	"testing"
)

// TestDaemonAttachToAlreadyStartedCoreSettles is CASE 4 — the incident.
//
// The daemon's core is ALREADY started when the launcher applies a config. The
// status stream is a pure EDGE stream (SubscribeServiceStatus carries no
// snapshot and no replay), so the STARTED transition that would publish Running
// has already happened and will never be delivered again. The reported symptom
// was a permanent `waiting for the daemon to report STARTED`.
//
// The fix reads the daemon's CURRENT state after the apply instead of waiting for
// a future edge. This test asserts the outcome the user sees: after attaching,
// the authoritative runtime state says running.
func TestDaemonAttachToAlreadyStartedCoreSettles(t *testing.T) {
	d := newFakeDaemon(t, "started") // ALREADY running, before we do anything
	ac, b := newTestDaemonBackend(t, d, false)

	if ac.RunningState.IsRunning() {
		t.Fatal("precondition: the runtime state must start out stopped")
	}

	// The real reconciliation the apply path performs.
	b.reconcileDaemonRuntimeState("test")

	if !ac.RunningState.IsRunning() {
		t.Fatal("attaching to a daemon whose core is ALREADY started must settle the " +
			"runtime state as running; waiting for a STARTED edge that already passed " +
			"is the reported permanent hang")
	}
}

// TestDaemonAttachStillReportsStoppedWhenIdle is the other direction, and it is
// what keeps the fix from being "always say running".
//
// A daemon that reports idle has NOT run the core, so reconciliation must not
// claim it did. Without this, the fix for CASE 4 would introduce the opposite
// lie — the one the original comment at that call site was written to prevent.
func TestDaemonAttachStillReportsStoppedWhenIdle(t *testing.T) {
	d := newFakeDaemon(t, "idle")
	ac, b := newTestDaemonBackend(t, d, false)

	b.reconcileDaemonRuntimeState("test")

	if ac.RunningState.IsRunning() {
		t.Fatal("a daemon reporting idle must not produce a running runtime state")
	}
}

// TestDaemonAttachClearsRunningWhenTheCoreDied — reconciliation goes BOTH ways.
//
// The launcher may believe the core is running from an earlier session while the
// daemon has since gone idle or fatal. Reconciling only the true case would leave
// a dead core shown as connected.
func TestDaemonAttachClearsRunningWhenTheCoreDied(t *testing.T) {
	d := newFakeDaemon(t, "started")
	ac, b := newTestDaemonBackend(t, d, false)

	b.reconcileDaemonRuntimeState("test")
	if !ac.RunningState.IsRunning() {
		t.Fatal("precondition: reconciliation should have reported running")
	}

	// The daemon's core goes away while we are attached.
	d.setStatus("idle")
	b.reconcileDaemonRuntimeState("test")

	if ac.RunningState.IsRunning() {
		t.Error("a daemon that no longer reports a running core must clear the " +
			"running state; otherwise a dead core is shown as connected")
	}
}

// TestDaemonReconcileReportsFatalAsNotRunning — `fatal` is a definite answer.
func TestDaemonReconcileReportsFatalAsNotRunning(t *testing.T) {
	d := newFakeDaemon(t, "fatal")
	ac, b := newTestDaemonBackend(t, d, false)

	b.reconcileDaemonRuntimeState("test")

	if ac.RunningState.IsRunning() {
		t.Error("a FATAL daemon must not report a running core")
	}
}

// TestDaemonStatusMappingIsHonestAboutUnknownValues is the guard against a
// SILENT REGRESSION when the daemon grows a new status.
//
// Mapping an unrecognised string to "stopped" would report a running core as
// stopped — the exact class of bug this whole change exists to remove. The
// mapping must say "I do not know" instead, and the caller leaves the stream
// authoritative.
func TestDaemonStatusMappingIsHonestAboutUnknownValues(t *testing.T) {
	running, known := daemonStatusMeansRunning("started")
	if !running || !known {
		t.Errorf("started => (%v,%v), want (true,true)", running, known)
	}
	for _, s := range []string{"idle", "stopped", "fatal"} {
		running, known := daemonStatusMeansRunning(s)
		if running || !known {
			t.Errorf("%s => (%v,%v), want (false,true)", s, running, known)
		}
	}
	// Case and padding must not change the meaning: the daemon's own spelling is
	// not a contract we should depend on byte-for-byte.
	if running, known := daemonStatusMeansRunning("  STARTED \n"); !running || !known {
		t.Errorf("whitespace/case variant => (%v,%v), want (true,true)", running, known)
	}
	// An unknown value must be reported as UNKNOWN, never as stopped.
	for _, s := range []string{"", "warming-up", "reloading"} {
		running, known := daemonStatusMeansRunning(s)
		if known {
			t.Errorf("%q => (running=%v, known=true); an unrecognised status must be "+
				"reported as unknown so the caller does not silently assert stopped",
				s, running)
		}
	}
}

// TestDaemonAttachDoesNotPublishForARetiredBackend — ownership still wins.
//
// The read is a network round trip, so the window between the pre-apply
// ownership check and this reconciliation is real. A retired backend must not
// publish a state for an engine the user has left.
func TestDaemonAttachDoesNotPublishForARetiredBackend(t *testing.T) {
	d := newFakeDaemon(t, "started")
	ac, b := newTestDaemonBackend(t, d, false)

	// Simulate the user switching engines: the controller's backend is now
	// something else, so this backend is no longer active.
	other := &stubBackend{}
	ac.backend = other

	b.reconcileDaemonRuntimeState("test")

	if ac.RunningState.IsRunning() {
		t.Error("a retired backend must not publish a runtime state; the new engine owns it")
	}
}

// stubBackend is a minimal CoreBackend used only to displace a backend, so the
// retired-backend guard has something else to be active.
type stubBackend struct{}

func (s *stubBackend) Mode() BackendMode { return BackendClassic }
func (s *stubBackend) StartVPN(...bool)  {}
func (s *stubBackend) StopVPN()          {}
func (s *stubBackend) RestartVPN()       {}
func (s *stubBackend) OnAppExit() bool   { return false }
func (s *stubBackend) Close()            {}
