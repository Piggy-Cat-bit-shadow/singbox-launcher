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
)

// Request is a single client-to-backend call.
type Request struct {
	// ID correlates the response. Chosen by the client; opaque to backend.
	ID string `json:"id"`
	// Method is one of the Method* constants.
	Method string `json:"method"`
	// Params carries method-specific arguments. Nil for no-argument methods.
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

// Core state values used by CoreState.State.
const (
	CoreStateStopped  = "stopped"
	CoreStateStarting = "starting"
	CoreStateRunning  = "running"
	CoreStateStopping = "stopping"
	CoreStateError    = "error"
)
