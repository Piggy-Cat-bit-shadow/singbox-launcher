package swiftlogic_test

import (
	"runtime"
	"strings"
	"testing"

	"singbox-launcher/internal/swiftlogic"
)

// requireHarness decides whether the Swift harness can run here.
//
// THE DISTINCTION THAT MATTERS. A suite that quietly skips is how an entire layer
// of invariants disappears from CI without anyone noticing — the tests stay green
// while testing nothing. So the skip is allowed ONLY where the platform genuinely
// has no Swift compiler at all (a non-Darwin dev box), and it is forbidden on
// macOS, where `swiftc` ships with the command line tools and the product is
// built. On macOS a missing compiler is a broken environment and must fail.
func requireHarness(t *testing.T) {
	t.Helper()
	if swiftlogic.Available() {
		return
	}
	if runtime.GOOS == "darwin" {
		t.Fatal("swiftc is missing on macOS, so the frontend logic suite cannot run. " +
			"These assertions are the only execution of that code in this repository; " +
			"skipping here would leave them untested while reporting success.")
	}
	t.Skip("no Swift toolchain on " + runtime.GOOS + "; the macOS CI runner has one")
}

func runSwift(t *testing.T, body string) {
	t.Helper()
	res, err := swiftlogic.Run(body)
	if err != nil {
		t.Fatalf("harness build failed: %v", err)
	}
	for _, name := range res.Passed {
		t.Logf("ok: %s", name)
	}
	if len(res.Failed) > 0 {
		t.Errorf("swift assertions failed: %s\nstderr: %s",
			strings.Join(res.Failed, "; "), res.Stderr)
	}
	if len(res.Passed) == 0 {
		t.Fatal("the Swift harness reported no assertions at all")
	}
}

// TestCoreStateTerminalityPerGoal pins the rule that a core operation is
// finished only by a state that is terminal FOR ITS OWN GOAL.
//
// This is the root cause of UI-01. The old predicate was "not transitioning",
// which the state BEFORE the command satisfies — so a stop was considered
// complete while the core was still running, and a restart was considered
// complete at the `stopped` it passes through on the way back up.
func TestCoreStateTerminalityPerGoal(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// A stop is NOT finished while the core is still running: this is the exact
// state the old "is it transitioning?" check mistook for completion.
check("UI-01 running is not terminal for stop", !CoreState.running.isTerminal(for: .stop))
// A start is NOT finished while the core is still stopped.
check("UI-01 stopped is not terminal for start", !CoreState.stopped.isTerminal(for: .start))
// A restart passes THROUGH stopped; treating it as the end cleared the spinner
// halfway through and offered "Start" during the user's own restart.
check("UI-01 stopped is not terminal for restart", !CoreState.stopped.isTerminal(for: .restart))
check("UI-01 running is not terminal for restart is false", CoreState.running.isTerminal(for: .restart))

// The genuine finish lines.
check("stop ends at stopped", CoreState.stopped.isTerminal(for: .stop))
check("start ends at running", CoreState.running.isTerminal(for: .start))
check("restart ends at running", CoreState.running.isTerminal(for: .restart))

// Transitions end nothing, for any goal: the backend publishes them as work
// beginning, so releasing there would show progress as completion.
check("starting ends nothing", !CoreState.starting.isTerminal(for: .start)
    && !CoreState.starting.isTerminal(for: .stop)
    && !CoreState.starting.isTerminal(for: .restart))
check("stopping ends nothing", !CoreState.stopping.isTerminal(for: .start)
    && !CoreState.stopping.isTerminal(for: .stop)
    && !CoreState.stopping.isTerminal(for: .restart))

// A failure ends every goal: a failed operation is over, and the reason comes
// from the error fields rather than from leaving the UI busy forever.
check("error ends start", CoreState.error.isTerminal(for: .start))
check("error ends stop", CoreState.error.isTerminal(for: .stop))
check("error ends restart", CoreState.error.isTerminal(for: .restart))
`)
}
