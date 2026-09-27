// DTOs mirroring backend/protocol/protocol.go.
//
// Every field name here must match the Go JSON tag exactly: the contract tests
// on the Go side assert those names, and a rename on either side breaks the
// app at runtime with no compile error. Keep them in step; see
// docs/BACKEND_PROTOCOL.md.

import Foundation

/// Protocol version this frontend speaks. Must equal protocol.Version in Go.
let backendProtocolVersion = 1

// MARK: - Envelope

/// A request sent to the backend.
struct BackendRequest: Encodable {
    let id: String
    let method: String
    var params: [String: String]?

    init(id: String, method: String, params: [String: String]? = nil) {
        self.id = id
        self.method = method
        self.params = params
    }
}

/// Methods the frontend may call.
enum BackendMethod {
    static let handshake = "handshake"
    static let getAppSnapshot = "get_app_snapshot"
    static let subscribe = "subscribe"
    static let startCore = "start_core"
    static let stopCore = "stop_core"
    static let shutdown = "shutdown"
}

/// Events the backend may push.
enum BackendEventName {
    static let handshakeReady = "handshake_ready"
    static let coreStateChanged = "core_state_changed"
    static let logLine = "log_line"
    static let error = "error"
    static let shuttingDown = "shutting_down"
}

/// A structured backend failure.
struct BackendError: Decodable, Error {
    let code: String
    let message: String
    let recoverable: Bool

    /// Human-readable text for inline presentation.
    var displayText: String { message }
}

// MARK: - Handshake

struct Capabilities: Decodable {
    let daemon: Bool
    let elevation: Bool
    let remote: Bool
    let traffic: Bool
    let configurator: Bool
}

struct HandshakeResult: Decodable {
    let protocol_version: Int
    let backend_version: String
    let pid: Int
    let capabilities: Capabilities
}

// MARK: - Core state

/// Core runtime state. The strings match protocol.CoreState* in Go.
enum CoreState: String, Decodable {
    case stopped
    case starting
    case running
    case stopping
    case error

    /// Label for the status line.
    var label: String {
        switch self {
        case .stopped: return "Disconnected"
        case .starting: return "Starting…"
        case .running: return "Connected"
        case .stopping: return "Stopping…"
        case .error: return "Error"
        }
    }

    /// True while a transition is in flight, so the UI shows a spinner and
    /// disables the button instead of guessing.
    var isTransitioning: Bool {
        self == .starting || self == .stopping
    }
}

struct CoreStatus: Decodable {
    let state: CoreState
    let binary_exists: Bool
    let config_exists: Bool
    let core_version: String?
    let backend: String
    let error_message: String?
}

// MARK: - Settings

struct SettingsState: Decodable {
    let language: String
    let core_backend_mode: String
    let auto_ping_after_connect: Bool
    let auto_update_subscriptions: Bool
    let data_dir: String
    let config_path: String
    let logs_dir: String
}

// MARK: - Snapshot

struct AppSnapshot: Decodable {
    let snapshot_seq: Int64
    let handshake: HandshakeResult
    let core: CoreStatus
    let settings: SettingsState
}

// MARK: - Event

/// One unsolicited backend event.
struct BackendEvent: Decodable {
    let event: String
    let seq: Int64
    let payload: CoreStatus?
}

// MARK: - Response
//
// A response carries either a result or an error. Both are decoded lazily by
// the caller because the result type depends on the method that was called.

struct BackendResponse: Decodable {
    let id: String
    let error: BackendError?
}
