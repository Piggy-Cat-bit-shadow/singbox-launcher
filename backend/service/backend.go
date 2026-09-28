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

	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	// opTimeoutOverride shortens the operation deadline for tests. Zero means the
	// production constant.
	opTimeoutOverride time.Duration

	mu sync.Mutex
	// settingsMu serialises settings.json read-modify-write. Separate from mu, which
	// guards the EVENT SEQUENCE: a settings save must not block event delivery, and an
	// emit under the sequence lock must not wait on a disk write.
	settingsMu sync.Mutex
	// seq is the monotonic event sequence. It lets the frontend discard an
	// event that predates the snapshot it already applied.
	//
	// It restarts at 1 for every backend process, which is why it is only ever
	// interpreted together with sessionID.
	seq int64
	// runCtx is the parent of every background operation's context, cancelled
	// once when the backend shuts down. See runContext.
	runCtxOnce sync.Once
	runCtx     context.Context
	runCancel  context.CancelFunc
	// sessionID identifies THIS backend process. It is generated once at
	// construction and is never persisted or reused.
	//
	// It exists because `seq` is a per-process counter: without a session, a
	// client that remembers a high-water mark across a helper restart discards
	// every event the new backend sends (the counter looks stale) or applies a
	// dead backend's late event (the counter looks current). Both are real
	// bugs, and they have opposite fixes, so the ambiguity must be removed
	// rather than guessed at.
	sessionID string
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

	// shutdownOnce makes Shutdown exactly-once; the two channels are the two
	// DISTINCT states teardown has, and conflating them was a defect:
	//
	//   shutdownBegan — closed at the START of Shutdown. Everything that must
	//                   stop doing work listens here.
	//   shutdownDone  — closed when the teardown has FINISHED. Only a caller that
	//                   needs the process to be quiescent waits here.
	//
	// A single channel closed by a `defer` inside Shutdown meant "finished", while
	// every consumer treated it as "begun". The visible consequence: for the whole
	// duration of the teardown the backend reported that it was NOT shutting down,
	// and the group latency test — whose context hung off that channel — was
	// cancelled only after GracefulExit had completely returned, by which time
	// cancelling it is meaningless because the provider is already gone.
	shutdownOnce  sync.Once
	shutdownBegan chan struct{}
	shutdownDone  chan struct{}
	shutdownInit  sync.Once

	// events serialises sequence assignment WITH delivery.
	//
	// A separate mutex from `mu` on purpose: a subscriber may re-enter the backend
	// (reading a snapshot, asking whether it is shutting down), so delivering under
	// the state lock would deadlock. Holding THIS lock across both steps makes the
	// numbers subscribers observe strictly increasing, which is the only thing that
	// makes the client's "discard seq <= my high-water mark" rule safe.
	eventMu    sync.Mutex
	dispatchMu sync.Mutex

	// runtimeCfg records which config content the RUNNING core loaded, so a
	// config/state divergence is detectable instead of silent.
	runtimeCfg runtimeConfig
}

// shutdownSignal lazily creates the shutdown channels.
//
// Lazily, because Backend is also constructed directly in tests as a zero value, and
// a nil channel would make both predicates report the wrong answer.
func (b *Backend) shutdownSignal() (began, done chan struct{}) {
	b.shutdownInit.Do(func() {
		if b.shutdownBegan == nil {
			b.shutdownBegan = make(chan struct{})
		}
		if b.shutdownDone == nil {
			b.shutdownDone = make(chan struct{})
		}
	})
	return b.shutdownBegan, b.shutdownDone
}

// ShutdownBegan is closed as soon as Shutdown starts.
//
// This is what background work must select on: a new operation, a proxy test, a
// subscription fetch or a daemon retry should refuse or abort the moment teardown
// begins, not when it has finished.
func (b *Backend) ShutdownBegan() <-chan struct{} {
	began, _ := b.shutdownSignal()
	return began
}

// ShutdownDone is closed when the teardown has completed.
func (b *Backend) ShutdownDone() <-chan struct{} {
	_, done := b.shutdownSignal()
	return done
}

// runContext returns a context that is cancelled when the backend shuts down.
//
// Every long-running background operation derives from this ONE context instead of
// building its own watcher over the shutdown channel. The previous shape created a
// context plus a "watch for shutdown and cancel" goroutine PER OPERATION, and
// returned only the context — so the watcher goroutine could not be released until
// the whole backend exited. A user who ran 200 latency tests accumulated 200 idle
// goroutines, each parked on a channel that would not close for hours.
//
// Deriving from a shared parent removes the need for a watcher entirely: the
// cancellation propagates through the context tree, so there is nothing to leak.
func (b *Backend) runContext() context.Context {
	b.runCtxOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		b.runCtx = ctx
		b.runCancel = cancel
		// The shutdown channel still exists for callers that select on it
		// directly; this bridge is created ONCE for the whole backend rather than
		// once per operation.
		go func() {
			// BEGAN, not done: every operation derived from this context must stop
			// when teardown STARTS.
			began, _ := b.shutdownSignal()
			<-began
			cancel()
		}()
	})
	return b.runCtx
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
	b := &Backend{ac: ac, sessionID: newSessionID()}
	// THE HOOK GOES FIRST, and the order is the whole point.
	//
	// installOwnershipPolicy ADOPTS OR REBUILDS a config at construction time, which
	// promotes one. Installing the hook after it meant the very first promotion of
	// every launch happened with no hook, so the marker that is supposed to make
	// "no config reaches disk undescribed" true was absent exactly for the file the
	// process had just written. Nothing about that is visible in either function; it
	// is a property of their order, which is why the comment is here rather than
	// there.
	installConfigPromotionProvenance(b)
	b.installOwnershipPolicy()
	b.watchCoreState()
	// Let the runtime settle any operation still in flight as the app exits. The
	// record lives here and the exit path lives in core, so core calls back
	// through this registration rather than importing the IPC layer.
	ac.RegisterExitSettler(b.settleOperationsAtExit)
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
// settleOperationsAtExit clears any operation record still in flight as the
// application exits, so a stop cannot outlive the process that owns its record.
//
// Called from GracefulExit after every stop path has had its chance. It does NOT
// touch the running state: whatever the stop paths established stands, so a core
// that survived the stop is still reported as running to anyone who asks before
// the process ends.
func (b *Backend) settleOperationsAtExit() {
	if b == nil {
		return
	}
	if op := b.ops.snapshotOp(); op != nil {
		debuglog.WarnLog("backend: exiting with %s (id=%d) still in flight; clearing the record",
			op.kind, op.id)
		if settled := b.ops.finishOp(op, nil); settled == settleCommitted {
			b.EmitCoreState()
		}
	}
}

func (b *Backend) watchCoreState() {
	if b.ac == nil || b.ac.EventBus == nil {
		return
	}
	b.cancelCoreWatch = b.ac.EventBus.Subscribe(events.VpnStateChanged, func(ev events.Event) {
		running := false
		teardown := events.TeardownNone
		if p, ok := ev.Payload.(events.VpnStateChangedPayload); ok {
			running = p.Running
			teardown = p.Teardown
			// Run the sampler exactly while the core is up: a menu bar should
			// not keep polling a socket that is not listening.
			if running {
				b.Traffic().Start()
				// Capture WHICH config just went live. This is the only moment the
				// answer is knowable, and it is what lets the proxy surfaces tell the
				// difference between "the core is serving what is on disk" and "the
				// core is serving what was on disk when it started".
				b.recordRunningConfig()
			} else {
				b.Traffic().Stop()
				// Nothing is live, so there is no runtime config to diverge from.
				b.runtimeCfg.clear()
			}
		}

		// A stop operation ENDS here, on the authoritative runtime transition.
		//
		// This is the terminal state the stop operation previously never
		// reached: `runCoreOpFireAndForget` registered the operation and returned
		// without ever clearing it, so `op == stop` stayed set forever and the
		// wire state reported `stopping` for the rest of the session (or until
		// some unrelated operation happened to overwrite the record).
		//
		// The transition alone is not enough to decide: a CRASH also moves the
		// running flag from true to false. Only a deliberate stop may end a stop
		// operation, so `completeStop` requires that a stop operation is actually
		// the current one — a crash leaves no stop operation to settle, and the
		// crash path keeps its own decision about restarting.
		if !running {
			// An in-flight latency test is measuring through a transport that no
			// longer exists. Cancel it here rather than letting it run out its
			// per-node timeouts against a dead socket: the results would be
			// meaningless, and the work would keep the process busy during Quit.
			//
			// This is the wiring `CancelActive` documented but never had. The
			// method existed with a comment claiming it was called on core stop,
			// engine switch and shutdown, and grep found zero callers — so a core
			// killed externally, a daemon FATAL, a mode switch and a backend
			// shutdown all left the test running.
			b.groupTests.CancelActive()

			// The REASON travels with the transition, because "the flag went
			// false" cannot distinguish a user stop from a restart's teardown —
			// and the two require opposite responses.
			b.completeStop(string(teardown))
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
	// THE SEQUENCE MUST DESCRIBE THE CONTENT, NOT AN EARLIER MOMENT.
	//
	// This read the sequence under the lock, released it, and only then built the
	// snapshot from separately-read fields. The number therefore described a moment
	// BEFORE any of the content was captured, so a change landing in between appeared
	// in the snapshot without being reflected in its sequence — and a client comparing
	// that number against incoming events could neither accept the state as current nor
	// know it was newer. The version check was unusable in exactly the case it exists
	// for.
	//
	// Capturing both under one critical section makes the number a real version point.
	// The fields are cheap reads, so holding the lock across them costs nothing
	// measurable and removes the window entirely.
	b.mu.Lock()
	seq := b.seq
	handshake := b.Handshake()
	core := b.coreState()
	settings := b.settingsState()
	proxy := b.proxySummary()
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
		Session:     b.SessionID(),
		Handshake:   handshake,
		Core:        core,
		Settings:    settings,
		Proxy:       proxy,
	}
}

// newSessionID mints an identifier for one backend process.
//
// Random rather than derived from the pid: a pid is recycled by the OS, and a
// frontend that reconnects to a NEW process that happens to have the OLD
// process's pid would then accept the previous session's sequence numbers —
// reintroducing exactly the confusion the session exists to remove.
func newSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// A session id must still be unique even if the entropy source fails.
		// The clock plus the pid is weaker, but it is strictly better than an
		// empty string, which would make every process look like the same
		// session and silently disable the defence.
		debuglog.WarnLog("backend: cannot read random session id: %v", err)
		return fmt.Sprintf("fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// SessionID reports the identifier of this backend process.
//
// Stable for the lifetime of the process and different in every new one, so a
// client can tell "the backend I am talking to" from "a backend that has been
// replaced". A client MUST reset its event high-water mark when this changes.
func (b *Backend) SessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Minted on first use rather than only in New, so that EVERY construction
	// path has a session. A zero-value Backend (built directly by a test, or by
	// any future caller) would otherwise report an empty session, and an empty
	// session compares equal to another empty session — which silently disables
	// the very defence this field provides.
	//
	// Still stable for the process's lifetime: it is written once, under the
	// same lock every reader takes.
	if b.sessionID == "" {
		b.sessionID = newSessionID()
	}
	return b.sessionID
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
	// The unified lifecycle error store is the SECOND source, and it is the one
	// that survives: coreErrorInfo covers the operation record (a start that
	// failed in this session), while the store also holds failures produced by
	// paths that never run an operation — a rebuild rejected by the core, a
	// refused reload, an authorization that was cancelled. Without this a
	// rejected config reached the UI as a healthy "connected" state.
	lifecycle := b.ac.LifecycleError()
	if lifecycle != nil {
		if errCode == "" {
			errCode = string(lifecycle.Code)
			errMessage = lifecycle.Detail
		}
		// A recorded failure makes the state an error even when nothing is
		// running, which is the whole point: the user must see why.
		if state == protocol.CoreStateStopped {
			state = protocol.CoreStateError
		}
	}

	version, err := b.ac.GetInstalledCoreVersion()
	if err != nil {
		version = ""
	}

	backend := backendIdentity(b.ac)

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
		ErrorMessage: func() string {
			if lifecycle != nil {
				return lifecycle.Message
			}
			return ""
		}(),
		Recoverable: func() bool { return lifecycle != nil && lifecycle.Recoverable }(),
		Operation:   func() string { return lifecycleOp(lifecycle) }(),
		ConfigError: func() string {
			if lifecycle != nil && lifecycle.ConfigError {
				return lifecycle.Message
			}
			return ""
		}(),
	}
}

// lifecycleOp returns the operation name for a recorded failure, or "".
func lifecycleOp(e *core.LifecycleErrorSnapshot) string {
	if e == nil {
		return ""
	}
	return e.Operation
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

// backendIdentity names the ACTIVE engine, as a wire string.
//
// IDENTITY AND PERSISTENCE ARE INDEPENDENT AXES. This used to be derived from
// `CorePersistsAfterAppExit()`, which answers a completely different question:
// "when the launcher quits, does the core keep running?" Conflating them meant
// that a user on the daemon engine who switched OFF "keep running after quit"
// was reported as running the CLASSIC engine — the identity flipped because a
// shutdown preference changed, while the actual engine never moved.
//
// That is not cosmetic. The frontend reads this field as `activeEngine` and
// drives engine-specific UI from it: the wrong value shows the Classic engine
// while the daemon is serving, mislabels the mode screen, and makes every
// button whose behaviour depends on the engine act on the wrong one.
//
// The engine's own Mode() is the only authoritative answer, and BackendMode()
// reads it from the live backend object rather than from any stored preference.
func backendIdentity(ac *core.AppController) string {
	if ac == nil {
		return string(core.BackendClassic)
	}
	return string(ac.BackendMode())
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

	// MOTIME IS NOT A CONSISTENCY SOURCE, and the one-second tolerance made the
	// check miss real edits: a config built and then modified 500 ms later has a
	// NEWER state than config, but the difference is under a second, so the
	// launcher reported "not stale" and never offered the rebuild.
	//
	// The tolerance existed to absorb same-instant write ordering during a
	// rebuild. That is a DISPLAY concern, and the durable answer is a content
	// identity rather than a clock: if the recorded build revision differs from
	// the state on disk, the config is out of date no matter how close the two
	// timestamps are.
	//
	// mtime remains as a LEGACY fallback for installs that predate the revision
	// marker, where no revision has been recorded yet. It is deliberately
	// conservative there: a strict inequality with no tolerance, because a false
	// "stale" only offers a rebuild, while a false "fresh" hides one.
	if stale, known := core.ConfigIsStaleVersusRecorded(b.ac.FileService.ConfigPath); known {
		return stale
	}
	return stateInfo.ModTime().After(configInfo.ModTime())
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
	// SEQUENCE ASSIGNMENT AND DELIVERY ARE ONE OPERATION.
	//
	// They used to be two: the number was taken under `mu`, the lock was released,
	// and only then were subscribers called. Two goroutines could therefore be
	// numbered 10 and 11 in order and delivered 11-then-10 — and the client's rule
	// "discard any event whose seq is <= my high-water mark" then threw away the
	// lower one as stale. A lost `core_state_changed` overtaken by a
	// `settings_changed` leaves the UI on a stale core state until something
	// unrelated re-emits it.
	//
	// The fix is NOT to deliver under `mu`: a subscriber may re-enter the backend,
	// and that would deadlock. It is to give sequence-plus-delivery its own lock,
	// so the two steps are atomic with respect to each other while remaining
	// independent of the state mutex.
	b.eventMu.Lock()
	defer b.eventMu.Unlock()

	b.mu.Lock()
	b.seq++
	// The session is stamped here, under the same lock that hands out the
	// sequence, so the pair (session, seq) is always consistent: no event can
	// carry one process's sequence under another process's identity.
	if b.sessionID == "" {
		b.sessionID = newSessionID()
	}
	ev := protocol.Event{Event: name, Seq: b.seq, Session: b.sessionID, Payload: payload}
	subs := make([]func(protocol.Event), 0, len(b.subscribers))
	for _, fn := range b.subscribers {
		if fn != nil {
			subs = append(subs, fn)
		}
	}
	b.mu.Unlock()

	// Delivered while `eventMu` is held, so no later-numbered event can overtake.
	// Callers that want to observe this ordering need no additional synchronisation.
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
	return b.runCoreOp("start", b.opTimeout(), func(ctx context.Context) error {
		return b.ac.StartVPNContext(ctx)
	})
}

// StopCore stops the sing-box core and publishes the resulting state.
func (b *Backend) StopCore() error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	debuglog.InfoLog("backend: stop_core requested")
	// Stop is deliberately NOT awaited on this thread: it tears down processes
	// and the TUN device, which can take a while. The operation's terminal state
	// comes from the runtime transition, and a failure is reported through the
	// contextual stop — which is how a headless frontend learns that the tunnel
	// is still up instead of seeing a state that never settles.
	//
	// The contextual path carries the REASON; the runtime transition carries the
	// COMPLETION. Neither alone is enough.
	return b.runCoreOpFireAndForget("stop", func(ctx context.Context) error {
		return b.ac.StopVPNContext(ctx)
	})
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
	began, done := b.shutdownSignal()
	b.shutdownOnce.Do(func() {
		// THE ORDER OF THESE TWO CLOSES IS THE FIX.
		//
		// `began` closes FIRST, before any teardown work, so every consumer that
		// must stop doing work — the run context, the group latency test, new
		// operations — is released immediately. `done` closes at the END, for
		// callers that genuinely need the process quiescent. Previously one channel
		// closed only at the end and was read as "begun" by everyone.
		close(began)
		defer close(done)
		if b.ac == nil {
			return
		}
		started := time.Now()
		debuglog.InfoLog("backend: shutdown requested")

		// A latency test must not hold up the exit. Cancel it BEFORE the teardown
		// begins, so its workers stop probing rather than being waited on.
		//
		// This belongs here rather than relying on the core-stop event above: the
		// daemon backend can leave the core RUNNING across an app exit, so no
		// "core stopped" transition is guaranteed to arrive — and the test would
		// then outlive the shutdown that was supposed to cancel it.
		b.groupTests.CancelActive()

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
	began, _ := b.shutdownSignal()
	select {
	case <-began:
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
	return b.runCoreOp("restart", b.opTimeout(), func(ctx context.Context) error {
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

	binDir := b.ac.FileService.Layout.Data.Bin()
	previous := b.ac.BackendMode()
	st := locale.LoadSettings(binDir)
	// The EXACT prior text, not the resolved engine name: "" means "unset" and is
	// a different state from "classic", even though both resolve to classic. A
	// rollback that writes the resolved name would mutate a setting the user
	// never changed.
	previousStored := st.CoreBackendMode

	// PERSIST FIRST, THEN SWITCH. The order IS the transaction.
	//
	// This used to switch the engine and save afterwards, so a failed save left
	// the launcher running the NEW engine in memory and the OLD one on disk: the
	// frontend received an error while the app was demonstrably on the daemon,
	// and reopening it silently reverted to classic. Runtime and settings
	// disagreed and nothing could say which was authoritative.
	//
	// Saving first inverts the failure mode into the safe one — if the choice
	// cannot be stored, the switch never happens, so what the user sees and what
	// is on disk are the same engine.
	if st.CoreBackendMode != mode {
		st.CoreBackendMode = mode
		if err := locale.SaveSettings(binDir, st); err != nil {
			debuglog.WarnLog("backend: cannot persist the core mode: %v", err)
			return &protocol.Error{
				Code: "persist_failed",
				Message: "the engine was NOT switched, because saving the choice failed: " +
					err.Error(),
				Recoverable: true,
			}
		}
	}

	// A latency test is measuring through the engine that is about to be replaced, so
	// it must be cancelled — but ONLY IF THE SWITCH ACTUALLY HAPPENS.
	//
	// This cancelled before the switch, and the switch can legitimately be REFUSED
	// ("stop the VPN before switching"). A refused mode change therefore killed a
	// running latency test for nothing: the user clicked a control that did nothing,
	// and lost up to a full group-test network budget in the process. Cancelling after
	// success keeps the reason intact — the test is stopped because its engine is
	// going away, which is only true once the engine really is going away.
	if err := b.ac.SwitchBackendMode(core.BackendMode(mode)); err != nil {
		// The switch was refused (busy engine, unreachable daemon, …). Roll the
		// persisted choice back: the mirror image of the old divergence would be a
		// settings file naming an engine the launcher is not running.
		if st.CoreBackendMode != previousStored {
			st.CoreBackendMode = previousStored
			if saveErr := locale.SaveSettings(binDir, st); saveErr != nil {
				debuglog.ErrorLog("backend: mode switch refused AND the rollback failed: %v", saveErr)
				b.emit(protocol.EventSettingsChanged, b.settingsState())
				return &protocol.Error{
					Code: "persist_failed",
					Message: "the engine did not switch and the choice could not be restored: " +
						saveErr.Error() + " (the engine is still " + string(previous) + ")",
					Recoverable: true,
				}
			}
		}
		// The running-state refusal is an ordinary, expected outcome — the
		// frontend shows "stop the core first", not a crash report.
		return &protocol.Error{
			Code:        "mode_locked",
			Message:     err.Error(),
			Recoverable: true,
		}
	}

	// The engine really is going away, so a test measuring through it must stop.
	b.groupTests.CancelActive()

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
// updateSettings serialises one settings.json read-modify-write.
//
// settings.json is a whole-file format, so two concurrent mutations is not a lost
// update but a corrupted file: each save is a truncate-and-write the other can walk
// into. Requests are dispatched CONCURRENTLY, so this is reachable the moment two
// setters are in flight together — which is why the lock belongs here, at the one place
// that performs the write, rather than at each handler that happens to call it.
//
// The lock covers the LOAD as well as the save. Locking only the save lets this write
// back a snapshot it read before another toggle landed, so one of two changes silently
// disappears while both report success.
func (b *Backend) updateSettings(mutate func(*locale.Settings)) error {
	if b.ac == nil {
		return &protocol.Error{Code: "not_ready", Message: "backend not initialised", Recoverable: true}
	}
	b.settingsMu.Lock()
	defer b.settingsMu.Unlock()
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
