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
    static let importCoreFile = "import_core_file"
    static let importSubscriptionFile = "import_subscription_file"
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
    /// Whether a user-selected core binary can be installed. False on
    /// platforms whose core import the backend cannot perform, so the version
    /// row must not offer the action there.
    ///
    /// Defaulted because an older backend does not send the key at all: absent
    /// must decode as "no", never as a crash or as "yes".
    let core_import: Bool?
    /// Whether a local subscription file can be imported.
    let local_subscription_import: Bool?
    /// Always false: remote machines and the config wizard were removed from
    /// the product. Kept so the block still decodes against older backends.
    let remote: Bool
    let configurator: Bool

    /// Capability flags are asked for, never inferred from the platform.
    var canImportCore: Bool { core_import ?? false }
    var canImportLocalSubscription: Bool { local_subscription_import ?? false }
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
    /// Localized label for the status line.
    ///
    /// Takes the language rather than reading the environment because this is a
    /// model type; the view decides which language to draw in.
    func label(_ language: Localization) -> String {
        switch self {
        case .stopped: return L.disconnected.tr(language)
        case .starting: return L.starting.tr(language)
        case .running: return L.connected.tr(language)
        case .stopping: return L.stopping.tr(language)
        case .error: return L.coreError.tr(language)
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
    /// Whether JiejieBox may rebuild this config.
    ///
    /// False for a config written by hand or by another tool: a rebuild replays
    /// the wizard state, which would overwrite somebody else's file. The UI must
    /// not offer a Reload action in that case — the backend would refuse it, and
    /// a button whose only outcome is an error is worse than no button.
    let config_rebuildable: Bool
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
    /// False when the ACTIVE ENGINE cannot list proxies at all, as opposed to
    /// having none to list or not being reachable yet.
    ///
    /// Optional because an older backend omits the key, and an absent value
    /// must read as "supported": defaulting it to false would disable the
    /// Proxies screen against a backend that works perfectly.
    let supported: Bool?
    /// Machine-readable cause, present only when `supported` is false. The
    /// frontend owns the wording; the backend only names the engine limitation.
    let unsupported_reason: String?

    /// Whether the engine can list proxies at all.
    var isSupported: Bool { supported ?? true }
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

    /// How this source was created: "remote" for a provider URL,
    /// "local_snapshot" for a file the user imported.
    ///
    /// Optional because a backend from before local import does not send the
    /// key; an absent value means "remote", which is what every source was
    /// before this feature existed.
    let input_kind: String?
    /// Whether the backend can refresh this source from its provider. The
    /// frontend never decides this itself — a local snapshot has no provider to
    /// ask, and offering Refresh on one would only ever produce an error.
    let can_refresh: Bool?
    /// Original filename of an imported file, for display.
    let filename: String?

    let profile_title: String?
    let support_url: String?

    let last_attempt: String?
    let last_success: String?
    let last_status: String?
    let last_error: String?
    let http_status_code: Int?
    let nodes_fetched: Int?

    /// True when this source came from a local file rather than a provider.
    var isLocalSnapshot: Bool { (input_kind ?? "remote") == "local_snapshot" }

    /// Whether a Refresh action may be offered.
    var isRefreshable: Bool { can_refresh ?? !isLocalSnapshot }

    /// Label preference: the provider's own title, then the user's name, then
    /// the URL host. Providers rename profiles, and their name is the one the
    /// user recognises from the provider's site.
    var label: String {
        if let title = profile_title, !title.isEmpty { return title }
        if !name.isEmpty { return name }
        return url
    }

    /// Where this source came from, for the row's secondary line.
    ///
    /// A local snapshot has no URL to show — showing an empty one would look
    /// like a broken source, so the file it came from is named instead.
    func sourceSummary(_ language: Localization) -> String {
        if isLocalSnapshot {
            if let f = filename, !f.isEmpty {
                return "\(L.importedFrom.tr(language)) \(f)"
            }
            return L.importedFromAFile.tr(language)
        }
        return url
    }

    /// True when the last fetch failed.
    var hasError: Bool { last_status == "err" }

    /// True when the source has never been fetched successfully.
    var neverFetched: Bool { (last_success ?? "").isEmpty }

    /// Human summary of the node count.
    func nodeSummary(_ language: Localization) -> String {
        node_count == 1
            ? "\(node_count) \(L.nodesCount.tr(language))"
            : "\(node_count) \(L.nodesCount.tr(language))"
    }

    /// "Updated 2h ago", "Never updated", or the error.
    func statusSummary(_ language: Localization) -> String {
        if hasError {
            if let err = last_error, !err.isEmpty { return err }
            return L.lastUpdateFailed.tr(language)
        }
        // A local snapshot is never fetched, so "Never updated" would read as a
        // fault rather than as the normal state of an imported file.
        if isLocalSnapshot {
            if let success = last_success, !success.isEmpty {
                return "\(L.imported.tr(language)) \(RelativeTime.describe(success))"
            }
            return L.imported.tr(language)
        }
        if let success = last_success, !success.isEmpty {
            return "\(L.updatedAgo.tr(language)) \(RelativeTime.describe(success))"
        }
        return L.neverUpdated.tr(language)
    }
}

/// Result of a list request.
struct SubscriptionListResponse: Decodable {
    let subscriptions: [Subscription]
}

/// Result of importing a local subscription file.
///
/// The frontend reports what the backend actually did — it never parses,
/// decodes or counts nodes itself. `nodes_imported` is the backend's count
/// after the shared pipeline ran, so the two can never disagree.
struct SubscriptionImportResult: Decodable {
    let subscription: Subscription
    let nodes_imported: Int
    /// Nodes the shared parser recognised but cannot express as an outbound.
    /// Surfaced because silently dropping them would misrepresent the file.
    let unsupported_count: Int
    let warnings: [String]?
    let warnings_count: Int
    let raw_bytes: Int64
    /// True when the imported nodes are not yet in the built config.
    let config_stale: Bool
    /// Whether the config may be rebuilt from state. False for an
    /// externally-managed config, which must never be overwritten.
    let config_rebuildable: Bool

    /// Human summary of the import, for the confirmation line.
    var summary: String {
        var text = nodes_imported == 1
            ? "Imported 1 node."
            : "Imported \(nodes_imported) nodes."
        if unsupported_count > 0 {
            text += " \(unsupported_count) entr"
                + (unsupported_count == 1 ? "y was" : "ies were")
                + " not recognised."
        }
        return text
    }
}

/// Result of installing a user-selected core binary.
struct CoreImportResult: Decodable {
    /// Version that was in place before the import, empty when unknown.
    let old_version: String?
    let new_version: String
    /// Where the binary now lives.
    let installed_path: String
    /// Which binary will actually run — differs from `installed_path` when an
    /// override such as SINGBOX_LAUNCHER_CORE is in effect.
    let active_path: String
    /// "env", "data", "app" or "path": where the running core comes from.
    let core_source: String
    /// True when the candidate was actually run against the current config.
    let config_checked: Bool
    /// Whether the current config parsed under the new core.
    let config_compatible: Bool
    /// True when a running daemon still serves the previous binary, so the
    /// service must be refreshed before the change takes effect.
    let daemon_update_required: Bool
    let warning: String?

    /// Core state after the swap, so the UI updates without a second round
    /// trip.
    let core: CoreStatus

    /// Confirmation line for the version row.
    var summary: String {
        if let old = old_version, !old.isEmpty, old != new_version {
            return "Core updated: \(old) → \(new_version)"
        }
        return "Core installed: \(new_version)"
    }
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
    ///
    /// Driven by the protocol value, not by the rendered `summary` string: a
    /// label chosen by matching display text would break in any other language.
    /// The `default` case falls back to the raw value rather than to a
    /// translated guess, so an unknown state is visible instead of mislabelled.
    func serviceLabel(_ language: Localization) -> String {
        switch service {
        case "not_installed": return L.daemonNotInstalled.tr(language)
        case "unsafe": return L.daemonUnsafe.tr(language)
        case "stale": return L.daemonUpdateRequired.tr(language)
        case "not_running": return L.daemonServiceStopped.tr(language)
        case "process_stale": return L.daemonRestartRequired.tr(language)
        case "ok": return L.daemonRunning.tr(language)
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

    /// One-line state summary, for display only.
    ///
    /// Views must NOT branch on this text — `nextStep` and `service` carry the
    /// machine-readable state. This exists so a status line can be shown without
    /// the caller re-deriving the same precedence.
    func summary(_ language: Localization) -> String {
        if !supported { return L.daemonUnavailable.tr(language) }
        if active_mode { return L.daemonActive.tr(language) }
        if ready { return L.daemonReady.tr(language) }
        if !installed || needs_install { return L.daemonSetupRequired.tr(language) }
        if needs_start { return L.daemonServiceStopped.tr(language) }
        if !paired { return L.daemonPairingRequired.tr(language) }
        return L.daemonUnavailable.tr(language)
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
