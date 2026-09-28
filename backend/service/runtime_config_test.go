package service

import (
	"os"
	"path/filepath"
	"testing"

	"singbox-launcher/core"
	"singbox-launcher/core/events"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/paths"
)

// TestRunningCoreReportsTheConfigItActuallyLoaded is statement 10 (§34 name).
//
// The Clash API answers about the config the RUNNING core was started with, while the
// group picker reads config.json from disk. After a rebuild those are different
// documents: the picker lists groups the live core does not have, and switching to one
// fails with an error naming a group the user can see on screen.
//
// Nothing compared the two, so the divergence was invisible and lasted until the user
// happened to restart. The identity of the live config is now captured at the running
// transition and the disagreement is reported.
func TestRunningCoreReportsTheConfigItActuallyLoaded(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[]}`)

	// Nothing is running, so there is nothing to diverge from.
	if matches, running := b.ConfigMatchesRuntime(); running || matches {
		t.Fatalf("a stopped core reported running=%v matches=%v; with nothing live there "+
			"is no runtime config and callers must fall back to the file", running, matches)
	}

	// The core starts with what is on disk.
	b.recordRunningConfig()
	matches, running := b.ConfigMatchesRuntime()
	if !running {
		t.Fatal("a recorded runtime config did not report a running core, so no caller " +
			"can ever detect a divergence")
	}
	if !matches {
		t.Fatal("the config that was just recorded does not match itself")
	}

	// A rebuild rewrites config.json while the core keeps running the old document.
	if err := os.WriteFile(b.ac.FileService.ConfigPath,
		[]byte(`{"outbounds":[{"type":"selector","tag":"NewGroup"}]}`), 0o644); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	matches, running = b.ConfigMatchesRuntime()
	if !running {
		t.Fatal("the core is still running")
	}
	if matches {
		t.Fatal("config.json was rewritten after the core started, but the runtime config " +
			"still reports a match — the app cannot tell that the live core is serving the " +
			"previous document, so the picker offers groups the core does not have")
	}
	if !b.RuntimeConfigDiverged() {
		t.Fatal("the divergence is not reported, so nothing can offer the user a restart")
	}
}

// TestRuntimeConfigClearedWhenCoreStops — a stopped core has no runtime identity, so the
// stale hash must not keep reporting a divergence forever.
func TestRuntimeConfigClearedWhenCoreStops(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[]}`)
	b.recordRunningConfig()

	if err := os.WriteFile(b.ac.FileService.ConfigPath, []byte(`{"outbounds":[1]}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !b.RuntimeConfigDiverged() {
		t.Fatal("a divergence while running was not detected")
	}

	// The core stops. On the next start it will load whatever is on disk, so there is
	// nothing to be inconsistent with.
	b.runtimeCfg.clear()
	if b.RuntimeConfigDiverged() {
		t.Fatal("a stopped core still reports a divergent runtime config; the flag would " +
			"never clear and the UI would offer a restart forever")
	}
}

// TestConfigMatchesRuntimeDetectsAPathChange — the same bytes at a different path is still
// a different runtime config, because the core was told to load the other one.
func TestConfigMatchesRuntimeDetectsAPathChange(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[]}`)
	b.recordRunningConfig()

	_, running := b.ConfigMatchesRuntime()
	if !running {
		t.Fatal("expected a running core")
	}

	// Simulate the layout moving the config underneath the recorded identity.
	body, rerr := os.ReadFile(b.ac.FileService.ConfigPath)
	if rerr != nil {
		t.Fatalf("read config: %v", rerr)
	}
	b.runtimeCfg.set("/some/other/config.json", hashConfigBytes(body), body)
	if matches, running := b.ConfigMatchesRuntime(); !running || matches {
		t.Fatal("a runtime config recorded at a different path reported a match; the core " +
			"was started with the other file")
	}
}

// TestProxyListReportsRuntimeRestartRequired is statement 11 (§34 name).
//
// The group names come from config.json and the live selection comes from the Clash API.
// When those describe different documents the response must SAY so, because a list that
// silently mixes them is a list the user cannot trust.
func TestProxyListReportsRuntimeRestartRequired(t *testing.T) {
	b := newTestBackendWithConfig(t, `{
		"outbounds":[{"type":"selector","tag":"Proxy","outbounds":["a"]}],
		"route":{"final":"Proxy"}
	}`)

	// Not running: no divergence, and the file is the truth.
	list, err := b.ProxyGroups()
	if err != nil {
		t.Fatalf("ProxyGroups while stopped: %v", err)
	}
	if list.RuntimeRestartRequired {
		t.Error("a stopped core reported that a restart is required; with nothing running " +
			"the file on disk is what will be loaded next")
	}

	// The core starts, then the config changes underneath it.
	b.recordRunningConfig()
	if err := os.WriteFile(b.ac.FileService.ConfigPath, []byte(`{
		"outbounds":[{"type":"selector","tag":"Proxy","outbounds":["a"]}],
		"route":{"final":"Proxy"},
		"experimental":{"clash_api":{"external_controller":"127.0.0.1:9090"}}
	}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	list, err = b.ProxyGroups()
	if err != nil {
		t.Fatalf("ProxyGroups: %v", err)
	}
	if !list.RuntimeRestartRequired {
		t.Fatal("config.json changed after the core started, but the proxy list does not " +
			"report that a restart is required — the UI will present groups read from a " +
			"document the live core never loaded")
	}
}

// TestDivergenceIsNotReportedWhenContentIsEquivalent — a re-serialised but identical
// config must not trigger a restart prompt.
func TestDivergenceIsNotReportedWhenContentIsEquivalent(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[],"route":{"final":"direct"}}`)
	b.recordRunningConfig()

	// Same document, different formatting.
	if err := os.WriteFile(b.ac.FileService.ConfigPath,
		[]byte("{\n  \"outbounds\": [],\n  \"route\": {\"final\": \"direct\"}\n}\n"), 0o644); err != nil {
		t.Fatalf("reformat: %v", err)
	}

	if b.RuntimeConfigDiverged() {
		t.Error("a reformatted but equivalent config was reported as divergent; the user " +
			"would be told to restart for a change that does not exist")
	}
}

// --- helpers ---

// newTestBackendWithConfig builds a backend whose config.json holds the given document.
//
// Mirrors backendWithConfig, but for a document the test chooses: the divergence tests
// need to rewrite the config AFTER the core started, so the fixture has to be under their
// control rather than a fixed string.
func newTestBackendWithConfig(t *testing.T, config string) *Backend {
	t.Helper()
	dir := t.TempDir()
	layout, err := paths.Resolve(filepath.Join(dir, "jiejiebox-backend"),
		func(key string) string {
			if key == constants.EnvDataDir {
				return filepath.Join(dir, "data")
			}
			return ""
		}, "darwin", func(string) bool { return true })
	if err != nil {
		t.Skipf("cannot resolve a temp layout: %v", err)
	}
	if err := os.MkdirAll(layout.Data.Bin(), 0o755); err != nil {
		t.Fatalf("cannot create the temp data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout.Data.Bin(), "config.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("cannot write the config fixture: %v", err)
	}

	ac, err := core.NewAppController(layout, nil, nil, nil, nil)
	if err != nil {
		t.Skipf("cannot build a controller in this environment: %v", err)
	}
	b := &Backend{ac: ac}
	b.installOwnershipPolicy()
	b.watchCoreState()
	return b
}

// TestALifecycleRefreshDoesNotClearTheDivergence is statement 15's own main path, and the
// case that made the original fix fail in practice.
//
// The runtime config was captured on ANY `VpnStateChanged` with Running==true. But that event
// is published for refreshes too — a recorded lifecycle error clearing, a late privileged
// adoption, the lifecycle picture being re-published — and NONE of those loads a config. On a
// refresh the capture re-read the CURRENT config.json and recorded it as the document the
// running core had loaded.
//
// That silently CLEARS the divergence. The exact sequence:
//
//  1. the core starts on config A;
//  2. a rebuild promotes config B to disk while the core keeps serving A;
//  3. an unrelated lifecycle refresh fires;
//  4. B is recorded as live, so `RuntimeConfigDiverged()` goes false;
//  5. the proxy surfaces stop reporting that a restart is needed, and the user — who is
//     looking at a B that the core is not serving — is never told.
//
// The tests that shipped with the fix called `recordRunningConfig()` DIRECTLY, so they never
// exercised the transition wiring and could not see any of this. This one drives the event.
func TestALifecycleRefreshDoesNotClearTheDivergence(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configA := []byte(`{"outbounds":[{"type":"direct","tag":"A"}]}`)
	configB := []byte(`{"outbounds":[{"type":"direct","tag":"B"}]}`)
	if err := os.WriteFile(configPath, configA, 0o644); err != nil {
		t.Fatalf("write config A: %v", err)
	}

	b := newTestBackendWithConfig(t, string(configA))
	b.ac.FileService.ConfigPath = configPath

	// 1. The core starts on A: the transition that carries StartedHere.
	startEvent := events.Event{
		Kind: events.VpnStateChanged,
		Payload: events.VpnStateChangedPayload{
			Running: true, StartedHere: true,
		},
	}
	b.handleCoreStateEvent(startEvent)

	if diverged := b.RuntimeConfigDiverged(); diverged {
		t.Fatal("the fixture diverged immediately; the test would prove nothing")
	}

	// 2. A rebuild promotes B to disk while the core keeps serving A.
	if err := os.WriteFile(configPath, configB, 0o644); err != nil {
		t.Fatalf("write config B: %v", err)
	}
	if !b.RuntimeConfigDiverged() {
		t.Fatal("the promoted config was not detected as diverged; the fixture is wrong")
	}

	// 3. An unrelated lifecycle REFRESH fires — Running is still true, nothing was loaded.
	refresh := events.Event{
		Kind: events.VpnStateChanged,
		Payload: events.VpnStateChangedPayload{
			Running: true, StartedHere: false,
		},
	}
	b.handleCoreStateEvent(refresh)

	// 4. The divergence must SURVIVE the refresh.
	if !b.RuntimeConfigDiverged() {
		t.Error("a lifecycle refresh CLEARED the divergence between the config the core " +
			"loaded and the one on disk. Nothing was loaded by that event, so recording the " +
			"current file as live makes the app claim the core is serving a config it has " +
			"never read — and the restart prompt disappears")
	}

	// 5. And a genuine restart DOES re-capture, or the fix would be "never recapture".
	b.handleCoreStateEvent(startEvent)
	if b.RuntimeConfigDiverged() {
		t.Error("a real start did not re-capture the config, so the record could never " +
			"follow a legitimate restart")
	}
}

// TestAReassertedRunningStateDoesNotClearTheDivergence — the same bug through a SECOND door.
//
// The fix above keyed `recordRunningConfig` on `StartedHere`, and `StartedHere` was derived
// from the running VALUE: the reasoning was that `set` dedups no-op writes, so reaching the
// publish with `true` means the flag just changed, and therefore a core came up. The dedup
// does guarantee the CHANGE; it does not guarantee the CAUSE.
//
// `core/lifecycle_error.go` calls `RunningState.Set(true)` when a daemon STOP cannot be
// confirmed — the core may still be up, so the running flag is restated to stop a stale
// "stopped" from reaching the UI. That is a genuine false→true transition produced by a
// statement about a belief, so it sailed past the dedup and published `StartedHere: true`.
// Everything the first fix prevented therefore remained reachable through the failure path of
// a stop: the backend re-read the CURRENT config.json and recorded it as what the core had
// loaded, clearing the divergence.
//
// The lesson is in the shape of the fix, not the trigger: `StartedHere` is now STATED by the
// publisher. `SetReasserted` is the entry point for "I believe it is still running", and it is
// the one the unconfirmed-stop path uses.
func TestAReassertedRunningStateDoesNotClearTheDivergence(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configA := []byte(`{"outbounds":[{"type":"direct","tag":"A"}]}`)
	configB := []byte(`{"outbounds":[{"type":"direct","tag":"B"}]}`)
	if err := os.WriteFile(configPath, configA, 0o644); err != nil {
		t.Fatalf("write config A: %v", err)
	}

	b := newTestBackendWithConfig(t, string(configA))
	b.ac.FileService.ConfigPath = configPath

	// Now the LOAD-BEARING part: the daemon stop could not be confirmed, so the running
	// state is RE-ASSERTED.
	//
	// THE EVENT IS PRODUCED BY THE REAL CONTROLLER, NOT HAND-BUILT. The first version
	// constructed `{Running: true, StartedHere: false}` itself and asserted the consumer's
	// behaviour — so it PASSED with `StartedHere: value` restored in `set` AND
	// `lifecycle_error.go` back on `Set(true)`, i.e. with the entire bug present. A test that
	// supplies the correct input cannot observe a bug in the code that produces it.
	//
	// This drives the actual publisher: a core is recorded as running, then a daemon stop
	// fails to confirm, and whatever the controller PUBLISHES is what the backend receives.
	bus := b.ac.EventBus
	if bus == nil {
		t.Fatal("the controller has no event bus, so the publisher cannot be driven")
	}

	// A REAL transition is required, and `set` DEDUPS a no-op write — it returns early when
	// `running` already equals the new value and publishes nothing. That dedup is why an
	// earlier version of this test proved nothing: it set the running flag to `true` and then
	// asked for another `true`, so `SetReasserted` did nothing at all and the assertion below
	// held regardless of what the publisher would have said.
	//
	// The real sequence: the core is up and on A, the running flag is CLEARED (a stop that
	// then failed to confirm), and the re-assertion restores it. That is exactly when the
	// daemon path calls `SetReasserted`, and unlike the no-op it publishes.
	b.ac.RunningState.Set(true)
	b.ac.RunningState.Set(false)

	// The backend must have recorded A, so the fixture is the real one: a live core serving
	// A. `StartedHere: true` is the start claim the SDK makes on a genuine load.
	b.handleCoreStateEvent(events.Event{
		Kind:    events.VpnStateChanged,
		Payload: events.VpnStateChangedPayload{Running: true, StartedHere: true},
	})
	if err := os.WriteFile(configPath, configB, 0o644); err != nil {
		t.Fatalf("write config B: %v", err)
	}
	if !b.RuntimeConfigDiverged() {
		t.Fatal("the fixture is wrong: no divergence before the re-assertion")
	}

	// The backend subscribes to this bus ITSELF (`backend.go`, `cancelCoreWatch`), and it is
	// registered BEFORE this observer. That is deliberate and load-bearing for the test: the
	// backend's own reaction must be what the assertion reads, not a replay. The previous
	// version replayed the recorded payload through `handleCoreStateEvent` — but the reverted
	// backend had ALREADY cleared the divergence when the bus delivered, and replaying a
	// corrected payload afterwards could not resurrect it. The test therefore passed against
	// the bug. Observing is not participating.
	var published []events.VpnStateChangedPayload
	cancel := bus.Subscribe(events.VpnStateChanged, func(ev events.Event) {
		if p, ok := ev.Payload.(events.VpnStateChangedPayload); ok {
			published = append(published, p)
		}
	})
	defer cancel()

	// `EndDaemonStop(owner, confirmed=false)` is what a failed daemon stop calls.
	b.ac.EndDaemonStop(nil, false)

	if len(published) == 0 {
		t.Fatal("the unconfirmed daemon stop published no state change at all; the path " +
			"under test was not exercised")
	}
	for _, p := range published {
		if p.StartedHere {
			t.Fatalf("the unconfirmed daemon stop published StartedHere=true. Nothing "+
				"started: the flag records a BELIEF that the core is still up, and claiming "+
				"a start here lets the backend re-read config.json and clear the divergence "+
				"(published: %+v)", p)
		}
	}

	// NO REPLAY. The backend already consumed the event through its own subscription when the
	// bus delivered it, so the state below is the state the shipped code produced.

	if !b.RuntimeConfigDiverged() {
		t.Error("a re-asserted running state cleared the divergence. Nothing started and " +
			"no config was loaded, so the record of what the live core is serving must " +
			"not have been rewritten from the file — that is the entire condition the " +
			"record exists to report, and the user is now never told a restart is needed")
	}
}

// TestOnlyARealStartClaimsStartedHere pins the publisher side, which is where the bug lived.
//
// The backend test above drives the EVENT; this one checks that the CONTROLLER produces the
// right event, because a hand-built payload can only prove what the consumer does with it.
// Deriving the flag from the value is the mistake, and only a source-level check of the call
// sites catches a future `Set(true)` that means "I believe it is running".
func TestOnlyARealStartClaimsStartedHere(t *testing.T) {
	src := readServiceSource(t, "core/controller.go")
	body := functionBodyForTest(t, src, "func (r *RunningState) set(")

	if !contains(body, "startedHere") {
		t.Fatal("`set` no longer receives a `startedHere` argument, so the published flag " +
			"must be derived from the value again — which is what let an unconfirmed " +
			"daemon stop be reported as a start")
	}
	if contains(body, "StartedHere: value") {
		t.Error("`set` derives `StartedHere` from the running value. The dedup guarantees " +
			"the flag CHANGED, not that a core STARTED, and the unconfirmed-stop path " +
			"produces the same false→true transition")
	}

	// `SetReasserted` must exist and must not claim a start.
	reassert := functionBodyForTest(t, src, "func (r *RunningState) SetReasserted()")
	if !contains(reassert, "false") {
		t.Error("`SetReasserted` does not pass a false `startedHere`, so it still claims a " +
			"core started")
	}

	// And the unconfirmed-stop path must USE it, or the entry point is decoration.
	lifecycle := readServiceSource(t, "core/lifecycle_error.go")
	if !contains(lifecycle, "SetReasserted()") {
		t.Error("the unconfirmed daemon stop does not use `SetReasserted`, so it still " +
			"publishes `StartedHere: true` for a core that did not start")
	}
	if contains(lifecycle, "RunningState.Set(true)") {
		t.Error("the unconfirmed daemon stop still calls `Set(true)`, which claims a start")
	}
}

// drainPublished converts the recorded payloads into events and empties the slice.
func drainPublished(ps *[]events.VpnStateChangedPayload) []events.Event {
	out := make([]events.Event, 0, len(*ps))
	for _, p := range *ps {
		out = append(out, events.Event{Kind: events.VpnStateChanged, Payload: p})
	}
	*ps = nil
	return out
}
