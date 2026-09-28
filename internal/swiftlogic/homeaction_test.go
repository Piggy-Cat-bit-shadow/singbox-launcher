package swiftlogic_test

import "testing"

// TestNonRecoverableCoreErrorDoesNotOfferGenericRetry covers the deterministic
// failure.
//
// CoreStatus.recoverable exists to say "retrying this will not help" — an
// occupied port, a missing binary, an unusable bind address. The button ignored it
// and always read "Retry", so a user clicked Start in a loop against a failure that
// could never succeed, with no path to the information explaining it. Offering a
// control whose action cannot work is worse than offering none.
func TestNonRecoverableCoreErrorDoesNotOfferGenericRetry(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// THE DEFECT. The backend says the failure is deterministic.
check("UI-24 a non-recoverable error does not offer a retry",
      homePrimaryAction(state: .error, pendingStart: false, pendingStop: false,
                        recoverable: false) == .reviewDetails)
check("UI-24 and the rule agrees when asked directly",
      !coreErrorOffersRetry(recoverable: false))

// A RECOVERABLE failure still offers a retry: the core may well start on the next
// attempt, and removing the control would strand the user.
check("a recoverable error offers a retry",
      homePrimaryAction(state: .error, pendingStart: false, pendingStop: false,
                        recoverable: true) == .retry)
check("a recoverable error is retryable",
      coreErrorOffersRetry(recoverable: true))

// AN OLDER BACKEND CANNOT ANSWER. nil means the field is absent, and guessing
// "unrecoverable" would remove the only way forward from a core that might start.
check("UI-24 an unknown recoverability stays retryable",
      homePrimaryAction(state: .error, pendingStart: false, pendingStop: false,
                        recoverable: nil) == .retry)
check("an absent recoverability is treated as retryable",
      coreErrorOffersRetry(recoverable: nil))

// The review path is NOT a command: it navigates, and a control that navigated
// while claiming to issue a command would leave the core untouched while the user
// believes they acted on it.
check("reviewDetails is not a command", !HomePrimaryAction.reviewDetails.isCommand)
check("retry IS a command", HomePrimaryAction.retry.isCommand)
check("start is a command", HomePrimaryAction.start.isCommand)
check("stop is a command", HomePrimaryAction.stop.isCommand)
// A transitional state is a report, not a command.
check("starting is not a command", !HomePrimaryAction.starting.isCommand)
check("stopping is not a command", !HomePrimaryAction.stopping.isCommand)

// The exhaustiveness that makes the rule safe to rely on: for every state and
// every recoverability, the answer is one of the six cases and an ERROR never
// yields a transition label.
var wrong = 0
for rec in [Bool?.none, .some(true), .some(false)] {
    for s in [CoreState?.none, .some(.stopped), .some(.starting), .some(.running),
              .some(.stopping), .some(.error)] {
        let action = homePrimaryAction(state: s, pendingStart: false,
                                       pendingStop: false, recoverable: rec)
        // An errored core must never be offered a start directly: it goes through
        // retry or review, so the failure is acknowledged.
        if s == .some(.error) && action == .start { wrong += 1 }
        // A stopped core must never be offered a stop.
        if s == .some(.stopped) && action == .stop { wrong += 1 }
        // A running core must never be offered a start.
        if s == .some(.running) && action == .start { wrong += 1 }
    }
}
check("UI-24 no state yields a contradictory action", wrong == 0)
`)
}

// TestPendingOutranksReportedState covers the acknowledgment window.
//
// start_core returns before the core reaches starting, so a button driven by
// state alone still read "Start" after being clicked — inviting a second click on
// the control the user had just used. The user's own click is the freshest evidence
// about what is happening.
func TestPendingOutranksReportedState(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// The backend has not caught up: it still reports stopped, but the user just
// pressed Start.
check("a pending start reads as starting even while stopped",
      homePrimaryAction(state: .stopped, pendingStart: true, pendingStop: false,
                        recoverable: nil) == .starting)
// Same for a stop, while the backend still reports running.
check("a pending stop reads as stopping even while running",
      homePrimaryAction(state: .running, pendingStart: false, pendingStop: true,
                        recoverable: nil) == .stopping)

// Pending also outranks an unknown state: the click is evidence even when the
// last snapshot is missing.
check("a pending start reads as starting with no state at all",
      homePrimaryAction(state: nil, pendingStart: true, pendingStop: false,
                        recoverable: nil) == .starting)

// With nothing pending, the reported state decides.
check("a settled stop reads as start",
      homePrimaryAction(state: .stopped, pendingStart: false, pendingStop: false,
                        recoverable: nil) == .start)
check("a running core reads as stop",
      homePrimaryAction(state: .running, pendingStart: false, pendingStop: false,
                        recoverable: nil) == .stop)
// No state and no pending command: the honest default is Start, which is what a
// first-launch user needs.
check("an unread state offers start",
      homePrimaryAction(state: nil, pendingStart: false, pendingStop: false,
                        recoverable: nil) == .start)
`)
}
