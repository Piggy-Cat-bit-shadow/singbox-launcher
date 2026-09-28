package core

import (
	"testing"

	"singbox-launcher/core/services"
	"singbox-launcher/internal/paths"
)

// newPhaseProbeController builds the minimum AppController the classic runtime
// needs, so these tests exercise the REAL classicRuntime rather than a stub.
func newPhaseProbeController(t *testing.T) *AppController {
	t.Helper()
	dataDir := t.TempDir()
	ac := &AppController{
		FileService: &services.FileService{
			Layout: paths.Layout{
				Data: paths.DataDir(dataDir),
				Logs: paths.LogDir(dataDir + "/logs"),
			},
		},
	}
	ac.RunningState = &RunningState{controller: ac}
	return ac
}

// TestPrivilegedCommitRecordsRunningPhase is CASE 2's structural half, and the
// regression for the reported Classic symptom.
//
// The user's log shows a fully successful privileged start — root-owned copy
// verified, sing-box started, runtime config matched, PID file written, Clash API
// up, four proxy groups loaded — while the UI still said "正在启动".
//
// The cause was that the privileged commit recorded OWNERSHIP and set
// RunningState, but left the classic PHASE at `starting`. `phaseToWireState`
// reads the phase, so the wire state stayed `starting` no matter what the
// runtime knew. The late-adoption path set the phase; the first start did not.
func TestPrivilegedCommitRecordsRunningPhase(t *testing.T) {
	ac := newPhaseProbeController(t)
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("beginOperation refused to start a classic operation")
	}

	// ProcessService.commitPrivilegedStartLocked is the layer that owns
	// RunningState; classicRuntime owns the phase and the process identity. The
	// test drives the real commit so both happen as they do in production.
	svc := NewProcessService(ac)
	if !svc.commitPrivilegedStartLocked(gen, 1111, 2222, "/tmp/x.pid", "/bin/true") {
		t.Fatal("commitPrivilegedStartLocked refused a current generation")
	}

	if !ac.RunningState.IsRunning() {
		t.Error("a committed privileged start must record a running runtime state")
	}
	if got := ac.classic.currentPhase(); got != ClassicRunning {
		t.Errorf("phase after a successful privileged commit = %q, want %q.\n"+
			"phaseToWireState maps %q to `starting`, so the UI shows 正在启动 while "+
			"the core is verifiably up — the reported symptom.",
			got, ClassicRunning, got)
	}
}

// TestPrivilegedCommitDoesNotPromoteASupersededGeneration — the phase write must
// not weaken the generation guard.
//
// commitPrivileged already refuses a stale generation; promoting the phase
// unconditionally would let a superseded start mark the CURRENT generation as
// running, which is the stale-write class this codebase guards everywhere else.
func TestPrivilegedCommitDoesNotPromoteASupersededGeneration(t *testing.T) {
	ac := newPhaseProbeController(t)
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("beginOperation refused")
	}
	// A second operation SUPERSEDES the first. beginOperation refuses while the
	// phase is unsettled, which is the busy guard, so the superseding operation
	// is started the way a restart starts one: allowWhileBusy.
	// The generation only moves via a real teardown, so this test instead pins
	// the other half — that a commit for a generation the runtime has moved past
	// is refused. `newGeneration` is the runtime's own way of moving on.
	ac.classic.renewGeneration()
	if ac.classic.commitPrivileged(gen, 1111, 2222, "/tmp/x.pid", "/bin/true") {
		t.Fatal("commitPrivileged must refuse a generation the runtime has moved past")
	}
	if got := ac.classic.currentPhase(); got == ClassicRunning {
		t.Errorf("a superseded commit promoted the phase to %q; the new operation's "+
			"state must not be overwritten by an older one", got)
	}
}

// TestClassicRunningPhaseSurvivesStrandedWrapper is CASE 2's runtime half — the
// semantics the phase change must not break.
//
// The macOS privileged chain is launcher → helper script → root sing-box. The
// wrapper may exit while the root core keeps running, so the runtime must not
// treat the wrapper's lifetime as the core's. The phase is about the CORE, and
// this pins that the recorded identity is the core PID, not the wrapper's.
func TestClassicRunningPhaseSurvivesStrandedWrapper(t *testing.T) {
	ac := newPhaseProbeController(t)
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("beginOperation refused")
	}
	// Distinct PIDs: 1111 = wrapper script, 2222 = the actual core.
	if !ac.classic.commitPrivileged(gen, 1111, 2222, "/tmp/x.pid", "/bin/true") {
		t.Fatal("commitPrivileged refused")
	}

	identity, owned, privileged := ac.classic.ownedProcess()
	if !owned || !privileged {
		t.Fatalf("the privileged core must be owned after commit (owned=%v privileged=%v)",
			owned, privileged)
	}
	if identity.PID != 2222 {
		t.Errorf("the owned identity is PID %d; the CORE pid (2222) is the one whose "+
			"lifetime matters — the wrapper is a shell that may exit while the root "+
			"core keeps running", identity.PID)
	}
	// The phase is a statement about the core, so it stays running even though a
	// wrapper process would have exited.
	if got := ac.classic.currentPhase(); got != ClassicRunning {
		t.Errorf("phase = %q after a committed privileged start, want %q",
			got, ClassicRunning)
	}
}
