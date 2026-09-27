package service

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"singbox-launcher/backend/protocol"
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
		"auto_update_subscriptions", "data_dir", "config_path",
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
