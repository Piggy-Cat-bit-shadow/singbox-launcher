// Package service implements the headless JiejieBox backend that the SwiftUI
// frontend drives over the JSON IPC protocol.
//
// The backend owns all business state. The frontend is a projection: it sends
// commands, receives a snapshot plus an event stream, and never decides on its
// own whether the core is running, which proxy is selected or whether a
// machine is connected.
package service

import (
	"os"
	"sync"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/paths"
)

// Backend is the headless application. It wraps the existing core services
// without a Fyne application: every UI touchpoint inside core is guarded, so
// no widget is ever constructed.
type Backend struct {
	ac *core.AppController

	mu sync.Mutex
	// seq is the monotonic event sequence. It lets the frontend discard an
	// event that predates the snapshot it already applied.
	seq int64
	// subscribers receive every emitted event. The IPC layer registers one
	// writer; tests register their own.
	subscribers []func(protocol.Event)
}

// New builds the backend for a resolved data layout.
//
// It passes nil icon data to core.NewAppController, which is what selects the
// headless path: no Fyne application and no UIService are created.
func New(layout paths.Layout) (*Backend, error) {
	ac, err := core.NewAppController(layout, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return &Backend{ac: ac}, nil
}

// Handshake returns the version and capability block.
func (b *Backend) Handshake() protocol.HandshakeResult {
	return protocol.HandshakeResult{
		ProtocolVersion: protocol.Version,
		BackendVersion:  constants.AppVersion,
		PID:             os.Getpid(),
		Capabilities:    b.capabilities(),
	}
}

// capabilities reports which features this build actually has, so the
// frontend can hide settings instead of guessing from the OS version.
func (b *Backend) capabilities() protocol.Capabilities {
	return protocol.Capabilities{
		// The daemon engine exists on macOS and Windows (non-386).
		Daemon:       core.DaemonEngineAvailable(),
		Elevation:    core.ElevationSupported(),
		Remote:       true,
		Traffic:      true,
		Configurator: true,
	}
}

// Snapshot returns the complete initial state.
func (b *Backend) Snapshot() protocol.AppSnapshot {
	b.mu.Lock()
	seq := b.seq
	b.mu.Unlock()

	return protocol.AppSnapshot{
		SnapshotSeq: seq,
		Handshake:   b.Handshake(),
		Core:        b.coreState(),
		Settings:    b.settingsState(),
	}
}

// coreState projects the controller's runtime state into the wire DTO.
//
// The single source of truth is core: RunningState for the running flag and
// GetVPNButtonState for the derived button state. The backend never keeps a
// second copy of "is it running".
func (b *Backend) coreState() protocol.CoreState {
	if b.ac == nil {
		return protocol.CoreState{State: protocol.CoreStateStopped, Backend: "classic"}
	}

	bs := b.ac.GetVPNButtonState()
	state := protocol.CoreStateStopped
	switch {
	case bs.IsRunning:
		state = protocol.CoreStateRunning
	case !bs.BinaryExists:
		state = protocol.CoreStateError
	}

	version, err := b.ac.GetInstalledCoreVersion()
	if err != nil {
		version = ""
	}

	backend := "classic"
	if b.ac.CorePersistsAfterAppExit() {
		backend = "daemon"
	}

	return protocol.CoreState{
		State:        state,
		BinaryExists: bs.BinaryExists,
		ConfigExists: configExists(b.ac),
		CoreVersion:  version,
		Backend:      backend,
	}
}

// configExists reports whether config.json is present.
func configExists(ac *core.AppController) bool {
	if ac == nil || ac.FileService == nil {
		return false
	}
	_, err := os.Stat(ac.FileService.ConfigPath)
	return err == nil
}

// settingsState projects the business settings the frontend displays.
//
// UI-only preferences (appearance, window frame) are deliberately absent:
// those belong to Swift and never reach the backend.
func (b *Backend) settingsState() protocol.SettingsState {
	if b.ac == nil || b.ac.FileService == nil {
		return protocol.SettingsState{}
	}
	binDir := b.ac.FileService.Layout.Data.Bin()
	st := locale.LoadSettings(binDir)

	mode := st.CoreBackendMode
	if mode == "" {
		mode = "classic"
	}

	lang := st.Lang
	if lang == "" {
		lang = "en"
	}

	return protocol.SettingsState{
		Language:                lang,
		CoreBackendMode:         mode,
		AutoPingAfterConnect:    !st.AutoPingAfterConnectDisabled,
		AutoUpdateSubscriptions: !st.SubscriptionAutoUpdateDisabled,
		DataDir:                 string(b.ac.FileService.Layout.Data),
		ConfigPath:              b.ac.FileService.ConfigPath,
	}
}

// Subscribe registers a listener for emitted events and returns an
// unsubscribe function.
func (b *Backend) Subscribe(fn func(protocol.Event)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers = append(b.subscribers, fn)
	idx := len(b.subscribers) - 1
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if idx < len(b.subscribers) {
			b.subscribers[idx] = nil
		}
	}
}

// emit assigns the next sequence number and delivers an event to every
// subscriber.
//
// Emission happens under the lock so sequence numbers are handed out in the
// same order subscribers observe them; without that, two concurrent state
// changes could arrive at the frontend out of order.
func (b *Backend) emit(name string, payload any) {
	b.mu.Lock()
	b.seq++
	ev := protocol.Event{Event: name, Seq: b.seq, Payload: payload}
	subs := make([]func(protocol.Event), 0, len(b.subscribers))
	for _, fn := range b.subscribers {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	b.mu.Unlock()

	for _, fn := range subs {
		fn(ev)
	}
}

// EmitCoreState publishes the current core state as an event.
//
// Called after a command changes the core so the frontend re-renders from
// backend truth rather than assuming the click succeeded.
func (b *Backend) EmitCoreState() {
	b.emit(protocol.EventCoreStateChanged, b.coreState())
}

// StartCore starts the sing-box core and publishes the resulting state.
func (b *Backend) StartCore() error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	debuglog.InfoLog("backend: start_core requested")
	core.StartSingBoxProcess()
	b.EmitCoreState()
	return nil
}

// StopCore stops the sing-box core and publishes the resulting state.
func (b *Backend) StopCore() error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	debuglog.InfoLog("backend: stop_core requested")
	core.StopSingBoxProcess()
	b.EmitCoreState()
	return nil
}

// Shutdown releases backend resources. The core stop policy is unchanged:
// the same graceful-exit semantics the Fyne build used.
func (b *Backend) Shutdown() {
	if b.ac == nil {
		return
	}
	debuglog.InfoLog("backend: shutdown requested")
	b.emit(protocol.EventShuttingDown, nil)
	b.ac.GracefulExit()
}
