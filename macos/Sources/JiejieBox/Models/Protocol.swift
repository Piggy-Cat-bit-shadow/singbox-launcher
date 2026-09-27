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
    static let listSubscriptions = "list_subscriptions"
    static let addSubscription = "add_subscription"
    static let updateSubscription = "update_subscription"
    static let removeSubscription = "remove_subscription"
    static let setSubscriptionEnabled = "set_subscription_enabled"
    static let refreshSubscription = "refresh_subscription"
    static let getDaemonStatus = "get_daemon_status"
    static let daemonInstall = "daemon_install"
    static let daemonStart = "daemon_start"
    static let daemonRepair = "daemon_repair"
    static let daemonUninstall = "daemon_uninstall"
    static let pairDaemon = "pair_daemon"
    static let unpairDaemon = "unpair_daemon"
    static let setDaemonKeepRunning = "set_daemon_keep_running"
    static let shutdown = "shutdown"
}

/// Events the backend may push.
enum BackendEventName {
    static let coreStateChanged = "core_state_changed"
    static let shuttingDown = "shutting_down"
    static let settingsChanged = "settings_changed"
    static let proxiesChanged = "proxies_changed"
    static let proxySelectionChanged = "proxy_selection_changed"
    static let trafficRate = "traffic_rate"
    static let subscriptionsChanged = "subscriptions_changed"
    static let daemonChanged = "daemon_changed"
}

/// A structured backend failure.
struct BackendError: Decodable, Error {
    let code: String
    let message: String
    let recoverable: Bool

    /// Human-readable text for inline presentation.
    var displayText: String { message }
}

// Conforming to LocalizedError is what makes the backend's own message reach
// the user. Without it, `error.localizedDescription` — which every AppModel
// catch block uses — falls back to the generic
// "The operation couldn't be completed. (JiejieBox.BackendError error 1.)",
// so a precise backend explanation such as "stop the VPN before switching
// engines" was replaced by a message that says nothing.
extension BackendError: LocalizedError {
    var errorDescription: String? {
        message.isEmpty ? code : message
    }
}

// MARK: - Handshake

struct Capabilities: Decodable {
    let daemon: Bool
    let elevation: Bool
    /// The lightweight 1 Hz rate readout, not the full profiler.
    let traffic: Bool
    /// The subscription manager.
    let subscriptions: Bool
    /// Always false: remote machines and the config wizard were removed from
    /// the product. Kept so the block still decodes against older backends.
    let remote: Bool
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
    /// True when the built config has fallen behind the state (for example
    /// after editing subscriptions). The product never rebuilds on its own, so
    /// the UI surfaces this and offers a reload.
    let config_stale: Bool
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

// MARK: - Subscriptions

/// One subscription source, as the backend reports it.
///
/// Deliberately a projection rather than the full state record: the app manages
/// name, URL, enabled and refresh, and never sees node bodies or identity
/// overrides. See SubscriptionDTO in the Go protocol for the same reasoning.
struct Subscription: Decodable, Identifiable, Hashable {
    let id: String
    let name: String
    let url: String
    let enabled: Bool
    let node_count: Int
    let max_nodes: Int

    let profile_title: String?
    let support_url: String?

    let last_attempt: String?
    let last_success: String?
    let last_status: String?
    let last_error: String?
    let http_status_code: Int?
    let nodes_fetched: Int?

    /// Label preference: the provider's own title, then the user's name, then
    /// the URL host. Providers rename profiles, and their name is the one the
    /// user recognises from the provider's site.
    var label: String {
        if let title = profile_title, !title.isEmpty { return title }
        if !name.isEmpty { return name }
        return url
    }

    /// True when the last fetch failed.
    var hasError: Bool { last_status == "err" }

    /// True when the source has never been fetched successfully.
    var neverFetched: Bool { (last_success ?? "").isEmpty }

    /// Human summary of the node count.
    var nodeSummary: String {
        node_count == 1 ? "1 node" : "\(node_count) nodes"
    }

    /// "Updated 2h ago", "Never updated", or the error.
    var statusSummary: String {
        if hasError {
            if let err = last_error, !err.isEmpty { return err }
            return "Last update failed"
        }
        if let success = last_success, !success.isEmpty {
            return "Updated \(RelativeTime.describe(success))"
        }
        return "Never updated"
    }
}

/// Result of a list request.
struct SubscriptionListResponse: Decodable {
    let subscriptions: [Subscription]
}

// MARK: - Daemon

/// Daemon engine setup state.
struct DaemonStatus: Decodable {
    let supported: Bool
    let service: String
    let service_detail: String?
    let installed: Bool
    let paired: Bool
    let reachable: Bool
    let ready: Bool
    let active_mode: Bool

    let address: String?
    let fingerprint: String?
    let core_status: String?
    let daemon_version: String?
    let running_version: String?
    let launcher_version: String?
    let core_supports_lxd: Bool

    let needs_install: Bool
    let needs_start: Bool
    let persists_after_quit: Bool
    let error: String?

    /// Short label for the service state, for a status row.
    var serviceLabel: String {
        switch service {
        case "not_installed": return "Not installed"
        case "unsafe": return "Unsafe"
        case "stale": return "Update required"
        case "not_running": return "Stopped"
        case "process_stale": return "Restart required"
        case "ok": return "Running"
        default: return service
        }
    }

    /// The single next step, or nil when everything is in place.
    ///
    /// One step at a time on purpose: a checklist of five parallel actions
    /// leaves the user guessing which one matters now.
    var nextStep: DaemonNextStep? {
        if !supported { return nil }
        if !core_supports_lxd { return nil }
        if !installed || needs_install { return .install }
        if needs_start { return .start }
        if !paired { return .pair }
        if !reachable { return .wait }
        return nil
    }

    /// One-line state summary for the Core Mode row.
    var summary: String {
        if !supported { return "Unavailable" }
        if active_mode { return "Active" }
        if ready { return "Ready" }
        if !installed || needs_install { return "Setup required" }
        if needs_start { return "Service stopped" }
        if !paired { return "Pairing required" }
        return "Unavailable"
    }
}

/// The single next daemon setup step.
enum DaemonNextStep {
    case install
    case start
    case pair
    case wait

    var label: String {
        switch self {
        case .install: return "Install Service"
        case .start: return "Start Service"
        case .pair: return "Pair Service"
        case .wait: return "Waiting for service…"
        }
    }
}

/// Result of a daemon setup step.
struct DaemonCommandResult: Decodable {
    let operation: String
    let command: String
    let available: Bool
    let message: String
    let needs_admin: Bool
    let follow_up: String
    let status: DaemonStatus
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
    /// Lightweight current-proxy summary, so Home can show what is in use
    /// without loading a full node list.
    let proxy: ProxySummary
}

/// The current proxy selection, without the node list.
struct ProxySummary: Decodable {
    let group: String?
    let proxy: String?
    let proxy_display: String?
    /// Last known latency in ms; negative means never measured.
    let delay: Int64

    var label: String {
        if let d = proxy_display, !d.isEmpty { return d }
        if let p = proxy, !p.isEmpty { return p }
        return ""
    }

    var hasSelection: Bool { !label.isEmpty }

    var delayLabel: String? { delay >= 0 ? "\(delay) ms" : nil }
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

// MARK: - Relative time

/// Renders RFC3339 timestamps as short relative phrases.
///
/// The backend sends RFC3339 UTC because that is what the state file stores;
/// turning it into "2h ago" is presentation, so it lives in the frontend.
enum RelativeTime {
    private static let parser: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    private static let parserNoFraction: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    /// "just now", "5m ago", "2h ago", "3d ago", or the raw string if it
    /// cannot be parsed (better to show something than nothing).
    static func describe(_ rfc3339: String) -> String {
        guard let date = parser.date(from: rfc3339) ?? parserNoFraction.date(from: rfc3339) else {
            return rfc3339
        }
        let seconds = Int(Date().timeIntervalSince(date))
        if seconds < 0 { return "just now" }
        if seconds < 60 { return "just now" }
        if seconds < 3600 { return "\(seconds / 60)m ago" }
        if seconds < 86_400 { return "\(seconds / 3600)h ago" }
        return "\(seconds / 86_400)d ago"
    }
}
