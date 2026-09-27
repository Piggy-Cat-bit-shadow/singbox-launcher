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
/// A JSON value, so requests can carry strings, booleans, numbers or lists
/// without the caller stringifying everything.
enum JSONValue: Codable {
    case string(String)
    case bool(Bool)
    case int(Int)
    case double(Double)
    case array([JSONValue])
    case object([String: JSONValue])
    case null

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Int.self) { self = .int(v) }
        else if let v = try? c.decode(Double.self) { self = .double(v) }
        else if let v = try? c.decode([JSONValue].self) { self = .array(v) }
        else if let v = try? c.decode([String: JSONValue].self) { self = .object(v) }
        else { self = .null }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let v): try c.encode(v)
        case .bool(let v): try c.encode(v)
        case .int(let v): try c.encode(v)
        case .double(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        case .null: try c.encodeNil()
        }
    }
}

struct BackendRequest: Encodable {
    let id: String
    let method: String
    var params: [String: JSONValue]?

    init(id: String, method: String, params: [String: JSONValue]? = nil) {
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
    static let restartCore = "restart_core"
    static let setCoreMode = "set_core_mode"
    static let setAutoPing = "set_auto_ping"
    static let setAutoUpdate = "set_auto_update_subscriptions"
    static let getProxyGroups = "get_proxy_groups"
    static let getProxies = "get_proxies"
    static let switchProxy = "switch_proxy"
    static let testProxy = "test_proxy"
    static let testProxyGroup = "test_proxy_group"
    static let reloadConfig = "reload_config"
    static let updateSubscriptions = "update_subscriptions"
    static let shutdown = "shutdown"
}

/// Events the backend may push.
enum BackendEventName {
    static let handshakeReady = "handshake_ready"
    static let coreStateChanged = "core_state_changed"
    static let logLine = "log_line"
    static let error = "error"
    static let shuttingDown = "shutting_down"
    static let settingsChanged = "settings_changed"
    static let proxiesChanged = "proxies_changed"
    static let proxySelectionChanged = "proxy_selection_changed"
    static let trafficRate = "traffic_rate"
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

// MARK: - Proxies

/// One switchable selector group.
struct ProxyGroup: Decodable, Identifiable, Hashable {
    let name: String
    let display_name: String
    let type: String?
    let selected: String?
    let selected_display: String?
    let count: Int

    /// Groups are identified by their outbound tag, which is what switch
    /// requests carry.
    var id: String { name }

    var label: String { display_name.isEmpty ? name : display_name }
}

/// One node inside a group.
///
/// `delay` is -1 when the node has never been measured. That is deliberately
/// distinct from 0 ms, so the UI can say "—" instead of claiming a latency the
/// core never reported.
struct ProxyNode: Decodable, Identifiable, Hashable {
    let name: String
    let display_name: String
    let type: String?
    let delay: Int64
    let group: String
    let selected: Bool
    let last_error: String?

    var id: String { name }

    var label: String { display_name.isEmpty ? name : display_name }

    /// True when a latency measurement exists.
    var isMeasured: Bool { delay >= 0 }

    /// Latency rendered for the row.
    var delayLabel: String {
        guard isMeasured else { return "—" }
        return "\(delay) ms"
    }

    /// Coarse quality bucket, used only to colour the value.
    var isFast: Bool { isMeasured && delay < 200 }
    var isSlow: Bool { isMeasured && delay >= 600 }
}

/// Reply to get_proxy_groups / get_proxies / switch_proxy / test_proxy.
struct ProxyList: Decodable {
    let groups: [ProxyGroup]
    let proxies: [ProxyNode]
    let group: String?
    let available: Bool
}

/// Result of a config rebuild or a subscription refresh.
struct MaintenanceResult: Decodable {
    let ok: Bool
    let message: String
    let total_sources: Int
    let succeeded_sources: Int
    let failed_sources: Int
    let nodes_count: Int
    let core_skips: [String]
}

/// One periodic up/down speed sample, in bytes per second.
struct TrafficRate: Decodable {
    let up: Int64
    let down: Int64
    let total_up: Int64
    let total_down: Int64
    let at_unix_ms: Int64
}

/// Byte formatting shared by the speed readout and the totals.
enum ByteFormat {
    /// "1.2 MB/s" — used for the live rate.
    static func rate(_ bytesPerSecond: Int64) -> String {
        guard bytesPerSecond > 0 else { return "0 KB/s" }
        return size(bytesPerSecond) + "/s"
    }

    /// "1.2 MB" — used for cumulative totals.
    static func size(_ bytes: Int64) -> String {
        let units = ["B", "KB", "MB", "GB", "TB"]
        var value = Double(max(0, bytes))
        var index = 0
        while value >= 1024 && index < units.count - 1 {
            value /= 1024
            index += 1
        }
        // Bytes are whole; larger units read better with one decimal.
        if index == 0 {
            return "\(Int(value)) \(units[index])"
        }
        return String(format: "%.1f %@", value, units[index])
    }
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
///
/// The payload type depends on `event`, so the raw JSON is kept and decoded on
/// demand. Adding an event therefore does not mean adding another optional
/// field here, which is what a directly-typed payload would force.
struct BackendEvent {
    let event: String
    let seq: Int64
    let payload: Data
}

extension BackendEvent: Decodable {
    private enum CodingKeys: String, CodingKey { case event, seq, payload }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        event = try c.decode(String.self, forKey: .event)
        seq = try c.decode(Int64.self, forKey: .seq)
        // Round-trip the payload back to bytes; type is decided by the caller.
        if let value = try? c.decode(JSONValue.self, forKey: .payload) {
            payload = (try? JSONEncoder().encode(value)) ?? Data()
        } else {
            payload = Data()
        }
    }
}

/// Decode an event payload as a specific type.
extension BackendEvent {
    func decode<T: Decodable>(_ type: T.Type) -> T? {
        try? JSONDecoder().decode(T.self, from: payload)
    }
}

// MARK: - Response
//
// A response carries either a result or an error. Both are decoded lazily by
// the caller because the result type depends on the method that was called.

struct BackendResponse: Decodable {
    let id: String
    let error: BackendError?
}
