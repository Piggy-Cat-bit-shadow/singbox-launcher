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

    /// Stop the helper: ask politely, then terminate.
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
    func call(_ method: String) async throws {
        _ = try await rawRequest(method)
    }

    private func rawRequest(_ method: String) async throws -> Data {
        nextRequestID += 1
        let id = String(nextRequestID)
        let request = BackendRequest(id: id, method: method)

        return try await withCheckedThrowingContinuation { cont in
            pending[id] = cont
            do {
                try write(request)
            } catch {
                pending.removeValue(forKey: id)
                cont.resume(throwing: error)
            }
        }
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
    func requestShutdown() async throws { try await call(BackendMethod.shutdown) }
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
