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
                    await self?.apply(event)
                }
            }

            let snapshot = try await client.snapshot()
            // Snapshot FIRST, then the events that raced it — never the reverse.
            await applyBaseline(snapshot)

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
            connection = .idle
        } catch {
            connection = .failed(error.localizedDescription)
        }
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
        await client.shutdown()
        connection = .idle
    }

    /// Restart the backend after a crash.
    func restart() async {
        await stop()
        await start()
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

    func updateSubscription(id: String, name: String, url: String) async -> Bool {
        var saved = false
        await withPending(.savingSubscription, success: nil) {
            _ = try await self.client.updateSubscription(id: id, name: name, url: url)
            saved = true
            self.showTransient(L.subscriptionSaved.tr(self.resolvedLanguage))
        }
        if saved { await loadSubscriptions() }
        return saved
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

    func loadDaemonStatus() async {
        daemonLoading = true
        defer { daemonLoading = false }
        do {
            daemon = try await client.daemonStatus()
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Ask the backend for a setup command (install, start, repair, uninstall).
    ///
    /// This never switches engines. Setting up the service and activating it
    /// are separate steps on purpose: doing both from one click is what made
    /// selecting Daemon look like a freeze.
    func daemonSetup(_ step: DaemonSetupStep) async {
        await withPending(.configuringDaemon, success: nil) {
            switch step {
            case .install: self.daemonCommand = try await self.client.daemonInstall()
            case .start: self.daemonCommand = try await self.client.daemonStart()
            case .repair: self.daemonCommand = try await self.client.daemonRepair()
            case .uninstall: self.daemonCommand = try await self.client.daemonUninstall(purge: false)
            case .removeAll: self.daemonCommand = try await self.client.daemonUninstall(purge: true)
            }
            if let cmd = self.daemonCommand { self.daemon = cmd.status }
        }
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

    func pairDaemon(invite: String) async -> Bool {
        var paired = false
        await withPending(.pairingDaemon, success: nil) {
            self.daemon = try await self.client.pairDaemon(invite: invite)
            paired = self.daemon?.paired ?? false
            if paired {
                self.showTransient(L.daemonPaired.tr(self.resolvedLanguage))
                // The invite is spent; leaving the command on screen would
                // invite the user to paste it again, which cannot work.
                self.daemonCommand = nil
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
            lastError = error.localizedDescription
            return
        }
        // Hold the marker until a lifecycle event settles it. If no event ever
        // arrives (a lost notification, a backend that died mid-operation), the
        // deadline releases it rather than leaving the UI permanently busy.
        await waitForCoreToSettle()
    }

    /// How long a core operation may hold the UI before the marker is released.
    ///
    /// Longer than the backend's own 45s budget so the normal slow path still
    /// settles by EVENT rather than by this timeout; the value exists only to
    /// release a UI that would otherwise spin forever.
    private static let coreOperationTimeout: TimeInterval = 75

    /// Wait until the backend reports a settled core state, or the deadline passes.
    private func waitForCoreToSettle() async {
        while Date() < (coreOpDeadline ?? .distantPast) {
            if let state = core?.state, !state.isTransitioning {
                // The backend has finished transitioning: running, stopped or
                // error. Either way the operation is over and the marker goes.
                break
            }
            // Poll the LOCAL snapshot rather than the backend: the state arrives
            // through the event stream that is already running, and issuing a
            // request here would add load without adding information.
            try? await Task.sleep(nanoseconds: 150_000_000)
        }
        pending = nil
        coreOpDeadline = nil
    }

    private func withPending(_ op: PendingOperation,
                             success: String?,
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
            if let success { showTransient(success) }
        } catch {
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
    func clearDaemonCommand() { daemonCommand = nil }

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

    /// True while the whole group is being measured.
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
    private func applyBaseline(_ snapshot: AppSnapshot) async {
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
            await apply(event)
        }
    }

    private func apply(_ event: BackendEvent) async {
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
                    await loadGroups()
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
                await loadGroups()
            }
        case BackendEventName.proxySelectionChanged:
            if !selectedGroup.isEmpty {
                await loadProxies(group: selectedGroup)
            }
        case BackendEventName.subscriptionsChanged:
            // Sources were edited, or a refresh changed their node counts and
            // status. Re-read so the Subscriptions screen and Home's count do
            // not keep showing a stale list. The query emits nothing, so this
            // cannot feed back on itself.
            await loadSubscriptions()
        case BackendEventName.daemonChanged:
            // The daemon's setup state changed underneath us (installed,
            // started, paired, removed). Without this the Daemon screen would
            // keep showing the state it loaded on entry, and a command prepared
            // for a previous state would linger as if still valid — the user
            // would be told to run an invite command the service no longer
            // needs.
            await loadDaemonStatus()
            // A command is only valid for the state that produced it: once the
            // daemon reports ready, a prepared install or invite command is
            // stale and would send the user to run something already done.
            if daemon?.ready == true {
                daemonCommand = nil
            }
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
}
