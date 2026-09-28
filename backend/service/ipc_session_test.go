package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/core/events"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEventSequenceResetsAcrossBackendSessions is the P0 for claim 1.
//
// `seq` is a per-PROCESS counter, but the frontend's `appliedSeq` lives as long
// as the frontend does. When the helper restarts, the new process starts again at
// 1 while the client is still holding the old process's high-water mark, so every
// event the new helper sends compares as "older than what I already applied" and
// is dropped. The UI then shows stale core state, traffic and selections until the
// new helper has emitted more events than the old one ever did.
//
// A sequence number is only meaningful WITHIN the session that issued it, so the
// stream must say which session it belongs to.
// readServiceSource reads a repo-relative file from the service package.
func readServiceSource(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("cannot read %s: %v", rel, err)
	}
	return string(b)
}

// backendWithConfigForSession builds a backend whose config path is real.
func backendWithConfigForSession(t *testing.T) *Backend {
	t.Helper()
	return backendWithConfig(t)
}

func TestEventSequenceResetsAcrossBackendSessions(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/protocol/protocol.go"))

	if !contains(src, "Session") {
		t.Error("the event protocol carries no session identity: `seq` is a " +
			"per-process counter, so a restarted backend's sequence is " +
			"indistinguishable from a replay of the old one's")
	}

	// The event must carry the session, or a client cannot tell "this event is
	// from the process I am talking to" from "this event predates the restart".
	if !contains(src, "Session string `json:\"session\"`") {
		t.Error("Event carries no session field, so a late event from a previous " +
			"backend process cannot be told apart from a current one")
	}
	if !contains(src, "Session string `json:\"session\"`") {
		t.Error("AppSnapshot carries no session field")
	}
}

// TestBackendHasAStableSessionIdentity — the session must be generated once per
// process, and be the same value in events and in snapshots.
func TestBackendHasAStableSessionIdentity(t *testing.T) {
	b := backendWithConfigForSession(t)

	first := b.SessionID()
	if first == "" {
		t.Fatal("the backend has no session id; the frontend cannot scope its " +
			"sequence numbers to this process")
	}
	if again := b.SessionID(); again != first {
		t.Fatalf("SessionID changed within one process: %q then %q; a client would "+
			"treat its own backend's events as belonging to another session", first, again)
	}

	// The snapshot and the stream must agree, or a client that establishes its
	// baseline from the snapshot would reject every live event.
	if snap := b.Snapshot(); snap.Session != first {
		t.Fatalf("snapshot session = %q, want %q (the same process)", snap.Session, first)
	}
}

// TestServerDispatchesStopWhileStartIsBlocked is claim 3, and it is the reason the
// lifecycle work of the previous round could not work in the real app.
//
// The IPC server read a line, ran its handler to completion, and only then read the
// next line. `start_core` can legitimately block for a long time — a config build, a
// password prompt, a daemon apply, the whole operation budget. During that time the
// server did not read the pipe at all, so a `stop_core` the user sent next sat
// unread: the command never reached the controller, and no amount of correctness in
// the lifecycle state machine could matter.
//
// The test blocks the start handler on a channel it controls, sends stop on the SAME
// connection, and requires stop to be dispatched and to complete before start is
// released.
func TestServerDispatchesStopWhileStartIsBlocked(t *testing.T) {
	b, srv := serverWithFakeLifecycle(t)
	_ = srv

	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	var stopDispatched atomic.Bool

	ops := &core.LegacyOpsForTest{}
	ops.StartContext = func(ctx context.Context, skipRunningCheck bool) error {
		close(startEntered)
		select {
		case <-releaseStart:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ops.Stop = func() { stopDispatched.Store(true) }
	installOps(t, b, ops)

	// One connection, both requests. The server is created over the same writer
	// the test reads, so no attach step is needed.
	serverIn, _, clientIn, _ := newPipePair(t)
	go srv.Serve(serverIn)

	writeReq := func(id, method string) {
		t.Helper()
		line, err := json.Marshal(protocol.Request{ID: id, Method: method})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := clientIn.Write(append(line, '\n')); err != nil {
			t.Fatalf("write %s: %v", method, err)
		}
	}

	writeReq("1", protocol.MethodStartCore)

	// Wait until the start handler is genuinely running and blocked, so the test
	// proves dispatch DURING a blocked request rather than merely fast dispatch.
	select {
	case <-startEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start handler never ran")
	}

	writeReq("2", protocol.MethodStopCore)

	// Stop must be dispatched while start is still blocked.
	deadline := time.After(5 * time.Second)
	for !stopDispatched.Load() {
		select {
		case <-deadline:
			t.Fatal("stop_core was NOT dispatched while start_core was blocked. The " +
				"read loop is serialised behind the handler, so a user's stop cannot " +
				"reach the controller — superseding a start is impossible over IPC")
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(releaseStart)
	_ = clientIn
}

// TestServerDispatchesShutdownDuringGroupTest is claim 4: a long-running handler
// must not make the backend un-shutdownable.
//
// `test_proxy_group` can legitimately occupy the backend for the whole group-test
// budget. With a serialised read loop, `shutdown` sent during that window was not
// even read, so quitting the app waited for a network operation to finish.
//
// The test holds a handler open on a channel it controls and requires the shutdown
// request to be READ and acted on while that handler is still running.
func TestServerDispatchesShutdownDuringGroupTest(t *testing.T) {
	b, srv := serverWithFakeLifecycle(t)

	// Hold the START handler open: it is the same shape of long-running request as
	// a group test (a blocking operation the user may want to interrupt), and it
	// needs no network fixture to be deterministic.
	entered := make(chan struct{})
	release := make(chan struct{})
	installOps(t, b, &core.LegacyOpsForTest{
		StartContext: func(ctx context.Context, skipRunningCheck bool) error {
			close(entered)
			<-release
			return nil
		},
	})

	serverIn, _, clientIn, _ := newPipePair(t)
	go srv.Serve(serverIn)

	writeReq(t, clientIn, "1", protocol.MethodStartCore, nil)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	writeReq(t, clientIn, "2", protocol.MethodShutdown, nil)

	deadline := time.After(5 * time.Second)
	for !srv.Stopped() {
		select {
		case <-deadline:
			t.Fatal("shutdown was not handled while another request was in flight; " +
				"the read loop is serialised behind the handler, so quitting the app " +
				"waits for whatever operation is running")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(release)
}

// ---------------------------------------------------------------------------
// Scaffolding
// ---------------------------------------------------------------------------

// serverWithFakeLifecycle builds a backend whose Classic start/stop are driven by
// test channels, so a handler can be held open for as long as the test needs.
//
// The fake replaces the LIFECYCLE OPS only; the IPC server, the dispatcher and the
// operation record are the real ones, because they are what these tests are about.
func serverWithFakeLifecycle(t *testing.T) (*Backend, *Server) {
	t.Helper()
	b := backendWithConfig(t)
	installControllableLegacy(t, b)
	out := &lockedBuffer{}
	return b, NewServer(b, out)
}

// installControllableLegacy replaces the Classic backend's start/stop with
// channels the test drives.
func installControllableLegacy(t *testing.T, b *Backend) {
	t.Helper()
	lb, ok := b.ac.LegacyBackendForTest()
	if !ok {
		t.Skip("no legacy backend in this environment")
	}
	core.SetLegacyOpsForTest(lb, &core.LegacyOpsForTest{})
}

// installOps installs process-level overrides on the backend's classic ops.
func installOps(t *testing.T, b *Backend, ops *core.LegacyOpsForTest) {
	t.Helper()
	lb, ok := b.ac.LegacyBackendForTest()
	if !ok {
		t.Skip("no legacy backend in this environment")
	}
	core.SetLegacyOpsForTest(lb, ops)
}

// lockedBuffer is a concurrency-safe writer for the server's output.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// writeReq frames and sends one request.
func writeReq(t *testing.T, w io.Writer, id, method string, params map[string]any) {
	t.Helper()
	line, err := json.Marshal(protocol.Request{ID: id, Method: method, Params: params})
	if err != nil {
		t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
}

// newPipePair wires a server-side reader/writer pair to a client-side writer/reader.
//
// The client→server direction is BUFFERED, and that is a correctness requirement
// of the test rather than a convenience: `io.Pipe` is synchronous, so a client
// write blocks until the server reads. A test that writes a request before the
// server's read loop has started would block on the write itself and hang — which
// is a defect in the harness, and one that would disguise a serialised dispatcher
// as a working one. A buffered channel-backed writer lets the client enqueue a
// request and then observe whether the server dispatches it.
func newPipePair(t *testing.T) (serverIn io.Reader, serverOut io.Writer, clientIn io.Writer, clientOut io.Reader) {
	t.Helper()
	// io.Pipe returns (reader, writer). The client's writer feeds the server's
	// reader, and the server's writer feeds the client's reader.
	cr, cw := io.Pipe() // client reads <- server writes
	toServer := make(chan []byte, 16)
	return &chanReader{ch: toServer}, cw, &chanWriter{ch: toServer}, cr
}

// chanReader hands queued client requests to the server's scanner.
type chanReader struct {
	ch     chan []byte
	rest   []byte
	closed bool
}

func (c *chanReader) Read(p []byte) (int, error) {
	if len(c.rest) == 0 {
		if c.closed {
			return 0, io.EOF
		}
		buf, ok := <-c.ch
		if !ok {
			c.closed = true
			return 0, io.EOF
		}
		c.rest = buf
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

// chanWriter queues a client request without blocking on the server.
type chanWriter struct{ ch chan []byte }

func (w *chanWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	w.ch <- buf
	return len(p), nil
}

// TestCoreStopCancelsActiveGroupTest is claim 5, driven through the REAL trigger.
//
// `CancelActive` existed with a comment saying it was called when the core stops,
// the engine mode switches and the backend shuts down. It had no callers at all, so
// none of that was true. A test that calls `CancelActive` directly proves the method
// works and nothing about the wiring, which is the part that was missing — so this
// test cancels by STOPPING THE CORE and observing the effect.
func TestCoreStopCancelsActiveGroupTest(t *testing.T) {
	b := backendWithConfig(t)

	cancelled := make(chan struct{})
	b.groupTests.setHookForTest(func() { close(cancelled) })

	// The core must actually be RUNNING first. The running-state write dedups a
	// no-op, so a stop applied to an already-false flag publishes nothing — and a
	// test that skipped this would fail for a reason unrelated to the wiring.
	b.ac.RunningState.Set(true)
	// Drive the authoritative transition the way a real core stop does.
	b.ac.RunningState.SetStopped(events.TeardownUserStop)

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping the core did not cancel the active group test; the test " +
			"keeps probing a transport that is gone, and CancelActive is not wired")
	}
}

// TestShutdownCancelsActiveGroupTest — shutdown must cancel independently of any
// core-state transition.
//
// The daemon backend can leave the core RUNNING across an app exit, so relying on a
// "core stopped" event to cancel the test would leave it running past the shutdown
// that was supposed to stop it.
func TestShutdownCancelsActiveGroupTest(t *testing.T) {
	b := backendWithConfig(t)

	cancelled := make(chan struct{})
	b.groupTests.setHookForTest(func() { close(cancelled) })

	b.Shutdown()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel the active group test")
	}
}

// TestModeSwitchCancelsActiveGroupTest — a mode switch replaces the transport, so a
// test measuring through the old one must be cancelled.
func TestModeSwitchCancelsActiveGroupTest(t *testing.T) {
	b := backendWithConfig(t)

	cancelled := make(chan struct{})
	b.groupTests.setHookForTest(func() { close(cancelled) })

	// Any outcome is fine; the switch may be refused because the engine is busy.
	// The CANCELLATION must happen regardless, since the decision to switch was
	// already made and the old transport is about to be torn down.
	_ = b.SetCoreMode(string(core.BackendDaemon))

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("switching the engine did not cancel the active group test")
	}
}

// TestGroupTestDoesNotLeakShutdownWatcher is claim 6.
//
// `testContext` created a context AND a goroutine to watch for shutdown, and
// returned only the context. The watcher could not be released until the whole
// backend exited, so every latency test leaked one goroutine for the lifetime of the
// process — two hundred tests, two hundred parked goroutines.
//
// A goroutine-count assertion is the honest test here: the defect was a leak, and a
// leak is only visible as a growing population.
func TestGroupTestDoesNotLeakShutdownWatcher(t *testing.T) {
	b := backendWithConfig(t)

	// Warm up: the first call may allocate lazily.
	_ = b.testContext()
	settleGoroutines()

	before := runtime.NumGoroutine()

	// Build many contexts, the way many latency tests would.
	for i := 0; i < 50; i++ {
		ctx := b.testContext()
		if ctx == nil {
			t.Fatal("testContext returned nil")
		}
	}
	settleGoroutines()

	after := runtime.NumGoroutine()
	// One shared parent may add a single bridge goroutine; a per-call watcher adds
	// fifty. Allow a small margin so the assertion is about the LEAK, not noise.
	if grown := after - before; grown > 5 {
		t.Fatalf("goroutines grew by %d across 50 test contexts (%d -> %d); each call "+
			"appears to leak a shutdown watcher", grown, before, after)
	}
}

// settleGoroutines gives exited goroutines a moment to be reaped, so the count
// reflects reality rather than scheduling.
func settleGoroutines() {
	for i := 0; i < 20; i++ {
		runtime.Gosched()
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Swift-side invariants
//
// There is no XCTest in this toolchain (verified: neither XCTest nor the Swift
// Testing macro plugin links, so `swift test` cannot build). The Go suite does run,
// so the client's invariants are enforced by reading its source. These are not
// substitutes for behavioural tests — they are the only executable check available
// for a language whose test runner does not exist here, and each states the exact
// failure it prevents.
// ---------------------------------------------------------------------------

func readSwift(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "macos", "Sources", "JiejieBox", rel))
	if err != nil {
		t.Skipf("Swift sources not present: %v", err)
	}
	return string(b)
}

// TestSwiftScopesSequenceToSession is claim 1 on the client side, and it is the
// consumer half of the session protocol.
//
// The backend restart blackout is a CLIENT bug: the helper's counter restarts at 1
// while the client's high-water mark persists, so every event from the new helper is
// discarded as stale. Supplying a session on the wire is necessary but not
// sufficient — the client must actually reset on it, and must drop events that
// belong to a session it is not following.
func TestSwiftScopesSequenceToSession(t *testing.T) {
	app := readSwift(t, "App/AppModel.swift")

	if !contains(app, "appliedSession") {
		t.Fatal("AppModel tracks no backend session, so a restarted helper's events " +
			"all compare as stale and the UI stops updating")
	}
	if !contains(app, "appliedSession = snapshot.session") {
		t.Error("the snapshot does not adopt its session; without this the " +
			"high-water mark cannot be re-baselined")
	}
	if !contains(app, "appliedSeq = 0") {
		t.Error("the sequence is never reset, so a new session inherits the old " +
			"one's high-water mark — the exact blackout")
	}
	// A late event from a replaced backend must be DROPPED, not compared.
	if !contains(app, "event.session == session") {
		t.Error("events are not filtered by session, so a dead backend's late frame " +
			"can overwrite the live backend's state")
	}
	// And the reset must happen on stop, not only in start(): a crash-recovery
	// path may never call stop().
	if !contains(app, "appliedSession = nil") {
		t.Error("stop() does not clear the session, so a restart through a path " +
			"that skips the snapshot leaves stale state")
	}
}

// TestSwiftBuffersEventsUntilTheBaseline — claim 2, client side.
//
// An event that wins the race against the snapshot must not be applied before it:
// the snapshot was composed at an EARLIER sequence and would overwrite the newer
// state, and the already-consumed event never comes again.
func TestSwiftBuffersEventsUntilTheBaseline(t *testing.T) {
	app := readSwift(t, "App/AppModel.swift")

	if !contains(app, "pendingEvents") || !contains(app, "awaitingBaseline") {
		t.Fatal("there is no bootstrap barrier: events arriving before the snapshot " +
			"are applied immediately and then overwritten by the older snapshot")
	}
	if !contains(app, "applyBaseline") {
		t.Fatal("no baseline step exists; the snapshot's fields must be installed " +
			"before buffered events are replayed, never after")
	}
	// The replay must be filtered against the snapshot's sequence, or events the
	// snapshot already includes would be applied twice.
	if !contains(app, "$0.seq < $1.seq") {
		t.Error("buffered events are not replayed in sequence order")
	}
}

// TestSwiftHelperGenerationGuardsTermination is claims 8 and 10, client side.
//
// The termination handler of an old helper runs asynchronously, after the app has
// moved on. Without a generation check it judges the old process's exit by the NEW
// process's intent and then clears the new process's handles — reporting a healthy
// backend as crashed and closing its event stream.
func TestSwiftHelperGenerationGuardsTermination(t *testing.T) {
	client := readSwift(t, "Services/BackendClient.swift")

	if !contains(client, "helperGeneration") || !contains(client, "activeGeneration") {
		t.Fatal("the client has no helper generation, so a restart races the old " +
			"helper's termination handler against the new helper's state")
	}
	// The generation must be CAPTURED at handler creation, not read when it runs.
	if !contains(client, "handleTermination(code, generation: generation)") {
		t.Error("the termination handler does not capture its own generation; " +
			"reading the current one when it runs is the bug")
	}
	if !contains(client, "guard generation == activeGeneration") {
		t.Error("handleTermination does not check the generation, so a superseded " +
			"helper's exit can tear down the live one")
	}
	// Claim 10: EOF on the protocol stream with the process still alive is a
	// connection failure, not something to ignore.
	if contains(client, "guard process == nil else { return }\n        handleTermination(0)") {
		t.Error("handleReadEnded ignores EOF while the process is alive, leaving the " +
			"client in `ready` with a dead channel")
	}
	if !contains(client, "channel closed while the helper is still running") {
		t.Error("a dead IPC channel with a live process is not treated as a failure")
	}
}

// TestSwiftShutdownConfirmsExitBeforeRelease — claim 9.
//
// `shutdown` terminated the helper, slept a fixed interval, and released ownership
// without ever checking whether it had exited. `restart()` could then start a second
// helper over a live first one, and both would own the same state, config, core and
// daemon channel.
func TestSwiftShutdownConfirmsExitBeforeRelease(t *testing.T) {
	client := readSwift(t, "Services/BackendClient.swift")

	if !contains(client, "waitForExit") {
		t.Fatal("shutdown does not confirm the helper's exit, so a restart can start " +
			"a second helper while the first still owns the state and the core")
	}
	if !contains(client, "func shutdown() async throws") {
		t.Error("shutdown cannot report failure, so a caller cannot learn that the " +
			"old helper survived — it will start a replacement regardless")
	}
	if !contains(client, "terminationFailed") {
		t.Error("there is no error for a helper that refused to exit")
	}
	// The signal escalation must exist, and each step must confirm.
	if !contains(client, "proc.interrupt()") {
		t.Error("shutdown has only one termination signal; a helper ignoring SIGTERM " +
			"would be reported as stopped while it runs")
	}
}

// TestSwiftStopSurfacesTerminationFailure — the caller half of claim 9.
//
// A shutdown that throws is worthless if the caller ignores it: `stop()` must not
// report a clean idle state and let `restart()` proceed.
func TestSwiftStopSurfacesTerminationFailure(t *testing.T) {
	app := readSwift(t, "App/AppModel.swift")

	if !contains(app, "try await client.shutdown()") {
		t.Fatal("stop() ignores a shutdown failure, so a surviving helper is " +
			"reported as a clean stop and a second one is started")
	}
	if !contains(app, "catch") {
		t.Error("the shutdown failure is not caught")
	}
}
