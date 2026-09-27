// BackendClient — owns the Go helper process and speaks the JSON IPC protocol.
//
// Responsibilities, deliberately kept in one place so no view touches Process
// or FileHandle:
//
//   - launch the bundled helper and keep its stdin/stdout
//   - newline-delimited JSON framing in both directions
//   - request id allocation and correlation with continuations
//   - decode unsolicited events into an AsyncStream
//   - surface backend death so the UI can offer a restart
//
// Threading: this is an actor. Views run on @MainActor and never block on IO.

import Foundation
import os

/// Errors the frontend can show when the backend misbehaves.
enum BackendClientError: LocalizedError {
    case helperMissing
    case launchFailed(String)
    case protocolMismatch(expected: Int, got: Int)
    case backendUnavailable
    case decodingFailed(String)
    case notRunning
    case timedOut(method: String)

    var errorDescription: String? {
        switch self {
        case .helperMissing:
            return "The bundled backend helper is missing from the app."
        case .launchFailed(let detail):
            return "The backend could not be started: \(detail)"
        case .protocolMismatch(let expected, let got):
            return "Backend protocol \(got) does not match the app's \(expected). Update JiejieBox."
        case .backendUnavailable:
            return "The backend is not running."
        case .decodingFailed(let detail):
            return "Unexpected backend response: \(detail)"
        case .notRunning:
            return "The backend is not running."
        case .timedOut(let method):
            return "Operation timed out (\(method))."
        }
    }
}

/// A response envelope parameterised by its result type.
///
/// Declared at file scope: Swift does not allow a generic type nested inside a
/// generic function.
struct ResponseEnvelope<T: Decodable>: Decodable {
    let result: T?
    let error: BackendError?
}

/// One decoded line arriving from the backend.
private enum Incoming {
    case response(BackendResponse, raw: Data)
    case event(BackendEvent)
}

actor BackendClient {
    private let log = Logger(subsystem: "com.piggycat.jiejiebox", category: "backend")

    private var process: Process?
    private var stdinHandle: FileHandle?
    private var readTask: Task<Void, Never>?

    /// Pending requests keyed by id, each awaiting one response line.
    private var pending: [String: CheckedContinuation<Data, Error>] = [:]
    private var nextRequestID = 0

    /// Event stream plumbing.
    private var eventContinuations: [UUID: AsyncStream<BackendEvent>.Continuation] = [:]

    /// Called when the process exits unexpectedly.
    private var onTermination: (@Sendable (Int32) -> Void)?

    // MARK: - Lifecycle

    /// Locate the helper inside the running bundle.
    ///
    /// Uses Bundle APIs rather than a hardcoded path, so the app works from any
    /// install location and from a CI staging directory.
    nonisolated static func helperURL() -> URL? {
        let bundle = Bundle.main
        let helper = bundle.builtInPlugInsURL?
            .deletingLastPathComponent()
            .appendingPathComponent("Helpers/jiejiebox-backend")
        if let helper, FileManager.default.isExecutableFile(atPath: helper.path) {
            return helper
        }
        // Fallback for a SwiftPM run (no .app yet): look next to the binary.
        let beside = bundle.bundleURL
            .deletingLastPathComponent()
            .appendingPathComponent("jiejiebox-backend")
        if FileManager.default.isExecutableFile(atPath: beside.path) {
            return beside
        }
        return nil
    }

    /// Launch the helper and start reading its output.
    func start(onTermination: @escaping @Sendable (Int32) -> Void) throws {
        guard process == nil else { return }
        self.onTermination = onTermination

        guard let helper = Self.helperURL() else {
            throw BackendClientError.helperMissing
        }

        let proc = Process()
        proc.executableURL = helper
        let inPipe = Pipe()
        let outPipe = Pipe()
        let errPipe = Pipe()
        proc.standardInput = inPipe
        proc.standardOutput = outPipe
        // stderr carries logs only; the protocol never uses it.
        proc.standardError = errPipe

        proc.terminationHandler = { [weak self] finished in
            let code = finished.terminationStatus
            Task { await self?.handleTermination(code) }
        }

        do {
            try proc.run()
        } catch {
            throw BackendClientError.launchFailed(error.localizedDescription)
        }

        process = proc
        stdinHandle = inPipe.fileHandleForWriting
        log.info("backend started pid=\(proc.processIdentifier)")

        // Drain stderr so a chatty backend cannot block on a full pipe.
        let errHandle = errPipe.fileHandleForReading
        Task.detached {
            while let line = try? errHandle.readline() {
                if line.isEmpty { break }
                Logger(subsystem: "com.piggycat.jiejiebox", category: "backend")
                    .debug("\(line, privacy: .public)")
            }
        }

        startReading(outPipe.fileHandleForReading)
    }

    private func startReading(_ handle: FileHandle) {
        readTask = Task.detached { [weak self] in
            while let line = try? handle.readline() {
                guard let self else { return }
                await self.handleLine(line)
            }
            await self?.handleReadEnded()
        }
    }

    /// Ask the backend to exit, then wait for it to do so.
    ///
    /// Distinct from `shutdown()`: this sends the `shutdown` command and gives
    /// the backend time to run its own graceful exit — which is where the
    /// core stop/keep decision lives. Killing the helper immediately after the
    /// command would truncate that teardown and could leave the core in an
    /// inconsistent state.
    func shutdownGracefully() async {
        // Best effort: the backend may already be gone.
        try? await requestShutdown()

        guard let proc = process else {
            await cleanupAfterExit()
            return
        }

        // Closing stdin is the backend's second signal: its read loop ends and
        // it runs the same graceful path.
        if let stdinHandle {
            try? stdinHandle.close()
            self.stdinHandle = nil
        }

        // Wait for the process to leave on its own before forcing anything.
        for _ in 0..<50 where proc.isRunning {   // up to ~5 s
            try? await Task.sleep(nanoseconds: 100_000_000)
        }
        if proc.isRunning {
            log.warning("backend did not exit after shutdown; terminating")
            proc.terminate()
        }
        await cleanupAfterExit()
    }

    private func cleanupAfterExit() async {
        readTask?.cancel()
        readTask = nil
        process = nil
        log.info("backend stopped")
    }

    /// Stop the helper without asking the backend to exit (used when the
    /// connection is being torn down for a restart).
    func shutdown() async {
        readTask?.cancel()
        readTask = nil

        if let stdinHandle {
            try? stdinHandle.close()
        }
        guard let proc = process, proc.isRunning else {
            process = nil
            return
        }
        proc.terminate()
        // Give it a moment to exit cleanly, then stop waiting.
        for _ in 0..<20 where proc.isRunning {
            try? await Task.sleep(nanoseconds: 100_000_000)
        }
        if proc.isRunning { proc.interrupt() }
        log.info("backend stopped")
        process = nil
    }

    private func handleTermination(_ code: Int32) {
        log.warning("backend exited with code \(code)")
        // Fail every in-flight request so no caller awaits forever.
        let waiting = pending
        pending.removeAll()
        for (_, cont) in waiting {
            cont.resume(throwing: BackendClientError.backendUnavailable)
        }
        for (_, cont) in eventContinuations { cont.finish() }
        eventContinuations.removeAll()
        process = nil
        onTermination?(code)
    }

    private func handleReadEnded() {
        guard process == nil else { return }
        handleTermination(0)
    }

    // MARK: - Framing

    private func handleLine(_ data: Data) async {
        guard !data.isEmpty else { return }

        // An event carries "event"; a response carries "id". Distinguishing on
        // the envelope rather than guessing keeps decoding unambiguous.
        if let probe = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           probe["event"] != nil {
            if let event = try? JSONDecoder().decode(BackendEvent.self, from: data) {
                for (_, cont) in eventContinuations { cont.yield(event) }
            }
            return
        }

        guard let envelope = try? JSONDecoder().decode(BackendResponse.self, from: data) else {
            log.error("undecodable backend line")
            return
        }
        guard let cont = pending.removeValue(forKey: envelope.id) else {
            log.debug("response for unknown request id")
            return
        }
        cont.resume(returning: data)
    }

    private func write(_ request: BackendRequest) throws {
        guard let stdinHandle else { throw BackendClientError.backendUnavailable }
        var payload = try JSONEncoder().encode(request)
        payload.append(0x0A) // newline-delimited framing
        try stdinHandle.write(contentsOf: payload)
    }

    // MARK: - Requests

    /// Send a request and decode its result.
    func request<T: Decodable>(_ method: String, as type: T.Type) async throws -> T {
        let data = try await rawRequest(method)

        // A response is either {"result": ...} or {"error": ...}.
        let envelope = try JSONDecoder().decode(ResponseEnvelope<T>.self, from: data)
        if let error = envelope.error { throw error }
        guard let result = envelope.result else {
            throw BackendClientError.decodingFailed("neither result nor error in response")
        }
        return result
    }

    /// Send a request that returns no useful payload.
    func call(_ method: String, params: [String: JSONValue]? = nil) async throws {
        _ = try await rawRequest(method, params: params)
    }

    /// Send a request and decode its result, with parameters.
    func request<T: Decodable>(_ method: String, params: [String: JSONValue], as type: T.Type) async throws -> T {
        let data = try await rawRequest(method, params: params)
        let envelope = try JSONDecoder().decode(ResponseEnvelope<T>.self, from: data)
        if let error = envelope.error { throw error }
        guard let result = envelope.result else {
            throw BackendClientError.decodingFailed("neither result nor error in response")
        }
        return result
    }

    private func rawRequest(_ method: String, params: [String: JSONValue]? = nil) async throws -> Data {
        nextRequestID += 1
        let id = String(nextRequestID)
        let request = BackendRequest(id: id, method: method, params: params)

        // Bound every request. A response that never arrives would otherwise
        // leave its continuation pending forever, which surfaces as a button
        // stuck on "Switching…" or "Updating…" with no way out — the single
        // worst failure mode for a menu bar, because the panel is the only UI.
        let timeout = Self.timeout(for: method)

        return try await withThrowingTaskGroup(of: Data.self) { group in
            group.addTask { [weak self] in
                guard let self else { throw BackendClientError.notRunning }
                return try await self.awaitResponse(id: id, request: request)
            }
            group.addTask {
                try await Task.sleep(nanoseconds: timeout)
                throw BackendClientError.timedOut(method: method)
            }
            defer { group.cancelAll() }
            guard let first = try await group.next() else {
                throw BackendClientError.timedOut(method: method)
            }
            return first
        }
    }

    /// Wait for the response with a given id.
    private func awaitResponse(id: String, request: BackendRequest) async throws -> Data {
        try await withCheckedThrowingContinuation { cont in
            pending[id] = cont
            do {
                try write(request)
            } catch {
                pending.removeValue(forKey: id)
                cont.resume(throwing: error)
            }
        }
    }

    /// Timeout budget per method, in nanoseconds.
    ///
    /// Split by what the operation actually does rather than one global value:
    /// a state read that takes two seconds is broken, while a subscription
    /// fetch that takes two seconds is normal. A single number would either
    /// abort healthy network work or let a wedged query hang the panel.
    static func timeout(for method: String) -> UInt64 {
        let seconds: Double
        switch method {
        case BackendMethod.handshake, BackendMethod.getAppSnapshot,
             BackendMethod.getProxyGroups, BackendMethod.getProxies:
            seconds = 8
        case BackendMethod.switchProxy, BackendMethod.setCoreMode,
             BackendMethod.restartCore, BackendMethod.startCore,
             BackendMethod.stopCore:
            seconds = 20
        case BackendMethod.testProxy, BackendMethod.testProxyGroup,
             BackendMethod.refreshSubscription, BackendMethod.updateSubscriptions,
             BackendMethod.reloadConfig:
            // Network work: fetching providers and measuring latency. Generous,
            // because aborting a legitimate slow fetch is worse than waiting.
            seconds = 120
        case BackendMethod.getDaemonStatus:
            // Talks to the control plane and probes the core binary; the
            // earlier freeze was exactly this call never returning.
            seconds = 15
        case BackendMethod.daemonInstall, BackendMethod.daemonStart,
             BackendMethod.daemonRepair, BackendMethod.daemonUninstall:
            seconds = 20
        case BackendMethod.pairDaemon:
            seconds = 30
        default:
            seconds = 20
        }
        return UInt64(seconds * 1_000_000_000)
    }

    // MARK: - Events

    /// Subscribe to backend events.
    ///
    /// Returns an AsyncStream so the caller can iterate with `for await` on the
    /// main actor without polling.
    func events() -> AsyncStream<BackendEvent> {
        AsyncStream { continuation in
            let token = UUID()
            eventContinuations[token] = continuation
            continuation.onTermination = { [weak self] _ in
                Task { await self?.removeEventContinuation(token) }
            }
        }
    }

    private func removeEventContinuation(_ token: UUID) {
        eventContinuations.removeValue(forKey: token)
    }

    // MARK: - Handshake

    /// Perform the version handshake and return the result.
    func handshake() async throws -> HandshakeResult {
        let result = try await request(BackendMethod.handshake, as: HandshakeResult.self)
        guard result.protocol_version == backendProtocolVersion else {
            throw BackendClientError.protocolMismatch(
                expected: backendProtocolVersion, got: result.protocol_version)
        }
        return result
    }

    func snapshot() async throws -> AppSnapshot {
        try await request(BackendMethod.getAppSnapshot, as: AppSnapshot.self)
    }

    func startCore() async throws { try await call(BackendMethod.startCore) }
    func stopCore() async throws { try await call(BackendMethod.stopCore) }
    func restartCore() async throws { try await call(BackendMethod.restartCore) }
    func requestShutdown() async throws { try await call(BackendMethod.shutdown) }

    /// Switch the core engine. Returns the refreshed snapshot on success.
    func setCoreMode(_ mode: String) async throws -> AppSnapshot {
        try await request(BackendMethod.setCoreMode,
                          params: ["mode": .string(mode)],
                          as: AppSnapshot.self)
    }

    /// Toggle a persisted setting. Returns the settings the backend stored, so
    /// the UI reflects what was written rather than what was clicked.
    func setAutoPing(_ enabled: Bool) async throws -> SettingsState {
        try await request(BackendMethod.setAutoPing,
                          params: ["enabled": .bool(enabled)],
                          as: SettingsState.self)
    }

    func setAutoUpdateSubscriptions(_ enabled: Bool) async throws -> SettingsState {
        try await request(BackendMethod.setAutoUpdate,
                          params: ["enabled": .bool(enabled)],
                          as: SettingsState.self)
    }

    // MARK: - Proxies

    func proxyGroups() async throws -> ProxyList {
        try await request(BackendMethod.getProxyGroups, as: ProxyList.self)
    }

    /// List the nodes of a group. An empty group means "the config default".
    func proxies(group: String) async throws -> ProxyList {
        try await request(BackendMethod.getProxies,
                          params: ["group": .string(group)],
                          as: ProxyList.self)
    }

    func switchProxy(group: String, name: String) async throws -> ProxyList {
        try await request(BackendMethod.switchProxy,
                          params: ["group": .string(group), "name": .string(name)],
                          as: ProxyList.self)
    }

    func testProxy(group: String, name: String) async throws -> ProxyList {
        try await request(BackendMethod.testProxy,
                          params: ["group": .string(group), "name": .string(name)],
                          as: ProxyList.self)
    }

    func testProxyGroup(_ group: String) async throws -> ProxyList {
        try await request(BackendMethod.testProxyGroup,
                          params: ["group": .string(group)],
                          as: ProxyList.self)
    }

    // MARK: - Maintenance

    func reloadConfig() async throws -> MaintenanceResult {
        try await request(BackendMethod.reloadConfig, as: MaintenanceResult.self)
    }

    func updateSubscriptions() async throws -> MaintenanceResult {
        try await request(BackendMethod.updateSubscriptions, as: MaintenanceResult.self)
    }

    // MARK: - Subscriptions

    func listSubscriptions() async throws -> [Subscription] {
        let response = try await request(BackendMethod.listSubscriptions,
                                         as: SubscriptionListResponse.self)
        return response.subscriptions
    }

    func addSubscription(name: String, url: String) async throws -> Subscription {
        try await request(BackendMethod.addSubscription,
                          params: ["name": .string(name), "url": .string(url)],
                          as: Subscription.self)
    }

    func updateSubscription(id: String, name: String, url: String) async throws -> Subscription {
        try await request(BackendMethod.updateSubscription,
                          params: ["id": .string(id),
                                   "name": .string(name),
                                   "url": .string(url)],
                          as: Subscription.self)
    }

    func removeSubscription(id: String) async throws -> [Subscription] {
        let response = try await request(BackendMethod.removeSubscription,
                                         params: ["id": .string(id)],
                                         as: SubscriptionListResponse.self)
        return response.subscriptions
    }

    func setSubscriptionEnabled(id: String, enabled: Bool) async throws -> Subscription {
        try await request(BackendMethod.setSubscriptionEnabled,
                          params: ["id": .string(id), "enabled": .bool(enabled)],
                          as: Subscription.self)
    }

    func refreshSubscription(id: String) async throws -> Subscription {
        try await request(BackendMethod.refreshSubscription,
                          params: ["id": .string(id)],
                          as: Subscription.self)
    }

    // MARK: - Daemon

    func daemonStatus() async throws -> DaemonStatus {
        try await request(BackendMethod.getDaemonStatus, as: DaemonStatus.self)
    }

    func daemonInstall() async throws -> DaemonCommandResult {
        try await request(BackendMethod.daemonInstall, as: DaemonCommandResult.self)
    }

    func daemonStart() async throws -> DaemonCommandResult {
        try await request(BackendMethod.daemonStart, as: DaemonCommandResult.self)
    }

    func daemonRepair() async throws -> DaemonCommandResult {
        try await request(BackendMethod.daemonRepair, as: DaemonCommandResult.self)
    }

    func daemonUninstall(purge: Bool) async throws -> DaemonCommandResult {
        try await request(BackendMethod.daemonUninstall,
                          params: ["purge": .bool(purge)],
                          as: DaemonCommandResult.self)
    }

    func pairDaemon(invite: String) async throws -> DaemonStatus {
        try await request(BackendMethod.pairDaemon,
                          params: ["invite": .string(invite)],
                          as: DaemonStatus.self)
    }

    func unpairDaemon() async throws -> DaemonStatus {
        try await request(BackendMethod.unpairDaemon, as: DaemonStatus.self)
    }

    func setDaemonKeepRunning(_ enabled: Bool) async throws -> DaemonStatus {
        try await request(BackendMethod.setDaemonKeepRunning,
                          params: ["enabled": .bool(enabled)],
                          as: DaemonStatus.self)
    }
}

// MARK: - FileHandle line reading

private extension FileHandle {
    /// Read one newline-delimited line, or nil at EOF.
    ///
    /// FileHandle has `bytes` on newer systems but not with a blocking,
    /// line-oriented contract; this reads byte by byte, which is fine for the
    /// request/response rates involved here.
    func readline() throws -> Data? {
        var buffer = Data()
        while true {
            let chunk = try read(upToCount: 1)
            guard let chunk, !chunk.isEmpty else {
                return buffer.isEmpty ? nil : buffer
            }
            if chunk[chunk.startIndex] == 0x0A {
                return buffer
            }
            buffer.append(chunk)
        }
    }
}
