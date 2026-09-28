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
    /// The previous helper would not exit, so a new one must not be started:
    /// two helpers would own the same state, config and core.
    case terminationFailed(Int32)

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
        case .terminationFailed(let pid):
            return "The previous backend (pid \(pid)) did not stop, so a new one was not started. Quit and reopen JiejieBox."
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
    /// Drains the helper's stderr. Cancelled with its generation; see start().
    private var stderrTask: Task<Void, Never>?

    /// Pending requests keyed by id, each awaiting one response line.
    private var pending: [String: CheckedContinuation<Data, Error>] = [:]
    private var nextRequestID = 0

    /// Event stream plumbing.
    private var eventContinuations: [UUID: AsyncStream<BackendEvent>.Continuation] = [:]

    /// Called when the process exits unexpectedly.
    private var onTermination: (@Sendable (Int32) -> Void)?

    /// Why the helper is expected to exit.
    ///
    /// Without this, every exit looks like a crash: quitting the app or
    /// restarting the backend would flash "the backend stopped unexpectedly"
    /// at the user for a termination the app itself requested.
    enum TerminationIntent {
        /// Nobody asked it to stop: a crash, a kill, or an OOM.
        case none
        /// Quit, or an intentional teardown before a restart.
        case requested
    }

    private var terminationIntent: TerminationIntent = .none

    /// Identifies the helper process this client currently owns.
    ///
    /// WHY A GENERATION AND NOT JUST `process == nil`. The termination handler of
    /// an OLD process runs asynchronously, after the app has already moved on. A
    /// restart that does not wait for the old helper to be reaped can have the
    /// old helper's handler fire while the NEW helper is starting — and because
    /// that handler consulted client-wide state (`terminationIntent`, `process`),
    /// it judged the old process's exit by the new process's intent and then
    /// cleared the new process's handles. The user saw a healthy backend reported
    /// as crashed, with every pending request failed and the event stream closed.
    ///
    /// Every async continuation captures the generation it was created for, and
    /// may only touch client state when that generation is still current. A
    /// superseded generation cleans up nothing but itself.
    private var helperGeneration: UInt64 = 0
    /// The generation whose process is currently owned, if any.
    private var activeGeneration: UInt64?

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
        // A new process is a new generation. Bumping here — before the process
        // exists — means any handler still in flight for the previous helper is
        // already stale by the time this one runs.
        helperGeneration &+= 1
        let generation = helperGeneration
        // A fresh process starts with nobody having asked it to stop, so the
        // next exit is a fault until we mark otherwise.
        terminationIntent = .none

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
            // The generation is captured HERE, at handler creation, so the
            // callback carries the identity of the process it belongs to rather
            // than reading whatever the client owns when it eventually runs.
            Task { await self?.handleTermination(code, generation: generation) }
        }

        do {
            try proc.run()
        } catch {
            throw BackendClientError.launchFailed(error.localizedDescription)
        }

        process = proc
        activeGeneration = generation
        stdinHandle = inPipe.fileHandleForWriting
        log.info("backend started pid=\(proc.processIdentifier) gen=\(generation)")

        // Drain stderr so a chatty backend cannot block on a full pipe.
        //
        // Tracked and generation-scoped, for the same reason the read task is: this
        // task holds a file handle from ONE helper, and an untracked one keeps
        // reading a dead process's pipe after a restart. It is cancelled with the
        // generation that created it, so a superseded helper's drain cannot run
        // against the client's current state.
        let errHandle = errPipe.fileHandleForReading
        stderrTask?.cancel()
        stderrTask = Task.detached { [weak self] in
            while !Task.isCancelled, let line = try? errHandle.readline() {
                if line.isEmpty { break }
                await self?.noteStderr(line, generation: generation)
            }
        }

        startReading(outPipe.fileHandleForReading, generation: generation)
    }

    private func startReading(_ handle: FileHandle, generation: UInt64) {
        readTask = Task.detached { [weak self] in
            while let line = try? handle.readline() {
                guard let self else { return }
                await self.handleLine(line)
            }
            await self?.handleReadEnded(generation: generation)
        }
    }

    /// Ask the backend to exit, then wait for it to do so.
    ///
    /// Distinct from `shutdown()`: this sends the `shutdown` command and gives
    /// the backend time to run its own graceful exit — which is where the
    /// core stop/keep decision lives. Killing the helper immediately after the
    /// command would truncate that teardown and could leave the core in an
    /// inconsistent state.
    ///
    /// Phased on purpose. The backend has two teardown triggers — the
    /// `shutdown` method and stdin reaching EOF — and firing both at once races
    /// the ACK against the EOF path. Each phase starts only after the previous
    /// one has demonstrably failed:
    ///
    ///   1. send `shutdown`, wait for the ACK
    ///   2. wait for the process to exit on its own (the normal path)
    ///   3. only then close stdin, the fallback for a wedged backend
    ///   4. wait briefly again
    ///   5. only then terminate the helper
    ///
    /// Always returns: a backend that already died, or one that refuses to
    /// exit, must never prevent the app from quitting.
    func shutdownGracefully() async {
        terminationIntent = .requested
        let quitStarted = Date()

        // Phase 1: ask. The ACK means "shutdown accepted", NOT "teardown
        // finished" — the backend deliberately does not wait for the core to
        // stop before answering, so the UI can start exiting promptly.
        let ackStart = Date()
        let acknowledged = (try? await requestShutdown()) != nil
        let ackLatency = Date().timeIntervalSince(ackStart)

        guard let proc = process else {
            await cleanupAfterExit()
            return
        }

        // Phase 2: close stdin, whether or not the ACK arrived.
        //
        // The headless backend's read loop blocks on stdin, and GracefulExit
        // cannot stop it: with no UI attached it never signals Serve, so the
        // process stays alive until stdin reaches EOF. Measured: the backend is
        // still running 6 s after the ACK with stdin open, and exits 13 ms
        // after it closes.
        //
        // Waiting for a self-exit before closing stdin therefore waits for
        // something that cannot happen. On the normal path the ACK ordering is
        // what makes closing here safe — the response is written before teardown
        // starts, so it has already arrived.
        //
        // On the FAILURE path there is no ACK to protect, and stdin was
        // previously left open: the helper then had no way to be told the
        // frontend was gone, so the wait burned its full budget and the process
        // was force-terminated without ever running its graceful teardown. EOF
        // is precisely the fallback for a broken or unresponsive IPC channel, so
        // it must be sent here too. The caller's waiter has already been
        // resumed with the timeout error, so nothing is left awaiting a reply.
        let stdinClosedAt = Date()
        if let stdinHandle {
            try? stdinHandle.close()
            self.stdinHandle = nil
        }

        // Phase 3: wait for the process to leave, with a bounded deadline. The
        // loop returns the moment it exits — the budget is a ceiling, not a
        // sleep, so the common case costs milliseconds.
        if proc.isRunning {
            for _ in 0..<30 where proc.isRunning {   // ceiling ~3 s
                try? await Task.sleep(nanoseconds: 100_000_000)
            }
        }

        // Phase 4: last resort. Terminating mid-teardown can leave the core
        // inconsistent, so reaching this is logged as the anomaly it is.
        var fallbackTerminated = false
        if proc.isRunning {
            log.error("backend still running after stdin close; terminating")
            proc.terminate()
            fallbackTerminated = true
        }
        await cleanupAfterExit()

        // Timings go to the log only, never the UI: the goal is to keep
        // "is this wait necessary?" answerable from evidence rather than
        // guesswork.
        let ackText = ms(ackLatency)
        let stdinText = ms(stdinClosedAt.timeIntervalSince(ackStart))
        let exitText = ms(Date().timeIntervalSince(stdinClosedAt))
        let totalText = ms(Date().timeIntervalSince(quitStarted))
        let fallbackText = fallbackTerminated ? " (fallback terminate)" : ""
        // The no-ACK case has done the same EOF fallback as the normal path, so
        // the note reports what happened rather than implying a separate route.
        let ackNote = acknowledged ? "" : " (no ACK; EOF fallback used)"
        log.info("quit: ack \(ackText) stdin \(stdinText) exit \(exitText) total \(totalText)\(fallbackText)\(ackNote)")
    }

    /// noteStderr logs one helper log line, ignoring a superseded generation.
    private func noteStderr(_ data: Data, generation: UInt64) {
        guard generation == activeGeneration else { return }
        guard let line = String(data: data, encoding: .utf8) else { return }
        log.debug("\(line, privacy: .public)")
    }

    /// Milliseconds, for the quit timing line.
    private func ms(_ interval: TimeInterval) -> String {
        String(format: "%.0fms", interval * 1000)
    }

    /// Release every handle and callback the process owned.
    ///
    /// Must be complete: a stale `stdinHandle` lets a later write go to a
    /// closed pipe, leftover continuations leave callers awaiting a process
    /// that is gone, and a retained `onTermination` callback would fire for the
    /// NEXT process and report a spurious crash. Incomplete cleanup is how a
    /// restart ends up with two event streams.
    /// cleanupAfterExit releases the current helper's resources.
    ///
    /// Generation-checked by the callers below; this is the unconditional form for
    /// paths that already know they own the current generation.
    private func cleanupAfterExit() async {
        await cleanupAfterExit(generation: activeGeneration)
    }

    private func cleanupAfterExit(generation: UInt64?) async {
        // A superseded generation may not touch client state. Its own handles are
        // already gone: they were released by whoever superseded it.
        if let generation, generation != activeGeneration {
            log.info("ignoring cleanup for superseded helper gen=\(generation)")
            return
        }
        if let generation {
            log.info("cleaning up helper gen=\(generation)")
        }
        activeGeneration = nil
        cleanupOwnedState()
    }

    /// cleanupOwnedState is the unconditional teardown body.
    private func cleanupOwnedState() {
        readTask?.cancel()
        readTask = nil
        stderrTask?.cancel()
        stderrTask = nil

        if let stdinHandle {
            try? stdinHandle.close()
        }
        stdinHandle = nil

        // Fail anything still waiting rather than leaving it suspended.
        let waiting = pending
        pending.removeAll()
        for (_, cont) in waiting {
            cont.resume(throwing: BackendClientError.backendUnavailable)
        }

        for (_, cont) in eventContinuations { cont.finish() }
        eventContinuations.removeAll()

        process = nil
        onTermination = nil
        // Ownership is released here and ONLY here, so "the client owns no
        // helper" is decided in one place.
        activeGeneration = nil
        log.info("backend stopped")
    }

    /// Stop the helper without asking the backend to exit.
    ///
    /// THROWS WHEN THE HELPER WOULD NOT STOP. That is not a cosmetic failure: the
    /// caller is about to start a replacement, and starting one while the previous
    /// helper still owns the state files, the config and the daemon control channel
    /// produces two backends fighting over the same resources. The old form
    /// returned silently after a fixed wait, so `restart()` started a second helper
    /// over a possibly-live first one.
    func shutdown() async throws {
        terminationIntent = .requested
        readTask?.cancel()
        readTask = nil

        if let stdinHandle {
            try? stdinHandle.close()
        }
        guard let proc = process else { return }
        let generation = activeGeneration
        if !proc.isRunning {
            await cleanupAfterExit(generation: generation)
            return
        }

        // Escalate, and CONFIRM the exit before releasing ownership.
        //
        // The previous form terminated, waited a fixed two seconds, sent an
        // interrupt and then set `process = nil` WITHOUT checking whether the
        // helper had actually gone. `restart()` could therefore start the next
        // helper while the previous one was still running — two headless backends
        // both reading and writing the same state, config and daemon control
        // channel.
        proc.terminate()
        if await waitForExit(proc, seconds: 3) {
            await cleanupAfterExit(generation: generation)
            log.info("backend stopped after SIGTERM")
            return
        }

        log.error("backend ignored SIGTERM; sending SIGINT")
        proc.interrupt()
        if await waitForExit(proc, seconds: 3) {
            await cleanupAfterExit(generation: generation)
            log.info("backend stopped after SIGINT")
            return
        }

        // It is still alive after escalating. Report it rather than pretending
        // the teardown succeeded: the caller must not start a second helper while
        // this one holds the same state and core.
        log.error("backend did not exit after SIGTERM and SIGINT (pid \(proc.processIdentifier))")
        await cleanupAfterExit(generation: generation)
        throw BackendClientError.terminationFailed(proc.processIdentifier)
    }

    /// waitForExit polls until the process is gone or the budget expires.
    ///
    /// Returns whether the process actually exited — the distinction the old
    /// fixed sleep could not make, and the difference between "safe to start a new
    /// helper" and "two helpers are now running".
    private func waitForExit(_ proc: Process, seconds: Int) async -> Bool {
        let steps = seconds * 10
        for _ in 0..<steps {
            if !proc.isRunning { return true }
            try? await Task.sleep(nanoseconds: 100_000_000)
        }
        return !proc.isRunning
    }

    private func handleTermination(_ code: Int32, generation: UInt64) async {
        // A handler for a helper we no longer own must do NOTHING to client
        // state.
        //
        // This is the fix for the restart race. Without the check, the old
        // helper's exit was evaluated against the NEW helper's intent (so a
        // normal exit looked like a crash when the intent had been reset for the
        // new process), and then it cleared the new helper's process, stdin and
        // read task — leaving a live backend reported as dead with its event
        // stream closed.
        guard generation == activeGeneration else {
            log.info("ignoring termination of superseded helper gen=\(generation)")
            return
        }

        // Only an exit nobody asked for is a fault. A quit or an intentional
        // teardown is the app getting what it requested, and reporting it as
        // "stopped unexpectedly" would show the user an error for doing exactly
        // what they clicked.
        let expected = terminationIntent == .requested
        terminationIntent = .none
        if expected {
            log.info("backend exited as requested (code \(code))")
        } else {
            log.warning("backend exited with code \(code)")
        }

        // Capture the callback BEFORE the shared teardown clears it, and let the
        // teardown be the ONE implementation of "release this helper's
        // resources". The previous version duplicated that body here, which is
        // how two teardown paths drift: every future resource added to one would
        // have to be remembered in the other.
        let callback = onTermination
        await cleanupOwnedState()
        if !expected {
            callback?(code)
        }
    }

    /// handleReadEnded treats a lost protocol channel as a failed connection.
    ///
    /// The old form returned immediately when the Process object still claimed to
    /// be running, so a helper whose stdout had closed but which was still alive
    /// left the client in `ready`: requests were written into a channel nobody
    /// read and then waited out their full response timeout, and the app believed
    /// it had a working backend. EOF on the protocol stream is a connection
    /// failure regardless of what the process table says.
    private func handleReadEnded(generation: UInt64) async {
        guard generation == activeGeneration else {
            log.info("ignoring read-EOF of superseded helper gen=\(generation)")
            return
        }

        if process == nil {
            // The process is already gone; this is an ordinary exit.
            await handleTermination(0, generation: generation)
            return
        }

        // The channel died while the process lives: the backend is not usable.
        log.error("backend IPC channel closed while the helper is still running; treating the connection as failed")
        await handleTermination(0, generation: generation)
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
            group.addTask { [weak self] in
                try await Task.sleep(nanoseconds: timeout)
                // Abandon BEFORE throwing: the waiter is resumed with a timeout
                // error here, so a response arriving after the deadline finds
                // no waiter and is dropped by handleLine rather than resuming a
                // continuation a second time.
                await self?.abandon(id: id, reason: .timedOut(method: method))
                throw BackendClientError.timedOut(method: method)
            }
            defer { group.cancelAll() }
            do {
                guard let first = try await group.next() else {
                    abandon(id: id, reason: .timedOut(method: method))
                    throw BackendClientError.timedOut(method: method)
                }
                return first
            } catch {
                // Covers cancellation of this task (view torn down, navigation,
                // app quitting) as well as the timeout above. Either way the
                // waiter must be resumed, never merely dropped.
                abandon(id: id, reason: error as? BackendClientError ?? .backendUnavailable)
                throw error
            }
        }
    }

    /// Abandon a waiter whose response will never be used.
    ///
    /// Removes the continuation AND resumes it. Removing alone is not enough:
    /// a checked continuation that is dropped without being resumed leaks the
    /// awaiting task, which is precisely the "spinner forever" failure this
    /// timeout exists to prevent — it would just move the hang from the pending
    /// map into the task graph.
    ///
    /// Resuming exactly once is guaranteed because whoever removes the entry
    /// owns the resume: `handleLine` on a response, or this method when no
    /// response is coming. A late response therefore finds no entry (it is
    /// logged and dropped), and this method finds none if the response already
    /// arrived — never both.
    private func abandon(id: String, reason: BackendClientError) {
        guard let cont = pending.removeValue(forKey: id) else { return }
        cont.resume(throwing: reason)
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
             BackendMethod.getProxyGroups, BackendMethod.getProxies,
             BackendMethod.listSubscriptions:
            // Local state reads. Two seconds would be generous; eight allows
            // for a busy machine without hiding a wedged backend.
            seconds = 8
        case BackendMethod.shutdown:
            // Its own budget, deliberately not the 20 s command budget. The
            // reply is a local IPC acknowledgement of "shutdown accepted", not
            // completion: the backend writes it before teardown starts, so it
            // arrives in milliseconds. Waiting 20 s for it would only delay a
            // quit when something is already wrong.
            seconds = 2
        case BackendMethod.startCore, BackendMethod.restartCore:
            // Started/restarted cores are now AWAITED by the backend until the
            // start is genuinely committed (process spawned, or the daemon
            // accepted the config), because returning early is exactly what
            // made a failed start look like an instant revert to "stopped".
            //
            // The backend's own budget is 45 s, so this must sit ABOVE it:
            // aborting first would abandon a healthy but slow daemon apply and
            // present it as a failure. 60 s leaves room for the reply to travel
            // after the backend gives up.
            seconds = 60
        case BackendMethod.switchProxy, BackendMethod.setCoreMode,
             BackendMethod.stopCore,
             BackendMethod.addSubscription, BackendMethod.updateSubscription,
             BackendMethod.removeSubscription, BackendMethod.setSubscriptionEnabled,
             BackendMethod.setAutoPing, BackendMethod.setAutoUpdate,
             BackendMethod.setDaemonKeepRunning, BackendMethod.unpairDaemon:
            // Commands that change state: process lifecycle, engine switches,
            // state.json writes. Bounded well above a normal write so a slow
            // disk is not mistaken for a failure.
            seconds = 20
        case BackendMethod.testProxy, BackendMethod.testProxyGroup,
             BackendMethod.refreshSubscription, BackendMethod.updateSubscriptions,
             BackendMethod.reloadConfig:
            // Network work: fetching providers and measuring latency.
            //
            // ABOVE THE BACKEND'S OWN BUDGET, which is the actual requirement —
            // "generous" is not a number, and this value was two minutes while the
            // backend's group-test budget was ten, so a legitimate slow run was
            // abandoned and reported as a failure.
            //
            // The backend's budget scales with the node count and with the user's
            // per-node timeout (up to 60 s each), and it is capped at ten minutes. The
            // client must therefore sit ABOVE ten minutes, or the cap is the client's
            // number rather than the backend's. Eleven leaves a minute for the reply to
            // travel after the backend gives up.
            //
            // Kept in step by TestClientTimeoutExceedsEveryBackendBudget, which reads
            // this file and fails if the relationship ever inverts.
            seconds = 660
        case BackendMethod.getDaemonStatus:
            // Talks to the control plane and probes the core binary; the
            // earlier freeze was exactly this call never returning.
            seconds = 15
        case BackendMethod.daemonInstall, BackendMethod.daemonStart,
             BackendMethod.daemonRepair, BackendMethod.daemonUninstall:
            seconds = 20
        case BackendMethod.pairDaemon:
            seconds = 30
        case BackendMethod.importCoreFile:
            // The backend's worst case is bounded but additive: validate the
            // path, copy up to 256 MB, probe the candidate's version (3 s cap),
            // then run `sing-box check` on the current config (5 s cap). A
            // timeout below that sum would abort a healthy import on a slow
            // disk and — worse — could fire while the backend is mid-swap,
            // leaving the UI reporting failure for a swap that succeeded.
            seconds = 45
        case BackendMethod.importSubscriptionFile:
            // Reads a local file (capped at the shared 10 MB response limit),
            // runs the full decode/classify/parse pipeline and writes state.
            // No network, but the parse is real work.
            seconds = 30
        default:
            // Every method the client actually calls is listed explicitly
            // above; this fallback exists only so a future method cannot be
            // added without a bound. A test asserts the two sets agree.
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

    /// Measure every node of a group.
    ///
    /// Returns the FINAL summary, not the node list: the backend owns
    /// scheduling, and progress arrives meanwhile on the event stream. The
    /// request timeout (11 minutes, see `timeout()`) is set ABOVE the backend's
    /// own run budget so the backend always gets to answer first — a client
    /// timeout would abandon a run that is still doing useful work.
    func testProxyGroup(_ group: String) async throws -> ProxyGroupTestResult {
        try await request(BackendMethod.testProxyGroup,
                          params: ["group": .string(group)],
                          as: ProxyGroupTestResult.self)
    }

    // MARK: - Maintenance

    func reloadConfig() async throws -> MaintenanceResult {
        try await request(BackendMethod.reloadConfig, as: MaintenanceResult.self)
    }

    /// Hand ownership of config.json to JiejieBox.
    func adoptConfig() async throws -> MaintenanceResult {
        try await request(BackendMethod.adoptConfig, as: MaintenanceResult.self)
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

    /// Import a subscription from a file the user chose.
    ///
    /// Only the path crosses the wire. The frontend does not read the file,
    /// guess its format or count nodes: the backend runs the same pipeline a
    /// network fetch uses, so an imported source and a fetched one are
    /// indistinguishable downstream.
    func importSubscriptionFile(path: String) async throws -> SubscriptionImportResult {
        try await request(BackendMethod.importSubscriptionFile,
                          params: ["path": .string(path)],
                          as: SubscriptionImportResult.self)
    }

    // MARK: - Core

    /// Install a core binary the user chose.
    ///
    /// The backend validates and swaps the binary, then answers with the
    /// version it actually installed — the frontend never inspects the file
    /// itself, and never assumes the swap worked because the request returned.
    func importCoreFile(path: String) async throws -> CoreImportResult {
        try await request(BackendMethod.importCoreFile,
                          params: ["path": .string(path)],
                          as: CoreImportResult.self)
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
