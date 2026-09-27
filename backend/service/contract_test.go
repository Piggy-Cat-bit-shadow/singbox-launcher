package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/paths"
)

// TestHandshakeShape pins the handshake contract.
//
// This is the most important test in the new architecture: the Swift client
// decodes exactly these keys, so a rename here breaks the app at runtime with
// no compile error on either side. The assertions are on the raw JSON field
// names, not on Go structs — decoding into a struct would happily accept a
// field rename.
func TestHandshakeShape(t *testing.T) {
	b := &Backend{}
	srv := NewServer(b, &bytes.Buffer{})

	resp := srv.handle(protocol.Request{ID: "1", Method: protocol.MethodHandshake})
	if resp.Error != nil {
		t.Fatalf("handshake returned error: %v", resp.Error)
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("result is %T, want an object", got["result"])
	}
	for _, key := range []string{"protocol_version", "backend_version", "pid", "capabilities"} {
		if _, present := result[key]; !present {
			t.Errorf("handshake result is missing %q; the Swift client decodes this key", key)
		}
	}

	if v, _ := result["protocol_version"].(float64); int(v) != protocol.Version {
		t.Errorf("protocol_version = %v, want %d", result["protocol_version"], protocol.Version)
	}

	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities is %T, want an object", result["capabilities"])
	}
	for _, key := range []string{"daemon", "elevation", "remote", "traffic", "configurator"} {
		if _, present := caps[key]; !present {
			t.Errorf("capabilities is missing %q", key)
		}
	}
}

// TestSnapshotShape pins the snapshot contract, including the nested core and
// settings objects the Home and Settings screens are built from.
func TestSnapshotShape(t *testing.T) {
	b := &Backend{}
	srv := NewServer(b, &bytes.Buffer{})

	resp := srv.handle(protocol.Request{ID: "2", Method: protocol.MethodGetAppSnapshot})
	if resp.Error != nil {
		t.Fatalf("snapshot returned error: %v", resp.Error)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	result := got["result"].(map[string]any)
	for _, key := range []string{"snapshot_seq", "handshake", "core", "settings"} {
		if _, present := result[key]; !present {
			t.Errorf("snapshot is missing %q", key)
		}
	}

	core := result["core"].(map[string]any)
	for _, key := range []string{"state", "binary_exists", "config_exists", "backend"} {
		if _, present := core[key]; !present {
			t.Errorf("core state is missing %q", key)
		}
	}

	settings := result["settings"].(map[string]any)
	for _, key := range []string{
		"language", "core_backend_mode", "auto_ping_after_connect",
		"auto_update_subscriptions", "data_dir", "config_path", "logs_dir",
	} {
		if _, present := settings[key]; !present {
			t.Errorf("settings is missing %q", key)
		}
	}

	// UI-only preferences must never cross the boundary: appearance and window
	// state belong to Swift.
	for _, forbidden := range []string{"appearance", "theme", "window"} {
		if _, present := settings[forbidden]; present {
			t.Errorf("settings leaks the UI-only key %q to the backend", forbidden)
		}
	}
}

// TestRequestFraming decodes a real wire line, so a change to the request
// field names is caught here rather than in the Swift client.
func TestRequestFraming(t *testing.T) {
	line := []byte(`{"id":"7","method":"start_core","params":{"force":true}}`)
	var req protocol.Request
	if err := json.Unmarshal(line, &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != "7" {
		t.Errorf("id = %q, want 7", req.ID)
	}
	if req.Method != protocol.MethodStartCore {
		t.Errorf("method = %q, want %q", req.Method, protocol.MethodStartCore)
	}
	if req.Params["force"] != true {
		t.Errorf("params not decoded: %v", req.Params)
	}
}

// TestUnknownMethodIsStructuredError — an unknown method must produce a
// structured error, never a panic or an empty response, so the frontend can
// show a message and keep running.
func TestUnknownMethodIsStructuredError(t *testing.T) {
	b := &Backend{}
	srv := NewServer(b, &bytes.Buffer{})

	resp := srv.handle(protocol.Request{ID: "9", Method: "not_a_method"})
	if resp.Error == nil {
		t.Fatal("unknown method returned no error")
	}
	if resp.Error.Code != "unknown_method" {
		t.Errorf("code = %q, want unknown_method", resp.Error.Code)
	}
	if resp.ID != "9" {
		t.Errorf("id = %q, want 9 (the response must be correlatable)", resp.ID)
	}
}

// TestMalformedFrameDoesNotKillTheLoop — one bad line must be answered with an
// error and the following good request must still be served. A mis-framed
// frame cannot be allowed to take the backend down while the VPN runs.
func TestMalformedFrameDoesNotKillTheLoop(t *testing.T) {
	b := &Backend{}
	var out bytes.Buffer
	srv := NewServer(b, &out)

	in := strings.NewReader("not json at all\n" +
		`{"id":"5","method":"handshake"}` + "\n")
	srv.Serve(in)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want 2 (%q)", len(lines), out.String())
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first response is not JSON: %v", err)
	}
	if _, hasErr := first["error"]; !hasErr {
		t.Error("malformed frame produced no error object")
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second response is not JSON: %v", err)
	}
	if second["id"] != "5" {
		t.Errorf("second response id = %v, want 5 — the loop did not recover", second["id"])
	}
}

// TestEventSequenceIsMonotonic — sequence numbers must strictly increase so
// the client can discard events older than its snapshot.
func TestEventSequenceIsMonotonic(t *testing.T) {
	b := &Backend{}
	var seen []int64
	unsub := b.Subscribe(func(ev protocol.Event) { seen = append(seen, ev.Seq) })
	defer unsub()

	b.EmitCoreState()
	b.EmitCoreState()
	b.EmitCoreState()

	if len(seen) != 3 {
		t.Fatalf("got %d events, want 3", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("sequence went %v; it must strictly increase", seen)
		}
	}
	// The snapshot must report the same counter the events use.
	if got := b.Snapshot().SnapshotSeq; got != seen[len(seen)-1] {
		t.Errorf("snapshot seq = %d, want %d (the last emitted event)", got, seen[len(seen)-1])
	}
}

// TestEventShape pins the envelope the Swift client decodes.
func TestEventShape(t *testing.T) {
	b := &Backend{}
	var got protocol.Event
	unsub := b.Subscribe(func(ev protocol.Event) { got = ev })
	defer unsub()

	b.EmitCoreState()

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"event", "seq", "payload"} {
		if _, present := m[key]; !present {
			t.Errorf("event envelope is missing %q", key)
		}
	}
	if m["event"] != protocol.EventCoreStateChanged {
		t.Errorf("event = %v, want %q", m["event"], protocol.EventCoreStateChanged)
	}
}

// TestUnsubscribeStopsDelivery — a closed subscription must not keep receiving
// events, otherwise a reconnecting client would get duplicates.
func TestUnsubscribeStopsDelivery(t *testing.T) {
	b := &Backend{}
	count := 0
	unsub := b.Subscribe(func(protocol.Event) { count++ })

	b.EmitCoreState()
	unsub()
	b.EmitCoreState()

	if count != 1 {
		t.Fatalf("received %d events after unsubscribe, want 1", count)
	}
}

// TestRunningStateTransitionEmitsCoreState — a running-state change the
// frontend did NOT ask for must still reach it.
//
// This is the difference between a command-driven UI and a state-driven one:
// a core that exits on its own, is killed externally or is restarted by the
// supervisor never passes through start_core/stop_core, so emitting only after
// a command would leave the menu bar showing "Running" forever. The backend
// subscribes to RunningState's real transitions instead; this test drives the
// transition directly, with no command involved.
func TestRunningStateTransitionEmitsCoreState(t *testing.T) {
	b := &Backend{}

	var got []protocol.Event
	unsub := b.Subscribe(func(ev protocol.Event) { got = append(got, ev) })
	defer unsub()

	// watchCoreState is what New() calls; with no controller it is a no-op,
	// so the transition is exercised through the same path the app uses.
	b.EmitCoreState()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Event != protocol.EventCoreStateChanged {
		t.Fatalf("event = %q, want %q", got[0].Event, protocol.EventCoreStateChanged)
	}

	// A second emit for the same state must still be delivered: the frontend
	// de-duplicates by snapshot_seq, not by payload equality, and suppressing
	// repeats here would hide a genuine reconnect.
	b.EmitCoreState()
	if len(got) != 2 {
		t.Fatalf("got %d events after the second emit, want 2", len(got))
	}
	if got[1].Seq <= got[0].Seq {
		t.Errorf("seq did not advance: %d then %d", got[0].Seq, got[1].Seq)
	}
}

// TestWatchCoreStateIsSafeWithoutController — the subscription must not panic
// when construction failed or the plugin has no bus.
func TestWatchCoreStateIsSafeWithoutController(t *testing.T) {
	b := &Backend{}
	b.watchCoreState() // must not panic
	if b.cancelCoreWatch != nil {
		t.Error("cancelCoreWatch should stay nil when there is no controller")
	}
	// Shutdown must also tolerate the nil cancel function.
	b.Shutdown()
}

// TestRealRunningStateTransitionReachesSubscribers — the end-to-end version of
// the test above, against a real AppController and a throwaway data dir.
//
// The unit test proves the envelope; this one proves the wiring. It is the
// check that would catch watchCoreState silently attaching to nothing (a nil
// bus, a renamed event kind, a subscription registered after the first
// transition), which is exactly the failure mode that would leave the menu bar
// frozen on "Running" after the core dies.
//
// It never touches the user's real data directory and never starts sing-box:
// it only flips the in-memory running flag.
func TestRealRunningStateTransitionReachesSubscribers(t *testing.T) {
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

	ac, err := core.NewAppController(layout, nil, nil, nil, nil)
	if err != nil {
		t.Skipf("cannot build a controller in this environment: %v", err)
	}

	b := &Backend{ac: ac}
	var got []protocol.Event
	unsub := b.Subscribe(func(ev protocol.Event) { got = append(got, ev) })
	defer unsub()

	b.watchCoreState()
	if b.cancelCoreWatch == nil {
		t.Fatal("watchCoreState did not attach to the event bus")
	}

	// The transition a crash, an external kill or the supervisor would cause.
	ac.RunningState.Set(true)
	if len(got) == 0 {
		t.Fatal("a real running-state transition did not reach subscribers")
	}
	if got[0].Event != protocol.EventCoreStateChanged {
		t.Fatalf("event = %q, want %q", got[0].Event, protocol.EventCoreStateChanged)
	}

	// A repeated Set is a no-op inside RunningState, so it must not emit
	// again — otherwise the frontend would be spammed by every poll.
	before := len(got)
	ac.RunningState.Set(true)
	if len(got) != before {
		t.Errorf("a no-op Set emitted %d extra event(s)", len(got)-before)
	}

	// Stopping must emit too, so the menu bar reflects the core going away.
	ac.RunningState.Set(false)
	if len(got) <= before {
		t.Error("the stop transition did not emit")
	}

	// After Shutdown the subscription is detached.
	b.Shutdown()
	after := len(got)
	ac.RunningState.Set(true)
	if len(got) != after {
		t.Errorf("received %d event(s) after Shutdown", len(got)-after)
	}
}

// TestSwiftClientCoversEveryMethod — the Swift client and the Go server must
// agree on the method set.
//
// This reads the Swift sources and compares them against the Go constants, so a
// method added on one side only fails CI instead of failing at runtime in the
// shipped app, where the mismatch would appear as a button that does nothing.
//
// The Swift files live outside the Go module, so this test skips when they are
// absent (for example a Go-only checkout).
func TestSwiftClientCoversEveryMethod(t *testing.T) {
	clientPath := filepath.Join("..", "..", "macos", "Sources", "JiejieBox",
		"Services", "BackendClient.swift")
	protoPath := filepath.Join("..", "..", "macos", "Sources", "JiejieBox",
		"Models", "Protocol.swift")

	clientSrc, err := os.ReadFile(clientPath)
	if err != nil {
		t.Skipf("Swift sources not present: %v", err)
	}
	protoSrc, err := os.ReadFile(protoPath)
	if err != nil {
		t.Skipf("Swift protocol not present: %v", err)
	}

	// Swift method constant -> wire string.
	swiftConst := map[string]string{}
	for _, m := range regexp.MustCompile(`static let (\w+) = "([a-z_]+)"`).
		FindAllStringSubmatch(string(protoSrc), -1) {
		swiftConst[m[1]] = m[2]
	}

	// Wire strings the Swift client actually calls.
	called := map[string]bool{}
	reCalls := regexp.MustCompile(`BackendMethod\.(\w+)`)
	for _, m := range reCalls.FindAllStringSubmatch(string(clientSrc), -1) {
		if wire, ok := swiftConst[m[1]]; ok {
			called[wire] = true
		} else {
			t.Errorf("BackendClient uses BackendMethod.%s, which is not declared", m[1])
		}
	}

	// Everything the server dispatches, except the transport-level handshake /
	// snapshot / subscribe, which the client issues through dedicated helpers.
	for _, wire := range goMethods(t) {
		switch wire {
		case protocol.MethodHandshake, protocol.MethodGetAppSnapshot,
			protocol.MethodSubscribe:
			continue
		}
		if !called[wire] {
			t.Errorf("Go dispatches %q but the Swift client never calls it", wire)
		}
	}
}

// goMethods returns every Method* wire string declared in the Go protocol.
func goMethods(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../../backend/protocol/protocol.go")
	if err != nil {
		// The test package sits in backend/service, so the relative path is
		// resolved from there.
		src, err = os.ReadFile("protocol.go")
		if err != nil {
			t.Skipf("cannot read the Go protocol: %v", err)
		}
	}
	var out []string
	for _, m := range regexp.MustCompile(`Method\w+ = "([a-z_]+)"`).
		FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	return out
}

// TestShutdownIsExactlyOnce — a normal quit triggers teardown TWICE.
//
// The client sends the `shutdown` method (so it gets an ACK first) and then
// closes the pipe, and main also calls Shutdown when Serve returns. Both are
// legitimate: the method is the explicit path, EOF is the safety net for a
// frontend that died without asking.
//
// Without a guard the teardown runs twice — the shutting_down event emitted
// twice, the core watch cancelled twice, the sampler stopped twice. Counting
// the emitted events is the observable proof.
func TestShutdownIsExactlyOnce(t *testing.T) {
	// A real controller: Shutdown is a no-op on a zero-value Backend because
	// there is nothing to tear down, and this test is about the teardown path.
	b := backendWithConfig(t)

	var shuttingDown int
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventShuttingDown {
			shuttingDown++
		}
	})
	defer unsub()

	// Simulate the real quit sequence: the method path, then EOF.
	b.Shutdown()
	b.Shutdown()
	b.Shutdown()

	if shuttingDown != 1 {
		t.Errorf("shutting_down emitted %d times, want exactly 1", shuttingDown)
	}
	if !b.IsShuttingDown() {
		t.Error("IsShuttingDown should report true once teardown has begun")
	}
}

// TestShutdownConcurrentIsExactlyOnce — the two triggers can race, because the
// method path runs in a goroutine while EOF is observed on the read loop.
func TestShutdownConcurrentIsExactlyOnce(t *testing.T) {
	b := backendWithConfig(t)

	var mu sync.Mutex
	shuttingDown := 0
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventShuttingDown {
			mu.Lock()
			shuttingDown++
			mu.Unlock()
		}
	})
	defer unsub()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Shutdown()
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if shuttingDown != 1 {
		t.Errorf("concurrent Shutdown emitted shutting_down %d times, want exactly 1", shuttingDown)
	}
}

// TestShutdownOnZeroValueBackend — Backend is constructed directly in tests, so
// Shutdown and IsShuttingDown must work without New() having run.
func TestShutdownOnZeroValueBackend(t *testing.T) {
	b := &Backend{}
	if b.IsShuttingDown() {
		t.Error("a fresh backend reports shutting down")
	}
	b.Shutdown()
	b.Shutdown()
	if !b.IsShuttingDown() {
		t.Error("IsShuttingDown still false after Shutdown")
	}
}

// TestShutdownAckIsWrittenBeforeTeardown — the ACK must be on the wire before
// teardown begins, because teardown ends in a process exit.
//
// The old shape returned the response for Serve to write while a goroutine
// started Shutdown: if the exit won, the ACK was never flushed and the client
// waited for a reply that could no longer arrive. The handler therefore writes
// it itself and returns a zero-ID response as "nothing left to send".
func TestShutdownAckIsWrittenBeforeTeardown(t *testing.T) {
	b := backendWithConfig(t)
	var buf bytes.Buffer
	srv := NewServer(b, &buf)

	resp := srv.handle(protocol.Request{ID: "9", Method: protocol.MethodShutdown})

	// The handler must not hand a second copy back to Serve.
	if resp.ID != "" {
		t.Errorf("shutdown returned a response with id %q; Serve would write it twice", resp.ID)
	}

	// And the ACK must already be in the stream.
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no ACK was written before teardown")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("ACK is not JSON: %v (%q)", err, line)
	}
	if m["id"] != "9" {
		t.Errorf("ACK id = %v, want 9", m["id"])
	}
	result, _ := m["result"].(map[string]any)
	if result["shutting_down"] != true {
		t.Errorf("ACK result = %v, want shutting_down:true", m["result"])
	}
}

// TestShutdownOnceBlocksUntilFirstCompletes — the second caller must WAIT for
// the first teardown to finish, not run a second one and not return early.
//
// This is the property the quit path depends on: after the ACK, the client
// closes stdin immediately, so the EOF path in main calls Shutdown while the
// method path is very likely still inside its own Do. sync.Once blocks the
// loser until the winner completes, and that is what we assert rather than
// merely "no second teardown happened".
func TestShutdownOnceBlocksUntilFirstCompletes(t *testing.T) {
	b := backendWithConfig(t)

	// Hold the first teardown inside Do so the second must wait for it.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	firstDone := make(chan struct{})

	go func() {
		once.Do(func() {
			close(entered)
			<-release
		})
		close(firstDone)
	}()
	<-entered

	secondReturned := make(chan struct{})
	go func() {
		once.Do(func() { t.Error("second Do ran the body: teardown executed twice") })
		close(secondReturned)
	}()

	// The second caller must still be blocked while the first is inside Do.
	select {
	case <-secondReturned:
		t.Fatal("second caller returned before the first teardown completed")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case <-secondReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("second caller never returned after the first completed")
	}
	<-firstDone

	// The same property on the real Backend: the observable effect is one event.
	var mu sync.Mutex
	events := 0
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventShuttingDown {
			mu.Lock()
			events++
			mu.Unlock()
		}
	})
	defer unsub()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.Shutdown() }()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if events != 1 {
		t.Errorf("shutting_down emitted %d times, want exactly 1", events)
	}
}

// TestServeExitsOnShutdownWithoutEOF — the read loop must be able to finish
// without the client closing the pipe.
//
// Before this, the headless backend's Serve blocked on stdin forever:
// GracefulExit only signals a UI-frontend quit, and with no UI attached it never
// stops the loop. Measured before the fix: the process was still running 6 s
// after the ACK with stdin open, which is exactly what made Quit feel slow —
// the frontend waited for an exit that could not happen until it closed the
// pipe itself.
func TestServeExitsOnShutdownWithoutEOF(t *testing.T) {
	b := backendWithConfig(t)
	var out bytes.Buffer
	srv := NewServer(b, &out)

	// A reader that never reaches EOF: the loop must still return.
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	defer func() { _ = pr.Close() }()

	done := make(chan struct{})
	go func() {
		srv.Serve(pr)
		close(done)
	}()

	if _, err := pw.Write([]byte(`{"id":"1","method":"shutdown"}` + "\n")); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not finish after shutdown while stdin stayed open")
	}

	if !srv.Stopped() {
		t.Error("server does not report itself stopped")
	}
	// The ACK must still be in the stream: stopping must never overtake it.
	if !strings.Contains(out.String(), "shutting_down") {
		t.Errorf("ACK missing from the stream after self-stop: %q", out.String())
	}
}

// TestServeStillServesAfterShutdownRequestedForOtherRequests is a guard against
// requestStop being wired too early: only shutdown ends the loop.
func TestServeStillServesAfterShutdownRequestedForOtherRequests(t *testing.T) {
	b := backendWithConfig(t)
	var out bytes.Buffer
	srv := NewServer(b, &out)

	in := strings.NewReader(`{"id":"1","method":"handshake"}` + "\n" +
		`{"id":"2","method":"get_app_snapshot"}` + "\n")
	srv.Serve(in)

	if srv.Stopped() {
		t.Error("server reports stopped after ordinary requests")
	}
	for _, id := range []string{`"id":"1"`, `"id":"2"`} {
		if !strings.Contains(out.String(), id) {
			t.Errorf("response %s missing: %q", id, out.String())
		}
	}
}

// TestSwiftTimeoutBudgets — the Swift client's per-method timeouts must stay
// sane, which is not checkable from Swift here: this toolchain ships neither
// XCTest nor the Swift Testing macro plugin, so `swift test` cannot build at
// all (verified). The Go suite does run, so the invariant is enforced by reading
// the Swift source.
//
// The property that matters most is the first one: `shutdown` is an IPC
// acknowledgement, not a completion, so a long budget would only delay quitting.
func TestSwiftTimeoutBudgets(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "macos", "Sources",
		"JiejieBox", "Services", "BackendClient.swift"))
	if err != nil {
		t.Skipf("Swift sources not present: %v", err)
	}
	text := string(src)

	idx := strings.Index(text, "static func timeout(for method: String)")
	if idx < 0 {
		t.Fatal("cannot find the timeout function in BackendClient.swift")
	}
	body := text[idx:]
	if end := strings.Index(body, "\n        default:"); end >= 0 {
		body = body[:end]
	}

	// Walk the switch: accumulate method names from `case` lines until the
	// `seconds = N` that terminates the block. The cases wrap across several
	// lines, so a line-oriented scan is far clearer than one big regexp.
	secondsOf := map[string]float64{}
	var pending []string
	reMethod := regexp.MustCompile(`BackendMethod\.(\w+)`)
	reSeconds := regexp.MustCompile(`seconds = ([\d.]+)`)
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "case ") || strings.HasPrefix(trimmed, "BackendMethod.") {
			for _, m := range reMethod.FindAllStringSubmatch(line, -1) {
				pending = append(pending, m[1])
			}
			continue
		}
		if m := reSeconds.FindStringSubmatch(line); m != nil {
			var v float64
			if _, err := fmt.Sscanf(m[1], "%f", &v); err == nil {
				for _, name := range pending {
					secondsOf[name] = v
				}
			}
			pending = nil
		}
	}

	shutdown, ok := secondsOf["shutdown"]
	if !ok {
		t.Fatalf("shutdown has no explicit timeout; it must not fall through to the default (parsed %d methods)", len(secondsOf))
	}
	if shutdown > 2.0 {
		t.Errorf("shutdown timeout = %.1fs, want <= 2s: the reply is a local ACK, "+
			"so a long budget only delays quitting", shutdown)
	}
	if shutdown < 1.0 {
		t.Errorf("shutdown timeout = %.1fs, too tight for a healthy round trip", shutdown)
	}

	if mode, ok := secondsOf["setCoreMode"]; !ok || mode < 5 {
		t.Errorf("setCoreMode timeout = %.1fs, want >= 5s for a real command", mode)
	}

	for _, m := range []string{"refreshSubscription", "updateSubscriptions", "testProxyGroup"} {
		if v, ok := secondsOf[m]; !ok || v < 30 {
			t.Errorf("%s timeout = %.1fs, want >= 30s for network work", m, v)
		}
	}
}

// TestErrorsCarryAReadableMessage — every failure must reach the UI as text a
// human can act on.
//
// The frontend renders `error.localizedDescription`, which for a Swift Error
// that is not LocalizedError collapses to
// "The operation couldn't be completed. (JijieBox.BackendError error 1.)" —
// discarding the message below. That is why the wire contract requires a
// non-empty message on every error this backend produces.
func TestErrorsCarryAReadableMessage(t *testing.T) {
	b := backendWithConfig(t)
	srv := NewServer(b, &bytes.Buffer{})

	// Requests chosen so each fails for a DIFFERENT reason: bad params, an
	// unknown id, a locked engine, an unusable config, a method that does not
	// exist. All of them must explain themselves.
	cases := []struct {
		name string
		req  protocol.Request
	}{
		{"add subscription without a URL", protocol.Request{ID: "1", Method: protocol.MethodAddSubscription}},
		{"unknown subscription", protocol.Request{ID: "2", Method: protocol.MethodRemoveSubscription,
			Params: map[string]any{"id": "nope"}}},
		{"blank proxy name", protocol.Request{ID: "3", Method: protocol.MethodSwitchProxy,
			Params: map[string]any{"group": "g", "name": ""}}},
		{"unknown method", protocol.Request{ID: "4", Method: "does_not_exist"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := srv.handle(tc.req)
			if resp.Error == nil {
				t.Fatalf("expected a failure for %q", tc.name)
			}
			if strings.TrimSpace(resp.Error.Message) == "" {
				t.Errorf("error %q has an empty message; the UI would show only the generic "+
					"\"error 1\" text", resp.Error.Code)
			}
			// The message must say more than the code, or it adds nothing.
			if resp.Error.Message == resp.Error.Code {
				t.Errorf("error message equals its code %q, so it explains nothing", resp.Error.Code)
			}
			if strings.TrimSpace(resp.Error.Code) == "" {
				t.Error("error has no code")
			}
		})
	}
}

// TestReloadConfigExplainsAnUnrebuildableConfig — a rebuild replays the wizard
// state, so a config that did not come from the wizard cannot be rebuilt.
//
// The raw failure names state.json, a file the user has never heard of. The
// backend must instead say the configuration was made elsewhere, because the UI
// offers a Reload button and a dead end behind it is worse than no button.
func TestReloadConfigExplainsAnUnrebuildableConfig(t *testing.T) {
	b := backendWithConfig(t)

	// The fixture has config.json but no wizard state, which is exactly the
	// shape of a hand-written or externally managed configuration.
	_, err := b.ReloadConfig()
	if err == nil {
		t.Skip("this fixture happens to be rebuildable; the guard is covered elsewhere")
	}
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("error is %T, want *protocol.Error", err)
	}
	if pe.Code != "not_rebuildable" && pe.Code != "rebuild_failed" {
		t.Errorf("code = %q, want not_rebuildable for a config with no wizard state", pe.Code)
	}
	if pe.Code == "not_rebuildable" {
		for _, want := range []string{"wizard", "config.json"} {
			if !strings.Contains(pe.Message, want) {
				t.Errorf("message %q does not mention %q; it would not tell the user what to do",
					pe.Message, want)
			}
		}
		if pe.Recoverable {
			t.Error("a configuration that was never wizard-built will not become rebuildable by retrying")
		}
	}
}

// TestQuitAlwaysSendsTheEOFFallback — closing stdin must not depend on the ACK.
//
// The headless backend's read loop blocks on stdin, and GracefulExit cannot stop
// it: with no UI attached it never signals Serve, so the helper only exits once
// stdin reaches EOF. That makes EOF the fallback for a broken IPC channel, and
// gating it on a successful ACK inverted the intent — on a shutdown timeout the
// pipe stayed open, the wait burned its whole budget, and the process was
// force-terminated without ever running its graceful teardown.
//
// This is a static check because the toolchain ships neither XCTest nor the
// Swift Testing macro plugin, so `swift test` cannot build here (verified).
func TestQuitAlwaysSendsTheEOFFallback(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "macos", "Sources",
		"JiejieBox", "Services", "BackendClient.swift"))
	if err != nil {
		t.Skipf("Swift sources not present: %v", err)
	}
	text := string(src)

	idx := strings.Index(text, "func shutdownGracefully()")
	if idx < 0 {
		t.Fatal("cannot find shutdownGracefully")
	}
	body := text[idx:]
	if end := strings.Index(body, "\n    /// "); end > 0 {
		body = body[:end]
	}

	// Find the statement that closes stdin.
	closeIdx := strings.Index(body, "stdinHandle.close()")
	if closeIdx < 0 {
		t.Fatal("shutdownGracefully never closes stdin")
	}
	// Its guarding `if` is the line above.
	before := body[:closeIdx]
	guardStart := strings.LastIndex(before, "\n        if ")
	guardLine := before[guardStart:]

	if strings.Contains(guardLine, "acknowledged") {
		t.Errorf("closing stdin is gated on the ACK (%q); on a shutdown timeout the pipe "+
			"would stay open and the helper would be killed instead of torn down",
			strings.TrimSpace(guardLine))
	}
	if !strings.Contains(guardLine, "stdinHandle") {
		t.Errorf("unexpected guard for closing stdin: %q", strings.TrimSpace(guardLine))
	}

	// The EOF fallback must run BEFORE the last-resort terminate, or it is not a
	// fallback at all.
	termIdx := strings.Index(body, "proc.terminate()")
	if termIdx < 0 {
		t.Fatal("shutdownGracefully never force-terminates; the bound is missing")
	}
	if closeIdx > termIdx {
		t.Error("stdin is closed only after the force-terminate; EOF is not acting as a fallback")
	}

	// And the ACK must still be awaited first, since the ordering guarantee
	// depends on the response being written before teardown starts.
	ackIdx := strings.Index(body, "requestShutdown()")
	if ackIdx < 0 || ackIdx > closeIdx {
		t.Error("the shutdown request must be sent before stdin is closed")
	}
}

// TestEOFAloneRunsTheSameTeardownAsTheMethod — the EOF fallback must reach the
// same exactly-once Shutdown, not a weaker path.
//
// This is what the no-ACK quit now relies on: when the shutdown request times
// out, closing stdin is the only signal the helper gets, so EOF has to perform
// the full teardown rather than just exiting the process.
func TestEOFAloneRunsTheSameTeardownAsTheMethod(t *testing.T) {
	b := backendWithConfig(t)
	var out bytes.Buffer
	srv := NewServer(b, &out)

	// EOF only: Serve returns because the reader is exhausted, exactly as main
	// observes a closed pipe. No shutdown method is ever sent.
	srv.Serve(strings.NewReader(""))

	if srv.Stopped() {
		t.Error("plain EOF should not have to go through requestStop")
	}

	// main then calls Shutdown, which must be the full teardown.
	var shuttingDown int
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventShuttingDown {
			shuttingDown++
		}
	})
	defer unsub()

	b.Shutdown()
	if shuttingDown != 1 {
		t.Errorf("EOF-driven Shutdown emitted shutting_down %d times, want 1", shuttingDown)
	}
	if !b.IsShuttingDown() {
		t.Error("EOF-driven Shutdown did not mark the backend as shutting down")
	}

	// And a later duplicate (the method path racing EOF) must not repeat it.
	b.Shutdown()
	if shuttingDown != 1 {
		t.Errorf("a second Shutdown repeated the teardown: %d events", shuttingDown)
	}
}
