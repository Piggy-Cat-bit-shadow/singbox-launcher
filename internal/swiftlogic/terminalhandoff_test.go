package swiftlogic_test

import "testing"

// TestTerminalSuccessWaitsForOSAScriptExit covers the success report.
//
// Handing a command to Terminal returning means the request was ACCEPTED, not that
// Terminal opened. Reporting success at that moment told the user their command was
// running when nothing had happened — the worst outcome for a step they are about
// to wait on, because they sit and wait for a command that was never typed.
//
// The honest signal is the helper's exit status, which is why the report waits for
// it. This pins both directions: zero means opened, and ANY non-zero means the
// handoff failed and the user must be told.
func TestTerminalSuccessWaitsForOSAScriptExit(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// A clean exit is the only success.
check("a zero exit reports the Terminal opened",
      terminalHandoffOutcome(exitStatus: 0) == .opened)

// Non-zero means the handoff FAILED and the user must be told, or they wait for
// nothing. Every failure code must be reported as a failure, not just 1.
var allReported = true
for code: Int32 in [1, 2, 3, 64, 127, 255, -1, -2] {
    if terminalHandoffOutcome(exitStatus: code) != .failed { allReported = false }
}
check("every non-zero exit reports a failure", allReported)

// The two outcomes must stay distinguishable: collapsing them would restore the
// defect where a failed handoff was shown as success.
check("opened and failed are different outcomes",
      TerminalHandoffOutcome.opened != TerminalHandoffOutcome.failed)
`)
}
