// Package service implements the headless JiejieBox backend that the SwiftUI
// frontend drives over the JSON IPC protocol.
//
// The backend owns all business state. The frontend is a projection: it sends
// commands, receives a snapshot plus an event stream, and never decides on its
// own whether the core is running, which proxy is selected or whether a
// machine is connected.
package service

import (
	"context"
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
	// groupTests owns the single active proxy latency test. One run at a time:
	// the UI presents one progress state, and a new run supersedes the old.
	groupTests groupTestManager
	ac         *core.AppController
	// adoptOnce runs the legacy-adoption attempt at most once per instance.
	adoptOnce adoptOnceState
	// ops tracks the in-flight start/stop request and the last start failure.
	// Kept apart from `mu` because a start can block for tens of seconds on a
	// daemon apply, and holding the snapshot mutex for that long would stall
	// every other IPC call.
	ops coreOpState

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

	// shutdownOnce makes Shutdown exactly-once; shutdownStarted lets a caller
	// observe that teardown has begun.
	shutdownOnce    sync.Once
	shutdownStarted chan struct{}
	shutdownInit    sync.Once
}

// shutdownSignal lazily creates the channel Shutdown closes.
//
// Lazily, because Backend is also constructed directly in tests as a zero
// value, and a nil channel would make IsShuttingDown report the wrong answer.
func (b *Backend) shutdownSignal() chan struct{} {
	b.shutdownInit.Do(func() {
		if b.shutdownStarted == nil {
			b.shutdownStarted = make(chan struct{})
		}
	})
	return b.shutdownStarted
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
	b.installOwnershipPolicy()
	b.watchCoreState()
	return b, nil
}

// installOwnershipPolicy hands core the ONE answer to "may a rebuild replace
// config.json?".
//
// Without this the pre-start hook had no idea about provenance and rebuilt
// regardless, so pressing Start could overwrite a config the Home screen had
// just said it would not touch. Installing it here — where the marker is parsed
// — keeps that rule in a single implementation while letting both engines
// (classic spawn and daemon apply) honour it.
func (b *Backend) installOwnershipPolicy() {
	if b.ac == nil {
		return
	}
	b.ac.SetConfigOwnershipPolicy(func() bool { return b.configIsRebuildable() })
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
//
// This is a product contract, not a historical capability list: it must
// describe what the shipped app can do. Remote machines and the config
// configurator were deliberately removed, so they are reported false even
// though core still contains the machinery — a capability that claims a
// deleted feature invites a client to offer UI that cannot work.
func (b *Backend) capabilities() protocol.Capabilities {
	return protocol.Capabilities{
		// The daemon engine exists on macOS and Windows (non-386).
		Daemon:        core.DaemonEngineAvailable(),
		Elevation:     core.ElevationSupported(),
		Traffic:       true,
		Subscriptions: true,
		// Platform-specific: importing a core needs a runnable-executable check
		// for this machine's format, so it is reported rather than assumed.
		CoreImport:              CoreImportSupported(),
		LocalSubscriptionImport: true,
		Remote:                  false,
		Configurator:            false,
	}
}

// Snapshot returns the complete initial state.
func (b *Backend) Snapshot() protocol.AppSnapshot {
	b.mu.Lock()
	seq := b.seq
	b.mu.Unlock()

	// One adoption attempt per snapshot, and only while ownership is UNKNOWN.
	// This is where a legacy config gets recognised: the user opens the app, the
	// snapshot is built, and a config that reproduces from our own state is
	// adopted before the UI ever asks what it is looking at. After the marker is
	// written the ownership read below returns MANAGED, so the cost is paid once.
	//
	// Deliberately here rather than at startup: provenance must not add work (or
	// a build) to every launch of an app whose config is already accounted for.
	b.adoptLegacyConfig()

	return protocol.AppSnapshot{
		SnapshotSeq: seq,
		Handshake:   b.Handshake(),
		Core:        b.coreState(),
		Settings:    b.settingsState(),
		Proxy:       b.proxySummary(),
	}
}

// proxySummary projects the current selection for the home screen.
//
// Read from the controller's cached state, so this costs nothing and does not
// touch the network. The full node list stays on the Proxies screen where it is
// actually needed.
func (b *Backend) proxySummary() protocol.ProxySummary {
	if b.ac == nil {
		return protocol.ProxySummary{Delay: delayNotMeasured}
	}

	summary := protocol.ProxySummary{Delay: delayNotMeasured}
	if b.ac.APIService != nil {
		summary.Group = b.ac.APIService.GetSelectedClashGroup()
		active := b.ac.APIService.GetActiveProxyName()
		summary.Proxy = active
		summary.ProxyDisplay = active
	}

	// The cached proxy list carries the last measured delay for the active
	// node, so the summary can show a latency without a fresh measurement.
	if active := summary.Proxy; active != "" {
		for _, p := range b.ac.GetProxiesList() {
			if p.Name == active && p.Delay > 0 {
				summary.Delay = p.Delay
				if d := p.DisplayOrName(); d != "" {
					summary.ProxyDisplay = d
				}
				break
			}
		}
	}
	return summary
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
	// The runtime transition decides, and the operation record only fills the
	// gaps the runtime cannot express: a start that has been accepted but has
	// not produced a running core yet, an in-flight stop, and a remembered
	// failure. Crucially the state is NOT derived from "is it running", which
	// is what made a successful click look like an immediate revert.
	state := b.coreLifecycleState()
	if !bs.BinaryExists && state == protocol.CoreStateStopped {
		// No core binary: an error the user must act on (install/point at a
		// binary). Still subordinate to running — a core that is up is up even
		// if the configured path later goes missing.
		state = protocol.CoreStateError
	}
	errCode, errMessage := b.coreErrorInfo()

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
		// Report ownership alongside staleness: the UI pairs "this is out of
		// date" with "and I can/cannot fix it", so they belong in one payload.
		ConfigRebuildable: b.configIsRebuildable(),
		ConfigOwnership:   string(b.configOwnership()),
		// ErrorCode is a STABLE token the frontend localizes; ErrorDetail is the
		// technical text for logs and the tooltip. Sending only a Go error
		// string would leave the UI with nothing translatable, which is why the
		// failure used to reach the screen as silence.
		ErrorCode:   errCode,
		ErrorDetail: errMessage,
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
	return b.runCoreOp("start", coreOpTimeout, func(ctx context.Context) error {
		return b.ac.StartVPNContext(ctx)
	})
}

// StopCore stops the sing-box core and publishes the resulting state.
func (b *Backend) StopCore() error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	debuglog.InfoLog("backend: stop_core requested")
	// Stop is deliberately NOT awaited: it tears down processes and the TUN
	// device, which can take a while, and the outcome is reported by the
	// runtime transition anyway. Only the pending flag and the initial
	// "stopping" state are published here.
	return b.runCoreOpFireAndForget("stop", func() { core.StopSingBoxProcess() })
}

// Shutdown releases backend resources. The core stop policy is unchanged:
// the same graceful-exit semantics the Fyne build used.
// Shutdown releases backend resources and asks the core to follow its exit
// policy. The core stop decision is unchanged from the Fyne build: classic
// stops the child process, daemon leaves the service running unless
// stop-on-exit is configured.
//
// Exactly-once, because there are two legitimate triggers and both are
// expected in a normal quit:
//
//  1. the `shutdown` IPC method, so the client gets an ACK first; and
//  2. stdin reaching EOF, which is the safety net for a frontend that died
//     without asking — it must not leave an orphan helper holding the core.
//
// A quit that sends `shutdown` and then closes the pipe hits BOTH. Without this
// guard the teardown would run twice: the shutting_down event emitted twice,
// the core watch cancelled twice, the traffic sampler stopped twice, and
// GracefulExit entered twice. GracefulExit happens to be idempotent internally,
// but relying on that would mean every future side effect added here must also
// be — a fragile invariant. The guard makes it true by construction.
func (b *Backend) Shutdown() {
	signal := b.shutdownSignal()
	b.shutdownOnce.Do(func() {
		defer close(signal)
		if b.ac == nil {
			return
		}
		started := time.Now()
		debuglog.InfoLog("backend: shutdown requested")

		if b.cancelCoreWatch != nil {
			b.cancelCoreWatch()
			b.cancelCoreWatch = nil
		}
		if b.traffic != nil {
			b.traffic.Stop()
		}
		b.emit(protocol.EventShuttingDown, nil)
		watchersDone := time.Now()

		b.ac.GracefulExit()
		// Per-phase timings go to the log only. They exist so "is this wait
		// necessary?" can be answered from evidence: a teardown that always
		// costs exactly the configured ceiling means the core stop is not being
		// observed, which is a state-transition bug rather than a timeout that
		// needs shortening.
		debuglog.InfoLog("backend: shutdown complete in %s (watchers %s, teardown %s)",
			time.Since(started).Round(time.Millisecond),
			watchersDone.Sub(started).Round(time.Millisecond),
			time.Since(watchersDone).Round(time.Millisecond))
	})
}

// IsShuttingDown reports whether Shutdown has begun, so a caller can avoid
// starting work the teardown is about to cancel.
func (b *Backend) IsShuttingDown() bool {
	select {
	case <-b.shutdownSignal():
		return true
	default:
		return false
	}
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
	return b.runCoreOp("restart", coreOpTimeout, func(ctx context.Context) error {
		return b.ac.RestartVPNContext(ctx)
	})
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
