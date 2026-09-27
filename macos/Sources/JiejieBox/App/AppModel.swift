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
        case restarting
        case reloadingConfig
        case updatingSubscriptions
        case updatingSetting
        case switchingProxy(String)
        case testingProxy(String)
        case testingGroup
    }

    private(set) var pending: PendingOperation?
    /// Transient success line, cleared by the view after a moment.
    var transientStatus: String?

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

    /// Free-text filter over the node list. Purely a view concern, kept here
    /// because two views (the list and its empty state) must agree on it.
    var proxySearch: String = ""

    /// Latest speed sample, nil until the backend sends one. Cleared when the
    /// core stops so a stale rate is never shown as live.
    private(set) var traffic: TrafficRate?

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

    func setLaunchAtLogin(_ enabled: Bool) {
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
        } catch {
            // fall through: re-read the real state below
        }
        launchAtLogin = SMAppService.mainApp.status == .enabled
    }

    enum Screen: Hashable {
        case coreDetails
        case coreMode
        case proxies
        case more
        case about
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

    // MARK: - Lifecycle

    /// Start the backend, handshake, take a snapshot and begin consuming events.
    func start() async {
        guard connection != .ready else { return }
        connection = .connecting
        lastError = nil

        do {
            try await client.start { [weak self] code in
                Task { @MainActor in
                    self?.connection = .failed("The backend stopped unexpectedly (code \(code)).")
                }
            }

            handshake = try await client.handshake()

            // Subscribe before the snapshot: an event that races the snapshot
            // is filtered by sequence number rather than lost.
            let stream = await client.events()
            eventTask = Task { [weak self] in
                for await event in stream {
                    await self?.apply(event)
                }
            }

            let snapshot = try await client.snapshot()
            apply(snapshot)

            connection = .ready
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
        eventTask?.cancel()
        eventTask = nil
        await client.shutdownGracefully()
        connection = .idle
    }

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

    func startCore() async { await run { try await self.client.startCore() } }
    func stopCore() async { await run { try await self.client.stopCore() } }

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
        await withPending(.updatingSetting, success: nil) {
            self.settings = try await self.client.setAutoPing(enabled)
        }
    }

    func setAutoUpdateSubscriptions(_ enabled: Bool) async {
        await withPending(.updatingSetting, success: nil) {
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
        defer { proxiesLoading = false }
        do {
            let target = group ?? selectedGroup
            let list = try await client.proxies(group: target)
            apply(list)
        } catch {
            lastError = error.localizedDescription
        }
    }

    /// Load the group list (used when the Proxy screen first appears).
    func loadGroups() async {
        proxiesLoading = true
        defer { proxiesLoading = false }
        do {
            let list = try await client.proxyGroups()
            groups = list.groups
            proxiesAvailable = list.available
            // Prefer the config's default group, then the first one, then
            // wherever we already were.
            if selectedGroup.isEmpty || !list.groups.contains(where: { $0.name == selectedGroup }) {
                selectedGroup = list.group ?? list.groups.first?.name ?? ""
            }
            if !selectedGroup.isEmpty {
                let nodes = try await client.proxies(group: selectedGroup)
                apply(nodes)
            }
        } catch {
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
            transientStatus = result.message.isEmpty ? success : result.message
        } else {
            lastError = result.message
        }
        if !result.core_skips.isEmpty {
            transientStatus = (transientStatus.map { $0 + " " } ?? "") + result.core_skips.joined(separator: " ")
        }
    }

    /// Run an operation with a pending marker and consistent error reporting.
    private func withPending(_ op: PendingOperation,
                             success: String?,
                             _ body: @escaping () async throws -> Void) async {
        guard pending == nil else { return }
        pending = op
        lastError = nil
        defer { pending = nil }
        do {
            try await body()
            if let success { transientStatus = success }
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

    func clearError() { lastError = nil }

    // MARK: - Derived state for the views

    var coreMissing: Bool { core?.binary_exists == false }
    var configMissing: Bool { core?.config_exists == false }

    var coreModeLabel: String {
        (settings?.core_backend_mode ?? "classic").capitalized
    }

    /// True once the handshake succeeded and the backend is answering.
    var isReady: Bool { connection == .ready }

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
    func toggleCore() async {
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
        appliedSeq = max(appliedSeq, snapshot.snapshot_seq)
    }

    private func apply(_ event: BackendEvent) async {
        // Drop anything the snapshot already covers.
        guard event.seq > appliedSeq else { return }
        appliedSeq = event.seq

        switch event.event {
        case BackendEventName.coreStateChanged:
            if let status = event.decode(CoreStatus.self) {
                core = status
                // A stopped core has no speed; keeping the last sample would
                // show traffic that is not flowing.
                if status.state != .running { traffic = nil }
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
            // The backend rebuilt the config or refreshed subscriptions, so
            // the node list we hold is stale. Reloading only when the Proxy
            // screen has ever been opened avoids paying for it on every
            // background update.
            if !selectedGroup.isEmpty {
                await loadProxies(group: selectedGroup)
                await loadGroups()
            }
        case BackendEventName.proxySelectionChanged:
            if !selectedGroup.isEmpty {
                await loadProxies(group: selectedGroup)
            }
        default:
            break
        }
    }
}
