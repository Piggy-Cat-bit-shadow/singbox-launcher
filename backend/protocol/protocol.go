// Package protocol defines the versioned JSON IPC contract between the
// JiejieBox macOS frontend and the Go backend.
//
// The transport is newline-delimited JSON over the backend's stdin/stdout:
// one complete JSON value per line, no framing headers. stdout carries
// protocol traffic only — all logging goes to stderr, so a stray log line can
// never corrupt the stream.
//
// Three shapes travel over the wire:
//
//	request   {"id":"1","method":"get_app_snapshot","params":{...}}
//	response  {"id":"1","result":{...}}    or    {"id":"1","error":{...}}
//	event     {"event":"core_state_changed","seq":7,"payload":{...}}
//
// This package is deliberately dependency-free apart from the standard
// library: it is imported by both the backend and (via the same schema) the
// Swift client, and it must stay cheap to compile and easy to review.
package protocol

// Version is the IPC protocol version. It is exchanged in the handshake and
// echoed in the snapshot so a mismatch produces a clear error instead of
// mysterious decode failures.
//
// Increment on any breaking change to a request, response or event shape.
const Version = 1

// Method names. Kept as constants so the backend, the tests and the Swift
// client agree on spelling without duplicating string literals.
const (
	// MethodHandshake performs the version/capability exchange.
	MethodHandshake = "handshake"
	// MethodGetAppSnapshot returns one complete state snapshot.
	MethodGetAppSnapshot = "get_app_snapshot"
	// MethodSubscribe requests the event stream. The response acknowledges;
	// events then arrive unsolicited on stdout.
	MethodSubscribe = "subscribe"
	// MethodStartCore starts the sing-box core.
	MethodStartCore = "start_core"
	// MethodStopCore stops the sing-box core.
	MethodStopCore = "stop_core"
	// MethodShutdown asks the backend to exit cleanly.
	MethodShutdown = "shutdown"
	// MethodRestartCore restarts the core, honouring the graceful-exit policy.
	MethodRestartCore = "restart_core"
	// MethodSetCoreMode switches between the classic and daemon engines.
	MethodSetCoreMode = "set_core_mode"
	// MethodSetAutoPing toggles the post-connect ping pass.
	MethodSetAutoPing = "set_auto_ping"
	// MethodSetAutoUpdate toggles automatic subscription updates.
	MethodSetAutoUpdate = "set_auto_update_subscriptions"
	// MethodGetProxyGroups lists the selector groups from the active config.
	MethodGetProxyGroups = "get_proxy_groups"
	// MethodGetProxies lists the proxies of one selector group.
	MethodGetProxies = "get_proxies"
	// MethodSwitchProxy selects a proxy inside a selector group.
	MethodSwitchProxy = "switch_proxy"
	// MethodTestProxy measures one proxy's latency.
	MethodTestProxy = "test_proxy"
	// MethodTestProxyGroup measures latency for every proxy in a group.
	MethodTestProxyGroup = "test_proxy_group"
	// MethodReloadConfig rebuilds config.json from the current state.
	MethodReloadConfig = "reload_config"
	// MethodUpdateSubscriptions refreshes all subscription nodes.
	MethodUpdateSubscriptions = "update_subscriptions"
	// MethodListSubscriptions returns the configured subscription sources.
	MethodListSubscriptions = "list_subscriptions"
	// MethodAddSubscription appends a subscription source.
	MethodAddSubscription = "add_subscription"
	// MethodUpdateSubscription edits a subscription source.
	MethodUpdateSubscription = "update_subscription"
	// MethodRemoveSubscription deletes a subscription source.
	MethodRemoveSubscription = "remove_subscription"
	// MethodSetSubscriptionEnabled toggles one source.
	MethodSetSubscriptionEnabled = "set_subscription_enabled"
	// MethodRefreshSubscription fetches one source.
	MethodRefreshSubscription = "refresh_subscription"
	// MethodGetDaemonStatus reports the daemon engine's setup state.
	MethodGetDaemonStatus = "get_daemon_status"
	// MethodDaemonInstall returns the install/update command.
	MethodDaemonInstall = "daemon_install"
	// MethodDaemonStart returns the service-start command.
	MethodDaemonStart = "daemon_start"
	// MethodDaemonRepair returns the re-pairing command.
	MethodDaemonRepair = "daemon_repair"
	// MethodDaemonUninstall returns the service-removal command.
	MethodDaemonUninstall = "daemon_uninstall"
	// MethodPairDaemon completes pairing from a pasted invite.
	MethodPairDaemon = "pair_daemon"
	// MethodUnpairDaemon drops the local pairing.
	MethodUnpairDaemon = "unpair_daemon"
	// MethodSetDaemonKeepRunning stores the daemon exit policy.
	MethodSetDaemonKeepRunning = "set_daemon_keep_running"
)

// Request is a single client-to-backend call.
type Request struct {
	// ID correlates the response. Chosen by the client; opaque to backend.
	ID string `json:"id"`
	// Method is one of the Method* constants.
	Method string `json:"method"`
	// Params carries method-specific arguments as arbitrary JSON values, so a
	// request can send strings, booleans, numbers or lists without the client
	// stringifying everything.
	Params map[string]any `json:"params,omitempty"`
}

// Response is the backend's reply to exactly one Request.
type Response struct {
	// ID echoes Request.ID.
	ID string `json:"id"`
	// Result is set on success. Omitted on error.
	Result any `json:"result,omitempty"`
	// Error is set on failure. Omitted on success.
	Error *Error `json:"error,omitempty"`
}

// Event is an unsolicited backend-to-client notification.
type Event struct {
	// Event is one of the Event* constants.
	Event string `json:"event"`
	// Seq is a monotonic sequence number, starting at 1 for each backend
	// process. It lets the client detect gaps or reordering and discard an
	// event older than the snapshot it already applied.
	Seq int64 `json:"seq"`
	// Payload carries the event-specific body.
	Payload any `json:"payload,omitempty"`
}

// Event names.
const (
	// EventHandshakeReady is sent once the backend is initialised.
	EventHandshakeReady = "handshake_ready"
	// EventCoreStateChanged reports a core state transition.
	EventCoreStateChanged = "core_state_changed"
	// EventLogLine carries one backend log line for the Diagnostics view.
	EventLogLine = "log_line"
	// EventError reports a non-fatal backend problem.
	EventError = "error"
	// EventShuttingDown announces that the backend is exiting.
	EventShuttingDown = "shutting_down"
	// EventSettingsChanged reports that business settings changed.
	EventSettingsChanged = "settings_changed"
	// EventProxiesChanged reports that the proxy list was reloaded.
	EventProxiesChanged = "proxies_changed"
	// EventProxySelectionChanged reports that a group switched its node.
	EventProxySelectionChanged = "proxy_selection_changed"
	// EventTrafficRate carries a periodic up/down speed sample.
	EventTrafficRate = "traffic_rate"
	// EventSubscriptionsChanged reports that the source list changed.
	EventSubscriptionsChanged = "subscriptions_changed"
	// EventDaemonChanged reports that daemon setup state changed.
	EventDaemonChanged = "daemon_changed"
)

// Error is a structured failure. The frontend decides the user-facing
// wording; Message is a technical description for logs and detail views.
type Error struct {
	// Code is a stable machine-readable identifier (e.g. "core_not_found").
	Code string `json:"code"`
	// Message is a technical, human-readable description. English.
	Message string `json:"message"`
	// Details carries optional structured context (paths, exit codes).
	Details map[string]any `json:"details,omitempty"`
	// Recoverable reports whether retrying the same call may succeed.
	Recoverable bool `json:"recoverable"`
}

// Error implements the error interface so handlers can return *Error
// directly.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// HandshakeResult is the reply to MethodHandshake.
//
// It is also emitted as EventHandshakeReady with the same payload, so a
// client that subscribes before calling handshake still learns the version.
type HandshakeResult struct {
	// ProtocolVersion is the backend's protocol.Version.
	ProtocolVersion int `json:"protocol_version"`
	// BackendVersion is the application version of the Go backend build.
	BackendVersion string `json:"backend_version"`
	// PID is the backend process id, useful in logs and for diagnostics.
	PID int `json:"pid"`
	// Capabilities tells the frontend which features exist, so it can hide
	// settings rather than guessing from the OS or version.
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities describes optional backend features. The frontend must not
// infer these from the platform: it asks.
type Capabilities struct {
	// Daemon reports whether the lxd daemon engine is available.
	Daemon bool `json:"daemon"`
	// Elevation reports whether the backend can request privileges.
	Elevation bool `json:"elevation"`
	// Remote reports whether remote machine management is available.
	Remote bool `json:"remote"`
	// Traffic reports whether the traffic profiler is available.
	Traffic bool `json:"traffic"`
	// Configurator reports whether the config wizard backend is available.
	Configurator bool `json:"configurator"`
}

// AppSnapshot is the complete initial state, returned by
// MethodGetAppSnapshot.
//
// The frontend applies this once on connect and then patches it from events,
// instead of issuing a request per widget at startup.
type AppSnapshot struct {
	// SnapshotSeq is the event sequence at the moment the snapshot was
	// taken. Events with a lower Seq are already reflected here and must be
	// discarded by the client.
	SnapshotSeq int64 `json:"snapshot_seq"`
	// Handshake repeats the version/capability block for convenience.
	Handshake HandshakeResult `json:"handshake"`
	// Core is the runtime state of the sing-box core.
	Core CoreState `json:"core"`
	// Settings holds the business settings the backend owns.
	Settings SettingsState `json:"settings"`
}

// CoreState is the core runtime status.
//
// State is a string rather than an enum so an older frontend still renders an
// unknown future state instead of failing to decode.
type CoreState struct {
	// State is one of: "stopped", "starting", "running", "stopping",
	// "error".
	State string `json:"state"`
	// BinaryExists reports whether a sing-box executable was found.
	BinaryExists bool `json:"binary_exists"`
	// ConfigExists reports whether config.json is present.
	ConfigExists bool `json:"config_exists"`
	// CoreVersion is the installed core version, empty when unknown.
	CoreVersion string `json:"core_version,omitempty"`
	// Backend is "classic" or "daemon".
	Backend string `json:"backend"`
	// ConfigStale is true when the built config no longer matches the state —
	// for example after editing a subscription. The product never rebuilds on
	// its own, so the UI must surface this and offer a reload.
	ConfigStale bool `json:"config_stale"`
	// ErrorMessage carries the last failure, if any.
	ErrorMessage string `json:"error_message,omitempty"`
}

// SettingsState is the subset of business settings the frontend displays.
//
// UI-only preferences (appearance, window frame, sidebar state) are NOT here:
// those belong to the frontend and never reach the backend.
type SettingsState struct {
	// Language is the backend locale tag ("en", "ru").
	Language string `json:"language"`
	// CoreBackendMode is "classic" or "daemon".
	CoreBackendMode string `json:"core_backend_mode"`
	// AutoPingAfterConnect mirrors the connection-behaviour toggle.
	AutoPingAfterConnect bool `json:"auto_ping_after_connect"`
	// AutoUpdateSubscriptions mirrors the subscription toggle.
	AutoUpdateSubscriptions bool `json:"auto_update_subscriptions"`
	// DataDir is the resolved data directory, for the Storage settings page.
	DataDir string `json:"data_dir"`
	// ConfigPath is the resolved config.json path.
	ConfigPath string `json:"config_path"`
	// LogsDir is the resolved log directory. The frontend opens files and
	// folders itself; the backend only reports where they are, so the path
	// never has to be duplicated in Swift.
	LogsDir string `json:"logs_dir"`
}

// ProxyGroup is one selector group the user can switch.
//
// Groups come from the active config's selector outbounds, and Selected is the
// tag the group has chosen right now, so the frontend renders the checkmark
// from backend truth rather than from what it last clicked.
type ProxyGroup struct {
	// Name is the exact outbound tag (used for switch requests).
	Name string `json:"name"`
	// DisplayName is the human-facing label.
	DisplayName string `json:"display_name"`
	// Type is the outbound type reported by the core ("Selector", "URLTest").
	Type string `json:"type,omitempty"`
	// Selected is the tag currently in use, empty when unknown.
	Selected string `json:"selected,omitempty"`
	// SelectedDisplay is Selected, normalised for display.
	SelectedDisplay string `json:"selected_display,omitempty"`
	// Count is how many proxies the group contains, when known.
	Count int `json:"count"`
}

// Proxy is one node inside a group.
//
// Delay is milliseconds, or -1 when it has never been measured: a missing
// measurement is not the same as an unreachable node, and the UI shows the
// difference instead of printing "0 ms".
type Proxy struct {
	// Name is the exact tag from the Clash API (used for switch/test).
	Name string `json:"name"`
	// DisplayName is the normalised label for the UI.
	DisplayName string `json:"display_name"`
	// Type is the proxy type reported by the core ("VLESS", "Selector", …).
	Type string `json:"type,omitempty"`
	// Delay is the last measured latency in ms; -1 means "not measured".
	Delay int64 `json:"delay"`
	// Group is the group this proxy was listed from, so a switch request
	// always carries a consistent (group, name) pair.
	Group string `json:"group"`
	// Selected reports whether this proxy is the group's current choice.
	Selected bool `json:"selected"`
	// LastError is the last ping failure for this proxy, if any.
	LastError string `json:"last_error,omitempty"`
}

// ProxyList is the reply to MethodGetProxies and MethodGetProxyGroups.
type ProxyList struct {
	// Groups are the switchable selector groups.
	Groups []ProxyGroup `json:"groups"`
	// Proxies are the nodes of the requested group; empty when the request
	// asked for the group list only.
	Proxies []Proxy `json:"proxies"`
	// Group echoes the group the proxies were listed from.
	Group string `json:"group,omitempty"`
	// Available is false when no Clash API endpoint is configured, which is
	// the normal state while the core is stopped. The frontend uses it to
	// show "start the core first" instead of an empty list.
	Available bool `json:"available"`
}

// TrafficRate is one periodic speed sample.
//
// Up and Down are bytes per second over the interval since the previous
// sample; TotalUp and TotalDown are the cumulative counters, so a client that
// joins late can still show lifetime totals.
type TrafficRate struct {
	Up        int64 `json:"up"`
	Down      int64 `json:"down"`
	TotalUp   int64 `json:"total_up"`
	TotalDown int64 `json:"total_down"`
	// AtUnixMS is when the sample was taken, in milliseconds since the epoch.
	AtUnixMS int64 `json:"at_unix_ms"`
}

// SubscriptionDTO is the compact view of one subscription source.
//
// This is deliberately NOT state.Source: that record also carries node bodies,
// identity overrides, skip rules and update schedules, none of which a menu bar
// manages. Sending the whole thing would make the frontend depend on the state
// schema, so a state migration would break the app.
type SubscriptionDTO struct {
	// ID is the source ULID, used for every edit and refresh call.
	ID string `json:"id"`
	// Name is the user-visible label; auto-derived from the URL when blank.
	Name string `json:"name"`
	// URL is the subscription address.
	URL string `json:"url"`
	// Enabled excludes the source from the build when false.
	Enabled bool `json:"enabled"`
	// NodeCount is how many usable nodes the source currently contributes.
	NodeCount int `json:"node_count"`
	// MaxNodes is the per-source cap; 0 means "use the global setting".
	MaxNodes int `json:"max_nodes"`

	// ProfileTitle is the provider's own name for the profile, when the
	// provider announced one. Preferred over Name for display when set.
	ProfileTitle string `json:"profile_title,omitempty"`
	// SupportURL is the provider's support link, when announced.
	SupportURL string `json:"support_url,omitempty"`

	// LastAttempt / LastSuccess are RFC3339 UTC timestamps, empty when never.
	LastAttempt string `json:"last_attempt,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
	// LastStatus is "ok" or "err"; empty when never fetched.
	LastStatus string `json:"last_status,omitempty"`
	// LastError is the last failure message, for the row's error line.
	LastError string `json:"last_error,omitempty"`
	// HTTPStatusCode and NodesFetched come from the last fetch.
	HTTPStatusCode int `json:"http_status_code,omitempty"`
	NodesFetched   int `json:"nodes_fetched,omitempty"`
}

// Daemon service states. These mirror core.DaemonServiceState so the frontend
// can branch on a stable string rather than parsing detail text.
const (
	// DaemonServiceNotInstalled — no service definition on disk.
	DaemonServiceNotInstalled = "not_installed"
	// DaemonServiceUnsafe — the service runs a file the user could replace.
	DaemonServiceUnsafe = "unsafe"
	// DaemonServiceStale — the installed copy is not the launcher's core.
	DaemonServiceStale = "stale"
	// DaemonServiceNotRunning — installed correctly but not started.
	DaemonServiceNotRunning = "not_running"
	// DaemonServiceProcessStale — running an older image than on disk.
	DaemonServiceProcessStale = "process_stale"
	// DaemonServiceOK — installed, safe and running the current core.
	DaemonServiceOK = "ok"
)

// DaemonStatusDTO describes the daemon engine's setup state.
//
// The UI needs this to decide what to offer: an uninstalled service needs
// install, an unpaired one needs pairing, and only a ready one may be
// activated. It intentionally carries no secret.
type DaemonStatusDTO struct {
	// Supported is false when this build or platform has no daemon engine.
	Supported bool `json:"supported"`
	// Service is one of the DaemonService* constants.
	Service string `json:"service"`
	// ServiceDetail is the classifier's English reason, for diagnostics.
	ServiceDetail string `json:"service_detail,omitempty"`
	// Installed is true once a service definition exists.
	Installed bool `json:"installed"`
	// Paired is true when a client identity and server pin are stored.
	Paired bool `json:"paired"`
	// Reachable is true when the control plane answered.
	Reachable bool `json:"reachable"`
	// Ready is the gate for activating daemon mode: installed + paired +
	// reachable. Only then may the UI offer to switch engines.
	Ready bool `json:"ready"`
	// ActiveMode is true when the daemon is the selected engine.
	ActiveMode bool `json:"active_mode"`

	// Address is the control-channel endpoint.
	Address string `json:"address,omitempty"`
	// Fingerprint is the paired server pin, truncated for display.
	Fingerprint string `json:"fingerprint,omitempty"`
	// CoreStatus is the daemon's own core state (idle/started/fatal).
	CoreStatus string `json:"core_status,omitempty"`
	// DaemonVersion / RunningVersion / LauncherVersion are versions from the
	// daemon passport, the running image and the launcher's own core.
	DaemonVersion   string `json:"daemon_version,omitempty"`
	RunningVersion  string `json:"running_version,omitempty"`
	LauncherVersion string `json:"launcher_version,omitempty"`
	// CoreSupportsLxd is false when the installed core has no `lxd` subcommand,
	// which makes every setup step impossible.
	CoreSupportsLxd bool `json:"core_supports_lxd"`

	// NeedsInstall / NeedsStart point at the single next repair step.
	NeedsInstall bool `json:"needs_install"`
	NeedsStart   bool `json:"needs_start"`
	// PersistsAfterQuit is the positive phrasing of the exit policy: the VPN
	// keeps running when the app quits.
	PersistsAfterQuit bool `json:"persists_after_quit"`
	// Error carries the daemon's last reported problem, if any.
	Error string `json:"error,omitempty"`
}

// Core state values used by CoreState.State.
const (
	CoreStateStopped  = "stopped"
	CoreStateStarting = "starting"
	CoreStateRunning  = "running"
	CoreStateStopping = "stopping"
	CoreStateError    = "error"
)
