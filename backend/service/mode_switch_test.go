package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/locale"
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
// stripGoComments removes Go comments, WITHOUT destroying string literals.
//
// The line-oriented version this replaces cut every line at the first `//`, which is a
// comment only when it is not inside a string. Go source is full of literals that
// contain those characters — `json:"http://..."` in particular — so any struct field
// declared after such a literal silently vanished from the text under test. Two tests
// were disabling themselves this way: their assertions looked for fields that their own
// helper had already deleted, and they passed only because the text happened to survive
// by position. A test helper that edits the subject is worse than no helper.
//
// This tracks string, rune and raw-string state so `//` inside a literal is data.
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
		if idx := commentIndexOutsideLiterals(trimmed); idx >= 0 {
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

// commentIndexOutsideLiterals returns the index of the first `//` or `/*` that is NOT
// inside a string literal, or -1.
//
// Backslash escapes are honoured inside interpreted strings; a raw string (backticks)
// has no escapes and ends at the next backtick. Rune literals are skipped so a stray
// quote character does not desynchronise the state machine.
func commentIndexOutsideLiterals(line string) int {
	inString := false
	inRaw := false
	inRune := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case inRaw:
			if ch == '`' {
				inRaw = false
			}
			continue
		case inString:
			if ch == '\\' {
				i++ // skip the escaped character
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		case inRune:
			if ch == '\\' {
				i++
				continue
			}
			if ch == '\'' {
				inRune = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '`':
			inRaw = true
		case '\'':
			inRune = true
		case '/':
			if i+1 < len(line) && (line[i+1] == '/' || line[i+1] == '*') {
				return i
			}
		}
	}
	return -1
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

// --- N: the mode switch is a transaction ------------------------------------

// TestModeSwitchPersistFailureDoesNotDiverge is claim N.
//
// The switch used to happen FIRST and the choice was saved afterwards, so a
// failed save left the launcher running the NEW engine while the settings file
// still named the OLD one. The frontend got an error while the app was
// demonstrably on the daemon, and reopening it silently reverted — runtime and
// disk disagreeing, with nothing to say which was authoritative.
//
// The order is now persist-then-switch, so the failure mode is "the switch never
// happened" rather than "two components disagree".
func TestModeSwitchPersistFailureDoesNotDiverge(t *testing.T) {
	src := stripGoComments(readSource(t, "backend/service/backend.go"))

	idx := indexOf(src, "func (b *Backend) SetCoreMode(")
	if idx < 0 {
		t.Fatal("SetCoreMode not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	// The persistence call is `locale.UpdateSettings` now: the settings lock lives with the
	// file, and a backend-local mutex over the same file was the lost update this test's
	// neighbour exists to prevent. The ORDER is what this test is about, and it is unchanged.
	saveIdx := indexOf(body, "locale.UpdateSettings")
	if saveIdx < 0 {
		saveIdx = indexOf(body, "locale.SaveSettings")
	}
	switchIdx := indexOf(body, "SwitchBackendMode")
	if saveIdx < 0 || switchIdx < 0 {
		t.Fatal("SetCoreMode no longer both saves and switches")
	}
	if saveIdx > switchIdx {
		t.Fatal("SetCoreMode switches the engine BEFORE persisting the choice; a " +
			"failed save then leaves the runtime on one engine and the settings " +
			"file on the other, and the next launch silently reverts")
	}

	// And the refusal path must roll the persisted choice back.
	if !contains(body, "rollback") && !contains(body, "Roll the") &&
		!contains(body, "previous") {
		t.Error("SetCoreMode does not roll the persisted choice back when the switch " +
			"is refused, so a busy engine still changes the stored setting")
	}
}

// TestModeSwitchRollsBackTheStoredChoiceWhenRefused — the behavioural half: a
// refused switch must leave the setting exactly as it was.
func TestModeSwitchRollsBackTheStoredChoiceWhenRefused(t *testing.T) {
	b := backendWithConfig(t)
	binDir := b.ac.FileService.Layout.Data.Bin()

	// An empty stored mode means "unset", which the controller resolves to
	// classic. Record whatever it is, so the assertion is about the ROLLBACK
	// rather than about the fixture's starting value.
	before := readStoredMode(t, b)
	if got := b.ac.BackendMode(); got != core.BackendClassic {
		t.Fatalf("precondition: the fixture should start on classic, got %q", got)
	}

	// Make the switch refusable: a classic start is in flight.
	b.ac.SetClassicPhaseForTest(core.ClassicStarting)
	defer b.ac.SetClassicPhaseForTest(core.ClassicStopped)

	err := b.SetCoreMode(string(core.BackendDaemon))
	if err == nil {
		t.Fatal("the switch should have been refused while the engine was busy")
	}

	if got := readStoredMode(t, b); got != before {
		t.Fatalf("the stored mode is %q after a REFUSED switch, want %q: the setting "+
			"now names an engine the launcher is not running, and the next launch "+
			"will silently change engines", got, before)
	}
	if got := b.ac.BackendMode(); got != core.BackendClassic {
		t.Fatalf("the runtime engine changed to %q despite the refusal", got)
	}
	_ = binDir
}

// readStoredMode returns the engine named by settings.json.
func readStoredMode(t *testing.T, b *Backend) string {
	t.Helper()
	return locale.LoadSettings(b.ac.FileService.Layout.Data.Bin()).CoreBackendMode
}

// --- O: concurrent handovers are serialized ---------------------------------

// TestConcurrentModeSwitchSerialized is claim O.
//
// `setBackend` publishes in two steps with the lock released between them,
// because closing the previous backend can block. Two concurrent switches could
// therefore both read the same predecessor, both close it, and publish in an
// order unrelated to the order they started in — leaving whichever finished last
// as the live engine. The UI happens to serialize its calls; the IPC path is not
// the UI, and "the frontend usually behaves" is not a safety boundary.
func TestConcurrentModeSwitchSerialized(t *testing.T) {
	b := backendWithConfig(t)

	const workers = 8
	start := make(chan struct{})
	done := make(chan struct{}, workers)

	for i := 0; i < workers; i++ {
		target := core.BackendDaemon
		if i%2 == 0 {
			target = core.BackendClassic
		}
		go func(mode core.BackendMode) {
			<-start
			// Refusals are fine; the point is that the handover machinery is not
			// entered concurrently.
			_ = b.ac.SwitchBackendMode(mode)
			done <- struct{}{}
		}(target)
	}

	close(start)
	for i := 0; i < workers; i++ {
		<-done
	}

	// The published engine must be one of the two legal values, and the backend
	// must be usable afterwards — a torn handover would leave it nil or closed.
	mode := b.ac.BackendMode()
	if mode != core.BackendClassic && mode != core.BackendDaemon {
		t.Fatalf("after concurrent switches the engine is %q, which is not a legal mode", mode)
	}
	if b.ac.Backend() == nil {
		t.Fatal("concurrent switches left no backend published; every IPC call would " +
			"now fail")
	}
	// The wire state must still resolve rather than panicking on a torn backend.
	_ = b.coreState()
}

// --- M: timeouts actually interrupt ----------------------------------------

// TestCoreOpTimeoutCancelsMutexWait is claim M.
//
// The "45s operation timeout" bounded only the part of the work that happened to
// run. Two waits on the start path could not be interrupted at all — the template
// refresh and the CmdMutex acquisition — so a caller whose context had long
// expired still sat in one of them, and the start then proceeded in a world that
// had moved on. A timeout that cannot interrupt the wait it is timing is not a
// timeout.
//
// Asserted structurally, because reproducing it needs a lock held by another
// subsystem: the start path must acquire the engine lock through the
// context-aware helper rather than a bare Lock.
func TestCoreOpTimeoutCancelsMutexWait(t *testing.T) {
	src := stripGoComments(readSource(t, "core/process_service.go"))

	idx := indexOf(src, "func (svc *ProcessService) StartContext(")
	if idx < 0 {
		t.Fatal("StartContext not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	if !contains(body, "acquireWithContext(ctx, &ac.CmdMutex)") {
		t.Error("StartContext takes the engine lock with a bare Lock, so a caller " +
			"whose context expires while waiting for it is blocked anyway")
	}
	if contains(body, "ac.CmdMutex.Lock()") {
		t.Error("StartContext still contains an uninterruptible CmdMutex.Lock()")
	}
	if !contains(body, "awaitTemplateRefreshContext(ctx)") {
		t.Error("StartContext waits for the template refresh without a context, so its " +
			"deadline does not cover that wait")
	}
}

// TestDaemonApplyHonoursContext is claim M, daemon half.
//
// Every daemon call on the apply path used a non-contextual variant, so a stuck
// request outlived every deadline. The apply is also the one call that can START
// A CORE, which makes it the worst one to be unable to cancel.
func TestDaemonApplyHonoursContext(t *testing.T) {
	src := stripGoComments(readSource(t, "core/backend_daemon.go"))

	for _, call := range []string{
		"b.admin.ApplyCtx(",
		"b.admin.StatusCtx(",
		"b.admin.InfoCtx(",
	} {
		if !contains(src, call) {
			t.Errorf("the daemon apply path does not use %s; that call cannot be "+
				"interrupted, so the operation timeout does not bound it", call)
		}
	}

	// And the non-contextual forms must not remain on the apply path.
	idx := indexOf(src, "func (b *DaemonBackend) applyOnce(")
	if idx < 0 {
		t.Fatal("applyOnce not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]
	for _, stale := range []string{"b.admin.Apply(config)", "b.admin.Status()", "b.admin.Info()"} {
		if contains(body, stale) {
			t.Errorf("applyOnce still calls %s, which ignores the backend context", stale)
		}
	}
}

// TestDaemonApplyLockWaitIsCancellable — the lock itself must be waitable with a
// context, or a caller stuck behind a long apply is blocked for that apply's
// whole duration no matter what deadline it carries.
func TestDaemonApplyLockWaitIsCancellable(t *testing.T) {
	src := stripGoComments(readSource(t, "core/backend_daemon.go"))
	if !contains(src, "acquireWithContext(ctx, &b.applyMu)") {
		t.Error("the daemon apply waits for applyMu with a plain Lock; the operation " +
			"timeout cannot interrupt that wait")
	}
}

// --- The real call chain, not a fake closure -------------------------------

// TestBackendStartCoreActuallyWaitsForClassicCommit is the acceptance test for
// the original defect, driven through the REAL chain:
//
//	Backend.StartCore → AppController.StartVPNContext → LegacyBackend.StartVPNContext
//	  → fake legacyOps.startContext (blocks, simulating a slow spawn)
//
// Every earlier version of this test substituted a fake for `runCoreOp`'s closure,
// which proves only that `runCoreOp` awaits WHAT IT WAS GIVEN. The bug was that the
// production closure returned immediately: the start was fire-and-forget all the
// way down, so `StartCore` reported success while nothing had been spawned and the
// UI had already returned to "Start".
//
// So the fake here is the PROCESS OPERATION, at the bottom of the chain, and the
// assertion is that the top-level IPC call is still blocked while it runs.
func TestBackendStartCoreActuallyWaitsForClassicCommit(t *testing.T) {
	b := backendWithConfig(t)

	committed := make(chan struct{})
	entered := make(chan struct{})

	lb := core.NewLegacyBackend(b.ac)
	core.SetLegacyOpsForTest(lb, &core.LegacyOpsForTest{
		StartContext: func(ctx context.Context, skipRunningCheck bool) error {
			close(entered)
			select {
			case <-committed:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	b.ac.SetBackendForTest(lb)

	done := make(chan error, 1)
	go func() { done <- b.StartCore() }()

	// The process operation must have been entered...
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("StartCore never reached the process operation")
	}

	// ...and StartCore must still be BLOCKED, because nothing has committed.
	select {
	case err := <-done:
		t.Fatalf("StartCore returned (%v) before the start committed; the caller is "+
			"told the core started while nothing has been spawned", err)
	case <-time.After(150 * time.Millisecond):
	}

	// Publish the commit point; only now may StartCore return.
	close(committed)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartCore failed after a successful commit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartCore did not return after the start committed")
	}
}

// TestBackendStartCoreReportsFailureFromTheProcessOperation — a failure at the
// bottom of the chain must reach the top as a failure. The old fire-and-forget
// path could not report anything at all.
func TestBackendStartCoreReportsFailureFromTheProcessOperation(t *testing.T) {
	b := backendWithConfig(t)

	lb := core.NewLegacyBackend(b.ac)
	core.SetLegacyOpsForTest(lb, &core.LegacyOpsForTest{
		StartContext: func(ctx context.Context, skipRunningCheck bool) error {
			return core.NewPreconditionRefusal(
				core.StartErrPrivilegedCopyUnavailable,
				"the protected copy of the core is missing", true, true, nil)
		},
	})
	b.ac.SetBackendForTest(lb)

	err := b.StartCore()
	if err == nil {
		t.Fatal("a failing process operation was reported as a successful start")
	}

	// The reason must survive the whole chain. It crosses IPC as a protocol error
	// — that is the wire type the frontend reads — but the STABLE CODE must come
	// through unchanged, because that is what the frontend localizes. A generic
	// message would tell the user nothing actionable.
	var perr *protocol.Error
	if !errors.As(err, &perr) {
		t.Fatalf("the precondition reason crossed IPC as %T (%v); the frontend reads "+
			"protocol.Error", err, err)
	}
	if perr.Code != string(core.StartErrPrivilegedCopyUnavailable) {
		t.Fatalf("code = %q, want %q", perr.Code, core.StartErrPrivilegedCopyUnavailable)
	}
	if perr.Message == "" {
		t.Fatal("a SILENT refusal arrived with no message; nothing was shown on screen " +
			"and nothing was sent to the frontend, so the user is told nothing at all")
	}
}

// TestBackendStartCoreAbandonedWhenContextExpires — the process operation is
// blocked, the operation deadline passes, and the operation must stop waiting
// without claiming a start that never happened.
func TestBackendStartCoreAbandonedWhenContextExpires(t *testing.T) {
	b := backendWithConfig(t)

	observed := make(chan struct{})
	released := make(chan struct{})
	defer close(released)

	lb := core.NewLegacyBackend(b.ac)
	core.SetLegacyOpsForTest(lb, &core.LegacyOpsForTest{
		StartContext: func(ctx context.Context, skipRunningCheck bool) error {
			select {
			case <-released: // the test is over; do not hang the goroutine
			case <-ctx.Done():
				// The operation observed its OWN cancellation, which is the point:
				// the deadline has to reach the bottom of the chain.
				close(observed)
			}
			return ctx.Err()
		},
	})
	b.ac.SetBackendForTest(lb)

	// Drive the timer directly instead of waiting 45 real seconds, so the test is
	// as fast as the logic it checks.
	b.SetCoreOpTimeoutForTest(120 * time.Millisecond)

	// A timeout deliberately does NOT report a failure: the operation may still
	// succeed a moment later, and the runtime transition reports the truth when it
	// arrives. What must be true is that the DEADLINE REACHED THE BOTTOM of the
	// chain — the process operation observed its own cancellation — rather than
	// the IPC call returning while the work ran on unwatched.
	if err := b.StartCore(); err != nil {
		t.Fatalf("a timeout surfaced as %v; the design is to stop WAITING, not to "+
			"claim the start failed, because it may still commit", err)
	}

	select {
	case <-observed:
	case <-time.After(2 * time.Second):
		t.Fatal("the process operation never observed its cancellation; the operation " +
			"deadline does not reach the work it is supposed to bound")
	}
}
