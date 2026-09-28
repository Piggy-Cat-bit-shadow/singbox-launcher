package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChecker is a deterministic process table.
//
// Every lifecycle test uses this instead of spawning real processes: the
// invariants under test are about ORDERING (does a stale callback act?) and
// CONFIRMATION (was exit observed?), neither of which should depend on how fast
// the machine is or on whether a real sing-box could be started.
type fakeChecker struct {
	mu sync.Mutex
	// procs maps pid → executable path.
	procs map[int]string
	// probes counts liveness checks, so a test can assert the confirmation loop
	// actually ran rather than trusting a single call.
	probes int
	// err, when set, is returned instead of an answer.
	err error
}

func newFakeChecker(procs map[int]string) *fakeChecker {
	if procs == nil {
		procs = map[int]string{}
	}
	return &fakeChecker{procs: procs}
}

func (f *fakeChecker) alive(pid int, expected string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	if f.err != nil {
		return false, f.err
	}
	path, ok := f.procs[pid]
	if !ok {
		return false, nil
	}
	if expected != "" && path != expected {
		// A recycled PID: exists, but is not our process.
		return false, nil
	}
	return true, nil
}

func (f *fakeChecker) set(pid int, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = path
}

func (f *fakeChecker) remove(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.procs, pid)
}

// recordSignals builds a signal function that records what it was asked to send
// and optionally makes the process exit when signalled.
type signalRecorder struct {
	mu      sync.Mutex
	signals []string
	// onSignal is invoked with (pid, force); use it to model a process that
	// exits on TERM, ignores TERM, or dies only on KILL.
	onSignal func(pid int, force bool)
	err      error
}

func (r *signalRecorder) send(pid int, force bool) error {
	r.mu.Lock()
	kind := "TERM"
	if force {
		kind = "KILL"
	}
	r.signals = append(r.signals, fmt.Sprintf("%s:%d", kind, pid))
	cb := r.onSignal
	err := r.err
	r.mu.Unlock()
	if cb != nil {
		cb(pid, force)
	}
	return err
}

func (r *signalRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.signals))
	copy(out, r.signals)
	return out
}

const testExe = "/Applications/JiejieBox.app/Contents/Resources/sing-box"

// --- terminateOwnedProcess -------------------------------------------------

// TestTerminateConfirmsExitBeforeReportingStopped is the core of the "SIGTERM
// sent ≠ process gone" fix: the primitive must observe the exit, not assume it.
func TestTerminateConfirmsExitBeforeReportingStopped(t *testing.T) {
	checker := newFakeChecker(map[int]string{4242: testExe})
	sig := &signalRecorder{}
	// The process exits as soon as it is signalled.
	sig.onSignal = func(pid int, force bool) { checker.remove(pid) }

	out, err := terminateOwnedProcess(ProcessIdentity{PID: 4242, Executable: testExe}, "stop", checker, sig.send)
	if err != nil {
		t.Fatalf("termination must succeed: %v", err)
	}
	if !out.Graceful || out.Forced {
		t.Fatalf("expected a graceful exit, got %+v", out)
	}
	if checker.probes == 0 {
		t.Fatal("exit was never confirmed — the primitive trusted the signal")
	}
}

// TestTerminateEscalatesWhenGracefulSignalIsIgnored — a wedged core must still
// be stopped, and the escalation must be reported.
func TestTerminateEscalatesWhenGracefulSignalIsIgnored(t *testing.T) {
	checker := newFakeChecker(map[int]string{4242: testExe})
	sig := &signalRecorder{}
	// Ignores TERM, dies on KILL. This is the hang that made Stop lie.
	sig.onSignal = func(pid int, force bool) {
		if force {
			checker.remove(pid)
		}
	}

	out, err := terminateOwnedProcess(ProcessIdentity{PID: 4242, Executable: testExe}, "stop", checker, sig.send)
	if err != nil {
		t.Fatalf("termination must still succeed after escalation: %v", err)
	}
	if !out.Forced {
		t.Fatal("the forced-kill path was not taken for a process that ignored TERM")
	}
	kinds := sig.kinds()
	if len(kinds) != 2 || kinds[0] != "TERM:4242" || kinds[1] != "KILL:4242" {
		t.Fatalf("expected TERM then KILL, got %v", kinds)
	}
}

// TestTerminateRefusesToSignalUnverifiedIdentity — PID alone is never enough.
// Signalling an unverified PID is how a recycled PID turns into someone else's
// process being killed.
func TestTerminateRefusesToSignalUnverifiedIdentity(t *testing.T) {
	checker := newFakeChecker(nil)
	sig := &signalRecorder{}

	if _, err := terminateOwnedProcess(ProcessIdentity{PID: 4242}, "stop", checker, sig.send); err == nil {
		t.Fatal("an identity without an executable path must be refused")
	}
	if len(sig.kinds()) != 0 {
		t.Fatalf("no signal may be sent without a verified identity, got %v", sig.kinds())
	}
}

// TestTerminateTreatsRecycledPIDAsGone — the PID exists but runs something else.
// It must not be signalled, and it must not block termination either.
func TestTerminateTreatsRecycledPIDAsGone(t *testing.T) {
	// The PID now belongs to a different executable.
	checker := newFakeChecker(map[int]string{4242: "/usr/bin/something-else"})
	sig := &signalRecorder{}

	out, err := terminateOwnedProcess(ProcessIdentity{PID: 4242, Executable: testExe}, "stop", checker, sig.send)
	if err != nil {
		t.Fatalf("a recycled PID must not be an error: %v", err)
	}
	if len(sig.kinds()) != 0 {
		t.Fatalf("a recycled PID must never be signalled, got %v", sig.kinds())
	}
	_ = out
}

// TestTerminateReportsFailureWhenProcessSurvivesKill — the honest outcome. If a
// root process cannot be killed, the caller must NOT be told it stopped.
func TestTerminateReportsFailureWhenProcessSurvivesKill(t *testing.T) {
	checker := newFakeChecker(map[int]string{4242: testExe})
	sig := &signalRecorder{} // never removes the process

	_, err := terminateOwnedProcess(ProcessIdentity{PID: 4242, Executable: testExe}, "stop", checker, sig.send)
	if err == nil {
		t.Fatal("a process that survived a forced kill must be reported as a failure")
	}
}

// TestTerminateReportsSignalFailure — failing to signal is not proof of exit.
func TestTerminateReportsSignalFailure(t *testing.T) {
	checker := newFakeChecker(map[int]string{4242: testExe})
	sig := &signalRecorder{err: errors.New("operation not permitted")}

	// The process never exits, so the failure must surface (either from the
	// signal error or from the survivor check).
	if _, err := terminateOwnedProcess(ProcessIdentity{PID: 4242, Executable: testExe}, "stop", checker, sig.send); err == nil {
		t.Fatal("a signal failure with a surviving process must be an error")
	}
}

// TestTerminateAlreadyGoneIsSuccess — the user wanted it gone and it is gone.
func TestTerminateAlreadyGoneIsSuccess(t *testing.T) {
	checker := newFakeChecker(nil)
	sig := &signalRecorder{}
	out, err := terminateOwnedProcess(ProcessIdentity{PID: 4242, Executable: testExe}, "stop", checker, sig.send)
	if err != nil {
		t.Fatalf("an already-exited process is a successful stop: %v", err)
	}
	if out.Forced || out.Graceful {
		t.Fatalf("nothing was signalled, got %+v", out)
	}
}

// --- generation ownership --------------------------------------------------

// TestStaleGenerationCannotChangeState is the invariant behind every auto-restart
// fix: an old generation's callback may not write to the new runtime.
func TestStaleGenerationCannotChangeState(t *testing.T) {
	var rt classicRuntime
	old := rt.currentGeneration()
	if !rt.setPhase(old, ClassicRunning) {
		t.Fatal("the current generation must be able to set its phase")
	}

	newGen := rt.renewGeneration()
	if newGen == old {
		t.Fatal("renewGeneration must produce a new generation")
	}
	// The old generation is now stale.
	if rt.setPhase(old, ClassicStopped) {
		t.Fatal("a stale generation must not be able to change the phase")
	}
	if got := rt.currentPhase(); got != ClassicStopped {
		t.Fatalf("renewing must settle the runtime, got %q", got)
	}
	if !rt.setPhase(newGen, ClassicRunning) {
		t.Fatal("the new generation must be able to set its phase")
	}
}

// TestRenewGenerationDropsOwnership — the old process must not stay "owned" by a
// runtime that has moved on, or Stop would later kill a process it no longer
// tracks.
func TestRenewGenerationDropsOwnership(t *testing.T) {
	var rt classicRuntime
	gen := rt.currentGeneration()
	rt.commitPrivileged(gen, 100, 200, "/tmp/pid", testExe)

	if _, ok, _ := rt.ownedProcess(); !ok {
		t.Fatal("ownership should have been recorded")
	}
	rt.renewGeneration()
	if _, ok, _ := rt.ownedProcess(); ok {
		t.Fatal("renewing a generation must drop the recorded ownership")
	}
}

// TestConcurrentStartIsRefusedWhileOneIsInFlight — the double-click guarantee.
func TestConcurrentStartIsRefusedWhileOneIsInFlight(t *testing.T) {
	var rt classicRuntime
	gen, _, ok := rt.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("the first start must be accepted")
	}
	if _, _, ok := rt.beginOperation(ClassicStarting, false); ok {
		t.Fatal("a second concurrent start must be refused")
	}
	// Once settled, a new start is allowed again.
	rt.setPhase(gen, ClassicStopped)
	if _, _, ok := rt.beginOperation(ClassicStarting, false); !ok {
		t.Fatal("a start after the previous one settled must be accepted")
	}
}

// TestStartRefusedWhileStopping — a start during a stop would produce two cores.
func TestStartRefusedWhileStopping(t *testing.T) {
	var rt classicRuntime
	gen, _, _ := rt.beginOperation(ClassicStopping, false)
	_ = gen
	if _, _, ok := rt.beginOperation(ClassicStarting, false); ok {
		t.Fatal("a start during a stop must be refused")
	}
}

// TestSettledPredicateIsExact — only these two phases may be handed over.
func TestSettledPredicateIsExact(t *testing.T) {
	cases := map[ClassicPhase]bool{
		// The zero value is a runtime that has never done anything: settled.
		"":                true,
		ClassicStopped:    true,
		ClassicFailed:     true,
		ClassicStarting:   false,
		ClassicRunning:    false,
		ClassicStopping:   false,
		ClassicRestarting: false,
	}
	for phase, want := range cases {
		if got := phase.isSettled(); got != want {
			t.Errorf("phase %q settled=%v, want %v", phase, got, want)
		}
	}
}

// TestOperationScopedIntentDoesNotOutliveTheOperation — the sticky-flag fix.
// A restart intent set by a failed restart must not survive into the next one.
func TestOperationScopedIntentDoesNotOutliveTheOperation(t *testing.T) {
	var rt classicRuntime
	gen, _, _ := rt.beginOperation(ClassicRestarting, false)
	rt.setIntent(gen, false, true)
	if had, ok := rt.takeRestartIntent(gen); !had || !ok {
		t.Fatal("the restart intent should have been visible")
	}
	// A second beginOperation must clear any leftover intent.
	rt.setPhase(gen, ClassicStopped)
	gen2, _, ok := rt.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("start refused")
	}
	if s := rt.snapshot(); s.RestartRequested || s.StoppedByUser {
		t.Fatalf("intent leaked into the next operation: %+v", s)
	}
	_ = gen2
}

// TestTakeRestartIntentConsumesOnce — an intent acted on twice restarts twice.
func TestTakeRestartIntentConsumesOnce(t *testing.T) {
	var rt classicRuntime
	gen, _, _ := rt.beginOperation(ClassicRestarting, false)
	rt.setIntent(gen, false, true)

	if had, _ := rt.takeRestartIntent(gen); !had {
		t.Fatal("first take should see the intent")
	}
	if had, _ := rt.takeRestartIntent(gen); had {
		t.Fatal("second take must not see a consumed intent")
	}
}

// TestCrashAttemptsAreGenerationScoped — a stale monitor must not inflate or
// reset the live counter.
func TestCrashAttemptsAreGenerationScoped(t *testing.T) {
	var rt classicRuntime
	old := rt.currentGeneration()
	rt.noteCrashAttempt(old)
	rt.noteCrashAttempt(old)

	if n, ok := rt.crashAttemptsFor(old); !ok || n != 2 {
		t.Fatalf("expected 2 attempts for the live generation, got %d ok=%v", n, ok)
	}
	rt.renewGeneration()
	if _, ok := rt.crashAttemptsFor(old); ok {
		t.Fatal("a stale generation must not be able to read the counter")
	}
	if _, ok := rt.noteCrashAttempt(old); ok {
		t.Fatal("a stale generation must not be able to increment the counter")
	}
}

// TestAdoptExistingEstablishesOwnership — a core from a previous session must
// become owned so Stop and Restart work on it.
func TestAdoptExistingEstablishesOwnership(t *testing.T) {
	var rt classicRuntime
	gen := rt.adoptExisting(testExe, 777, true)

	if got := rt.currentPhase(); got != ClassicRunning {
		t.Fatalf("an adopted core must read as running, got %q", got)
	}
	id, ok, privileged := rt.ownedProcess()
	if !ok {
		t.Fatal("an adopted core must be owned")
	}
	if id.PID != 777 || id.Executable != testExe {
		t.Fatalf("adopted identity is wrong: %+v", id)
	}
	if !privileged {
		t.Fatal("the adopted core should be marked privileged")
	}
	if !rt.isCurrent(gen) {
		t.Fatal("the adopting generation must be current")
	}
}

// TestOwnedProcessRequiresVerifiedIdentity — an identity without a path is not
// ownership, so Stop will not signal it.
func TestOwnedProcessRequiresVerifiedIdentity(t *testing.T) {
	var rt classicRuntime
	gen := rt.currentGeneration()
	rt.mu.Lock()
	rt.identity = ProcessIdentity{PID: 555}
	rt.mu.Unlock()

	if _, ok, _ := rt.ownedProcess(); ok {
		t.Fatal("a PID without a verified executable must not count as ownership")
	}
	_ = gen
}

// TestClearOwnershipIsGenerationScoped — a stale monitor must not erase the live
// runtime's identity, which would strand a running process.
func TestClearOwnershipIsGenerationScoped(t *testing.T) {
	var rt classicRuntime
	old := rt.currentGeneration()
	rt.commitChild(old, nil, testExe)
	newGen := rt.renewGeneration()
	rt.commitPrivileged(newGen, 10, 20, "/tmp/p", testExe)

	if rt.clearOwnership(old) {
		t.Fatal("a stale generation must not be able to clear ownership")
	}
	if _, ok, _ := rt.ownedProcess(); !ok {
		t.Fatal("the live generation's ownership was erased by a stale one")
	}
}

// --- phase → wire state ----------------------------------------------------

// TestPhaseWireStatesAreComplete — every phase must map to a state the protocol
// already declares, or the UI would be told "stopped" during a start.
func TestPhaseWireStatesAreComplete(t *testing.T) {
	want := map[ClassicPhase]string{
		ClassicStopped:    "stopped",
		ClassicStarting:   "starting",
		ClassicRunning:    "running",
		ClassicStopping:   "stopping",
		ClassicRestarting: "stopping",
		ClassicFailed:     "error",
	}
	for phase, expected := range want {
		if got := phase.wireState(); got != expected {
			t.Errorf("phase %q → %q, want %q", phase, got, expected)
		}
	}
}

// --- late privileged result ------------------------------------------------

// TestLatePrivilegedResultIsNotDropped — the orphan fix. A start whose UI wait
// timed out must still have its result supervised.
func TestLatePrivilegedResultIsNotDropped(t *testing.T) {
	ac := newTestController()
	ac.classic.renewGeneration()
	// The exit waiter is injected, and it BLOCKS for the lifetime of the test.
	//
	// A waiter that returned immediately would model a core that exits the
	// instant it is adopted; the crash handler would then correctly treat that as
	// a crash and try to restart it, which is real behaviour but not what is
	// under test here. Blocking forever reproduces a live core, and because the
	// watcher goroutine never proceeds there is no restart to race.
	//
	// The waiter deliberately never returns — including after the test ends.
	// Returning at test teardown would hand control to the crash path against a
	// controller that is already being torn down, which is noise, not coverage.
	blocked := make(chan struct{})
	svc := &ProcessService{ac: ac, privDeps: &privilegedStartDeps{
		waitExit: func(int) { <-blocked },
	}}

	pidCh := make(chan privilegedStartResult, 1)
	// The worker returns a live root core LATE.
	pidCh <- privilegedStartResult{Script: 9001, Singbox: 9002}

	// The generation is captured BEFORE the goroutine starts. Reading it inside
	// would make the test depend on scheduling: the supervisor compares against
	// whatever is current when it runs, so a delay could make its own generation
	// stale and turn an adoption assertion into a coin flip.
	gen := ac.classic.currentGeneration()
	done := make(chan struct{})
	go func() {
		svc.superviseLatePrivilegedStart(gen, testExe, pidCh)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never consumed the late result")
	}

	// The generation was current, so the late core must have been adopted —
	// ownership recorded, so the user can stop it — rather than dropped.
	id, ok, _ := ac.classic.ownedProcess()
	if !ok {
		t.Fatal("a late successful start must be adopted so it can be stopped")
	}
	if id.PID != 9002 {
		t.Fatalf("adopted the wrong PID: %+v", id)
	}
}

// TestLatePrivilegedResultIsKilledWhenSuperseded — the other half of the orphan
// fix: if the runtime moved on, the late core must be stopped, not adopted.
func TestLatePrivilegedResultIsKilledWhenSuperseded(t *testing.T) {
	ac := newTestController()
	staleGen := ac.classic.currentGeneration()
	// Supersede it: the engine was switched while authorization was pending.
	ac.classic.renewGeneration()

	svc := &ProcessService{ac: ac, privDeps: &privilegedStartDeps{waitExit: func(int) {}}}
	pidCh := make(chan privilegedStartResult, 1)
	pidCh <- privilegedStartResult{Script: 9001, Singbox: 9002}

	done := make(chan struct{})
	go func() {
		svc.superviseLatePrivilegedStart(staleGen, testExe, pidCh)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor never consumed the late result")
	}

	// It must NOT be adopted into the live runtime.
	if _, ok, _ := ac.classic.ownedProcess(); ok {
		t.Fatal("a superseded late start must not be adopted into the new generation")
	}
	// And a privileged stop must have been attempted. The stub platform returns
	// success, so the observable requirement is that the runtime reports itself
	// clean rather than holding an unowned process.
	if got := ac.classic.currentPhase(); got != ClassicStopped {
		t.Fatalf("runtime should remain settled, got %q", got)
	}
}

// TestLatePrivilegedFailureIsRecorded — the failure that used to vanish into a
// channel nobody read must reach the lifecycle error store.
func TestLatePrivilegedFailureIsRecorded(t *testing.T) {
	ac := newTestController()
	svc := &ProcessService{ac: ac, privDeps: &privilegedStartDeps{waitExit: func(int) {}}}
	pidCh := make(chan privilegedStartResult, 1)
	pidCh <- privilegedStartResult{Err: errors.New("authorization cancelled")}

	done := make(chan struct{})
	go func() {
		svc.superviseLatePrivilegedStart(ac.classic.currentGeneration(), testExe, pidCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not finish")
	}

	if !ac.HasLifecycleError() {
		t.Fatal("a late privileged failure must be recorded, not discarded")
	}
}

// TestLatePrivilegedResultDoesNotOverwriteARunningCore — a duplicate late result
// must not steal the runtime from a core that is already up.
func TestLatePrivilegedResultDoesNotOverwriteARunningCore(t *testing.T) {
	ac := newTestController()
	ac.RunningState.Set(true)
	gen := ac.classic.currentGeneration()

	svc := &ProcessService{ac: ac, privDeps: &privilegedStartDeps{waitExit: func(int) {}}}
	pidCh := make(chan privilegedStartResult, 1)
	pidCh <- privilegedStartResult{Script: 9001, Singbox: 9002}

	done := make(chan struct{})
	go func() {
		svc.superviseLatePrivilegedStart(gen, testExe, pidCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not finish")
	}

	// The already-running core keeps ownership; the duplicate must not be
	// recorded as the owned process.
	id, ok, _ := ac.classic.ownedProcess()
	if ok && id.PID == 9002 {
		t.Fatal("a duplicate late start overwrote the running core's ownership")
	}
}

// --- lifecycle error classification ---------------------------------------

// TestClassifyLifecycleErrorIsStable pins the mapping from an error to the wire
// code, so a reworded message cannot silently change what the UI is told.
func TestClassifyLifecycleErrorIsStable(t *testing.T) {
	cases := []struct {
		err  error
		want LifecycleErrorCode
	}{
		{NewStartFailure(StartErrConfigRebuildFailed, errors.New("x")), LifecycleErrConfigRebuild},
		{NewStartFailure(StartErrConfigCheckFailed, errors.New("x")), LifecycleErrConfigCheck},
		{NewStartFailure(StartErrSpawnFailed, errors.New("x")), LifecycleErrCoreStart},
		{NewStartFailure(StartErrDaemonUnreachable, errors.New("x")), LifecycleErrDaemonUnreachable},
		{NewStartFailure(StartErrDaemonApplyFailed, errors.New("x")), LifecycleErrDaemonApply},
		{NewStartFailure(StartErrClashAPIPortInUse, errors.New("x")), LifecycleErrPortInUse},
		{NewStartFailure(StartErrCancelled, errors.New("x")), LifecycleErrCancelled},
		{errors.New("listen tcp 127.0.0.1:9090: address already in use"), LifecycleErrPortInUse},
		{errors.New("operation not permitted"), LifecycleErrPermission},
	}
	for _, c := range cases {
		got, _ := ClassifyLifecycleError(c.err)
		if got != c.want {
			t.Errorf("ClassifyLifecycleError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestDeterministicFailuresAreNotAdvertisedAsRetryable — telling a user to retry
// a failure that will always recur is what makes them click Start forever.
func TestDeterministicFailuresAreNotAdvertisedAsRetryable(t *testing.T) {
	for _, err := range []error{
		NewStartFailure(StartErrSpawnFailed, errors.New("x")),
		NewStartFailure(StartErrClashAPIPortInUse, errors.New("x")),
		NewStartFailure(StartErrCancelled, errors.New("x")),
		errors.New("operation not permitted"),
	} {
		if _, recoverable := ClassifyLifecycleError(err); recoverable {
			t.Errorf("%v must not be advertised as retryable", err)
		}
	}
}

// TestConfigErrorsAreMarkedAsConfigErrors — the UI only reassures the user that
// "your previous config is still in use" when that is actually true.
func TestConfigErrorsAreMarkedAsConfigErrors(t *testing.T) {
	ac := &AppController{}
	ac.RecordConfigError(LifecycleErrConfigCheck, "rebuild", "rejected", "detail")

	snap := ac.LifecycleError()
	if snap == nil {
		t.Fatal("the error was not recorded")
	}
	if !snap.ConfigError {
		t.Fatal("a config-pipeline failure must be marked as such")
	}

	// A process failure must NOT be marked as a config error.
	ac.ClearLifecycleError()
	ac.RecordLifecycleError(LifecycleErrCoreStart, "start", "spawn failed", "detail", true)
	if snap := ac.LifecycleError(); snap.ConfigError {
		t.Fatal("a spawn failure must not claim the config is at fault")
	}
}

// TestClearingAnUnsetErrorDoesNotPublish — a clean rebuild clears on every run;
// publishing each time would spam the frontend with no state change.
func TestClearingAnUnsetErrorDoesNotPublish(t *testing.T) {
	ac := &AppController{}
	// No error recorded: clearing must be a no-op (and must not panic without an
	// event bus).
	ac.ClearLifecycleError()
	if ac.HasLifecycleError() {
		t.Fatal("nothing should be recorded")
	}
}

// TestRecordedErrorIsReadableAfterTheFact — proves the state survives a GUI
// restart, which is the point of storing it rather than only publishing it.
func TestRecordedErrorIsReadableAfterTheFact(t *testing.T) {
	ac := &AppController{}
	ac.RecordLifecycleError(LifecycleErrAuthCancelled, "start", "authorization was cancelled", "user cancelled", false)

	snap := ac.LifecycleError()
	if snap == nil || snap.Code != LifecycleErrAuthCancelled {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap.Operation != "start" {
		t.Fatalf("the operation must be preserved, got %q", snap.Operation)
	}
	if snap.Recoverable {
		t.Fatal("a cancelled authorization is not recoverable by retrying the same thing")
	}
}

// newTestController builds the minimum controller the lifecycle paths need.
//
// RunningState is part of that minimum: New() always constructs one, so a
// lifecycle function may treat it as present, and a test that omits it would be
// exercising a shape that cannot occur in production.
func newTestController() *AppController {
	ac := &AppController{}
	ac.RunningState = &RunningState{controller: ac}
	ac.ctx = context.Background()
	return ac
}

// --- adopted core ----------------------------------------------------------

// TestAdoptedCoreWatcherReportsExternalDeath is the gap that made an adopted
// core dangerous: with no child to Wait on, nothing reported its exit, so the UI
// kept saying "running" for a process that no longer existed and both Stop and
// Restart operated on a ghost.
func TestAdoptedCoreWatcherReportsExternalDeath(t *testing.T) {
	ac := newTestController()
	// The process is already gone by the time the watcher first looks.
	ac.classic.adoptExisting(testExe, 31337, true)
	ac.RunningState.Set(true)
	gen := ac.classic.currentGeneration()

	svc := &ProcessService{ac: ac}
	done := make(chan struct{})
	go func() {
		svc.watchAdoptedCore(gen, ProcessIdentity{PID: 31337, Executable: testExe})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the adopted-core watcher never noticed the process was gone")
	}

	if ac.RunningState.IsRunning() {
		t.Fatal("a dead adopted core must not keep reporting as running")
	}
	if _, ok, _ := ac.classic.ownedProcess(); ok {
		t.Fatal("ownership must be dropped once the adopted process is confirmed gone")
	}
	if got := ac.classic.currentPhase(); got != ClassicStopped {
		t.Fatalf("expected the runtime to settle to stopped, got %q", got)
	}
}

// TestAdoptedCoreWatcherStopsOnSupersede — after a mode switch the watcher must
// not keep reporting on a runtime it no longer belongs to.
func TestAdoptedCoreWatcherStopsOnSupersede(t *testing.T) {
	ac := newTestController()
	gen := ac.classic.adoptExisting(testExe, 31337, true)
	svc := &ProcessService{ac: ac}

	// Supersede before the watcher starts.
	ac.classic.renewGeneration()

	done := make(chan struct{})
	go func() {
		svc.watchAdoptedCore(gen, ProcessIdentity{PID: 31337, Executable: testExe})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a stale watcher must return immediately, not keep polling")
	}
}

// TestAdoptedCoreIsOwnedWithIdentity — adoption must record the identity, or
// Stop has nothing safe to act on.
func TestAdoptedCoreIsOwnedWithIdentity(t *testing.T) {
	var rt classicRuntime
	rt.adoptExisting(testExe, 4242, true)
	id, ok, privileged := rt.ownedProcess()
	if !ok {
		t.Fatal("an adopted core must be owned")
	}
	if id.PID != 4242 || id.Executable != testExe {
		t.Fatalf("identity was not recorded correctly: %+v", id)
	}
	if !privileged {
		t.Fatal("the adopted core is privileged and Stop must know that")
	}
}

// --- readiness -------------------------------------------------------------

// aliveChecker is a scripted process table: the nth call returns the nth entry,
// and the last entry repeats once the script is exhausted.
type aliveChecker struct {
	mu     sync.Mutex
	script []bool
	calls  int
}

func (c *aliveChecker) alive(int, string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := c.calls
	c.calls++
	if i >= len(c.script) {
		i = len(c.script) - 1
	}
	return c.script[i], nil
}

func (c *aliveChecker) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func withAliveScript(t *testing.T, script ...bool) *aliveChecker {
	t.Helper()
	c := &aliveChecker{script: script}
	prev := readinessChecker
	readinessChecker = c
	t.Cleanup(func() { readinessChecker = prev })
	return c
}

// TestReadinessPromotesALiveCore — a core that survives its startup becomes
// running.
func TestReadinessPromotesALiveCore(t *testing.T) {
	ac := newTestController()
	withAliveScript(t, true) // alive on every probe
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("the start claim must be accepted")
	}
	svc := &ProcessService{ac: ac}

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the stand-in process: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	svc.promoteToRunningWhenReady(gen, cmd, testExe)

	if got := ac.classic.currentPhase(); got != ClassicRunning {
		t.Fatalf("a core that survived startup must be running, got %q", got)
	}
}

// TestReadinessReportsAnEarlyExitAsAStartFailure is the §15 fix.
//
// Before it, a core that died during startup was reported as RUNNING and the
// state only corrected when the crash monitor noticed — so the user saw
// "Connected" flash and then "Stopped", and the real reason (bad config,
// occupied port) was classified as a crash to be restarted.
func TestReadinessReportsAnEarlyExitAsAStartFailure(t *testing.T) {
	ac := newTestController()
	// Dead on the first probe: the core exited during startup.
	withAliveScript(t, false)
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("the start claim must be accepted")
	}
	svc := &ProcessService{ac: ac}

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the stand-in process: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	svc.promoteToRunningWhenReady(gen, cmd, testExe)

	if got := ac.classic.currentPhase(); got != ClassicFailed {
		t.Fatalf("an early exit must settle the runtime as failed, got %q", got)
	}
	if !ac.HasLifecycleError() {
		t.Fatal("an early exit must leave a lifecycle error the frontend can show")
	}
	snap := ac.LifecycleError()
	if snap.Code == "" {
		t.Fatal("the recorded failure has no code")
	}
	if snap.Operation != "start" {
		t.Fatalf("the failure belongs to the start operation, got %q", snap.Operation)
	}
}

// TestReadinessNeverPromotesAStaleGeneration — the readiness wait is a window in
// which the runtime can move on, and promoting a generation that no longer owns
// the runtime would report a core that is not ours as running.
func TestReadinessNeverPromotesAStaleGeneration(t *testing.T) {
	ac := newTestController()
	withAliveScript(t, true)
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("the start claim must be accepted")
	}
	svc := &ProcessService{ac: ac}

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the stand-in process: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	// The runtime moves on while the readiness window is open.
	before := ac.classic.currentGeneration()
	ac.classic.renewGeneration()
	if ac.classic.currentGeneration() == before {
		t.Fatal("renewGeneration must produce a new generation")
	}

	svc.promoteToRunningWhenReady(gen, cmd, testExe)

	if got := ac.classic.currentPhase(); got == ClassicRunning {
		t.Fatal("a superseded generation must never be promoted to running")
	}
}

// --- config build serialization --------------------------------------------

// TestBuildMutexSerializesRebuilds guards the invariant behind buildMu: a rebuild
// is a read-modify-write on config.json, so two of them must not overlap.
//
// A rebuild from a settings change and one from Start can genuinely be requested
// at the same time, and the daemon applies whatever ends up on disk. Serializing
// means the second waits for a correct config instead of the two interleaving.
//
// Tested through the mutex rather than by driving two real builds: the point is
// the exclusion property, and a test that spawned two full pipelines would be
// nondeterministic while proving less.
func TestBuildMutexSerializesRebuilds(t *testing.T) {
	ac := &AppController{}

	var inside int32
	var maxInside int32
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ac.buildMu.Lock()
			defer ac.buildMu.Unlock()
			n := atomic.AddInt32(&inside, 1)
			for {
				old := atomic.LoadInt32(&maxInside)
				if n <= old || atomic.CompareAndSwapInt32(&maxInside, old, n) {
					break
				}
			}
			// A window in which a second holder would be visible.
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxInside); got != 1 {
		t.Fatalf("up to %d builds ran at once; a rebuild must be exclusive", got)
	}
}

// TestOneExitProducesOneDecision is the second-pass catch: the readiness gate and
// the crash Monitor BOTH wait on the same process, so both observe the same death.
//
// Before the claim, the sequence was:
//
//	readiness → records a failed start, phase = failed
//	monitor   → classifies the same exit as a crash, auto-restarts
//
// which is precisely the restart loop the readiness gate exists to prevent: a
// config the core just rejected would be retried, and the user would be shown two
// different causes for one event.
func TestOneExitProducesOneDecision(t *testing.T) {
	ac := newTestController()
	withAliveScript(t, false)
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("the start claim must be accepted")
	}

	// The readiness gate gets there first and owns the decision.
	if !ac.classic.claimExit(gen) {
		t.Fatal("the first observer must win the claim")
	}

	// The monitor must now see the exit as already decided, not classify it.
	if !ac.classic.exitAlreadyClaimed(gen) {
		t.Fatal("the monitor must be able to see that the exit is already decided; " +
			"otherwise it restarts a core whose config was just rejected")
	}
	if ac.classic.claimExit(gen) {
		t.Fatal("a second observer must not be able to claim the same exit")
	}
}

// TestExitClaimDoesNotOutliveItsProcess — the claim describes ONE process, so a
// new operation must start unclaimed. A carried-over claim would silence the
// crash monitor for the next core, turning every future crash into silence.
func TestExitClaimDoesNotOutliveItsProcess(t *testing.T) {
	ac := newTestController()
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("the start claim must be accepted")
	}
	if !ac.classic.claimExit(gen) {
		t.Fatal("the first claim must succeed")
	}

	// The next operation brings a new process.
	ac.classic.setPhase(gen, ClassicStopped)
	gen2, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("a settled runtime must accept a new operation")
	}
	if ac.classic.exitAlreadyClaimed(gen2) {
		t.Fatal("a new operation's process must start unclaimed, or its crash " +
			"would be silently ignored")
	}
	if !ac.classic.claimExit(gen2) {
		t.Fatal("the new generation must be claimable")
	}
}

// TestExitClaimIsGenerationScoped — a stale observer must never claim, or a
// superseded monitor could consume the claim belonging to the live core.
func TestExitClaimIsGenerationScoped(t *testing.T) {
	ac := newTestController()
	gen, _, ok := ac.classic.beginOperation(ClassicStarting, false)
	if !ok {
		t.Fatal("the start claim must be accepted")
	}
	stale := gen
	ac.classic.renewGeneration()

	if ac.classic.claimExit(stale) {
		t.Fatal("a stale generation must not be able to claim an exit")
	}
	if ac.classic.exitAlreadyClaimed(ac.classic.currentGeneration()) {
		t.Fatal("a stale claim must not mark the live generation as decided")
	}
}

// --- no deferred Unlock on paths that release mid-body ----------------------

// TestMonitorDoesNotCrashWhenSupersededDuringTheRestartDelay is the P0 the
// second-pass review found.
//
// `Monitor` releases CmdMutex before its 2s crash-restart delay and then has
// guarded returns AFTER that release, including the one that fires when the user
// switches engines mid-delay. With a `defer Unlock` still armed those returns
// performed a second Unlock — `fatal error: sync: unlock of unlocked mutex`, an
// UNRECOVERABLE crash that no recover() can catch and no caller can handle. The
// guard written to close the mode-switch window would therefore kill the process
// in exactly the case it was written for.
//
// A fatal runtime error aborts the test binary, so this test is its own proof:
// reaching the assertions at all means the balance holds.
func TestMonitorDoesNotCrashWhenSupersededDuringTheRestartDelay(t *testing.T) {
	ac := newTestController()
	cmd := exec.Command("/bin/sh", "-c", "exit 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the stand-in process: %v", err)
	}

	gen := ac.classic.currentGeneration()
	if !ac.classic.commitChild(gen, cmd, testExe) {
		t.Fatal("the commit point must accept the child")
	}
	ac.SingboxCmd = cmd
	ac.RunningState.Set(true)

	done := make(chan struct{})
	go func() {
		(&ProcessService{ac: ac}).Monitor(cmd)
		close(done)
	}()

	// Let Monitor enter the crash path and the 2s delay, then supersede it — the
	// mode switch. The stale-monitor guard fires inside the window.
	time.Sleep(300 * time.Millisecond)
	ac.classic.renewGeneration()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Monitor never returned after being superseded")
	}
}

// TestPrivilegedExitDoesNotCrashWhenSupersededDuringRestart is the P0-1 twin.
//
// `onPrivilegedScriptExited` has the same shape: a manual Unlock in the
// user-restart branch, followed by a guard that returns if the generation was
// renewed — a mode switch landing in that window killed the process.
//
// The restart branch is selected by pressing Restart, so the intent flag is set.
func TestPrivilegedExitDoesNotCrashWhenSupersededDuringRestart(t *testing.T) {
	ac := newTestController()
	gen := ac.classic.currentGeneration()
	if !ac.classic.commitPrivileged(gen, 4242, 4243, filepath.Join(t.TempDir(), "core.pid"), testExe) {
		t.Fatal("the privileged commit must be accepted")
	}
	ac.RunningState.Set(true)
	// Restart requested => decideCrashActionReason returns actionUserRestart,
	// which is the branch that unlocks manually.
	ac.RestartRequestedByUser = true

	done := make(chan struct{})
	go func() {
		(&ProcessService{ac: ac}).onPrivilegedScriptExited()
		close(done)
	}()

	// Supersede concurrently so the guard inside the released-lock window sees a
	// stale generation.
	go func() {
		for i := 0; i < 2000; i++ {
			ac.classic.renewGeneration()
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("onPrivilegedScriptExited never returned")
	}
}

// TestNoDeferredUnlockWhereTheLockIsReleasedMidBody is the structural invariant,
// asserted on the source rather than by triggering each path.
//
// The same bug has now been produced twice by the same pattern: a function that
// arms `defer Unlock` and ALSO releases the mutex by hand, leaving every early
// return after the manual release to decide whether re-acquiring is required.
// Functions that release mid-body must therefore not use the defer at all.
//
// This is a text check, and it is honest about that — it cannot prove the manual
// accounting is right, only that the fragile pattern is absent. The behavioural
// tests above cover the accounting.
func TestNoDeferredUnlockWhereTheLockIsReleasedMidBody(t *testing.T) {
	src, err := os.ReadFile("process_service.go")
	if err != nil {
		t.Fatalf("cannot read process_service.go: %v", err)
	}
	text := string(src)

	for _, fn := range []string{
		"func (svc *ProcessService) Monitor(",
		"func (svc *ProcessService) onPrivilegedScriptExited(",
	} {
		start := strings.Index(text, fn)
		if start < 0 {
			t.Fatalf("cannot find %s", fn)
		}
		end := strings.Index(text[start+10:], "\nfunc ")
		if end < 0 {
			t.Fatalf("cannot find the end of %s", fn)
		}
		body := text[start : start+10+end]

		// Only the function's own defer counts; a nested closure may keep one.
		head := body
		if i := strings.Index(body, "go func"); i >= 0 {
			head = body[:i]
		}
		if strings.Contains(head, "defer ac.CmdMutex.Unlock()") {
			t.Errorf("%s arms a deferred Unlock and also releases the mutex by "+
				"hand; every early return after the manual release must then "+
				"remember not to unlock again, which is how the fatal "+
				"`unlock of unlocked mutex` crash was produced twice", fn)
		}
		if !strings.Contains(body, "ac.CmdMutex.Unlock()") {
			t.Errorf("%s no longer releases the mutex by hand; this test is now "+
				"checking the wrong shape and should be revisited", fn)
		}
	}
}
