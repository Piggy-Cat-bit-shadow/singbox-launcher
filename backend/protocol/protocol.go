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
	// MethodAdoptConfig hands ownership of config.json to JiejieBox.
	//
	// A separate method rather than a flag on reload: adoption is not a rebuild,
	// and it must only ever follow an explicit user confirmation that names the
	// overwrite consequence.
	MethodAdoptConfig = "adopt_config"
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
	// MethodImportCoreFile installs a user-selected sing-box as the Data core.
	MethodImportCoreFile = "import_core_file"
	// MethodImportSubscriptionFile imports a local file as a subscription
	// snapshot. Distinct from add_subscription on purpose: a remote URL and a
	// local file are different operations, and overloading one method would
	// mean guessing the intent from the payload.
	MethodImportSubscriptionFile = "import_subscription_file"
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
	//
	// SEQ IS ONLY MEANINGFUL WITHIN ITS SESSION. It restarts at 1 whenever the
	// backend process does, so comparing a new process's `seq` against a high-water
	// mark remembered from the previous one discards everything the new backend
	// sends until it has emitted more events than the old one ever did. A client
	// MUST scope its high-water mark to Session and reset it when Session changes.
	Seq int64 `json:"seq"`
	// Session identifies the backend PROCESS that emitted this event.
	//
	// It exists to make `seq` interpretable. Two different processes both count
	// from 1, so without this a client cannot distinguish a fresh event from a
	// late one belonging to a backend that has already been replaced — and both
	// mistakes are real: dropping the new backend's events (the counter looks
	// stale) or applying a dead backend's event (the counter looks current).
	//
	// Generated once per process; never reused or persisted.
	Session string `json:"session"`
	// Payload carries the event-specific body.
	Payload any `json:"payload,omitempty"`
}

// Event names.
const (
	// EventCoreStateChanged reports a core state transition.
	EventCoreStateChanged = "core_state_changed"
	// EventShuttingDown announces that the backend is exiting.
	EventShuttingDown = "shutting_down"
	// EventSettingsChanged reports that business settings changed.
	EventSettingsChanged = "settings_changed"
	// EventProxiesChanged reports that the proxy list was reloaded.
	EventProxiesChanged = "proxies_changed"

	// EventProxyTestProgress carries one delta frame of a group latency test:
	// started, one node's result, or finished. The final result also returns as
	// the test_proxy_group response, so a client that missed the stream still
	// converges on the same truth.
	EventProxyTestProgress = "proxy_test_progress"
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
	// Reason is a stable machine-readable token explaining WHY, when the code
	// alone is not enough to choose the right wording.
	//
	// Used by the proxy capability errors: "engine_lacks_rpc" tells the UI to
	// say the background service is too old, without the UI parsing an English
	// sentence or inspecting the backend mode.
	Reason string `json:"reason,omitempty"`
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
// It is returned only as a reply, and is also carried inside AppSnapshot. There
// is deliberately no handshake event: the frontend subscribes before taking its
// snapshot, so the snapshot seq already guarantees a consistent view, and an
// event nothing emitted was just dead protocol surface.
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
	// Traffic reports whether the traffic sampler is available. Note this is
	// the lightweight 1 Hz rate readout, not the full profiler.
	Traffic bool `json:"traffic"`
	// Subscriptions reports whether the subscription manager is available.
	Subscriptions bool `json:"subscriptions"`
	// CoreImport reports whether a user-selected core can be installed.
	CoreImport bool `json:"core_import"`
	// LocalSubscriptionImport reports whether a local subscription file can be
	// imported. Reported so the frontend never offers an action the backend
	// cannot perform.
	LocalSubscriptionImport bool `json:"local_subscription_import"`
	// Deprecated: remote machine management and the config wizard were removed
	// from the product. The fields stay so an older frontend still decodes the
	// block, but they are always false — a capability that claims a deleted
	// feature is worse than no capability, because a client would offer UI for
	// something that cannot work.
	Remote       bool `json:"remote"`
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
	//
	// Like Event.Seq, this is only comparable against events from the SAME
	// Session.
	SnapshotSeq int64 `json:"snapshot_seq"`
	// Session identifies the backend process that produced this snapshot.
	//
	// A client adopting a snapshot MUST adopt this session too: the snapshot is
	// the baseline for one process's stream, and carrying a previous process's
	// sequence across the boundary is precisely what makes a restarted backend
	// appear silent.
	Session string `json:"session"`
	// Handshake repeats the version/capability block for convenience.
	Handshake HandshakeResult `json:"handshake"`
	// Core is the runtime state of the sing-box core.
	Core CoreState `json:"core"`
	// Settings holds the business settings the backend owns.
	Settings SettingsState `json:"settings"`
	// Proxy is the lightweight "what am I connected through" summary, so the
	// home screen can answer that without loading a subscription's full node
	// list — which can be hundreds of entries and is only needed on the
	// Proxies screen.
	Proxy ProxySummary `json:"proxy"`
}

// ProxySummary is the current proxy selection, without the node list.
//
// Sourced from the controller's cached selection rather than a fresh Clash API
// read: the summary is for display, and issuing a network call on every
// snapshot would make the home screen slow and the backend chatty.
type ProxySummary struct {
	// Group is the selector group currently in use.
	Group string `json:"group,omitempty"`
	// Proxy is the selected node's tag.
	Proxy string `json:"proxy,omitempty"`
	// ProxyDisplay is the node tag normalised for display.
	ProxyDisplay string `json:"proxy_display,omitempty"`
	// Delay is the last known latency in ms; -1 when never measured.
	Delay int64 `json:"delay"`
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
	// ConfigRebuildable reports whether JiejieBox may rebuild this config.
	//
	// A rebuild replays the wizard state, so it is only valid for a config the
	// launcher itself built. A config written by hand or by another tool is not
	// ours to overwrite, and the frontend must know that BEFORE offering the
	// action — a button whose only possible outcome is an error is worse than no
	// button.
	//
	// The backend keeps enforcing this independently: hiding the affordance is
	// presentation, not a security boundary.
	ConfigRebuildable bool `json:"config_rebuildable"`
	// ConfigOwnership is who owns the config on disk: "managed" / "unknown" /
	// "external".
	//
	// The UI must branch on THIS rather than inferring "external" from
	// ConfigRebuildable=false. Those are different statements: a config written
	// by an older JijieBox has no provenance marker, so it is UNKNOWN — not
	// owned by some other tool — and telling the user otherwise is a false
	// accusation about their own file.
	ConfigOwnership string `json:"config_ownership"`
	// ErrorCode is a stable token naming why the core failed to start
	// ("config_rebuild_failed", "daemon_unreachable", ...), for localization.
	// Empty when there is no failure to report.
	ErrorCode string `json:"error_code,omitempty"`
	// ErrorDetail is the technical explanation, for logs and tooltips. Not for
	// direct display: it is not translated and may name internal functions.
	ErrorDetail string `json:"error_detail,omitempty"`
	// ErrorMessage carries the last failure, if any.
	ErrorMessage string `json:"error_message,omitempty"`
	// Recoverable tells the frontend whether retrying the same action can
	// plausibly succeed. It exists because the alternative — the UI guessing
	// from the error text — is how "needs update" style advice gets shown for
	// conditions an update cannot fix.
	Recoverable bool `json:"recoverable,omitempty"`
	// Operation names the lifecycle operation the error belongs to
	// ("start"/"stop"/"restart"/"rebuild"), so the frontend can attach the
	// failure to the action the user actually took.
	Operation string `json:"operation,omitempty"`
	// ConfigError is set when the failure came from building or activating the
	// config rather than from the core process. The frontend uses it to say
	// "config.json was NOT replaced; the previous working config is still in
	// use", which is the fact that decides whether the user still has a working
	// VPN. Empty for process-level failures.
	ConfigError string `json:"config_error,omitempty"`
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
	// Status classifies the latency result: success / timeout / failed /
	// unsupported / cancelled, or "" when this session has not measured it.
	//
	// A stable token so the row can be labelled in the user's language without
	// the frontend parsing an error string — Classic and daemon produce
	// completely different error text for the same condition.
	Status string `json:"status,omitempty"`
}

// ProxyActionCapabilities reports, per proxy action, what the active engine can
// do.
//
// PER ACTION, not one boolean. An engine can list nodes and be unable to test
// them, or list and test but not switch; a single flag forced those into "the
// proxy screen is broken", which disabled working features and hid the node
// list from users who could still see and choose from it.
//
// The frontend consumes these booleans and never branches on the backend mode:
// that keeps engine differences in the engine layer, where they can be tested
// against a transport, instead of spread through the UI.
type ProxyActionCapabilities struct {
	CanList   bool `json:"can_list"`
	CanSwitch bool `json:"can_switch"`
	// CanTestSingle — one node's latency can be measured.
	CanTestSingle bool `json:"can_test_single"`
	// CanTestGroup — the whole group can be measured ("Test All").
	CanTestGroup bool `json:"can_test_group"`
	// Reasons are stable tokens ("", "engine_lacks_rpc", "core_stopped",
	// "unknown") the UI maps to localized text. Never prose.
	ListReason   string `json:"list_reason,omitempty"`
	SwitchReason string `json:"switch_reason,omitempty"`
	TestReason   string `json:"test_reason,omitempty"`
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
	// Capabilities reports what the active engine can do, per action, so the UI
	// disables exactly the actions that cannot work instead of guessing from the
	// backend mode.
	Capabilities *ProxyActionCapabilities `json:"capabilities,omitempty"`
	// Available is false when no Clash API endpoint is configured, which is
	// the normal state while the core is stopped. The frontend uses it to
	// show "start the core first" instead of an empty list.
	Available bool `json:"available"`
	// Supported is false when the ACTIVE ENGINE cannot list proxies at all, as
	// opposed to having none to list or not being reachable yet.
	//
	// The three cases need three different screens, and collapsing them is what
	// produced a red error banner for a daemon that merely lacks the RPC:
	//
	//	Supported=false                  the engine has no such capability; no
	//	                                 action will change it, so the UI explains
	//	                                 instead of offering a retry
	//	Supported=true, Available=false  the engine can do it but is not up yet;
	//	                                 "start the core" is the next step
	//	Supported=true, Available=true   an empty list genuinely means no nodes
	//
	// A pointer so the wire can distinguish "absent" from "false": an older
	// backend omits the key, and the Swift DTO reads an absent value as true so
	// its absence never disables a feature that actually works.
	Supported *bool `json:"supported,omitempty"`
	// UnsupportedReason is a short machine-readable cause, present only when
	// Supported is false. It names the ENGINE rather than a user action, since
	// the user cannot change it.
	UnsupportedReason string `json:"unsupported_reason,omitempty"`

	// RuntimeRestartRequired is true when the running core is serving a DIFFERENT config
	// from the one on disk.
	//
	// The Clash API answers about the config the running core was started with, while the
	// group names here come from config.json. After a rebuild those disagree: the picker
	// lists groups the live core does not have, and switching to one fails with an error
	// naming a group the user can see on screen. Nothing compared the two before, so the
	// divergence was invisible and permanent until the user happened to restart.
	//
	// The UI uses this to offer a restart instead of presenting a broken list. It is NOT
	// an error: the config on disk is newer and correct, and the running core is simply
	// behind it.
	RuntimeRestartRequired bool `json:"runtime_restart_required,omitempty"`
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

	// InputKind is "remote" or "local_snapshot"; empty means remote, which keeps
	// every subscription created before local import working unchanged.
	InputKind string `json:"input_kind,omitempty"`
	// CanRefresh reports whether a network refresh is meaningful. False for a
	// local snapshot, so the UI hides Refresh without inferring the type from an
	// empty URL — inference is what produces "why is this source broken?".
	CanRefresh bool `json:"can_refresh"`
	// Filename is the original file's basename for a local snapshot. Never a
	// full path: the import is a snapshot, not a dependency on that file.
	Filename string `json:"filename,omitempty"`

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

	// ProtocolStale reports that the reachable daemon does not implement the
	// StartedService methods this launcher needs.
	//
	// A SEPARATE condition from `Service`, not folded into it. The existing
	// classifier compares binaries, hashes and versions, which answers "is the
	// installed service the copy we expect?" — it cannot see that a daemon is
	// the right BUILD but the wrong PROTOCOL. Collapsing the two would lose
	// which repair applies: reinstalling the service fixes a binary mismatch,
	// while a protocol mismatch needs a daemon built from a newer proto.
	//
	// Measured on the reporting machine: daemon 1.15.0-jiejie-masquerade.6 was
	// byte-correct and reported healthy, while answering Unimplemented for eight
	// of the ten methods the proxy and chain screens call.
	ProtocolStale bool `json:"protocol_stale"`
	// MissingRPCs names the absent methods, for the Daemon screen's detail line
	// and for a bug report. Sorted for stable output.
	MissingRPCs []string `json:"missing_rpcs,omitempty"`
}

// Core state values used by CoreState.State.
const (
	CoreStateStopped  = "stopped"
	CoreStateStarting = "starting"
	CoreStateRunning  = "running"
	CoreStateStopping = "stopping"
	CoreStateError    = "error"
)

// BoolPtr returns a pointer to v.
//
// Used for the optional ProxyList.Supported field, which must distinguish
// "absent" (an older backend) from "false" (this engine cannot list proxies).
// A plain bool cannot express that difference on the wire.
func BoolPtr(v bool) *bool { return &v }
