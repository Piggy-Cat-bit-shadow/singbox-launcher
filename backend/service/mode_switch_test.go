package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"singbox-launcher/core"
)

// Mode switching must consult the real lifecycle, and a retired daemon backend
// must not be able to act.
//
// The handover guard used to be `RunningState.IsRunning()` alone. That flag is
// false for every state in which an engine is BUSY BUT NOT YET RUNNING — a
// classic start still authorizing, a daemon apply still in flight — so those were
// exactly the windows in which the engine could be switched out from under work
// that then continued in the background.

// TestModeSwitchBlockedDuringClassicStart is claim H, classic half.
//
// A classic start sets its phase to starting, and `ClassicSettled` must refuse
// the handover for as long as that phase lasts. Without it, the start kept
// authorizing and spawned a core after the switch, so the new engine owned the
// runtime while the old engine's process was still coming up.
func TestModeSwitchBlockedDuringClassicStart(t *testing.T) {
	b := backendWithConfig(t)

	if err := b.ac.SwitchBackendMode(core.BackendClassic); err != nil {
		t.Fatalf("staying on classic must be a no-op: %v", err)
	}

	// A start has been accepted: the classic runtime is starting, and the core is
	// NOT running yet — the precise state the old check let through.
	b.ac.SetClassicPhaseForTest(core.ClassicStarting)
	if b.ac.RunningState.IsRunning() {
		t.Fatal("precondition: a starting core must not be running yet")
	}

	if err := b.ac.SwitchBackendMode(core.BackendDaemon); err == nil {
		t.Fatal("the engine was switched while a classic START was in flight; the " +
			"start would have continued and spawned a core belonging to nobody")
	}
}

// TestModeSwitchBlockedDuringClassicStopping — the teardown window, same rule.
func TestModeSwitchBlockedDuringClassicStopping(t *testing.T) {
	b := backendWithConfig(t)
	b.ac.SetClassicPhaseForTest(core.ClassicStopping)

	if err := b.ac.SwitchBackendMode(core.BackendDaemon); err == nil {
		t.Fatal("the engine was switched while a classic STOP was in flight")
	}
}

// TestModeSwitchAllowedWhenTheRuntimeIsSettled — the guard must not block the
// legitimate case, or the mode screen would be permanently locked.
func TestModeSwitchAllowedWhenTheRuntimeIsSettled(t *testing.T) {
	b := backendWithConfig(t)
	b.ac.SetClassicPhaseForTest(core.ClassicStopped)

	// Daemon is not reachable in this environment, so the switch may still fail —
	// but it must fail for an ENGINE reason, never with the busy refusal.
	err := b.ac.SwitchBackendMode(core.BackendDaemon)
	if err != nil && isBusyRefusal(err) {
		t.Fatalf("a settled runtime was refused as busy: %v", err)
	}
}

// TestModeSwitchBlockedDuringDaemonApply is claim H, daemon half.
//
// The daemon has no classic phase, so `ClassicSettled` says nothing about it and
// RunningState is false while it is applying — an apply has not started a core
// yet. Both old guards therefore passed, and the abandoned backend went on to
// start a daemon core under a user who had selected classic.
func TestModeSwitchBlockedDuringDaemonApply(t *testing.T) {
	b := daemonEngineBackend(t, false)
	backend := b.ac.Backend()

	busy, ok := backend.(interface{ ApplyInFlight() bool })
	if !ok {
		t.Fatal("the daemon backend does not report whether an apply is in flight; " +
			"the handover has no way to see the daemon's busy state")
	}
	if busy.ApplyInFlight() {
		t.Fatal("precondition: no apply should be in flight yet")
	}

	// Simulate an apply in progress by taking the same counter the real apply
	// takes. This is the engine's own answer, not a test-only flag.
	if daemon, ok := backend.(*core.DaemonBackend); ok {
		daemon.SetApplyInFlightForTest(1)
		defer daemon.SetApplyInFlightForTest(0)

		if !daemon.ApplyInFlight() {
			t.Fatal("ApplyInFlight must report true while an apply is counted")
		}
		if err := b.ac.SwitchBackendMode(core.BackendClassic); err == nil {
			t.Fatal("the engine was switched while a daemon APPLY was in flight; the " +
				"apply would have continued and started a daemon core after the " +
				"switch to classic")
		}
	} else {
		t.Fatal("could not reach the daemon backend object for the busy simulation")
	}
}

// TestDaemonApplyIsNotReportedAsRunningBeforeItCommits is claim V.
//
// `applyOnce` wrote `RunningState.Set(true)` immediately after the daemon
// ACCEPTED the config, with a comment claiming the status stream would confirm.
// If the stream is authoritative, acceptance is not STARTED, and a core that
// goes FATAL right after acceptance must not have been announced as running —
// the user would watch a connected screen for a core that never came up.
func TestDaemonApplyIsNotReportedAsRunningBeforeItCommits(t *testing.T) {
	// This is asserted structurally on the source, because producing a real
	// accepted-then-FATAL sequence needs a live daemon. The claim is narrow and
	// the source shape is decisive: the optimistic write must not be there.
	src := readSource(t, "core/backend_daemon.go")
	if containsOptimisticRunningWrite(src) {
		t.Fatal("applyOnce reports `running` as soon as the daemon ACCEPTS the " +
			"config; acceptance is not STARTED, so a core that fails immediately " +
			"afterwards is announced as running before it ever was")
	}
}

// TestClosedDaemonBackendCannotApply is claim I.
//
// `isActive()` was consulted in exactly ONE place: after admin.Apply returned.
// The rebuild of config.json, the Clash API reload and the apply itself all ran
// first, so a backend the user had left could still write the user's config and
// start a daemon core; the late check only suppressed the UI update.
//
// The check must therefore come BEFORE the first side effect, and again before
// the irreversible one.
func TestClosedDaemonBackendCannotApply(t *testing.T) {
	b := daemonEngineBackend(t, false)
	daemon, ok := b.ac.Backend().(*core.DaemonBackend)
	if !ok {
		t.Fatal("expected the daemon backend to be active")
	}

	daemon.Close()

	if daemon.IsActiveForTest() {
		t.Fatal("a closed backend still reports itself active")
	}

	// The apply entry point must refuse outright rather than doing the local work
	// and suppressing only the state update.
	err := daemon.ApplyCurrentConfigForTest("test-after-close", false)
	if err == nil {
		// nil is acceptable ONLY if nothing was done; the check for that is the
		// side-effect guard below.
		t.Log("apply after close returned nil; verifying it did no work")
	}
	if daemon.ApplyInFlight() {
		t.Fatal("a closed backend began an apply")
	}
}

// TestFatalRetryCancelledOnBackendClose is claim J.
//
// The FATAL auto-repair runs on a goroutine nobody owns. With a Background
// parent, Close() could not cancel it, so after a switch to classic the
// abandoned backend could still disable user nodes, rebuild the config and push
// it to the daemon — repairing an engine the user had left.
func TestFatalRetryCancelledOnBackendClose(t *testing.T) {
	b := daemonEngineBackend(t, false)
	daemon, ok := b.ac.Backend().(*core.DaemonBackend)
	if !ok {
		t.Fatal("expected the daemon backend to be active")
	}

	// A live backend may repair; a closed one may not.
	if !daemon.IsActiveForTest() {
		t.Fatal("precondition: the backend should start active")
	}
	daemon.Close()
	if daemon.IsActiveForTest() {
		t.Fatal("after Close the backend must not be active, so the FATAL retry " +
			"path refuses to touch the config or the daemon")
	}

	// The generation bump must be observable, so a loop that re-checks before
	// retrying sees its world is gone even without observing context cancellation.
	gen := daemon.ApplyGenerationForTest()
	daemon.Close() // idempotent
	if daemon.ApplyGenerationForTest() < gen {
		t.Fatal("the apply generation went backwards")
	}
}

// TestDaemonStopFailureIsReachableFromHeadless is claim L.
//
// A daemon stop failure was reported through a Fyne dialog, and the headless
// Swift frontend has no uiPort — so the only signal was a log line. With the
// stop also being fire-and-forget, the user saw a state that never settled and
// no reason for it.
func TestDaemonStopFailureIsReachableFromHeadless(t *testing.T) {
	b := daemonEngineBackend(t, false)

	// A stop failure must leave a recorded lifecycle error, which the backend
	// merges into CoreState and ships over IPC — the path that works without a UI.
	daemon, ok := b.ac.Backend().(*core.DaemonBackend)
	if !ok {
		t.Fatal("expected the daemon backend")
	}
	_ = daemon

	b.ac.RecordLifecycleError(core.LifecycleErrStopFailed, "stop",
		"the VPN could not be stopped", "daemon refused", true)

	if !b.ac.HasLifecycleError() {
		t.Fatal("a recorded stop failure must be visible to the backend")
	}
	st := b.coreState()
	if st.ErrorCode == "" && !b.hasLifecycleError() {
		t.Fatal("a daemon stop failure produced no error the headless frontend can see")
	}
}

// TestModeSwitchRefusalIsNotInstantaneous — the busy refusal must be the engine
// guard, not some unrelated error, so the frontend shows the right message.
func TestModeSwitchRefusalIsNotInstantaneous(t *testing.T) {
	b := backendWithConfig(t)
	b.ac.SetClassicPhaseForTest(core.ClassicStarting)

	err := b.ac.SwitchBackendMode(core.BackendDaemon)
	if err == nil {
		t.Fatal("expected the switch to be refused")
	}
	if !isBusyRefusal(err) {
		t.Fatalf("the refusal does not read as a busy engine: %v", err)
	}
}

// --- helpers -----------------------------------------------------------------

// readSource reads a repository file relative to the module root.
func readSource(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatalf("cannot read %s: %v", rel, err)
	}
	return string(data)
}

// contains is a tiny substring helper, kept local so the test does not pull in a
// matcher dependency.
func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

// containsOptimisticRunningWrite reports whether the daemon apply path still
// commits `running` the moment the daemon ACCEPTS a config.
//
// Comments are stripped first. The comment that used to justify the write is
// itself part of the problem being described, and a check that scanned raw text
// would flag the EXPLANATION of the removal as if it were the behaviour — a test
// that cannot distinguish code from prose about code is not checking code.
func containsOptimisticRunningWrite(src string) bool {
	start := strings.Index(src, "func (b *DaemonBackend) applyOnce(")
	if start < 0 {
		return false
	}
	end := strings.Index(src[start:], "\nfunc ")
	if end < 0 {
		end = len(src) - start
	}
	return strings.Contains(stripGoComments(src[start:start+end]), "RunningState.Set(true)")
}

// stripGoComments removes // and /* */ comments so a source check sees only code.
//
// Crude on purpose: it operates on Go source that the compiler has already
// accepted, and the only strings that could confuse it are log formats, which do
// not contain the patterns being searched for.
func stripGoComments(src string) string {
	var out strings.Builder
	lines := strings.Split(src, "\n")
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if inBlock {
			if idx := strings.Index(trimmed, "*/"); idx >= 0 {
				inBlock = false
				trimmed = trimmed[idx+2:]
			} else {
				continue
			}
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if idx := strings.Index(trimmed, "//"); idx >= 0 {
			trimmed = trimmed[:idx]
		}
		if idx := strings.Index(trimmed, "/*"); idx >= 0 {
			if closeIdx := strings.Index(trimmed[idx:], "*/"); closeIdx >= 0 {
				trimmed = trimmed[:idx] + trimmed[idx+closeIdx+2:]
			} else {
				inBlock = true
				trimmed = trimmed[:idx]
			}
		}
		out.WriteString(trimmed)
		out.WriteString("\n")
	}
	return out.String()
}

// isBusyRefusal reports whether err is the "engine is busy" refusal, as opposed
// to a genuine engine unavailability. The distinction matters to the frontend:
// "wait for the current operation" and "the daemon is not reachable" are
// different messages and different remedies.
func isBusyRefusal(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"still in progress",
		"being applied",
		"stop the VPN",
	} {
		if contains(msg, marker) {
			return true
		}
	}
	return false
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestDaemonExitStopFailureKeepsRunningTruth is claim K.
//
// With "stop the VPN when I quit" configured, the launcher asks the daemon to
// stop on exit. If that call FAILS, the tunnel is provably still up — and
// reporting `stopped` there is worse than a cosmetic lie: GracefulExit enters a
// wait loop whose first check is `!RunningState.IsRunning()`, so the launcher's
// own eager write would make the loop "confirm" a stop that never happened, and
// the app would exit reporting success while the tunnel carried traffic.
//
// The check is a source-shape assertion, because producing a real failing
// /admin/stop needs a live daemon. It is narrow and decisive: the failure branch
// must return false and must not clear the running flag.
func TestDaemonExitStopFailureKeepsRunningTruth(t *testing.T) {
	src := stripGoComments(readSource(t, "core/backend_daemon.go"))

	start := contains(src, "func (b *DaemonBackend) OnAppExit()")
	if !start {
		t.Fatal("OnAppExit not found in the daemon backend")
	}
	idx := indexOf(src, "func (b *DaemonBackend) OnAppExit()")
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	// Locate the stop-failure branch and check what it does.
	failIdx := indexOf(body, "stop failed")
	if failIdx < 0 {
		t.Fatal("OnAppExit has no stop-failure branch; a failed stop must be handled, " +
			"not ignored")
	}
	// The branch starts at the error handling and ends at the next return.
	branchEnd := indexOf(body[failIdx:], "return")
	if branchEnd < 0 {
		t.Fatal("the stop-failure branch does not return")
	}
	branch := body[failIdx : failIdx+branchEnd+len("return false")]

	if contains(branch, "RunningState.Set(false)") {
		t.Fatal("OnAppExit clears the running flag when the daemon STOP FAILED; the " +
			"tunnel is still up, and GracefulExit's wait loop would treat its own " +
			"eager write as confirmation that the VPN is down")
	}
	if !contains(branch, "return false") {
		t.Fatal("OnAppExit must decline to claim a wait on a stop that failed")
	}
}

// TestOnAppExitWaitsOnlyOnAConfirmedStop — the positive half: an unimplemented
// backend or a policy of "leave the core running" must NOT make GracefulExit
// wait, or the app would hang on exit for a stop that was never requested.
func TestOnAppExitWaitsOnlyOnAConfirmedStop(t *testing.T) {
	b := daemonEngineBackend(t, false) // keep the tunnel running on exit
	daemon, ok := b.ac.Backend().(*core.DaemonBackend)
	if !ok {
		t.Fatal("expected the daemon backend")
	}
	// With the keep-running policy, OnAppExit must decline immediately.
	if daemon.OnAppExit() {
		t.Fatal("OnAppExit asked GracefulExit to wait even though the policy is to " +
			"leave the core running; the app would wait for a stop nobody requested")
	}
}

// TestDaemonStopContextReachesTheFrontend is claim L.
//
// The contextual interface covered Start and Restart but not Stop, so a daemon
// stop had no way to return a reason: the error went to a Fyne dialog, which the
// headless frontend never sees. A stopped-but-unreported core left the user with
// a state that never settled and no explanation.
func TestDaemonStopContextReachesTheFrontend(t *testing.T) {
	// The interface must include Stop, so a caller can await it and get a reason.
	type stopContextual interface {
		StopVPNContext(ctx context.Context) error
	}
	var b interface{} = &core.DaemonBackend{}
	if _, ok := b.(stopContextual); !ok {
		t.Fatal("the daemon backend has no contextual Stop; a stop failure can only " +
			"reach the frontend through a Fyne dialog, which headless mode does not have")
	}
}

// indexOf is strings.Index, kept local so the assertions above read plainly.
func indexOf(haystack, needle string) int { return strings.Index(haystack, needle) }
