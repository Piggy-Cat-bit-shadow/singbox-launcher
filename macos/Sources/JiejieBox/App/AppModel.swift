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
        case coreMode
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

    func stop() async {
        eventTask?.cancel()
        eventTask = nil
        try? await client.requestShutdown()
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

    private func run(_ body: @escaping () async throws -> Void) async {
        do {
            try await body()
        } catch {
            lastError = error.localizedDescription
        }
    }

    func clearError() { lastError = nil }

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
            if let payload = event.payload {
                core = payload
            }
        default:
            break
        }
    }
}
