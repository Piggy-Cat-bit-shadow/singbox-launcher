// AppModel — the frontend's projection of backend state.
//
// The backend is the single source of truth: this model only caches what the
// snapshot and events reported. It never decides on its own that the core is
// running, and it never writes an optimistic state that the backend has not
// confirmed — that is what made the previous UI show a "Stop" button that could
// not be pressed.

// Uses @Observable rather than ObservableObject/@Published: the build machine
// has Command Line Tools only, and its SwiftPM ships the Observation macro
// plugin but NOT the SwiftUI one, so @State/@Environment/@Published cannot
// compile here. @Observable is also the modern approach and gives views
// field-level dependency tracking for free.

import AppKit
import Foundation
import Observation
import ServiceManagement
import SwiftUI

@MainActor
@Observable
final class AppModel {
    /// Connection to the Go helper.
    enum ConnectionState: Equatable {
        case idle
        case connecting
        case ready
        case failed(String)
    }

    private(set) var connection: ConnectionState = .idle
    private(set) var core: CoreStatus?
    private(set) var settings: SettingsState?
    private(set) var handshake: HandshakeResult?
    /// Current proxy selection, always available from the snapshot — unlike
    /// `groups`/`proxies`, which are only populated after the Proxies screen
    /// loads. Home must not depend on that having happened.
    private(set) var proxySummary: ProxySummary?
    private(set) var lastError: String?

    /// Navigation inside the menu-bar window.
    var path: [Screen] = []

    /// Drop the edit drafts for one subscription.
    ///
    /// Called when an edit is committed or the source is deleted, so reopening
    /// the form shows the stored record rather than a submitted draft. Without
    /// it, `primeFields` would see a non-empty draft and skip re-seeding — which
    /// is what keeps an in-progress edit safe, and what would keep a FINISHED one
    /// on screen forever.
    func clearEditDrafts(id: String) {
        drafts.clear(DraftStore.editName(id))
        drafts.clear(DraftStore.editURL(id))
        drafts.clear(DraftStore.editConfirmDelete(id))
    }

    /// Stable owner of every in-progress text field.
    ///
    /// Lives on the model, not on a View, so a re-render cannot discard what the
    /// user has typed — see DraftStore's header for the defect this fixes.
    let drafts = DraftStore()

    /// Which long-running operation the backend is performing, if any.
    ///
    /// Presentation only: the UI shows a spinner while a command is in
    /// flight, but every *result* still comes from backend state. This is not
    /// a second source of truth about the core.
    enum PendingOperation: Equatable {
        case switchingMode(String)
        case startingCore
        case stoppingCore
        case restarting
        case reloadingConfig
        case updatingSubscriptions
        case updatingSetting(SettingID)
        case switchingProxy(String)
        case testingProxy(String)
        case testingGroup
        case addingSubscription
        case savingSubscription
        case removingSubscription
        case refreshingSubscription
        /// Importing a subscription file. Distinct from adding: the file must
        /// be read and parsed, so the row shows its own progress text.
        case importingSubscription
        /// Installing a user-selected core binary.
        case importingCore
        case configuringDaemon
        case pairingDaemon

        /// The lifecycle goal, for the three core commands.
        ///
        /// Nil for every other operation, which is how the settle rule knows
        /// not to apply to them.
        ///
        /// The goal has to travel WITH the operation because the finish line
        /// differs: a stop ends at `stopped`, while a restart passes THROUGH
        /// `stopped` and is not finished until `running` again. A single
        /// "is it settled" predicate cannot express that.
        var coreKind: CoreGoal? {
            switch self {
            case .startingCore: return .start
            case .stoppingCore: return .stop
            case .restarting: return .restart
            default: return nil
            }
        }
    }

    /// A specific boolean setting, so pending feedback can name it.
    enum SettingID: Equatable {
        case autoPing
        case autoUpdateSubscriptions
        case daemonKeepRunning
        /// Enabling or disabling one subscription source. Its own case, because
        /// reusing another setting's id made that other row display "Saving…"
        /// for a save it was not performing.
        case subscriptionEnabled(String)
    }

    /// A daemon setup step the user can request.
    enum DaemonSetupStep {
        case install
        case start
        case repair
        case uninstall
        case removeAll
    }

    private(set) var pending: PendingOperation?

    /// Deadline for the in-flight core operation.
    ///
    /// Set when a core lifecycle command starts and cleared when the backend
    /// settles. Not a UI timer: it only bounds how long the marker may survive a
    /// LOST notification, so that a missed event degrades into "released, with
    /// whatever state the backend last reported" rather than a permanent spinner.
    private var coreOpDeadline: Date?
    /// Transient success line. Auto-clears; see `showTransient`.
    private(set) var transientStatus: String?
    /// Task that clears `transientStatus`, cancelled and restarted per message.
    private var transientTask: Task<Void, Never>?
    /// Identifies the message the current `transientTask` belongs to, so a
    /// superseded timer cannot clear a newer message.
    private var transientToken: UUID?

    // MARK: - Proxy state

    /// Groups offered by the active config.
    private(set) var groups: [ProxyGroup] = []
    /// Nodes of the selected group.
    private(set) var proxies: [ProxyNode] = []
    /// The group whose nodes are currently listed. Empty until the first load.
    private(set) var selectedGroup: String = ""
    /// False while the core is stopped or the Clash API is unconfigured, so
    /// the UI can explain the empty list instead of showing a blank panel.
    private(set) var proxiesAvailable: Bool = false
    /// True while a proxy/test request is in flight.
    private(set) var proxiesLoading: Bool = false
    /// False when the ACTIVE ENGINE has no proxy-listing capability at all.
    ///
    /// Distinct from `proxiesAvailable`, which is about the engine being up:
    /// this one is about the engine being ABLE. Retrying never changes it, so
    /// the screen explains instead of offering an action, and it must never
    /// raise the global error banner — that was the reported defect.
    private(set) var proxiesSupported: Bool = true
    /// Why the engine cannot list proxies, as a backend token. Drives which
    /// explanation is shown; the wording lives in the frontend.
    private(set) var proxiesUnsupportedReason: String?

    /// Why the node list is not usable, if it is not.
    ///
    /// One explicit state rather than a scatter of booleans, because the
    /// screen has to tell apart causes that look identical in data but need
    /// completely different messages and actions: a stopped core, an
    /// unreachable backend, an empty config, and a config that is merely out of
    /// date. Collapsing them produced a single "No nodes in this group" for
    /// every case, which is wrong for most of them.
    /// Live state of a "Test All" run.
    ///
    /// A dedicated type rather than a bool-like pending flag: the row shows a
    /// counter and each finished node updates immediately, so the UI needs
    /// total/completed plus which nodes are currently in flight. Cramming that
    /// into the shared pending enum is what made the old state unmaintainable.
    enum ProxyGroupTestState: Equatable {
        case idle
        case running(GroupTestProgress)

        var progress: GroupTestProgress? {
            if case .running(let p) = self { return p }
            return nil
        }

        var isRunning: Bool { progress != nil }
    }

    /// Progress of one running group test.
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

    enum ProxyListState: Equatable {
        /// Nothing requested yet.
        case idle
        /// A load is in flight and there is nothing to show yet.
        case loading
        /// The backend is not answering; nothing can be known.
        case backendUnavailable
        /// The core is not running, so the Clash API has no node list.
        case coreStopped
        /// The config has no selector groups at all.
        case noGroups
        /// The built config is behind the state; a reload is needed.
        case configStale
        /// The active engine cannot list proxies at all. Not an error: no
        /// user action changes an engine capability, so the screen explains
        /// the situation and names the way forward instead of offering Retry.
        case unsupportedByEngine
        /// The selected group genuinely has no nodes.
        case empty
        /// A group was never selected, so no nodes were requested.
        case noGroupSelected
        /// Nodes are present.
        case ready
        /// The last load failed; `proxyError` carries the backend's message.
        case failed
    }

    /// The backend's message for the last failed proxy load, if any.
    private(set) var proxyError: String?

    /// What the Proxies screen should present.
    ///
    /// Ordered by what the user can act on: an unreachable backend outranks a
    /// stale config, because nothing else can be determined until it answers.
    var proxyListState: ProxyListState {
        if shouldShowBackendDown { return .backendUnavailable }
        // Checked before `failed`: capability outranks a past failure, because
        // a retry cannot resolve it and the screen must not offer one.
        if !proxiesSupported { return .unsupportedByEngine }
        if let _ = proxyError, proxies.isEmpty, !proxiesLoading { return .failed }
        if proxiesLoading && proxies.isEmpty { return .loading }
        if core?.state != .running { return .coreStopped }
        if core?.config_stale == true && proxies.isEmpty { return .configStale }
        if groups.isEmpty {
            // Distinguish "the config defines no groups" from "we have not
            // asked yet": they need different messages.
            return proxiesAvailable ? .noGroups : .idle
        }
        if selectedGroup.isEmpty { return .noGroupSelected }
        if proxies.isEmpty { return .empty }
        return .ready
    }

    /// Free-text filter over the node list. Purely a view concern, kept here
    /// because two views (the list and its empty state) must agree on it.
    var proxySearch: String = ""

    /// Latest speed sample, nil until the backend sends one. Cleared when the
    /// core stops so a stale rate is never shown as live.
    private(set) var traffic: TrafficRate?

    // MARK: - Subscription state

    private(set) var subscriptions: [Subscription] = []
    private(set) var subscriptionsLoading = false

    /// Latest maintenance result, shown as a one-line outcome on the
    /// Subscriptions screen.
    private(set) var lastMaintenance: MaintenanceResult?

    // MARK: - Daemon state

    /// Daemon setup state, nil until first loaded.
    private(set) var daemon: DaemonStatus?
    private(set) var daemonLoading = false
    /// Command a setup step produced, for the user to copy or run.
    private(set) var daemonCommand: DaemonCommandResult?

    /// A prepared Terminal command, together with what would invalidate it.
    ///
    /// WHY THIS IS NOT JUST `daemonCommand`. A command is generated for a
    /// SPECIFIC situation ("the service is installed but not paired", "the
    /// service is not installed"), and it stays useful until that situation
    /// actually changes. The previous rule was far cruder: whenever the daemon
    /// was reported ready, ANY command was cleared. That threw away commands for
    /// the two operations whose whole point is that the state has NOT changed yet
    /// — Re-pair and Remove Service both generate a command while the service is
    /// perfectly healthy, so the command disappeared the moment it was created.
    ///
    /// A command now records the state it was generated FOR, and the daemon
    /// status is compared against that rather than against a constant. It
    /// survives any event that leaves its precondition true, which is exactly the
    /// behaviour "run this in Terminal, then come back" requires.
    private(set) var preparedCommand: PreparedDaemonCommand?

    /// A command plus the daemon state it was prepared for.
    struct PreparedDaemonCommand {
        let result: DaemonCommandResult
        /// The state the command was generated in, as the backend reported it.
        /// The command is stale only when the daemon's state has moved on from
        /// this.
        let preparedFor: DaemonStateFingerprint
        /// Monotonic id, so a late response cannot replace a newer command.
        let id: UInt64
    }

    /// The parts of the daemon status that decide whether a command still
    /// applies.
    ///
    /// Deliberately a small, named set rather than the whole DTO: comparing
    /// everything would make an unrelated field (a version string, a timestamp)
    /// invalidate a command the user is about to run, which is the same defect
    /// wearing a different hat.
    struct DaemonStateFingerprint: Equatable {
        let installed: Bool
        let ready: Bool
        let activeMode: Bool
        let paired: Bool

        init(_ status: DaemonStatus) {
            installed = status.installed
            ready = status.ready
            activeMode = status.active_mode
            paired = status.paired
        }
    }

    /// Monotonic counter for prepared commands.
    private var preparedCommandCounter: UInt64 = 0

    // MARK: - Navigation
    //
    // Back is an explicit model operation rather than the system affordance.
    // NavigationStack's automatic back button is a toolbar item that a menu-bar
    // panel cannot reliably show, and a page without a dependable exit is the
    // defect this whole navigation layer exists to prevent.

    /// True when there is somewhere to go back to.
    var canGoBack: Bool { !path.isEmpty }

    /// Go back one screen. Safe to call with an empty path.
    func goBack() {
        guard !path.isEmpty else { return }
        path.removeLast()
    }

    /// Return to the root screen.
    func goHome() { path.removeAll() }

    /// Leave the given screen, but only if the user is STILL on it.
    ///
    /// THE BUG THIS EXISTS TO PREVENT. An operation that navigates on success
    /// used to call `goBack()` unconditionally when its request returned. If the
    /// user pressed Back while that request was in flight, the two popped
    /// DIFFERENT screens: the user's own Back moved them one level, and the
    /// operation's completion then moved them another. One save sent the user
    /// two screens away from where they were.
    ///
    /// The operation must therefore express WHERE it expects to be, and give up
    /// the pop if the user has already left. That is this method: the same pop on
    /// the happy path, and a no-op once the user has taken their own exit.
    ///
    /// The check is by screen IDENTITY rather than by depth, so it stays correct
    /// when an intervening navigation changed the stack shape.
    ///
    /// `path.last == screen` also implies `canGoBack`, so an empty stack is safe.
    @discardableResult
    func popIfCurrent(_ screen: Screen) -> Bool {
        guard path.last == screen else { return false }
        path.removeLast()
        return true
    }

    /// Current screen, or nil at the root.
    var currentScreen: Screen? { path.last }

    /// Nodes matching the current search, in backend order.
    var filteredProxies: [ProxyNode] {
        let query = proxySearch.trimmingCharacters(in: .whitespaces)
        guard !query.isEmpty else { return proxies }
        return proxies.filter {
            $0.label.localizedCaseInsensitiveContains(query)
                || $0.name.localizedCaseInsensitiveContains(query)
                || ($0.type ?? "").localizedCaseInsensitiveContains(query)
        }
    }

    /// Appearance is a frontend-only preference; it never reaches the backend.
    /// Stored in UserDefaults directly because @AppStorage is a SwiftUI macro
    /// and unavailable in this toolchain.
    var appearance: AppearancePreference {
        didSet { UserDefaults.standard.set(appearance.rawValue, forKey: Self.appearanceKey) }
    }

    private static let appearanceKey = "appearance"

    /// The interface language.
    ///
    /// Owned here so the whole app observes one instance: the Language screen
    /// writes it, and every view re-renders from the same value. Persistence and
    /// resolution live in `LanguageStore`, which deliberately does not know about
    /// the backend — language is a frontend-only preference, exactly like
    /// appearance, so it must not grow an IPC method or a settings.json field.
    let language = LanguageStore()

    /// The language strings are currently drawn in, for non-view call sites that
    /// need a translated string (error text, transient messages).
    var resolvedLanguage: Localization { language.resolved }

    /// Launch at Login: a frontend-only preference handled by SMAppService, so
    /// the backend never learns about it.
    private(set) var launchAtLogin: Bool = SMAppService.mainApp.status == .enabled

    /// Enable or disable Launch at Login through SMAppService.
    ///
    /// The system can refuse (the app may not be in a location the service
    /// manager accepts, or the user may have denied it in System Settings). The
    /// toggle then snaps back to the real state — so the reason MUST be
    /// reported. Silently reverting looks like a broken switch, and the user has
    /// no way to learn why.
    func setLaunchAtLogin(_ enabled: Bool) {
        var failure: String?
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
        } catch {
            failure = error.localizedDescription
        }

        // Always report the system's real state, never what was requested: the
        // switch must show whether the login item is actually registered.
        launchAtLogin = SMAppService.mainApp.status == .enabled

        if let failure {
            lastError = enabled
                ? "Could not enable Launch at Login: \(failure)"
                : "Could not disable Launch at Login: \(failure)"
        } else if launchAtLogin != enabled {
            // No thrown error, but the service did not end up in the requested
            // state — usually a pending user approval in System Settings.
            lastError = enabled
                ? "Launch at Login was not enabled. Approve JiejieBox in System Settings › General › Login Items."
                : "Launch at Login is still enabled. Turn it off in System Settings › General › Login Items."
        } else {
            showTransient(enabled
                ? L.launchAtLoginEnabled.tr(resolvedLanguage)
                : L.launchAtLoginDisabled.tr(resolvedLanguage))
        }
    }

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

    enum AppearancePreference: String, CaseIterable, Identifiable {
        case system, light, dark
        var id: String { rawValue }

        /// Localized name for the picker's row value.
        ///
        /// Takes the language rather than reading the environment: this is a
        /// model type, and reaching into SwiftUI's environment from here would
        /// make the enum untestable and couple the model to the view layer. The
        /// view decides the language and passes it in.
        func label(_ language: Localization) -> String {
            switch self {
            case .system: return L.appearanceSystem.tr(language)
            case .light: return L.appearanceLight.tr(language)
            case .dark: return L.appearanceDark.tr(language)
            }
        }

        var colorScheme: ColorScheme? {
            switch self {
            case .system: return nil
            case .light: return .light
            case .dark: return .dark
            }
        }
    }

    init() {
        let stored = UserDefaults.standard.string(forKey: Self.appearanceKey)
        appearance = AppearancePreference(rawValue: stored ?? "") ?? .system
    }

    private let client = BackendClient()
    private var eventTask: Task<Void, Never>?
    /// Highest event sequence applied; events older than the snapshot are
    /// discarded so a late frame cannot roll the UI back.
    ///
    /// ONLY MEANINGFUL TOGETHER WITH `appliedSession`. The backend's sequence
    /// restarts at 1 in every new process, so carrying this value across a
    /// helper restart silently discards everything the new helper sends: its
    /// events all compare as "older than what I already applied". The UI then
    /// stops updating core state, traffic, selections and subscriptions until
    /// the new helper has emitted more events than the previous one ever did.
    private var appliedSeq: Int64 = 0
    /// The highest sequence the model has applied.
    ///
    /// A core operation compares against this to tell a state it CAUSED from a
    /// state that was already true. Exposed read-only: the only legitimate
    /// writer is the event/snapshot application path, which is what makes the
    /// number mean "the backend has published this much".
    var observedSeq: Int64 { appliedSeq }

    /// The backend session `appliedSeq` belongs to.
    ///
    /// `nil` before the first snapshot. A sequence number is only comparable
    /// against events from the SAME session, so a change of session resets the
    /// high-water mark rather than being compared against it.
    private var appliedSession: String?
    /// Events that arrived before a snapshot established the baseline.
    ///
    /// THE BOOTSTRAP RACE, and why buffering is the only correct answer. The
    /// stream is opened before the snapshot is requested, so an event can arrive
    /// while the snapshot is still in flight. Applying it immediately and then
    /// applying the snapshot on top is a rollback: the snapshot was composed at
    /// an earlier sequence number and describes the OLDER state, so the newer
    /// event is overwritten by stale data — and because that event has already
    /// been consumed, it never arrives again. The UI stays wrong until the next
    /// unrelated event.
    ///
    /// So events are held until a snapshot supplies the baseline, then the ones
    /// the snapshot already covers are discarded and the rest are replayed in
    /// order. Nothing is applied twice and nothing is lost.
    private var pendingEvents: [BackendEvent] = []
    /// True while `pendingEvents` is awaiting its baseline.
    private var awaitingBaseline = false

    /// Reloads the reducer deferred, so the event stream is never blocked by them.
    ///
    /// THE STREAM MUST NOT WAIT FOR A ROUND-TRIP. Events were consumed with
    /// `for await event in stream { await self?.apply(event) }`, and `apply`
    /// performed `/proxies`, `/groups`, `/subscriptions` and daemon-status
    /// requests inline. Awaiting the loop body means the stream advances only as
    /// fast as the slowest request: a `traffic_rate` tick arriving during a slow
    /// `/proxies` is not delivered until it returns, so the speed display freezes
    /// and every queued event lands in a burst afterwards. The events that report
    /// LIVENESS queue behind the same request, so a wedged call also delays
    /// learning that the backend is shutting down.
    ///
    /// The fix is to record WHAT needs reloading and let the stream continue, so
    /// this is a set rather than a queue: ten `subscriptions_changed` events mean
    /// one reload, not ten.
    private var pendingReloads: Set<PendingReload> = []
    /// The task performing the deferred reloads, if one is running.
    ///
    /// One at a time, deliberately. Overlapping reloads of the same resource can
    /// land out of order, and the older response would then overwrite the newer
    /// one — the exact staleness the sequence filter exists to prevent, arriving
    /// through a different door.
    private var reloadTask: Task<Void, Never>?

    /// What the reducer can ask for without blocking the stream.
    enum PendingReload: Hashable {
        case groups
        case proxies(group: String)
        case subscriptions
        case daemonStatus
    }
    /// The in-flight bootstrap, if any.
    ///
    /// A second caller awaits the SAME task rather than starting a second
    /// helper: without this, two concurrent `start()` calls (a re-created panel
    /// view, a retry racing the first attempt) would each launch a process,
    /// open a second event stream and handshake twice.
    private var bootstrap: Task<Void, Never>?

    // MARK: - Lifecycle

    /// Ensure the backend is running, without binding it to the caller's
    /// lifetime.
    ///
    /// The panel view calls this from `.task`, but SwiftUI cancels that task
    /// when the menu-bar window closes. Starting the helper inside the caller's
    /// task would therefore abort the bootstrap every time the user dismisses
    /// the panel — so the work is handed to a detached, model-owned task that
    /// survives the view, and the caller only awaits its completion.
    ///
    /// Idempotent: concurrent and repeated calls join the same attempt.
    func bootstrap() async {
        if connection == .ready { return }
        if let bootstrap {
            await bootstrap.value
            return
        }

        // Explicit type parameters and an explicit `Void` body: a bare
        // `Task { ... return () }` is ambiguous against
        // `Task.init(name:priority:operation:)` on newer toolchains, where the
        // trailing value makes the overload resolution fail outright.
        let task = Task<Void, Never> { [weak self] in
            await self?.performStart()
        }
        bootstrap = task
        await task.value
        // Cleared only if it is still ours, so a newer bootstrap is not dropped.
        if bootstrap == task { bootstrap = nil }
    }

    /// Start the backend, handshake, take a snapshot and begin consuming events.
    ///
    /// Kept as the explicit entry point for retries and tests; it is the same
    /// idempotent bootstrap.
    func start() async {
        await bootstrap()
    }

    private func performStart() async {
        connection = .connecting
        lastError = nil

        do {
            try await client.start { [weak self] code in
                Task { @MainActor in
                    // Only ever reported for an exit we did not ask for; the
                    // client filters intentional quits and restarts.
                    self?.abandonCoreOperation()
                    self?.connection = .failed("The backend stopped unexpectedly (code \(code)).")
                }
            }

            handshake = try await client.handshake()

            // Subscribe before the snapshot: an event that races the snapshot
            // is filtered by sequence number rather than lost. Any previous
            // stream is finished first, so an event can never be delivered to
            // two consumers after a restart.
            eventTask?.cancel()
            // Events may arrive from here on, before any baseline exists. They
            // are buffered by `apply` and replayed after the snapshot lands.
            awaitingBaseline = true
            pendingEvents.removeAll()
            let stream = await client.events()
            eventTask = Task<Void, Never> { [weak self] in
                for await event in stream {
                    // NOT awaited: awaiting the reducer here would let any slow
                    // request inside it back-pressure the whole stream. `apply`
                    // returns as soon as it has applied the payload and recorded
                    // what to reload; the reload itself runs on `reloadTask`.
                    self?.apply(event)
                }
            }

            let snapshot = try await client.snapshot()
            // Snapshot FIRST, then the events that raced it — never the reverse.
            applyBaseline(snapshot)

            // The token that proves this bootstrap was not superseded or
            // cancelled while it ran.
            try Task.checkCancellation()

            connection = .ready

            // Prime the summaries Home displays. Both are cheap reads, and both
            // were previously loaded only when their own screen was opened —
            // which left Home reporting "Subscriptions: None" and an unhelpful
            // daemon subtitle on a fresh launch, even with sources configured.
            // Home must not depend on the user having visited another page.
            await loadSubscriptions()
            await loadDaemonStatus()
        } catch is CancellationError {
            // The panel closed mid-start. The helper is still ours and the next
            // start() will finish the job, so leave the state resumable rather
            // than pinning the UI on "connecting" forever.
            disarmBaselineBarrier()
            connection = .idle
        } catch {
            // THE BARRIER MUST BE DISARMED ON EVERY FAILURE PATH.
            //
            // `awaitingBaseline` is armed before the stream opens and disarmed by
            // `applyBaseline`. If the handshake, the stream or the snapshot throws, the
            // barrier stays armed with `appliedSession == nil` — and in that state
            // `apply(event:)` DROPS every event: not applied, not buffered, and
            // `appliedSeq` never advances. The connection then reports `.failed` and
            // the app looks dead rather than merely disconnected, because the live
            // event stream is being thrown away.
            //
            // A failure here means there will be no baseline for this attempt, so the
            // barrier's premise is gone and it must not outlive it.
            disarmBaselineBarrier()

            // RELEASE THE HELPER, OR RETRY CANNOT WORK.
            //
            // This branch is reached with the process ALREADY LAUNCHED — the handshake
            // threw, the event stream could not be opened, or the snapshot request failed.
            // `BackendClient.start` begins with `guard process == nil else { return }`, so
            // leaving that helper running means the Retry button calls start(), which
            // returns immediately without doing anything, and the UI reports the same
            // failure again. The app looks broken rather than disconnected, and the only
            // way out is to quit and relaunch — which nothing on screen says.
            //
            // `stop()` releases the process and clears the session state, so the next
            // attempt is a genuine restart. Its own failure mode (a helper that will not
            // die) is handled below by reporting that instead of starting a second one.
            await stop()
            // `stop()` sets `.idle` on success; the failure is what the user must see, so
            // the message is applied after it.
            connection = .failed(error.localizedDescription)
        }
    }

    /// Disarms the bootstrap barrier, discarding anything buffered against it.
    ///
    /// Called on every exit from `performStart` that does not end in a baseline: the
    /// barrier is a promise that a snapshot is coming, and a failed bootstrap has
    /// broken that promise. Leaving it armed turns the event stream into a no-op.
    private func disarmBaselineBarrier() {
        awaitingBaseline = false
        pendingEvents.removeAll()
    }

    /// Shut everything down, letting the backend decide the core's fate.
    ///
    /// The order matters: the shutdown command lets the backend run its own
    /// graceful exit (which stops the core in classic mode and leaves it
    /// running in daemon mode with keep-running enabled). Only after that is
    /// the helper process torn down, and it gets a grace period to finish.
    func quit() async {
        // A second click while quitting must not start a second teardown.
        guard !isQuitting else { return }
        isQuitting = true
        clearTransientStatus()

        eventTask?.cancel()
        eventTask = nil
        // The app is leaving, so a deferred reload has nobody to run it for — and running
        // one during teardown means issuing requests to a backend that is on its way out.
        cancelPendingReloads()

        // Best effort by design: shutdownGracefully never throws, so a backend
        // that already died cannot block the quit.
        await client.shutdownGracefully()

        connection = .idle
        // Actually leave. Without this the helper exits but the menu-bar app
        // stays alive with no backend, which is the worst of both states: the
        // icon remains, and every control in it is inert.
        NSApplication.shared.terminate(nil)
    }

    /// True from the moment Quit is pressed until the app exits. Drives the
    /// button's disabled state so the UI cannot start a second teardown.
    private(set) var isQuitting: Bool = false

    /// Stop the connection without asking the backend to exit.
    func stop() async {
        eventTask?.cancel()
        eventTask = nil
        // Drop the session and everything buffered against it. The helper that
        // produced that sequence is going away, and its numbers mean nothing to
        // whatever helper starts next.
        //
        // Clearing here is a correctness requirement, not tidiness: a buffer
        // left populated would be replayed against the NEXT session's snapshot
        // and would apply a dead process's view of the world.
        appliedSession = nil
        appliedSeq = 0
        pendingEvents.removeAll()
        awaitingBaseline = false
        // The deferred reloads belong to the session that is going away, for the same
        // reason the buffered events do. A drain already in flight keeps issuing
        // /groups, /proxies, /subscriptions and daemon-status requests against a backend
        // that is shutting down — and a batch still queued would fire against the NEXT
        // helper, applying one session's intent to another's data.
        cancelPendingReloads()
        do {
            try await client.shutdown()
        } catch {
            // The previous helper would not stop. Surface it instead of starting a
            // replacement over a live one: two helpers would own the same state,
            // config and core.
            connection = .failed(error.localizedDescription)
            return
        }
        connection = .idle
    }

    /// Restart the backend after a crash.
    ///
    /// `stop()` can legitimately FAIL — the previous helper refused to die, and
    /// starting a replacement over a live one would leave two processes owning the
    /// same state, config and core. When that happens `start()` must not run, and the
    /// caller must be able to tell: silently returning would look like a restart that
    /// did nothing, which is exactly what it is.
    func restart() async {
        await stop()
        // A failed stop leaves the connection `.failed` and the helper alive. Starting
        // now would be the two-helpers case the stop refused to create, so the restart
        // reports the stop's failure rather than papering over it.
        guard case .failed = connection else {
            await start()
            return
        }
    }

    // MARK: - Commands
    //
    // Every command goes to the backend and waits for its event. The UI never
    // flips state itself.

    /// Start the core.
    ///
    /// Uses `withPending` like every other command. `start_core` returns as soon
    /// as the start is REQUESTED — the transition to `running` happens
    /// asynchronously — so without a pending marker a rapid double click sent two
    /// start commands before the state had changed to `starting`.
    func startCore() async {
        await withCorePending(.startingCore) {
            try await self.client.startCore()
        }
    }

    /// Stop the core. Same reasoning as `startCore`.
    func stopCore() async {
        await withCorePending(.stoppingCore) {
            try await self.client.stopCore()
        }
    }

    func restartCore() async {
        await withCorePending(.restarting) {
            try await self.client.restartCore()
        }
    }

    /// Switch the core engine.
    ///
    /// The backend refuses while the core is running and the error is shown
    /// verbatim, because that refusal is the real product rule rather than a
    /// failure.
    func setCoreMode(_ mode: String) async {
        await withPending(.switchingMode(mode), success: nil) {
            let snapshot = try await self.client.setCoreMode(mode)
            self.apply(snapshot)
        }
    }

    func setAutoPing(_ enabled: Bool) async {
        await withPending(.updatingSetting(.autoPing),
                          success: enabled ? "Auto ping enabled." : "Auto ping disabled.") {
            self.settings = try await self.client.setAutoPing(enabled)
        }
    }

    func setAutoUpdateSubscriptions(_ enabled: Bool) async {
        await withPending(.updatingSetting(.autoUpdateSubscriptions),
                          success: enabled ? "Automatic updates enabled." : "Automatic updates disabled.") {
            self.settings = try await self.client.setAutoUpdateSubscriptions(enabled)
        }
    }

    // MARK: - Proxies

    /// Load the groups and the nodes of the active group.
    ///
    /// Failures are reported through `lastError` but leave whatever list was
    /// already on screen, because losing the visible proxies to a transient
    /// API hiccup is worse than showing slightly stale ones.
    func loadProxies(group: String? = nil) async {
        proxiesLoading = true
        proxyError = nil
        defer { proxiesLoading = false }
        do {
            let target = group ?? selectedGroup
            // An empty group means "the config default", which the backend
            // resolves; asking for "" is deliberate, not a bug.
            let list = try await client.proxies(group: target)
            apply(list)
        } catch {
            // Feature-level by policy: recorded for THIS screen to explain
            // inline, and deliberately NOT copied into `lastError`.
            //
            // A proxy read failing is not a whole-product failure. Promoting it
            // to the global banner is what put "cannot read the proxies of
            // group …" on Home, where it displaced the core and config status a
            // user actually needs and offered no action. Home shows only
            // blocking conditions; this stays here.
            proxyError = error.localizedDescription
        }
    }

    /// Load the group list (used when the Proxy screen first appears).
    func loadGroups() async {
        proxiesLoading = true
        proxyError = nil
        defer { proxiesLoading = false }

        do {
            let list = try await client.proxyGroups()
            groups = list.groups
            proxiesAvailable = list.available
            // A capability answer, not a failure: recorded so the screen can
            // explain it, and it outranks a stale error from an earlier attempt.
            proxiesSupported = list.isSupported
            proxiesUnsupportedReason = list.unsupported_reason
            if list.isSupported { proxyError = nil }

            // Prefer the config's default group, then the first one, then
            // wherever we already were — but only keep a remembered group if
            // the config still defines it. Pointing at a group that no longer
            // exists made every later load fail with "group not found".
            if selectedGroup.isEmpty
                || !list.groups.contains(where: { $0.name == selectedGroup }) {
                selectedGroup = list.group ?? list.groups.first?.name ?? ""
            }
        } catch {
            // Same policy as loadProxies: this screen's own failure, not a
            // product-level one. See the note there.
            proxyError = error.localizedDescription
            return
        }

        // The node load is a SEPARATE step with its own failure. Bundling it
        // into the group request meant a node-level error was reported as a
        // group-level one, blaming the wrong thing and discarding a group list
        // that had in fact loaded fine.
        guard !selectedGroup.isEmpty else { return }

        proxiesLoading = true
        defer { proxiesLoading = false }
        do {
            let nodes = try await client.proxies(group: selectedGroup)
            apply(nodes)
        } catch {
            proxyError = error.localizedDescription
        }
    }

    func selectGroup(_ name: String) async {
        guard name != selectedGroup else { return }
        selectedGroup = name
        proxySearch = ""
        await loadProxies(group: name)
    }

    /// Switch the active node. The backend re-reads the core, so the checkmark
    /// reflects what actually happened rather than what was clicked.
    func switchProxy(_ node: ProxyNode) async {
        await withPending(.switchingProxy(node.name), success: nil) {
            let list = try await self.client.switchProxy(group: node.group, name: node.name)
            self.apply(list)
        }
    }

    /// Measure one node.
    ///
    /// A failure is data, not a global error: the row already shows "timeout"
    /// and the technical reason is available there. Writing `lastError` would
    /// put a per-node failure on the Home banner, which is the policy this
    /// screen was fixed to follow.
    func testProxy(_ node: ProxyNode) async {
        await withPending(.testingProxy(node.name), success: nil) {
            let list = try await self.client.testProxy(group: node.group, name: node.name)
            self.apply(list)
        }
    }

    /// Measure every node in the current group.
    ///
    /// The backend owns scheduling and concurrency; this only starts the run and
    /// consumes progress. Doing the fan-out here would move cancellation,
    /// aggregation and error policy into the UI, where they cannot be tested
    /// against a transport.
    func testGroup() async {
        guard !selectedGroup.isEmpty else { return }
        guard proxyActions.can_test_group else { return }
        let group = selectedGroup
        do {
            let result = try await client.testProxyGroup(group)
            // The response is authoritative. It may arrive before or after the
            // finished event, so both paths clear the state — applying them in
            // either order must converge, never resume "running".
            apply(result.proxies)
            if result.group == selectedGroup {
                groupTest = .idle
            }
        } catch {
            // A transport-level failure is the one case that clears the UI
            // without a result; without this the panel would stay on "测速 12/36"
            // forever.
            groupTest = .idle
            lastError = error.localizedDescription
        }
    }

    /// Apply one progress frame, ignoring anything from a superseded run.
    ///
    /// Filtering by BOTH run id and group is what stops a slow run from painting
    /// results onto a screen that has already moved to another group or another
    /// engine. Filtering by node name alone would let a stale frame update a row
    /// that now belongs to a different test.
    func applyGroupTestProgress(_ p: ProxyTestProgress) {
        if p.isStarted {
            groupTest = .running(GroupTestProgress(
                id: p.run_id, group: p.group, total: p.total,
                completed: 0, succeeded: 0, failed: 0, inFlight: []))
            return
        }

        guard var running = groupTest.progress, running.id == p.run_id,
              running.group == p.group, running.group == selectedGroup else {
            // A superseded run, or one for a group the user has left.
            return
        }

        switch p.phase {
        case "node_started":
            // Only this row spins. Marking every node as testing at the start
            // would suggest the app is hammering all of them at once, when in
            // fact only `concurrency` of them are in flight.
            if let node = p.node { running.inFlight.insert(node) }
            groupTest = .running(running)
        case "result":
            running.completed = p.completed
            running.succeeded = p.succeeded
            running.failed = p.failed
            if let node = p.node { running.inFlight.remove(node) }
            groupTest = .running(running)
            // The backend stores the measurement BEFORE emitting, so re-reading
            // here cannot show a stale value.
            if let node = p.node, let status = p.status,
               let delay = p.delay, let measured = ProxyMeasurementStatus(rawValue: status) {
                updateNodeMeasurement(name: node, delay: delay, status: measured, error: p.error)
            }
        case "finished":
            groupTest = .idle
        default:
            break
        }
    }

    /// Patch one node's latency from a progress frame.
    ///
    /// Deliberately a local patch rather than a full reload: the point of
    /// streaming progress is that a row updates the moment its result arrives,
    /// without waiting for all N nodes or issuing N requests.
    private func updateNodeMeasurement(
        name: String, delay: Int64, status: ProxyMeasurementStatus, error: String?
    ) {
        guard let idx = proxies.firstIndex(where: { $0.name == name }) else { return }
        // Success carries the number; every other outcome clears it, because a
        // node we just failed to reach is not "42 ms" on the strength of an
        // older reading.
        proxies[idx].delay = status == .success ? delay : -1
        proxies[idx].status = status.rawValue
        if let error { proxies[idx].last_error = error }
    }

    private func apply(_ list: ProxyList) {
        if let caps = list.capabilities {
            proxyActions = caps
        }
        if !list.groups.isEmpty { groups = list.groups }
        // A group-only reply must not wipe the visible nodes.
        if list.available || !list.proxies.isEmpty || !(list.group ?? "").isEmpty {
            if !list.proxies.isEmpty || (list.group ?? "") == selectedGroup {
                proxies = list.proxies
            }
        }
        if let g = list.group, !g.isEmpty { selectedGroup = g }
        proxiesAvailable = list.available
        // The node read carries the same capability answer as the group read,
        // so a `get_proxies` reply must not leave a stale "supported" behind.
        proxiesSupported = list.isSupported
        proxiesUnsupportedReason = list.unsupported_reason
    }

    // MARK: - Subscriptions

    func loadSubscriptions() async {
        subscriptionsLoading = true
        defer { subscriptionsLoading = false }
        do {
            subscriptions = try await client.listSubscriptions()
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Add a source and return to the list.
    ///
    /// Returns true on success so the caller can pop only when the source was
    /// actually stored — popping on failure would hide the error along with the
    /// form the user was filling in.
    func addSubscription(name: String, url: String) async -> Bool {
        var added = false
        await withPending(.addingSubscription, success: nil) {
            _ = try await self.client.addSubscription(name: name, url: url)
            added = true
            self.showTransient(L.subscriptionAdded.tr(self.resolvedLanguage))
        }
        if added { await loadSubscriptions() }
        return added
    }

    /// Edit one source. `clearName` requests removal of a custom name.
    func updateSubscription(id: String,
                            name: String,
                            url: String,
                            clearName: Bool = false) async -> Bool {
        var saved = false
        await withPending(.savingSubscription, success: nil) {
            _ = try await self.client.updateSubscription(
                id: id, name: name, url: url, clearName: clearName)
            saved = true
            self.showTransient(L.subscriptionSaved.tr(self.resolvedLanguage))
        }
        if saved { await loadSubscriptions() }
        return saved
    }

    /// True when "Update All" would actually fetch something.
    ///
    /// The row used to be enabled whenever the list was non-empty, which is not
    /// the same question: a list of only DISABLED sources, or of nothing but
    /// imported local files (which have no provider to fetch from), makes the
    /// button a no-op that looks available. An action that cannot do anything
    /// must not be offered as though it could.
    ///
    /// Stated on the model rather than inline in the view so the rule can be
    /// tested without a running UI.
    var canUpdateAllSubscriptions: Bool {
        guard pending == nil else { return false }
        return subscriptions.contains { $0.enabled && $0.isRefreshable }
    }

    func setSubscriptionEnabled(_ id: String, enabled: Bool) async {
        await withPending(.updatingSetting(.subscriptionEnabled(id)),
                          success: enabled ? "Subscription enabled." : "Subscription disabled.") {
            _ = try await self.client.setSubscriptionEnabled(id: id, enabled: enabled)
        }
        await loadSubscriptions()
    }

    func removeSubscription(_ id: String) async -> Bool {
        var removed = false
        await withPending(.removingSubscription, success: nil) {
            try await self.client.removeSubscription(id: id)
            removed = true
            self.showTransient(L.subscriptionRemoved.tr(self.resolvedLanguage))
        }
        if removed { await loadSubscriptions() }
        return removed
    }

    /// Refresh one source.
    ///
    /// A fetch failure is reported but is NOT fatal to the source: the user's
    /// URL stays configured with an error line, because a provider being down
    /// is not a reason to make someone retype their link.
    func refreshSubscription(_ id: String) async {
        await withPending(.refreshingSubscription, success: nil) {
            _ = try await self.client.refreshSubscription(id: id)
        }
        await loadSubscriptions()
    }

    /// Import a subscription from a local file the user picked.
    ///
    /// Returns true only when the backend actually stored the source, so the
    /// caller can dismiss its sheet on success and keep it open on failure —
    /// a sheet that closes on error takes the explanation with it.
    ///
    /// The imported nodes are NOT yet in the built config, so `config_stale`
    /// comes back true. This deliberately does not rebuild: rebuilding is the
    /// user's decision, and for an externally-managed config it must never
    /// happen at all.
    @discardableResult
    func importSubscriptionFile(path: String) async -> Bool {
        var imported = false
        await withPending(.importingSubscription, success: nil) {
            let result = try await self.client.importSubscriptionFile(path: path)
            imported = true
            self.showTransient(result.summary)
            // A count the parser could not express is worth naming: silently
            // dropping entries would misrepresent what the file contained.
            if result.unsupported_count > 0, let first = result.warnings?.first, !first.isEmpty {
                self.setError(first)
            }
        }
        if imported { await loadSubscriptions() }
        return imported
    }

    /// Install a core binary the user picked.
    ///
    /// Returns true when the swap succeeded. The version shown afterwards comes
    /// from the backend's read-back of the installed binary, never from the
    /// file the user selected — those differ whenever the backend rejects a
    /// candidate, and assuming otherwise would show a version that is not
    /// running.
    @discardableResult
    func importCoreFile(path: String) async -> Bool {
        var installed = false
        await withPending(.importingCore, success: nil) {
            let result = try await self.client.importCoreFile(path: path)
            installed = true
            self.showTransient(result.summary)
            // A warning here means the swap worked but something about it needs
            // saying — for example a daemon still serving the old binary.
            if let warning = result.warning, !warning.isEmpty {
                self.setError(warning)
            }
        }
        if installed {
            await refreshCoreState()
            await loadDaemonStatus()
        }
        return installed
    }

    /// Update every enabled source.
    func updateAllSubscriptions() async {
        await withPending(.updatingSubscriptions, success: nil) {
            let result = try await self.client.updateSubscriptions()
            self.lastMaintenance = result
            self.report(result, success: "Subscriptions updated.")
        }
        await loadSubscriptions()
        await refreshCoreState()
    }

    // MARK: - Daemon

    /// Re-read the daemon status.
    ///
    /// SINGLE-FLIGHT, and the newest request wins.
    ///
    /// This was a bare `await client.daemonStatus()` with a `daemonLoading`
    /// flag that only the defer cleared. Nothing rejected a second concurrent
    /// call — several Refresh controls did not even test the flag — so three
    /// rapid clicks issued three requests and whatever returned LAST wrote the
    /// status. A slow older response overwriting a newer one leaves the screen
    /// showing a state the backend has already moved past, and the user acts on
    /// it.
    ///
    /// Each call takes a generation, and a response is committed only if it is
    /// still the newest. A caller that arrives while a read is in flight joins
    /// that read instead of starting another, so repeated clicks cost one
    /// request and every waiter still gets a fresh answer.
    func loadDaemonStatus() async {
        // Join the read already in flight rather than starting a second one.
        if let existing = daemonStatusTask {
            await existing.value
            return
        }
        let generation = daemonStatusGeneration &+ 1
        daemonStatusGeneration = generation
        daemonLoading = true
        let task = Task { @MainActor in
            defer { daemonLoading = false }
            do {
                let status = try await client.daemonStatus()
                // Only the newest read may write, and only while it is newest.
                guard generation == daemonStatusGeneration else { return }
                applyDaemonStatus(status)
            } catch {
                guard generation == daemonStatusGeneration else { return }
                // A failed refresh must not destroy the last known status: the
                // screen would lose the context for the command it is showing.
                lastError = error.localizedDescription
            }
        }
        daemonStatusTask = task
        await task.value
        if generation == daemonStatusGeneration { daemonStatusTask = nil }
    }

    /// Adopt a status and retire any command the new state has invalidated.
    ///
    /// The ONLY place that decides a prepared command is dead, so there is one
    /// rule instead of a scattering of `daemonCommand = nil` assignments that
    /// each had to guess.
    private func applyDaemonStatus(_ status: DaemonStatus) {
        daemon = status
        guard let prepared = preparedCommand else { return }
        // Still the situation the command was generated for: it remains valid,
        // which is what lets the user go to Terminal and come back.
        if prepared.preparedFor == DaemonStateFingerprint(status) { return }
        // The state moved. Some commands are still meaningful afterward (a
        // fresh invite stays usable until it is redeemed or expires), so the
        // decision is per-operation rather than "any change clears it".
        if prepared.result.operation == DaemonOperation.freshInvite,
           status.installed, !status.paired {
            return
        }
        clearDaemonCommand()
    }

    /// The refresh currently in flight, if any, so callers can join it.
    private var daemonStatusTask: Task<Void, Never>?
    /// Monotonic id for daemon status reads.
    private var daemonStatusGeneration: UInt64 = 0

    /// Ask the backend for a setup command (install, start, repair, uninstall).
    ///
    /// This never switches engines. Setting up the service and activating it
    /// are separate steps on purpose: doing both from one click is what made
    /// selecting Daemon look like a freeze.
    func daemonSetup(_ step: DaemonSetupStep) async {
        await withPending(.configuringDaemon, success: nil) {
            let result: DaemonCommandResult
            switch step {
            case .install: result = try await self.client.daemonInstall()
            case .start: result = try await self.client.daemonStart()
            case .repair: result = try await self.client.daemonRepair()
            case .uninstall: result = try await self.client.daemonUninstall(purge: false)
            case .removeAll: result = try await self.client.daemonUninstall(purge: true)
            }
            // Record the command WITH the state it was generated for. Adopting
            // the status the backend returned alongside it is what makes the
            // fingerprint describe the situation on screen.
            self.daemon = result.status
            self.preparedCommand = PreparedDaemonCommand(
                result: result,
                preparedFor: DaemonStateFingerprint(result.status),
                id: self.nextPreparedCommandID())
            self.daemonCommand = result
        }
    }

    /// Ids are monotonic so "is this still the current command?" is answerable
    /// without comparing payloads.
    private func nextPreparedCommandID() -> UInt64 {
        preparedCommandCounter &+= 1
        return preparedCommandCounter
    }

    /// Begin pairing: produce the one-time invite command.
    ///
    /// "Pair Service" used to jump straight to an empty paste box, while the
    /// command that actually produces an invite was hidden under
    /// Advanced → Re-pair. A first-time user following the screen was sent to a
    /// form with nothing to paste and no hint of where the invite comes from.
    ///
    /// This is the same backend call Re-pair uses (`DaemonRepair` →
    /// `lxd client add`), surfaced where the user needs it. It stays on the
    /// Daemon screen so the command, its Copy/Open-in-Terminal actions and the
    /// "Continue to Pair" step are all visible together.
    ///
    /// Returns true when a usable command was produced.
    @discardableResult
    func prepareDaemonPairing() async -> Bool {
        await daemonSetup(.repair)
        return pairingInviteReady
    }

    /// True when the last daemon command was a usable fresh invite.
    ///
    /// Derived from the backend's own result rather than a separate flag, so
    /// there is no second source of truth that could disagree with what was
    /// actually produced.
    var pairingInviteReady: Bool {
        daemonCommand?.operation == "fresh_invite" && daemonCommand?.available == true
    }

    /// The outcome of the most recent PAIR attempt, for this screen's own
    /// warning about a spent invite.
    ///
    /// Held on the model because the toolchain cannot compile `@State` (see the
    /// header), and because it must describe the ATTEMPT rather than the app's
    /// general error line: reading `lastError` meant any unrelated failure made
    /// the Pair screen claim the user's invite had been used.
    ///
    /// Nil until a pair is attempted, so a freshly opened screen shows no warning.
    private(set) var lastPairAttemptFailed: Bool?

    func pairDaemon(invite: String) async -> Bool {
        var paired = false
        // Cleared before the attempt so a previous failure cannot describe this
        // one, and so the value always belongs to the click that produced it.
        lastPairAttemptFailed = nil
        await withPending(.pairingDaemon, success: nil, recordsPairFailure: true) {
            self.daemon = try await self.client.pairDaemon(invite: invite)
            paired = self.daemon?.paired ?? false
            if paired {
                self.showTransient(L.daemonPaired.tr(self.resolvedLanguage))
                // The invite is spent; leaving the command on screen would
                // invite the user to paste it again, which cannot work. Both
                // representations go, through the one method that owns them.
                self.clearDaemonCommand()
            }
        }
        return paired
    }

    func unpairDaemon() async {
        await withPending(.pairingDaemon, success: nil) {
            self.daemon = try await self.client.unpairDaemon()
            self.showTransient(L.pairingRemoved.tr(self.resolvedLanguage))
        }
    }

    func setDaemonKeepRunning(_ keepRunning: Bool) async {
        await withPending(.updatingSetting(.daemonKeepRunning),
                          success: keepRunning
                            ? "The VPN will keep running after quit."
                            : "The VPN will stop when JiejieBox quits.") {
            self.daemon = try await self.client.setDaemonKeepRunning(keepRunning)
        }
    }

    /// Activate daemon mode. Only offered once the daemon reports ready.
    func activateDaemonMode() async {
        guard daemon?.ready == true else {
            lastError = L.finishDaemonSetup.tr(resolvedLanguage)
            return
        }
        await setCoreMode("daemon")
        await loadDaemonStatus()
    }

    /// Switch back to the classic engine.
    func activateClassicMode() async {
        await setCoreMode("classic")
        await loadDaemonStatus()
    }

    /// Re-read the core snapshot (used after operations that change staleness).
    func refreshCoreState() async {
        do {
            apply(try await client.snapshot())
        } catch {
            lastError = error.localizedDescription
        }
    }

    // MARK: - Maintenance

    func reloadConfig() async {
        await withPending(.reloadingConfig, success: nil) {
            let result = try await self.client.reloadConfig()
            self.report(result, success: "Configuration reloaded.")
        }
    }

    /// Take ownership of an UNKNOWN config after the user confirmed it.
    ///
    /// Refreshes core state so the banner switches from the UNKNOWN message to
    /// the normal "configuration changed" one with a working Reload: the new
    /// ownership is exactly what makes that action possible.
    func adoptConfig() async {
        await withPending(.reloadingConfig, success: "JiejieBox now manages config.json.") {
            let result = try await self.client.adoptConfig()
            self.report(result, success: "JiejieBox now manages config.json.")
            await self.refreshCoreState()
        }
    }

    func updateSubscriptions() async {
        await withPending(.updatingSubscriptions, success: nil) {
            let result = try await self.client.updateSubscriptions()
            self.report(result, success: "Subscriptions updated.")
        }
    }

    /// Surface a maintenance result.
    ///
    /// A result that reports ok=false is a failure the user must see even
    /// though the call itself succeeded — an update where every source failed
    /// returns no error but changed nothing.
    private func report(_ result: MaintenanceResult, success: String) {
        if result.ok {
            showTransient(result.message.isEmpty ? success : result.message)
        } else {
            setError(result.message)
        }
        if !result.core_skips.isEmpty {
            // Node skips accompany the summary rather than replacing it.
            let summary = transientStatus ?? ""
            showTransient((summary + " " + result.core_skips.joined(separator: " ")).trimmingCharacters(in: .whitespaces))
        }
    }

    /// Run an operation with a pending marker and consistent error reporting.
    /// Run a core lifecycle command whose completion is NOT the IPC reply.
    ///
    /// THE BUG THIS FIXES. `start_core` returns as soon as the start is
    /// ACCEPTED. Clearing the pending marker at that moment was wrong, because
    /// the core may still be rebuilding its config, sitting in an authorization
    /// dialog, or waiting on the daemon's apply — for seconds, or (with a
    /// password prompt) minutes. During that window the UI looked idle: the
    /// button reverted to "Start" and the spinner vanished, which is exactly the
    /// "I clicked Start and it went back to stopped" report.
    ///
    /// The marker is now held until the BACKEND reports a settled state
    /// (running, stopped or error) or the wait times out. That makes the
    /// authoritative runtime state the only thing that clears the spinner, so a
    /// slow start looks slow instead of looking broken.
    ///
    /// A failure still clears immediately and reports the reason: an operation
    /// that failed must not leave the UI spinning forever.
    private func withCorePending(_ op: PendingOperation,
                                 _ body: @escaping () async throws -> Void) async {
        guard pending == nil else {
            lastError = L.anotherOperationRunning.tr(resolvedLanguage)
            return
        }

        // THE OPERATION IS IDENTIFIED BEFORE THE COMMAND, NOT AFTER IT.
        //
        // What settles a core operation is a lifecycle transition THAT THIS
        // OPERATION CAUSED. The sequence counter is what makes that decidable:
        // every published state change carries a new `seq`, so a state observed
        // at a sequence below this one was already true when the command was
        // sent and says nothing about whether the command has finished.
        //
        // The previous version asked only "is the core in a non-transitioning
        // state?", which the state BEFORE the operation satisfies immediately:
        //
        //   Stop,  core running: `stop_core` returns as soon as the stop is
        //          accepted; the `stopping` event has not arrived yet;
        //          `waitForCoreToSettle` sees `running` — not transitioning —
        //          and clears the marker at once. The button returns to "Stop"
        //          while the stop is still running.
        //
        //   Start, core stopped: identical, with `stopped` as the stale answer.
        //
        //   Restart: the old `running` is read as the restart's own success.
        //
        // The fix is to require a state change that came AFTER the command.
        // Only the three lifecycle commands reach here, and a missing goal
        // would silently settle on the wrong predicate, so it is a hard
        // failure rather than a default.
        guard let goal = op.coreKind else {
            assertionFailure("withCorePending used for a non-lifecycle operation")
            return
        }
        let opID = UUID()
        let startSeq = observedSeq
        coreOperation = CoreOperation(id: opID, kind: goal, startSeq: startSeq)
        pending = op
        lastError = nil
        coreOpDeadline = Date().addingTimeInterval(Self.coreOperationTimeout)

        do {
            try await body()
        } catch {
            // The command itself was rejected — nothing is running, so the
            // marker must go and the reason must be shown.
            pending = nil
            coreOpDeadline = nil
            coreOperation = nil
            lastError = error.localizedDescription
            return
        }

        // Hold the marker until THIS operation's outcome is observed. If no
        // event ever arrives (a lost notification, a backend that died
        // mid-operation), the deadline reconciles against an authoritative
        // snapshot rather than pretending the operation vanished.
        await waitForCoreToSettle(opID)
    }

    /// The lifecycle operation currently holding the UI, and what would end it.
    private struct CoreOperation {
        let id: UUID
        let kind: CoreGoal
        /// Sequence at the moment the command was sent. Any state at or below
        /// this describes the world BEFORE the operation.
        let startSeq: Int64
    }

    private var coreOperation: CoreOperation?

    /// True when the observed state has moved past the operation's start.
    ///
    /// The transition check and the sequence check are both required, and the
    /// sequence is what the old version was missing entirely:
    ///
    ///   * `stateChanged` alone is not enough — the pre-existing `running` is
    ///     itself an observation, so the very first poll would settle a stop.
    ///   * A sequence bump alone is not enough — an unrelated event (a settings
    ///     save, a traffic sample) also advances the counter while the core is
    ///     still mid-transition, and settling on that would clear the spinner
    ///     during the slow part of the work.
    ///
    /// Both together mean: the backend has published a state that is BOTH new
    /// and finished.
    private func coreOperationSettled(_ op: CoreOperation) -> Bool {
        guard observedSeq > op.startSeq else { return false }
        guard let state = core?.state else { return false }
        return state.isTerminal(for: op.kind)
    }

    /// How long a core operation may hold the UI before the marker is released.
    ///
    /// Longer than the backend's own 45s budget so the normal slow path still
    /// settles by EVENT rather than by this timeout; the value exists only to
    /// release a UI that would otherwise spin forever.
    private static let coreOperationTimeout: TimeInterval = 75

    /// Wait until THIS operation's outcome is observed, or the deadline passes.
    ///
    /// Polls the LOCAL snapshot while the deadline holds, because the state
    /// arrives through the event stream that is already running and a request
    /// per tick would add load without adding information.
    ///
    /// THE DEADLINE DOES NOT DECIDE THE OUTCOME. When it passes we do not know
    /// whether the operation finished and its event was lost, or is still
    /// running, or the backend is gone — so the previous code's response
    /// ("clear the marker, keep whatever state we have") was a guess presented
    /// as a fact. It reopened the buttons on an operation that might still be
    /// running, and the user could then send a second lifecycle command against
    /// a core mid-transition.
    ///
    /// The timeout therefore RECONCILES against an authoritative snapshot:
    ///
    ///   * the reply settles the operation -> release, as normal;
    ///   * the reply shows it is still running -> keep the busy marker, because
    ///     that is the truth, and say the backend is slow;
    ///   * the request fails -> the backend is unreachable, which is a
    ///     connection failure the user must see, not a silent release.
    private func waitForCoreToSettle(_ opID: UUID) async {
        while !Task.isCancelled {
            if let op = coreOperation, op.id == opID, coreOperationSettled(op) {
                finishCoreOperation(opID)
                return
            }
            guard Date() < (coreOpDeadline ?? .distantPast) else { break }
            try? await Task.sleep(nanoseconds: 150_000_000)
        }
        guard coreOperation?.id == opID else { return }
        await reconcileTimedOutCoreOperation(opID)
    }

    /// Ask the backend what is actually true after the wait gave up.
    ///
    /// The question the timeout cannot answer locally is "did my event get
    /// lost, or is this still running?" — and only the backend knows. Asking
    /// it is what turns an unknown into a decision:
    ///
    ///   * `running` / `stopped` consistent with the operation's goal, or an
    ///     `error` -> the operation did complete and the event was lost, so
    ///     adopt the snapshot and release.
    ///   * still `starting` / `stopping`, or a state that contradicts the goal
    ///     -> the work is genuinely unfinished. The marker STAYS, with an
    ///     explanation, so the user is not offered a control that would collide
    ///     with it.
    ///   * the request fails -> the backend is unreachable. That is reported as
    ///     a connection failure instead of a silent release.
    ///
    /// Only a snapshot that genuinely ends the operation releases the marker;
    /// every other path keeps it and says why.
    private func reconcileTimedOutCoreOperation(_ opID: UUID) async {
        guard let op = coreOperation, op.id == opID else { return }

        do {
            let snapshot = try await client.snapshot()
            guard coreOperation?.id == opID else { return }

            // Read the state from the SNAPSHOT, not from `core` after applying
            // it: `apply` advances `appliedSeq` to the snapshot's own sequence,
            // which would make the freshness test below trivially true — the
            // reply would always look "newer than the command" even when it
            // reports the very state the command started from.
            let authoritative = snapshot.core.state
            let settledHere = authoritative.isTerminal(for: op.kind)

            apply(snapshot)
            guard coreOperation?.id == opID else { return }

            if settledHere {
                // The work finished; only the notification was lost. This is
                // the case the deadline exists for, and it releases cleanly and
                // silently because the user's operation did succeed.
                finishCoreOperation(opID)
                return
            }

            // Still in flight. The marker stays — releasing it here is what
            // let a second lifecycle command race the first.
            lastError = L.coreOperationStillRunning.tr(resolvedLanguage)
        } catch {
            // The backend could not be reached. Releasing the marker would make
            // the UI claim an operation ended when we cannot see the backend at
            // all, so the failure is reported and the marker is kept until the
            // connection is re-established (which resets it — see `apply`
            // of a snapshot during reconnect).
            lastError = L.coreOperationUnconfirmed.tr(resolvedLanguage)
        }
    }

    /// Give up on a lifecycle operation whose outcome can never be confirmed.
    ///
    /// Called when the backend that accepted the command is gone: a new session,
    /// a failed connection, a crash. Distinct from `finishCoreOperation`, which
    /// means "the operation completed and we observed it" — this one means "we
    /// will never know", so it must not be confused with success.
    ///
    /// Stated once and called from every such point, because leaving any one of
    /// them out produces the same symptom: a UI that stays busy forever with no
    /// operation behind it.
    private func abandonCoreOperation() {
        guard coreOperation != nil else { return }
        coreOperation = nil
        coreOpDeadline = nil
        pending = nil
    }

    /// Clear the busy marker if it still belongs to this operation.
    ///
    /// Guarded by identity so a stale completion cannot release a NEWER
    /// operation's marker — the same class of bug as a stale proxy response
    /// overwriting the current screen.
    private func finishCoreOperation(_ opID: UUID) {
        guard coreOperation?.id == opID else { return }
        coreOperation = nil
        coreOpDeadline = nil
        pending = nil
    }

    private func withPending(_ op: PendingOperation,
                             success: String?,
                             /// Record the outcome as this screen's pair attempt.
                             ///
                             /// Set by `pairDaemon` only: the Pair screen's "your
                             /// invite may be spent" warning must describe a PAIR
                             /// failure, and sharing the app-wide error line made
                             /// it appear after unrelated failures. Recorded here
                             /// because this is the single place that observes
                             /// whether the command succeeded, so a caller cannot
                             /// forget to set it.
                             recordsPairFailure: Bool = false,
                             _ body: @escaping () async throws -> Void) async {
        // Overlap is prevented in the UI by disabling controls while an
        // operation is in flight; this guard is the second line of defence
        // against a rapid double click that slips through. It must never be a
        // SILENT no-op: a click that produces nothing is indistinguishable from
        // a broken button, so the reason is surfaced.
        guard pending == nil else {
            lastError = L.anotherOperationRunning.tr(resolvedLanguage)
            return
        }
        pending = op
        // A new action supersedes the previous failure, so a stale error does
        // not sit next to a fresh attempt.
        lastError = nil
        defer { pending = nil }
        do {
            try await body()
            if recordsPairFailure { lastPairAttemptFailed = false }
            if let success { showTransient(success) }
        } catch {
            if recordsPairFailure { lastPairAttemptFailed = true }
            lastError = error.localizedDescription
        }
    }

    private func run(_ body: @escaping () async throws -> Void) async {
        do {
            try await body()
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Report a failure.
    ///
    /// Blank text is ignored rather than stored, for the same reason the
    /// transient setter normalises: `""` is not a message, and storing it would
    /// render a banner with an icon and a dismiss button around nothing.
    func setError(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        lastError = trimmed.isEmpty ? nil : trimmed
    }

    /// Reload the configuration and re-read the proxy groups.
    ///
    /// Exposed as one call so views do not have to sequence two awaits inside a
    /// Task closure — a multi-statement `Task { }` is ambiguous against
    /// `Task.init(name:priority:operation:)` on newer Swift toolchains.
    func reloadAndReloadGroups() async {
        await reloadConfig()
        await loadGroups()
    }

    /// Refresh subscriptions, rebuild the config, then re-read the groups.
    ///
    /// The full recovery path for "the node list is out of date": fetch, rebuild,
    /// reload. Named for the same reason as above.
    func updateSubscriptionsAndReload() async {
        await updateAllSubscriptions()
        await reloadConfig()
        await loadGroups()
    }

    func clearError() { lastError = nil }

    /// Show a success line that clears itself after a few seconds.
    ///
    /// Centralised so every caller gets the same lifetime and a new message
    /// replaces the old one instead of stacking. Success and error are
    /// deliberately different: a success message is transient, while an error
    /// persists until the user dismisses it or a later action succeeds — an
    /// error that vanished on a timer is an error the user may never read.
    func showTransient(_ text: String, seconds: Double = 3.0) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else {
            clearTransientStatus()
            return
        }
        transientStatus = trimmed

        // Each message gets its own timer, and a new message cancels the
        // previous one. The token check is the actual guard against the race:
        // without it, message A's timer could fire after message B replaced it
        // and clear B, so a message would vanish early for no visible reason.
        transientTask?.cancel()
        let token = UUID()
        transientToken = token
        transientTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(seconds * 1_000_000_000))
            guard !Task.isCancelled else { return }
            await MainActor.run {
                guard let self, self.transientToken == token else { return }
                self.transientStatus = nil
                self.transientTask = nil
            }
        }
    }

    /// Replace the transient line from a view.
    ///
    /// Empty input is treated as a clear rather than stored: `""` is not a
    /// message, and storing it would render a banner with no text but with its
    /// icon and dismiss button — visible chrome reserving height for nothing.
    /// Normalising here means no caller can reintroduce that state.
    func setTransientStatus(_ text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty {
            clearTransientStatus()
        } else {
            showTransient(trimmed)
        }
    }

    /// Remove the transient message.
    ///
    /// The one supported way to clear it. `nil` is the single representation of
    /// "no message"; there is deliberately no empty-string form, because that
    /// sentinel is what previously left an invisible-but-present banner
    /// occupying layout.
    func clearTransientStatus() {
        transientTask?.cancel()
        transientTask = nil
        transientStatus = nil
    }

    /// Drop the last setup command, so a stale command is not shown next to
    /// refreshed status.
    /// Drop the prepared command and its invalidation state.
    ///
    /// Both fields together, always: leaving `preparedCommand` behind would let
    /// the staleness rule reason about a command that is no longer displayed.
    func clearDaemonCommand() {
        daemonCommand = nil
        preparedCommand = nil
    }

    // MARK: - Derived state for the views

    var coreMissing: Bool { core?.binary_exists == false }
    var configMissing: Bool { core?.config_exists == false }

    /// The engine actually in use, from RUNTIME state.
    ///
    /// `core.backend` is what the running backend reports; `settings` is only
    /// the saved preference. They can legitimately diverge — a switch can
    /// succeed while saving the preference fails — and showing the saved value
    /// as "Active" would then misdescribe what is actually running.
    /// Localized name of the engine currently running.
    ///
    /// Not `.capitalized` on the wire value any more: "classic"/"daemon" are
    /// protocol identifiers, and capitalizing them produced English words in a
    /// Chinese interface.
    func coreModeLabel(_ language: Localization) -> String {
        engineLabel(core?.backend, language)
    }

    /// The saved preference, which may differ from the active engine.
    func savedCoreModeLabel(_ language: Localization) -> String {
        engineLabel(settings?.core_backend_mode, language)
    }

    /// Engine identifiers are protocol values ("classic"/"daemon"), so they are
    /// mapped to localized words rather than capitalized — capitalizing produced
    /// English UI text inside a Chinese interface.
    private func engineLabel(_ raw: String?, _ language: Localization) -> String {
        switch raw {
        case "daemon": return L.daemon.tr(language)
        default: return L.classic.tr(language)
        }
    }

    /// Whether JiejieBox may rebuild the config it is running on.
    ///
    /// Reported by the backend, which decides ownership from an explicit
    /// provenance marker rather than from whether a state file happens to
    /// exist. The frontend never inspects files itself.
    var configRebuildable: Bool { core?.config_rebuildable ?? false }

    /// Who owns the config on disk.
    ///
    /// Derived from the backend's explicit `config_ownership`, NOT from
    /// `configRebuildable`. Those are different questions, and treating
    /// "cannot rebuild" as "somebody else owns it" is what made a config written
    /// by an older JiejieBox — which has no provenance marker — get reported to
    /// its owner as a foreign file.
    ///
    /// An absent field (older backend) reads as `.unknown`: the honest answer
    /// when the backend did not say, and one that cannot libel the user's file.
    var configOwnership: ConfigOwnership {
        guard let raw = core?.config_ownership else { return .unknown }
        return ConfigOwnership(rawValue: raw) ?? .unknown
    }

    /// True when the saved preference and the running engine disagree, which is
    /// what happens when a switch succeeded but persisting it did not. Surfaced
    /// rather than hidden: the next launch uses the saved value, so the user
    /// should know the preference did not stick.
    var coreModePreferenceDiverged: Bool {
        guard settings != nil, core != nil else { return false }
        return coreModeLabel(resolvedLanguage) != savedCoreModeLabel(resolvedLanguage)
    }

    /// True once the handshake succeeded and the backend is answering.
    var isReady: Bool { connection == .ready }

    // MARK: - Core operation policy
    //
    // One shared notion of "the core is in the middle of something", so Start,
    // Stop, Restart and the engine switch cannot overlap. Deriving it per view
    // is how a Stop and a mode switch end up racing each other.

    /// The core is in a settled state where a new operation may begin.
    ///
    /// `starting` and `stopping` are transitions, not states: acting during them
    /// means issuing a command against a core that is already moving, so they
    /// count as busy.
    var coreIsTransitioning: Bool { core?.state.isTransitioning ?? false }

    /// True while a proxy switch is in flight, for any node.
    ///
    /// Switches are serialised: two concurrent `switch_proxy` calls would race
    /// for the same selection, so no row offers a switch while one is running.
    var proxySwitchInFlight: Bool {
        guard let pending else { return false }
        if case .switchingProxy = pending { return true }
        return false
    }

    /// True while a SINGLE node measurement is in flight, for any node.
    ///
    /// The counterpart to `proxySwitchInFlight`, and it exists because the UI and
    /// the model disagreed about it: `withPending` admits ONE operation at a time,
    /// so a second single-node test is refused — but the row's own disable rule
    /// did not mention this state at all, leaving every other node's Test control
    /// looking live. Clicking one produced "another operation is running", which
    /// on that screen was not even visible.
    ///
    /// The product rule is serialization (measurements share the core's delay
    /// endpoint and the same measurement table), so the rule is named here ONCE
    /// and both the guard and every control read it. When measurement becomes
    /// genuinely concurrent, this is the single place that changes.
    var proxySingleTestInFlight: Bool {
        guard let pending else { return false }
        if case .testingProxy = pending { return true }
        return false
    }

    /// Live "Test All" state. Distinct from `pending`, which covers one-shot
    /// actions: a group test streams progress and must survive its own request
    /// being outstanding.
    private(set) var groupTest: ProxyGroupTestState = .idle

    /// Per-action engine capabilities, refreshed with every proxy list.
    private(set) var proxyActions: ProxyActionCapabilities = .all

    /// Legacy accessor kept so existing call sites keep working.
    var proxyGroupTestInFlight: Bool { groupTest.isRunning }

    /// True while any core-affecting command is in flight.
    var coreOperationBusy: Bool {
        if pending != nil { return true }
        if coreIsTransitioning { return true }
        return false
    }

    /// The engine may only be switched with the core fully stopped.
    ///
    /// Not "not running": a `starting` core is about to be running, and a
    /// `stopping` one has not released the process yet. A daemon engine cannot
    /// take over a live classic process, and vice versa, so anything other than
    /// a settled `stopped` state must refuse.
    var canSwitchCoreMode: Bool {
        guard isReady else { return false }
        guard let state = core?.state else { return false }
        if coreOperationBusy { return false }
        return state == .stopped
    }

    /// The engine actually in use, as the protocol identifier ("classic" or
    /// "daemon").
    ///
    /// Exposed so views can compare against a STABLE value. Comparing rendered
    /// labels would work in English and silently break in every other language.
    var activeEngine: String { core?.backend ?? "classic" }

    /// Why the engine cannot be switched, for an inline explanation.
    func coreModeBlockedReason(_ language: Localization) -> String? {
        guard let state = core?.state else { return nil }
        switch state {
        case .stopped:
            return nil
        case .running:
            return L.stopVPNFromHome.tr(language)
        case .starting:
            return L.coreStartingWait.tr(language)
        case .stopping:
            return L.coreStoppingWait.tr(language)
        case .error:
            return L.coreErrorRestart.tr(language)
        }
    }

    /// Whether the daemon engine is offered by this build.
    var daemonAvailable: Bool { handshake?.capabilities.daemon ?? false }

    /// Whether this backend can install a user-selected core binary.
    ///
    /// Asked of the backend rather than assumed from the platform: the swap is
    /// implemented only where the backend can do it, and a version row that
    /// offers an action the backend will refuse is worse than one that stays
    /// plain text.
    var coreImportAvailable: Bool { handshake?.capabilities.canImportCore ?? false }

    /// Whether this backend can import a subscription from a local file.
    var localSubscriptionImportAvailable: Bool {
        handshake?.capabilities.canImportLocalSubscription ?? false
    }

    // MARK: - Desktop actions (NSWorkspace)
    //
    // The backend reports paths; opening them is a frontend responsibility.

    func revealConfig() {
        guard let path = settings?.config_path, !path.isEmpty else { return }
        NSWorkspace.shared.selectFile(path, inFileViewerRootedAtPath: "")
    }

    func revealConfigFolder() {
        guard let dir = settings?.data_dir, !dir.isEmpty else { return }
        NSWorkspace.shared.open(URL(fileURLWithPath: dir))
    }

    /// Alias kept for view readability; same action as `revealConfigFolder`.
    func openConfigFolder() { revealConfigFolder() }

    func openLogs() {
        guard let dir = settings?.logs_dir, !dir.isEmpty else { return }
        NSWorkspace.shared.open(URL(fileURLWithPath: dir))
    }

    /// Convenience for the primary button.
    ///
    /// Guarded against a click while a command is already in flight: the button
    /// is disabled in that window, and this is the second line of defence.
    func toggleCore() async {
        guard !coreOperationBusy else {
            lastError = L.waitForCoreOperation.tr(resolvedLanguage)
            return
        }
        guard let state = core?.state else { return }
        switch state {
        case .running, .starting:
            await stopCore()
        default:
            await startCore()
        }
    }

    // MARK: - Applying backend state

    private func apply(_ snapshot: AppSnapshot) {
        // A snapshot from a DIFFERENT backend session re-baselines everything.
        //
        // This is the fix for the restarted-helper blackout. `appliedSeq` is a
        // high-water mark for one process's counter; when a new helper starts,
        // its counter is back at 1, and comparing 1 against the old process's
        // mark of (say) 137 discards the new helper's first 137 events.
        //
        // Resetting here rather than only in `start()` also covers the paths
        // that never call `stop()` first: a crash the supervisor recovers from,
        // a manual restart of the helper, and the initial connect.
        if appliedSession != snapshot.session {
            appliedSession = snapshot.session
            appliedSeq = 0
            // A new session invalidates any events buffered against the old
            // one: they describe a process that has been replaced.
            pendingEvents.removeAll()

            // A lifecycle operation belongs to the process that accepted it.
            // Once that process is gone its event can never arrive, so an
            // operation still holding the UI would hold it forever — and the
            // timeout's reconciliation would keep asking a backend that has
            // already been replaced. The operation is abandoned HERE, at the
            // one moment we learn the old process no longer exists, rather than
            // left to expire.
            //
            // Abandoned is not the same as succeeded: `core` is replaced by the
            // new snapshot immediately below, and any error of the old
            // operation was already reported when it happened.
            abandonCoreOperation()
        }

        handshake = snapshot.handshake
        core = snapshot.core
        settings = snapshot.settings
        proxySummary = snapshot.proxy
        // The snapshot reports the sequence it was taken at, so adopting it
        // raises the mark to that point. Events at or below it are already
        // reflected in the fields above.
        appliedSeq = max(appliedSeq, snapshot.snapshot_seq)
    }

    /// Establishes the baseline from a snapshot, then drains the events that
    /// raced it.
    ///
    /// The order is the whole point: the snapshot's fields are installed FIRST,
    /// then the buffered events are filtered against its sequence and replayed.
    /// Applying a buffered event before the snapshot is what allowed a stale
    /// snapshot to overwrite a newer event.
    private func applyBaseline(_ snapshot: AppSnapshot) {
        // Buffer anything that arrives between here and the drain below.
        awaitingBaseline = true
        apply(snapshot)
        let buffered = pendingEvents
        pendingEvents.removeAll()
        awaitingBaseline = false

        // Replay in sequence order. The buffer is append-ordered, and events are
        // delivered in order by the client, but sorting makes the replay
        // independent of that assumption rather than relying on it.
        for event in buffered.sorted(by: { $0.seq < $1.seq }) {
            apply(event)
        }
        // The replay may have asked for reloads; run them once, coalesced.
        drainPendingReloads()
    }

    /// Applies one event. DELIBERATELY NOT `async`.
    ///
    /// A non-async reducer cannot await a network round-trip, which is what keeps the event
    /// stream moving. Anything that needs a request is recorded in `pendingReloads` and
    /// performed by `drainPendingReloads()`.
    private func apply(_ event: BackendEvent) {
        // An event from a session we are not following is DROPPED, not merged.
        //
        // This is the other half of the session rule. A late frame from a
        // backend that has already been replaced carries sequence numbers from
        // that dead process's counter; comparing them against the current
        // session's mark would let a dead helper's view of the world overwrite
        // the live one — and because the numbers look plausible, nothing would
        // catch it. `appliedSession == nil` means no baseline yet, and an event
        // before the baseline is exactly the case the sequence cannot resolve.
        guard let session = appliedSession, event.session == session else {
            // No baseline yet: hold the event rather than applying it against a
            // baseline that does not exist. Applying it now would let the
            // snapshot that is still in flight — composed at an EARLIER
            // sequence — overwrite it, and the event would never come again.
            if awaitingBaseline && appliedSession == nil {
                pendingEvents.append(event)
            }
            return
        }
        // Drop anything the snapshot already covers.
        guard event.seq > appliedSeq else { return }
        appliedSeq = event.seq

        switch event.event {
        case BackendEventName.coreStateChanged:
            if let status = event.decode(CoreStatus.self) {
                let wasRunning = core?.state == .running
                core = status
                // A stopped core has no speed; keeping the last sample would
                // show traffic that is not flowing.
                if status.state != .running { traffic = nil }

                // The proxy list only exists while the core is up. Without this
                // the Proxies screen would keep saying "start the core" after
                // the user did exactly that, because nothing else reloads it.
                //
                // Gated on capability: on an engine that cannot list proxies
                // this request is guaranteed to fail, and firing it anyway
                // produced a failure on every core start — noise that looked
                // like a defect in the app rather than a limit of the engine.
                if status.state == .running && !wasRunning && proxiesSupported {
                    requestReload(.groups)
                }
            }
        case BackendEventName.trafficRate:
            if let rate = event.decode(TrafficRate.self) {
                traffic = rate
            }
        case BackendEventName.settingsChanged:
            if let settings = event.decode(SettingsState.self) {
                self.settings = settings
            }
        case BackendEventName.proxyTestProgress:
            // Streamed deltas from a running group test. Applied without a
            // reload: the whole point is that a row updates the moment its
            // result arrives, not after all N nodes finish.
            if let p = event.decode(ProxyTestProgress.self) {
                applyGroupTestProgress(p)
            }
        case BackendEventName.proxiesChanged:
            // The backend rebuilt the config or refreshed subscriptions, so the
            // node list we hold is stale. Only reload once the Proxy screen has
            // been opened, so a background update does not pay for it.
            //
            // GROUPS FIRST. A rebuild can rename or remove selector groups, and
            // loading nodes for a group that no longer exists fails — leaving
            // the stale name in place and the list empty. loadGroups() re-picks
            // a valid group, then loads its nodes.
            if !selectedGroup.isEmpty || !groups.isEmpty {
                requestReload(.groups)
            }
        case BackendEventName.proxySelectionChanged:
            if !selectedGroup.isEmpty {
                requestReload(.proxies(group: selectedGroup))
            }
        case BackendEventName.subscriptionsChanged:
            // Sources were edited, or a refresh changed their node counts and
            // status. Re-read so the Subscriptions screen and Home's count do
            // not keep showing a stale list. The query emits nothing, so this
            // cannot feed back on itself.
            requestReload(.subscriptions)
        case BackendEventName.daemonChanged:
            // The daemon's setup state changed underneath us (installed,
            // started, paired, removed). Without this the Daemon screen would
            // keep showing the state it loaded on entry.
            //
            // Whether a prepared command survives is decided by
            // `applyDaemonStatus`, from the state it was generated for — NOT
            // here, and not from a constant. The old rule ("if the daemon is
            // ready, drop any command") destroyed the commands for Re-pair and
            // Remove Service on creation, because both are generated precisely
            // while the daemon IS ready and stay valid until the user runs them.
            requestReload(.daemonStatus)
        case BackendEventName.shuttingDown:
            // The backend announced it is exiting. Handled rather than
            // ignored so the event is not dead protocol: it is how we learn
            // that a backend we did NOT ask to stop is going away anyway (the
            // EOF path, or a quit initiated from elsewhere). Recording intent
            // here keeps the process-exit callback from reporting a crash for
            // a shutdown the backend announced in advance.
            if !isQuitting {
                lastError = L.backendShuttingDown.tr(resolvedLanguage)
            }
        default:
            break
        }
    }

    /// Drops any queued or running deferred reloads.
    ///
    /// Called from every teardown path, so the deferred work is bound to the session that
    /// requested it. Leaving it would let a queued batch survive a stop/restart boundary and
    /// run against a different helper — the same "apply a dead process's view of the world"
    /// problem the buffered events are cleared to avoid.
    private func cancelPendingReloads() {
        reloadTask?.cancel()
        reloadTask = nil
        pendingReloads.removeAll()
    }

    /// Records that a resource needs reloading, and starts the drain if idle.
    ///
    /// A SET, so repeated events collapse into one reload. Ten subscription changes during a
    /// refresh are one fetch of the list, not ten.
    private func requestReload(_ what: PendingReload) {
        pendingReloads.insert(what)
        drainPendingReloads()
    }

    /// Performs the pending reloads on a task, so the caller never waits.
    ///
    /// One task at a time: overlapping reloads of the same resource can land out of order,
    /// and the older response would overwrite the newer one.
    private func drainPendingReloads() {
        guard reloadTask == nil, !pendingReloads.isEmpty else { return }
        reloadTask = Task<Void, Never> { [weak self] in
            await self?.runPendingReloads()
        }
    }

    /// Runs the coalesced reloads, then clears the task so the next one can start.
    ///
    /// The set is copied and cleared BEFORE the requests run, so work requested while they
    /// are in flight is not lost — it lands in a fresh set and is picked up by the next
    /// drain rather than being silently dropped by a clear at the end.
    private func runPendingReloads() async {
        while !pendingReloads.isEmpty {
            // A stop() during a reload cancels the task; the remaining requests belong to a
            // session that no longer exists.
            if Task.isCancelled {
                reloadTask = nil
                return
            }
            let batch = pendingReloads
            pendingReloads.removeAll()

            // GROUPS BEFORE PROXIES. A rebuild can rename or remove selector groups, and
            // loading nodes for a group that no longer exists fails, leaving the stale name
            // in place and the list empty.
            if batch.contains(.groups) {
                await loadGroups()
                // RE-CHECKED AFTER EVERY AWAIT, NOT ONLY AT THE TOP OF THE LOOP.
                //
                // `stop()` cancels this task while it is suspended in `await`, and the top of
                // the loop is not reached again until the whole batch has run. So a stop()
                // arriving during `loadGroups()` still let `.proxies`, `.subscriptions` and
                // `.daemonStatus` fire — every request in the batch except the one that
                // happened to be suspended — against a backend the user has just stopped.
                //
                // The requests are cheap and mostly harmless individually, which is why this
                // went unnoticed; what makes it wrong is that they belong to a session that no
                // longer exists, and a later one of them can land AFTER a newer session has
                // already loaded, overwriting fresh state with the old session's.
                if Task.isCancelled {
                    reloadTask = nil
                    return
                }
            }
            for case let .proxies(group) in batch {
                // Skip a group selection that a later event has already replaced: only the
                // most recent request is worth a round-trip.
                if group == selectedGroup {
                    await loadProxies(group: group)
                    if Task.isCancelled {
                        reloadTask = nil
                        return
                    }
                }
            }
            if batch.contains(.subscriptions) {
                await loadSubscriptions()
                if Task.isCancelled {
                    reloadTask = nil
                    return
                }
            }
            if batch.contains(.daemonStatus) {
                await loadDaemonStatus()
                if Task.isCancelled {
                    reloadTask = nil
                    return
                }
            }
        }
        reloadTask = nil

        // A request that arrived between the loop's last check and the task ending would
        // otherwise sit in the set with nothing to run it. Guarded on cancellation: after a
        // stop() the set is empty by construction, but a request must never re-arm a drain
        // for a session that is gone.
        if !pendingReloads.isEmpty && !Task.isCancelled {
            drainPendingReloads()
        }
    }
}
