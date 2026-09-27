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
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/core/events"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/locale"
	"singbox-launcher/internal/paths"
	"singbox-launcher/internal/platform"
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

	// cancelCoreWatch detaches the running-state subscription. Nil when the
	// controller has no event bus (a construction failure already returned).
	cancelCoreWatch events.Cancel
	// traffic is the lazy speed sampler, created on first use and stopped
	// with the core.
	traffic     *TrafficSampler
	trafficOnce sync.Once
}

// Traffic returns the process-wide speed sampler.
func (b *Backend) Traffic() *TrafficSampler {
	b.trafficOnce.Do(func() { b.traffic = NewTrafficSampler(b) })
	return b.traffic
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
	b := &Backend{ac: ac}
	b.watchCoreState()
	return b, nil
}

// watchCoreState subscribes to real running-state transitions.
//
// Emitting only after a command would mean the frontend never learns about a
// change it did not cause — a core that crashes, exits on its own, is killed
// from outside, or is stopped by the crash-handler supervisor. RunningState.Set
// dedups no-op calls and publishes VpnStateChanged on the actual transition, so
// subscribing there makes the frontend event-driven rather than
// command-driven, without inventing a polling loop or a second source of truth.
func (b *Backend) watchCoreState() {
	if b.ac == nil || b.ac.EventBus == nil {
		return
	}
	b.cancelCoreWatch = b.ac.EventBus.Subscribe(events.VpnStateChanged, func(ev events.Event) {
		// Run the sampler exactly while the core is up: a menu bar should
		// not keep polling a socket that is not listening.
		if p, ok := ev.Payload.(events.VpnStateChangedPayload); ok {
			if p.Running {
				b.Traffic().Start()
			} else {
				b.Traffic().Stop()
			}
		}
		// RunningState.Set can fire from any goroutine; emit is mutex-guarded,
		// so publishing straight from the bus handler is safe.
		b.EmitCoreState()
	})
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
		ConfigStale:  b.configStale(),
	}
}

// configStale reports whether the built config has fallen behind the state.
//
// Two sources, because the in-memory dirty markers are session-scoped and reset
// when the backend restarts:
//
//  1. the core's own markers, which catch changes made through the config
//     services in this session; and
//  2. a timestamp comparison between state.json and config.json, which survives
//     a restart.
//
// The second is what makes the flag honest across launches. Without it, adding
// a subscription, quitting and relaunching would silently drop the "reload
// needed" prompt and the user would have no way to learn their new nodes are
// not in the running config.
//
// The product rule is unchanged: this only REPORTS staleness. The backend never
// rebuilds on its own — rebuilding stays the user's decision.
func (b *Backend) configStale() bool {
	if b.ac == nil || b.ac.FileService == nil {
		return false
	}
	if b.ac.StateService != nil &&
		(b.ac.StateService.IsConfigStale() || b.ac.StateService.IsCacheStale()) {
		return true
	}
	return b.stateNewerThanConfig()
}

// stateNewerThanConfig compares modification times of state.json and config.json.
//
// A missing config is not "stale": a fresh install with no config has nothing to
// rebuild from, and reporting stale there would show a reload prompt for a
// config that never existed. A missing state is likewise not stale — there is
// nothing to build from.
func (b *Backend) stateNewerThanConfig() bool {
	statePath := platform.GetWizardStatePath(b.ac.FileService.Layout.Data)
	configPath := b.ac.FileService.ConfigPath
	if statePath == "" || configPath == "" {
		return false
	}

	stateInfo, err := os.Stat(statePath)
	if err != nil {
		return false
	}
	configInfo, err := os.Stat(configPath)
	if err != nil {
		return false
	}

	// A one-second tolerance absorbs the same-instant write ordering when a
	// rebuild saves state and config back to back.
	return stateInfo.ModTime().Sub(configInfo.ModTime()) > time.Second
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
		LogsDir:                 string(b.ac.FileService.Layout.Logs),
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
	if b.cancelCoreWatch != nil {
		b.cancelCoreWatch()
		b.cancelCoreWatch = nil
	}
	if b.traffic != nil {
		b.traffic.Stop()
	}
	b.emit(protocol.EventShuttingDown, nil)
	b.ac.GracefulExit()
}

// RestartCore restarts the core through the existing kill-and-let-the-watcher
// path, then publishes the resulting state.
//
// The restart logic is not reimplemented here: KillSingBoxForRestart is the
// same call the old UI's Restart invoked, and it leaves the supervisor to
// bring the process back, which is what makes the "restarting" state visible
// in between.
func (b *Backend) RestartCore() error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	debuglog.InfoLog("backend: restart_core requested")
	core.KillSingBoxForRestart()
	b.EmitCoreState()
	return nil
}

// SetCoreMode switches between the classic and daemon engines.
//
// Delegates to SwitchBackendMode, which owns the real semantics: it refuses
// while the VPN is running (a live classic process cannot be handed to the
// daemon and vice versa) and performs the engine-specific handover, including
// removing the system proxy the launcher installed for the daemon.
//
// The choice is persisted here, mirroring what the old settings UI did
// (load-mutate-save), so the mode survives a restart.
func (b *Backend) SetCoreMode(mode string) error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	if mode != string(core.BackendClassic) && mode != string(core.BackendDaemon) {
		return &protocol.Error{
			Code:        "bad_mode",
			Message:     "mode must be \"classic\" or \"daemon\"",
			Recoverable: false,
		}
	}

	if err := b.ac.SwitchBackendMode(core.BackendMode(mode)); err != nil {
		// The running-state refusal is an ordinary, expected outcome — the
		// frontend shows "stop the core first", not a crash report.
		return &protocol.Error{
			Code:        "mode_locked",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	binDir := b.ac.FileService.Layout.Data.Bin()
	st := locale.LoadSettings(binDir)
	st.CoreBackendMode = mode
	if err := locale.SaveSettings(binDir, st); err != nil {
		debuglog.WarnLog("backend: core mode switched but persisting failed: %v", err)
		b.emit(protocol.EventSettingsChanged, b.settingsState())
		return &protocol.Error{
			Code:        "persist_failed",
			Message:     "the engine switched, but saving the choice failed: " + err.Error(),
			Recoverable: true,
		}
	}

	debuglog.InfoLog("backend: core mode set to %q", mode)
	b.emit(protocol.EventSettingsChanged, b.settingsState())
	b.EmitCoreState()
	return nil
}

// SetAutoPing toggles the post-connect ping pass.
func (b *Backend) SetAutoPing(enabled bool) error {
	return b.updateSettings(func(st *locale.Settings) {
		st.AutoPingAfterConnectDisabled = !enabled
	})
}

// SetAutoUpdateSubscriptions toggles automatic subscription updates.
func (b *Backend) SetAutoUpdateSubscriptions(enabled bool) error {
	return b.updateSettings(func(st *locale.Settings) {
		st.SubscriptionAutoUpdateDisabled = !enabled
	})
}

// updateSettings applies a change to settings.json and publishes it.
//
// Settings live on disk (the backend owns them); the frontend only ever sees
// the result, so a toggle cannot drift from what was actually saved.
func (b *Backend) updateSettings(mutate func(*locale.Settings)) error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	binDir := b.ac.FileService.Layout.Data.Bin()
	st := locale.LoadSettings(binDir)
	mutate(&st)
	if err := locale.SaveSettings(binDir, st); err != nil {
		return &protocol.Error{
			Code:        "persist_failed",
			Message:     "could not save settings: " + err.Error(),
			Recoverable: true,
		}
	}
	b.emit(protocol.EventSettingsChanged, b.settingsState())
	return nil
}
