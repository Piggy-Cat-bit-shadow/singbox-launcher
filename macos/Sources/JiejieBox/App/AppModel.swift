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

    /// Why the node list is not usable, if it is not.
    ///
    /// One explicit state rather than a scatter of booleans, because the
    /// screen has to tell apart causes that look identical in data but need
    /// completely different messages and actions: a stopped core, an
    /// unreachable backend, an empty config, and a config that is merely out of
    /// date. Collapsing them produced a single "No nodes in this group" for
    /// every case, which is wrong for most of them.
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
            showTransient(enabled ? "Launch at Login enabled." : "Launch at Login disabled.")
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
        var label: String { rawValue.capitalized }

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
    private var appliedSeq: Int64 = 0
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
            let stream = await client.events()
            eventTask = Task<Void, Never> { [weak self] in
                for await event in stream {
                    await self?.apply(event)
                }
            }

            let snapshot = try await client.snapshot()
            apply(snapshot)

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
        await withPending(.startingCore, success: nil) {
            try await self.client.startCore()
        }
    }

    /// Stop the core. Same reasoning as `startCore`.
    func stopCore() async {
        await withPending(.stoppingCore, success: nil) {
            try await self.client.stopCore()
        }
    }

    func restartCore() async {
        await withPending(.restarting, success: "Core restarting…") {
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
            // Recorded separately from `lastError` so the Proxies screen can
            // explain its own failure inline instead of only raising a banner
            // on Home. Both are set: the banner is the cross-screen signal.
            proxyError = error.localizedDescription
            lastError = error.localizedDescription
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

            // Prefer the config's default group, then the first one, then
            // wherever we already were — but only keep a remembered group if
            // the config still defines it. Pointing at a group that no longer
            // exists made every later load fail with "group not found".
            if selectedGroup.isEmpty
                || !list.groups.contains(where: { $0.name == selectedGroup }) {
                selectedGroup = list.group ?? list.groups.first?.name ?? ""
            }
        } catch {
            proxyError = error.localizedDescription
            lastError = error.localizedDescription
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
            lastError = error.localizedDescription
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
    func testProxy(_ node: ProxyNode) async {
        await withPending(.testingProxy(node.name), success: nil) {
            let list = try await self.client.testProxy(group: node.group, name: node.name)
            self.apply(list)
            if let refreshed = list.proxies.first(where: { $0.name == node.name }),
               !refreshed.isMeasured {
                self.lastError = "\(node.label) did not respond."
            }
        }
    }

    /// Measure every node in the current group. Sequential in the backend to
    /// avoid starving live traffic, so this can take a while.
    func testGroup() async {
        guard !selectedGroup.isEmpty else { return }
        await withPending(.testingGroup, success: nil) {
            let list = try await self.client.testProxyGroup(self.selectedGroup)
            self.apply(list)
        }
    }

    private func apply(_ list: ProxyList) {
        if !list.groups.isEmpty { groups = list.groups }
        // A group-only reply must not wipe the visible nodes.
        if list.available || !list.proxies.isEmpty || !(list.group ?? "").isEmpty {
            if !list.proxies.isEmpty || (list.group ?? "") == selectedGroup {
                proxies = list.proxies
            }
        }
        if let g = list.group, !g.isEmpty { selectedGroup = g }
        proxiesAvailable = list.available
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
            self.showTransient("Subscription added.")
        }
        if added { await loadSubscriptions() }
        return added
    }

    func updateSubscription(id: String, name: String, url: String) async -> Bool {
        var saved = false
        await withPending(.savingSubscription, success: nil) {
            _ = try await self.client.updateSubscription(id: id, name: name, url: url)
            saved = true
            self.showTransient("Subscription saved.")
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
            self.showTransient("Subscription removed.")
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
                self.showTransient("Daemon paired.")
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
            self.showTransient("Pairing removed.")
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
            lastError = "Finish the daemon setup before switching to it."
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
    private func withPending(_ op: PendingOperation,
                             success: String?,
                             _ body: @escaping () async throws -> Void) async {
        // Overlap is prevented in the UI by disabling controls while an
        // operation is in flight; this guard is the second line of defence
        // against a rapid double click that slips through. It must never be a
        // SILENT no-op: a click that produces nothing is indistinguishable from
        // a broken button, so the reason is surfaced.
        guard pending == nil else {
            lastError = "Another operation is still running. Wait for it to finish."
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
    var coreModeLabel: String {
        (core?.backend ?? "classic").capitalized
    }

    /// The saved preference, which may differ from the active engine.
    var savedCoreModeLabel: String {
        (settings?.core_backend_mode ?? "classic").capitalized
    }

    /// True when the saved preference and the running engine disagree, which is
    /// what happens when a switch succeeded but persisting it did not. Surfaced
    /// rather than hidden: the next launch uses the saved value, so the user
    /// should know the preference did not stick.
    var coreModePreferenceDiverged: Bool {
        guard settings != nil, core != nil else { return false }
        return coreModeLabel != savedCoreModeLabel
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
    var proxyGroupTestInFlight: Bool { pending == .testingGroup }

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

    /// Why the engine cannot be switched, for an inline explanation.
    var coreModeBlockedReason: String? {
        guard let state = core?.state else { return nil }
        switch state {
        case .stopped:
            return nil
        case .running:
            return "Stop the VPN before switching engines."
        case .starting:
            return "The core is starting. Wait for it to settle."
        case .stopping:
            return "The core is stopping. Wait for it to settle."
        case .error:
            return "The core is in an error state. Restart it before switching."
        }
    }

    /// Whether the daemon engine is offered by this build.
    var daemonAvailable: Bool { handshake?.capabilities.daemon ?? false }

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
            lastError = "Wait for the current core operation to finish."
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
        handshake = snapshot.handshake
        core = snapshot.core
        settings = snapshot.settings
        proxySummary = snapshot.proxy
        appliedSeq = max(appliedSeq, snapshot.snapshot_seq)
    }

    private func apply(_ event: BackendEvent) async {
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
                if status.state == .running && !wasRunning {
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
                lastError = "The backend is shutting down."
            }
        default:
            break
        }
    }
}
