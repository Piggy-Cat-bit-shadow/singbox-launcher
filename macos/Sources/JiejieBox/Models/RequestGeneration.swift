// RequestGeneration — which reply is allowed to change the screen.
//
// THE DEFECT CLASS. Every async read on the proxy screen could be overtaken:
// the user switches group A -> B -> C, three requests are in flight, and whichever
// response happens to arrive LAST writes the screen. That is not the group the
// user last chose; it is whichever round trip was slowest. The same omission let a
// finished group test drag the user back to the group they had tested, and let a
// stale node list overwrite a newer one.
//
// The rule that fixes it is a generation: a request is stamped when it is sent,
// and only the newest stamp may commit. It lives in its own SwiftUI-free file so
// the Go suite EXECUTES it — this is a decision about what the user sees, and a
// rule that silently renders the wrong group is exactly the kind that must not
// rest on a source-code grep.

import Foundation

/// A monotonic stamp for async requests whose replies may arrive out of order.
struct RequestGeneration {
    private var current: UInt64 = 0

    /// Stamp a new request. Every later request supersedes this one.
    mutating func begin() -> UInt64 {
        current &+= 1
        return current
    }

    /// Invalidate everything in flight without issuing a new request.
    ///
    /// Used when the world changes underneath the requests — a new core
    /// generation, a rebuilt config, a changed engine — because a reply from the
    /// previous world describes something that no longer exists.
    mutating func invalidate() {
        current &+= 1
    }

    /// Whether a reply stamped `generation` is still the current one.
    func isCurrent(_ generation: UInt64) -> Bool { generation == current }

    /// The newest stamp, for a reply that must tag itself.
    var value: UInt64 { current }
}

/// What a proxy-list reply is allowed to change.
///
/// Modelled explicitly rather than as a bare Bool because "may I commit?" has two
/// independent reasons to be false, and the previous code conflated them:
///
///   * the reply is OLDER than a request already sent, so a newer one will
///     describe the screen instead;
///   * the reply describes a DIFFERENT GROUP from the one being displayed, so
///     committing it would move the user somewhere they did not navigate to.
///
/// Both must block, and separating them makes each one testable.
enum ProxyListCommitDecision: Equatable {
    /// Apply the reply: it is current and describes the displayed group.
    case commit
    /// Drop it silently. A newer request is outstanding and will paint the
    /// screen; flashing this one first would show the wrong list momentarily.
    case superseded
    /// Drop the LIST but keep the reply's metadata. A reply for another group
    /// still carries a valid capability answer and group list, which are not
    /// group-specific.
    case otherGroup
}

/// Decide whether a proxy-list reply may take over the visible list.
///
/// - Parameters:
///   - replyGeneration: the stamp the reply carries.
///   - currentGeneration: the newest stamp issued.
///   - replyGroup: the group the reply describes.
///   - displayedGroup: the group the user is looking at (what was requested).
func proxyListCommitDecision(replyGeneration: UInt64,
                             currentGeneration: UInt64,
                             replyGroup: String,
                             displayGroup: String) -> ProxyListCommitDecision {
    // Supersession is checked FIRST: a reply that is no longer current has no
    // authority over the screen at all, whatever group it names.
    guard replyGeneration == currentGeneration else { return .superseded }
    guard replyGroup == displayGroup else { return .otherGroup }
    return .commit
}

/// The Test All run's lifecycle, including the window before the backend answers.
///
/// THE WINDOW THIS MODELS. The button was guarded by "is a run in progress",
/// which only becomes true when the backend's `started` frame arrives. Between
/// the click and that frame the button still looked idle, so a fast second click
/// started a SECOND run. Waiting for the backend to confirm what we just asked
/// for is the wrong direction: the click is itself the evidence.
enum GroupTestState: Equatable {
    case idle
    /// The run has been requested; the backend has not reported it yet.
    case launching(group: String)
    /// The backend has named the run, and progress is streaming.
    case running(GroupTestProgress)

    /// True from the CLICK, not from the first backend frame. This is what every
    /// control must consult.
    var isRunning: Bool {
        if case .idle = self { return false }
        return true
    }

    /// Live progress, once the backend has named the run.
    ///
    /// Nil while `.launching`: the total is not known until the backend reports
    /// it, and inventing one would make the count meaningless.
    var progress: GroupTestProgress? {
        if case .running(let p) = self { return p }
        return nil
    }

    /// The group this run belongs to, in any non-idle state.
    var group: String? {
        switch self {
        case .idle: return nil
        case .launching(let g): return g
        case .running(let p): return p.group
        }
    }
}

/// Whether a `started` frame may be adopted as the current run.
///
/// A frame for a run WE did not launch is not ours to display: adopting it would
/// resurrect a test the user is not running, or reset the counters of the run
/// they are watching.
func acceptsGroupTestStart(_ state: GroupTestState,
                           startedGroup: String,
                           startedRunID: UInt64) -> Bool {
    switch state {
    case .idle:
        // No run of ours is outstanding, so this describes someone else's.
        return false
    case .launching(let group):
        return group == startedGroup
    case .running(let progress):
        // We are already tracking a run for a group. Only a frame for THAT group
        // may replace it, and only when it names a DIFFERENT run: a duplicate of
        // the run we are watching would reset the counters the user is reading,
        // and a frame for another group is not the run this screen is showing.
        return progress.group == startedGroup && progress.id != startedRunID
    }
}

/// Progress of one running group test.
///
/// Declared here rather than on the view model so `GroupTestState` can carry it
/// without SwiftUI, which is what lets the whole run lifecycle be executed under
/// test.
struct GroupTestProgress: Equatable {
    let id: UInt64
    let group: String
    let total: Int
    var completed: Int
    var succeeded: Int
    var failed: Int
    /// Nodes whose measurement is in flight, so only those show a spinner.
    var inFlight: Set<String>
}

// MARK: - Coalescing refreshes

/// A refresh that coalesces concurrent callers and ignores superseded replies.
///
/// WHY THIS SHAPE. The daemon status read is triggered by a button, by every
/// daemon event, and by the initial load. Two behaviours matter, and they pull in
/// opposite directions:
///
///   * REPEATED CLICKS MUST COST ONE REQUEST. A user who clicks Refresh three
///     times should not send three reads whose replies can arrive in any order —
///     and each of which would repaint the screen. A caller that arrives while a
///     read is in flight JOINS it.
///
///   * A FAILED REFRESH MUST NOT DESTROY WHAT IS ON SCREEN. The status is the
///     context for the command being displayed; clearing it because one read
///     failed would erase the very thing the user is looking at. So a failure
///     reports an error and changes nothing else.
///
/// Modelled as a value type so the Go suite can execute the ordering rules with
/// explicit "what if the reply arrives now" steps, which is impossible to express
/// reliably against a live backend.
struct CoalescingRefresh {
    private var generation = RequestGeneration()
    private(set) var inFlight = false

    /// Number of callers that joined the in-flight read instead of starting one.
    private(set) var joinedCount = 0

    /// A caller arrives. Returns the generation to use when STARTING a read, or
    /// nil when it should join one already in flight.
    mutating func arrive() -> UInt64? {
        if inFlight {
            joinedCount += 1
            return nil
        }
        inFlight = true
        return generation.begin()
    }

    /// Whether a reply stamped `stamp` may commit.
    func mayCommit(_ stamp: UInt64) -> Bool { generation.isCurrent(stamp) }

    /// Finish a read, whatever its outcome. A failure still ends the read, or the
    /// next click would join a read that has already finished and never return.
    mutating func finish(_ stamp: UInt64) {
        guard generation.isCurrent(stamp) else { return }
        inFlight = false
    }

    /// Discard everything in flight, e.g. because the session changed and any
    /// reply now describes a different backend.
    mutating func invalidate() {
        generation.invalidate()
        inFlight = false
    }
}
