// NavigationStackModel — the panel's screen stack, as a testable value.
//
// WHY THIS IS SEPARATE FROM AppModel. Navigation decides whether an operation
// that finishes asynchronously may move the user, and getting that wrong is a
// defect the user experiences as being thrown around the interface: press Back
// while a save is in flight and the completion pops a SECOND screen, so one
// action moves them two levels. That rule is worth executing under test, and
// AppModel cannot be (it imports SwiftUI, which this toolchain cannot load
// outside the app target). The stack lives here so the Go suite runs the real
// implementation.

import Foundation

/// A screen in the menu-bar panel.
enum Screen: Hashable {
    case coreDetails
    case coreMode
    case proxies
    case subscriptions
    case addSubscription
    case editSubscription(String)
    case daemon
    case daemonPair
    case more
    case about

    /// Title shown in the panel header.
    var title: String {
        switch self {
        case .coreDetails: return "Core Details"
        case .coreMode: return "Core Mode"
        case .proxies: return "Proxies"
        case .subscriptions: return "Subscriptions"
        case .addSubscription: return "Add Subscription"
        case .editSubscription: return "Subscription"
        case .daemon: return "Daemon"
        case .daemonPair: return "Pair Daemon"
        case .more: return "More"
        case .about: return "About"
        }
    }
}

/// The navigation stack, with operations that name what they expect to leave.
struct NavigationStackModel {
    /// The stack. Settable so callers that render it can bind to it, but every
    /// mutation that decides WHERE the user ends up goes through a method on this
    /// type, so the ownership rules cannot be bypassed by an assignment.
    var path: [Screen] = []

    /// True when there is somewhere to go back to.
    var canGoBack: Bool { !path.isEmpty }

    /// The screen currently on top, or nil at the root.
    var currentScreen: Screen? { path.last }

    /// Push a screen.
    mutating func push(_ screen: Screen) { path.append(screen) }

    /// Go back one screen. Safe to call with an empty path.
    mutating func goBack() {
        guard !path.isEmpty else { return }
        path.removeLast()
    }

    /// Return to the root.
    mutating func goHome() { path.removeAll() }

    /// Leave the given screen, but ONLY if the user is still on it.
    ///
    /// THE DEFECT THIS EXISTS TO PREVENT. An operation that navigates on success
    /// used to call `goBack()` unconditionally when its request returned. If the
    /// user pressed Back while that request was in flight, the two popped
    /// DIFFERENT screens: the user's own Back moved them one level, and the
    /// operation's completion moved them another. One save sent them two screens
    /// away from where they were.
    ///
    /// The operation must therefore name WHERE it expects to be, and give up the
    /// pop if the user has already left. That is this method: the same pop on the
    /// happy path, and a no-op once the user has taken their own exit.
    ///
    /// Matching is by screen IDENTITY rather than by depth, so it stays correct
    /// when an intervening navigation changed the shape of the stack.
    @discardableResult
    mutating func popIfCurrent(_ screen: Screen) -> Bool {
        guard path.last == screen else { return false }
        path.removeLast()
        return true
    }
}
