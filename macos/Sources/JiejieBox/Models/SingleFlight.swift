// SingleFlight — one operation at a time, and what a failure does next.
//
// WHY THIS IS ITS OWN FILE. Two rules live here, both about operations that
// overlap or chain:
//
//   * SINGLE-FLIGHT. A control that starts an operation must not start a second
//     one while the first is outstanding. Double-clicking Restart previously
//     issued two restart commands, and Refresh on the daemon page could have
//     several reads in flight whose replies arrived in any order.
//
//   * CHAINED RECOVERY. "Update everything, then reload, then reload the groups"
//     is a chain in which each step depends on the previous one SUCCEEDING. The
//     chain used to continue past a failure, so a reload ran against a config the
//     update had just failed to refresh — turning one clear failure into a
//     confusing second one, and sometimes into a success that masked the first.
//
// Both are decisions about what the user's click actually does. They live here so
// the Go suite EXECUTES them rather than reading them.

import Foundation

/// A guard that admits one operation at a time.
///
/// Deliberately a value type with explicit begin/end, so the rule is visible at
/// the call site instead of hidden in an `if pending == nil` that a later edit can
/// quietly drop.
struct SingleFlight {
    private(set) var inFlight = false

    /// Whether a new operation may start.
    var isIdle: Bool { !inFlight }

    /// Claim the slot. Returns false when an operation is already running, in
    /// which case the caller MUST do nothing — not queue, not replace.
    ///
    /// Replacing the running operation would leave its completion to clear a
    /// slot it no longer owns, which is how a guard becomes decorative.
    mutating func begin() -> Bool {
        guard !inFlight else { return false }
        inFlight = true
        return true
    }

    /// Release the slot.
    ///
    /// Idempotent: a completion path that runs twice (a response and an event
    /// both arriving) must not be able to release a slot some OTHER operation
    /// has since claimed.
    mutating func end() {
        inFlight = false
    }
}

/// The outcome of one step in a chained operation.
enum ChainStepOutcome: Equatable {
    case succeeded
    /// The step failed and the chain must STOP.
    case failed
    /// The step was skipped by the user or by policy; the chain must STOP, but
    /// this is not an error to report.
    case cancelled
}

/// Whether a chain should continue after a step.
///
/// A chain proceeds only from success. This is a function rather than an inline
/// `if` so that the rule is stated once and can be tested: the defect was a chain
/// that continued after a failure, and continuing is exactly the mistake a hurried
/// edit reintroduces.
func chainShouldContinue(after outcome: ChainStepOutcome) -> Bool {
    outcome == .succeeded
}

/// A step in a chained operation, for the harness and for the model's own
/// bookkeeping.
struct ChainStep: Equatable {
    let name: String
    let outcome: ChainStepOutcome
}

/// Run a list of already-decided steps and report which ones actually ran.
///
/// The model performs the work; this decides the SHAPE of the run — which steps
/// execute given the outcomes of the ones before them. Extracted so the
/// stop-on-failure behaviour is testable without a backend, a network, or a VPN.
func chainExecutionOrder(_ steps: [ChainStep]) -> [String] {
    var ran: [String] = []
    for step in steps {
        ran.append(step.name)
        if !chainShouldContinue(after: step.outcome) { break }
    }
    return ran
}
