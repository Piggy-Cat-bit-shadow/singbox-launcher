// ActionPolicy — which core and daemon controls are offered, and why not.
//
// WHY THIS IS ITS OWN FILE. Every rule here answers "may the user press this, and
// if not, what do we tell them". Both halves have been wrong in ways users feel
// immediately: a Start button that produced a backend error instead of being
// disabled, a Restart offered on a stopped core where it means nothing, an Import
// that opened a file chooser before revealing the VPN had to be stopped first,
// and — the worst of the family — a disabled control with NO explanation, which
// reads as a broken app rather than a refused action.
//
// None of that can be checked by reading the source with confidence, and the
// toolchain cannot build a Swift test target, so the decisions live in a file with
// no SwiftUI dependency and are EXECUTED by the Go suite.

import Foundation

/// What the core screen may offer right now.
struct CoreActionPolicy {
    let canStart: Bool
    let canStop: Bool
    let canRestart: Bool
    let canImportCore: Bool
    let canSwitchEngine: Bool
    /// Why an unavailable action is unavailable, for the tooltip.
    ///
    /// Reserved for a stated explanation; the CAUSE travels separately as a
    /// `CoreActionRefusal` so the message can be localized by the view, which is
    /// the layer that knows the language. A disabled control with nothing to say
    /// is the defect this whole policy exists to prevent.
    let reason: String?
}

/// The single answer to "may the daemon control plane be torn down?".
struct DaemonDestructivePolicy: Equatable {
    let allowed: Bool
    let reason: DaemonDestructiveBlock?
}

/// Which precondition is missing, when teardown is refused.
///
/// A named cause rather than a bare bool, because the UI must say WHICH
/// precondition is missing — and a caller that cannot tell them apart cannot
/// phrase the refusal it is showing.
enum DaemonDestructiveBlock: Equatable {
    /// The daemon status has not been read, so we do not know what we would be
    /// destroying.
    case statusUnknown
    /// The daemon is driving the core, but the core's state is unknown: the
    /// control channel is in use and we cannot prove it is safe.
    case coreStateUnknown
    /// The VPN is running through the daemon.
    case vpnRunning
    /// The core is coming up or going down through the same channel.
    case coreTransitioning
    /// The core is in an error state: tearing down the channel now would discard
    /// the diagnostics the user needs.
    case coreError
    /// Another operation is in flight.
    case busy
}

/// Decide what the core screen offers, and WHY when it offers nothing.
///
/// Returns the policy together with the refusal cause, or nil when the actions
/// are available. The cause is a separate value from the policy so this file
/// stays free of the localization table (which imports SwiftUI and therefore
/// cannot be compiled here).
///
/// - Parameters:
///   - connected: the backend link is up, so the state below can be trusted.
///   - busy: a core operation is outstanding. ONE OPERATION AT A TIME, ACROSS
///     EVERY CONTROL — this is what makes a concurrent Restart impossible rather
///     than merely unlikely, and it is checked before anything else.
///   - state: the core's state, or nil when it is not yet known.
///   - hasBinary: a core binary is present on disk.
func decideCoreActions(connected: Bool,
                      busy: Bool,
                      state: CoreState?,
                      hasBinary: Bool) -> (CoreActionPolicy, CoreActionRefusal?) {
    func refuse(_ why: CoreActionRefusal) -> (CoreActionPolicy, CoreActionRefusal?) {
        (CoreActionPolicy(canStart: false, canStop: false, canRestart: false,
                          canImportCore: false, canSwitchEngine: false, reason: nil), why)
    }

    guard connected else { return refuse(.backendNotConnected) }
    if busy { return refuse(.operationInFlight) }
    guard let state else { return refuse(.coreStateUnknown) }

    let transitioning = state.isTransitioning

    // Restart means "take a RUNNING core and bring it back running". It is not a
    // synonym for Start: on a stopped core the honest action is Start, and on an
    // errored one it is Retry. Offering Restart in those states presented a
    // control whose semantics matched none of them.
    let canRestart = state == .running && hasBinary

    // The backend refuses an import unless the core is SETTLED STOPPED, and it
    // says so with a distinct error. Checking only "no operation pending" let the
    // user open a file chooser, pick a binary, and only then learn the VPN had to
    // be stopped first.
    let canImport = !transitioning && state == .stopped && hasBinary

    // Engine switching is refused while the core is not stopped: the engine is
    // what runs the core, so changing it underneath a live one is not a
    // supported transition.
    let canSwitch = !transitioning && state == .stopped

    // The refusal EXPLAINS the disabled control. Every branch that disables
    // something must produce one: a disabled control with no reason reads as a
    // broken app rather than a refused action.
    var why: CoreActionRefusal?
    if transitioning {
        why = .coreTransitioning
    } else if state == .running {
        why = .coreRunning
    } else if !hasBinary {
        why = .coreMissing
    } else if state == .error {
        why = .coreErrored
    }

    return (CoreActionPolicy(
        canStart: !transitioning && (state == .stopped || state == .error) && hasBinary,
        canStop: !transitioning && state == .running,
        canRestart: canRestart,
        canImportCore: canImport,
        canSwitchEngine: canSwitch,
        reason: nil), why)
}

/// The reasons a core action can be refused. Kept as causes rather than strings
/// so the policy stays free of the localization table and can be tested without
/// it.
enum CoreActionRefusal: Equatable {
    case backendNotConnected
    case operationInFlight
    case coreStateUnknown
    case coreTransitioning
    case coreRunning
    case coreMissing
    case coreErrored
}

/// Decide whether the daemon control plane may be torn down.
///
/// THE SAFETY RULE, STATED ONCE. The view's `destructiveBlocked` asked only
/// whether the daemon was active and the core `running`. That is one state out of
/// several that make the same teardown unsafe:
///
///   * `starting` — the core is coming up THROUGH the daemon, and removing the
///     pairing under it aborts the start the user just requested;
///   * `stopping` — the core is mid-teardown using the same channel;
///   * an in-flight operation — the teardown would race whatever holds the
///     channel.
///
/// So the rule is not "is it running" but "is the daemon engine SETTLED and
/// IDLE". Expressed as a policy so the row's disabled state, its explanation and
/// the confirmation all read the same answer — and so it can be executed under
/// test without a UI.
///
/// Refusal is the default for anything unknown. "We could not determine the
/// state" is not evidence that destroying a privileged service is safe, and this
/// is one of the few actions a user cannot trivially undo.
func decideDaemonDestructiveActions(status: DaemonStatus?,
                                    pending: Bool,
                                    coreState: CoreState?)
    -> DaemonDestructivePolicy {
    guard let status else {
        return DaemonDestructivePolicy(allowed: false, reason: .statusUnknown)
    }
    guard status.active_mode else {
        // Installed but not carrying traffic: nothing depends on it.
        return DaemonDestructivePolicy(allowed: !pending,
                                       reason: pending ? .busy : nil)
    }
    // Active engine: the control channel is in use unless the core is fully
    // settled and nothing is in flight.
    if pending { return DaemonDestructivePolicy(allowed: false, reason: .busy) }
    guard let state = coreState else {
        return DaemonDestructivePolicy(allowed: false, reason: .coreStateUnknown)
    }
    if state == .running {
        return DaemonDestructivePolicy(allowed: false, reason: .vpnRunning)
    }
    if state.isTransitioning {
        return DaemonDestructivePolicy(allowed: false, reason: .coreTransitioning)
    }
    if state == .error {
        return DaemonDestructivePolicy(allowed: false, reason: .coreError)
    }
    return DaemonDestructivePolicy(allowed: true, reason: nil)
}

// MARK: - The Home screen's primary action

/// What the Home screen's primary button does and says.
enum HomePrimaryAction: Equatable {
    case start
    case stop
    /// Shown while a start or stop is in flight, including the window before the
    /// backend reports the transition.
    case starting
    case stopping
    /// Retry a failure the backend considers retryable.
    case retry
    /// A DETERMINISTIC failure: send the user to the details instead of offering a
    /// retry that cannot succeed.
    case reviewDetails

    /// Whether the button issues a lifecycle command, as opposed to reporting.
    var isCommand: Bool {
        switch self {
        case .start, .stop, .retry: return true
        case .starting, .stopping, .reviewDetails: return false
        }
    }
}

/// Decide the Home button's action.
///
/// THE DEFECT THIS MODELS. `CoreStatus.recoverable` exists to say "retrying this
/// will not help" — an occupied port, a missing binary, a bad bind address — and
/// the protocol documents it as exactly that. The button ignored it and always read
/// "Retry", so a user clicked Start in a loop against a failure that could never
/// succeed, with no path to the information that explained it.
///
/// Two details that must stay right:
///
///   * `recoverable == nil` means an older backend that cannot answer, and is
///     treated as RETRYABLE. Guessing "unrecoverable" would remove the only way
///     forward from a core that might well start on the next attempt.
///
///   * A pending command outranks the reported state. `start_core` returns before
///     the core reaches `starting`, so relying on state alone left a window where
///     the button still read "Start" after being clicked, inviting a second click.
func homePrimaryAction(state: CoreState?,
                       pendingStart: Bool,
                       pendingStop: Bool,
                       recoverable: Bool?) -> HomePrimaryAction {
    // Pending first: the user's own click is the freshest evidence about what is
    // happening, and the backend has not necessarily caught up.
    if pendingStart { return .starting }
    if pendingStop { return .stopping }
    guard let state else { return .start }
    switch state {
    case .running: return .stop
    case .starting: return .starting
    case .stopping: return .stopping
    case .stopped: return .start
    case .error:
        // Only an explicit `false` blocks the retry. Unknown stays retryable.
        return recoverable == false ? .reviewDetails : .retry
    }
}

/// Whether a core error should offer a generic retry.
///
/// Split out because the same question is asked by more than one control, and a
/// second implementation of it is how the two drift apart.
func coreErrorOffersRetry(recoverable: Bool?) -> Bool {
    recoverable != false
}

// MARK: - Proxy screen action policy

/// What one proxy row may offer, given what the screen is already doing.
struct ProxyRowPolicy: Equatable {
    /// Whether the row's Test control is offered at all.
    let canTest: Bool
    /// Whether the row's selection control is offered.
    let canSelect: Bool
    /// Whether the row is the node a switch is currently in flight for.
    let isSwitching: Bool
    /// Whether the row is being measured right now.
    let isTesting: Bool
}

/// Decide what a node row may do.
///
/// THE DEFECT THAT MADE THIS NECESSARY. `withPending` admits ONE operation at a
/// time, so a second single-node test is refused by the model — but the row's own
/// disable rule did not mention that state at all, leaving every OTHER node's Test
/// control looking live. Clicking one produced "another operation is running", an
/// error the proxy screen does not even display. The control promised something
/// the model would refuse, which is the general failure this whole file exists to
/// prevent.
///
/// The product rule is SERIALIZATION: measurements share the core's delay endpoint
/// and the same measurement table. Naming it here means the guard and every control
/// read one answer, and making measurement concurrent later changes this function
/// rather than every row.
///
/// - Parameters:
///   - rowID: the node this row represents.
///   - singleTestInFlightFor: the node whose single test is running, if any.
///   - switchInFlightFor: the node a switch is running for, if any.
///   - groupTestRunning: whether a Test All run is streaming.
///   - listLoading: whether the node list is being (re)read.
func proxyRowPolicy(rowID: String,
                    singleTestInFlightFor: String?,
                    switchInFlightFor: String?,
                    groupTestRunning: Bool,
                    listLoading: Bool) -> ProxyRowPolicy {
    // ANY node's in-flight operation disables EVERY row's test, because the model
    // admits one at a time. A row that looked live here would produce a refusal
    // the user cannot see the reason for.
    let busyElsewhere = singleTestInFlightFor != nil || switchInFlightFor != nil
    let canTest = !busyElsewhere && !groupTestRunning && !listLoading

    // Selection is serialised for the same reason: two concurrent switches race
    // for the same selection.
    let canSelect = switchInFlightFor == nil && !listLoading
    return ProxyRowPolicy(canTest: canTest,
                          canSelect: canSelect,
                          isSwitching: switchInFlightFor == rowID,
                          isTesting: singleTestInFlightFor == rowID)
}

/// Whether the shown node list still describes the running config.
///
/// Staleness is a property of the CONFIG, not of the list: the nodes on screen may
/// have been read successfully from a config that has since been superseded. The
/// screen must say so, because the user is choosing between nodes that the core may
/// no longer be running — and a list that looks authoritative while being out of
/// date is worse than one that admits it.
func isProxyListStale(configStale: Bool) -> Bool { configStale }

// MARK: - Launching Terminal

/// What to report after handing a command to Terminal.
///
/// THE DEFECT THIS MODELS. `NSWorkspace.open` (and `osascript`) returning means
/// the request was ACCEPTED, not that Terminal opened. Reporting success at that
/// point told the user the command was running when nothing had happened — the
/// worst outcome for a step they are about to sit and wait on, because they wait
/// for a command that was never even typed.
///
/// The honest signal is the helper process's exit status, which is why the report
/// waits for it. A non-zero status means the handoff failed and the user must be
/// told so rather than left waiting.
enum TerminalHandoffOutcome: Equatable {
    /// The command was handed over; Terminal is opening it.
    case opened
    /// The handoff failed. The user must be told, because they are about to wait
    /// for something that is not happening.
    case failed
}

/// Decide what to report from the helper's exit status.
func terminalHandoffOutcome(exitStatus: Int32) -> TerminalHandoffOutcome {
    exitStatus == 0 ? .opened : .failed
}

// MARK: - Subscription screen action policy

/// What the subscription screen may offer.
struct SubscriptionActionPolicy: Equatable {
    /// Whether a config reload can be started.
    let canReloadConfig: Bool
    /// Whether "Update All" is worth offering.
    let canUpdateAllSubscriptions: Bool
    /// Whether the Add form's submit is available.
    let canAddSubscription: Bool

    /// The key explaining why "Update All" is unavailable, when it is.
    ///
    /// Nil means there is nothing to explain — either the action is available, or
    /// the list is empty and the screen's own empty state already says so.
    let updateAllReason: SubscriptionActionRefusal?
}

/// The reasons subscription actions are refused.
enum SubscriptionActionRefusal: Equatable {
    /// Another operation is in flight.
    case busy
    /// No enabled source can actually be refreshed — a local snapshot has no URL
    /// to fetch from, so offering "Update All" would promise a network read that
    /// cannot happen.
    case nothingRefreshable
    /// The backend would refuse to overwrite a hand-written config.
    case configNotRebuildable
}

/// Decide what the subscription screen offers.
///
/// THE TWO HALVES THAT BOTH MATTER for a reload: the backend must consider the
/// config REBUILDABLE — a hand-written config would be overwritten, so the backend
/// refuses — and no other operation may be in flight. Stated once so every control
/// that offers a reload (the notice, the proxy screen, the subscription list)
/// agrees, instead of each rediscovering that the guard will refuse.
///
/// - Parameters:
///   - busy: another operation is outstanding.
///   - configRebuildable: the backend's answer about the config on disk.
///   - refreshableEnabledCount: how many ENABLED sources can actually be fetched.
///   - totalCount: how many sources exist, so "nothing refreshable" can be
///     distinguished from "nothing at all".
func decideSubscriptionActions(busy: Bool,
                               configRebuildable: Bool,
                               refreshableEnabledCount: Int,
                               totalCount: Int) -> SubscriptionActionPolicy {
    // Adding is refused only by a concurrent operation: it does not touch the
    // built config.
    let canAdd = !busy

    let canReload = !busy && configRebuildable

    // "Update All" needs something to update. A source that is disabled, or whose
    // input is a local snapshot, cannot be fetched — so the control would promise
    // a network read that cannot happen.
    let canUpdateAll = !busy && refreshableEnabledCount > 0

    var updateAllReason: SubscriptionActionRefusal?
    if busy {
        updateAllReason = .busy
    } else if refreshableEnabledCount == 0 && totalCount > 0 {
        // Only worth explaining when there ARE subscriptions: with none, the empty
        // state already accounts for the screen.
        updateAllReason = .nothingRefreshable
    }

    return SubscriptionActionPolicy(canReloadConfig: canReload,
                                    canUpdateAllSubscriptions: canUpdateAll,
                                    canAddSubscription: canAdd,
                                    updateAllReason: updateAllReason)
}

/// The reason a reload is refused, or nil when it is available.
func reloadConfigRefusal(busy: Bool, configRebuildable: Bool) -> SubscriptionActionRefusal? {
    if busy { return .busy }
    return configRebuildable ? nil : .configNotRebuildable
}
