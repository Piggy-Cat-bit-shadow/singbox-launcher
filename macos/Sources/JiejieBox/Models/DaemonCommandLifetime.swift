// DaemonCommandLifetime — when a prepared Terminal command stops being valid.
//
// WHY THIS IS ITS OWN FILE. The rule decides whether the user's command is still
// on screen, and getting it wrong is invisible in the worst way: the command
// simply disappears and the button they pressed appears to have done nothing.
// That is a decision worth testing, and the toolchain here cannot build a Swift
// test target at all. Keeping this file free of SwiftUI is what lets
// internal/swiftlogic compile and EXECUTE it under the Go suite, against the same
// source the app ships.
//
// THE DEFECT IT ENCODES. A command was cleared whenever the daemon reported
// `ready`. But Re-pair and Remove Service generate their command precisely WHILE
// the daemon is healthy — that is the situation they exist for — and generating
// a command changes nothing about the service. So the freshly created command
// was discarded immediately, every time, and those two buttons never appeared to
// work.

import Foundation

/// The parts of the daemon status that decide whether a command still applies.
///
/// A small, named set rather than the whole DTO: comparing everything would let
/// an unrelated field (a version string, a timestamp) invalidate a command the
/// user is about to run, which is the same defect wearing a different hat.
struct DaemonStateFingerprint: Equatable {
    let installed: Bool
    let ready: Bool
    let activeMode: Bool
    let paired: Bool

    init(installed: Bool, ready: Bool, activeMode: Bool, paired: Bool) {
        self.installed = installed
        self.ready = ready
        self.activeMode = activeMode
        self.paired = paired
    }

    /// Read the fingerprint off a status.
    ///
    /// `DaemonStatus` is a protocol DTO with no SwiftUI dependency, so this stays
    /// in the pure file and the whole rule remains executable by the harness.
    init(_ status: DaemonStatus) {
        self.init(installed: status.installed,
                  ready: status.ready,
                  activeMode: status.active_mode,
                  paired: status.paired)
    }
}

/// A command prepared for the user to run, and the state it was prepared for.
///
/// Carries the OPERATION rather than the whole `DaemonCommandResult`: the
/// lifetime rule reads only that, and depending on the full protocol DTO would
/// drag a decoded backend structure into a decision that is purely about state.
struct PreparedDaemonCommand {
    let operation: String
    let preparedFor: DaemonStateFingerprint
    let id: UInt64
}

/// The operation a prepared daemon command performs, as the backend names it.
enum DaemonOperation {
    static let install = "install"
    static let start = "start"
    static let freshInvite = "fresh_invite"
    static let uninstall = "uninstall"
}

/// Decide whether a prepared command survives a newly observed daemon status.
///
/// Returns true when the command is STILL VALID and should stay on screen.
///
/// The two rules, and why they differ:
///
///   * A FRESH INVITE stays valid across a state change as long as the service is
///     installed and not yet paired. That is the window in which the user is
///     meant to go and run it, and the state can move — a status re-read, an
///     unrelated event — without the invite becoming useless. It is retired when
///     it is actually redeemed (paired becomes true) or the service goes away.
///
///   * EVERY OTHER COMMAND is tied to the exact situation that produced it. An
///     install command for a service that is now installed is pointless, and a
///     start command for one already running likewise.
///
/// A command with no recorded fingerprint cannot be judged, so it is retired:
/// keeping a command whose precondition is unknown is how a user ends up running
/// something already done.
func daemonCommandSurvives(_ prepared: PreparedDaemonCommand?,
                           newStatus: DaemonStateFingerprint) -> Bool {
    guard let prepared else { return false }
    if prepared.preparedFor == newStatus { return true }

    // The state moved. An invite is still usable while the service is installed
    // and unpaired: that is the window the user is acting in.
    if prepared.operation == DaemonOperation.freshInvite {
        return newStatus.installed && !newStatus.paired
    }
    return false
}

// MARK: - Core operation reconciliation

/// What to do when a lifecycle operation's deadline expires without a confirming
/// event.
///
/// THE DEFECT THIS MODELS. Core operations wait for a backend event that says the
/// work finished. If that event is lost — a dropped frame, a reconnected stream —
/// the UI stays busy forever and every control is disabled, which is the worst
/// possible failure for a VPN client: the user cannot start, stop, or retry.
///
/// The deadline exists for exactly that case, but what it may conclude is narrow.
/// The snapshot is authoritative ONLY if it reports the operation's own goal
/// reached; anything else leaves the marker in place. Releasing it there is what
/// allowed a second lifecycle command to race the first — the UI declared the
/// operation over while the backend was still working.
enum CoreReconciliation: Equatable {
    /// The snapshot proves the operation succeeded; release the marker silently,
    /// because the user's action DID work and there is nothing to report.
    case confirmedFinished
    /// The snapshot shows the operation is still running: keep the marker and say
    /// so, rather than pretending it ended.
    case stillRunning
    /// The backend could not be reached, so the outcome cannot be confirmed: keep
    /// the marker and report that, because releasing it would claim an operation
    /// ended when we cannot see the backend at all.
    case unconfirmed
}

/// Decide what a timed-out lifecycle operation may conclude.
///
/// - Parameters:
///   - snapshotState: the state the backend reported, or nil if the read failed.
///   - kind: the goal the operation was trying to reach.
func reconcileCoreOperation(snapshotState: CoreState?, kind: CoreGoal) -> CoreReconciliation {
    guard let snapshotState else { return .unconfirmed }
    return snapshotState.isTerminal(for: kind) ? .confirmedFinished : .stillRunning
}

/// Whether a snapshot may conclude a core operation, given the sequence numbers.
///
/// THE FRESHNESS RULE. A snapshot older than the command that started the
/// operation describes the world BEFORE the user acted, so it cannot be evidence
/// about the outcome. The reply must be strictly newer than the command's own
/// observation.
///
/// Strictly: a snapshot at exactly the command's sequence reports the state the
/// operation started from, which for a stop is still `running` and for a start is
/// still `stopped`. Accepting it would settle the operation on the very state it
/// was issued to change.
func snapshotCanSettleOperation(snapshotSeq: UInt64, commandSeq: UInt64) -> Bool {
    snapshotSeq > commandSeq
}
