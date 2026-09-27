package service

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
)

// TestSubscriptionLifecycle — the whole point of the subscription manager is
// that add, edit, enable, refresh-target and delete all operate on the SAME
// canonical record. A manager that wrote anywhere else would drift from the
// wizard and the backup importer.
//
// The fixture is a throwaway data dir; nothing here touches user data or the
// network (refresh is only checked for its error contract, never executed
// against a provider).
func TestSubscriptionLifecycle(t *testing.T) {
	b := backendWithConfig(t)

	// A fresh install has no state.json at all, and the menu bar must still be
	// able to add the first subscription.
	initial, err := b.Subscriptions()
	if err != nil {
		t.Fatalf("Subscriptions on a fresh install: %v", err)
	}
	if len(initial) != 0 {
		t.Fatalf("fresh install has %d subscriptions, want 0", len(initial))
	}

	// Add with no name: the backend derives a display name from the host.
	added, err := b.AddSubscription("", "https://provider.example/subscribe?token=abc")
	if err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}
	if added.ID == "" {
		t.Error("added subscription has no id")
	}
	if added.Name != "provider.example" {
		t.Errorf("derived name = %q, want the host %q", added.Name, "provider.example")
	}
	if !added.Enabled {
		t.Error("a new subscription should be enabled by default")
	}

	// The canonical tree must actually contain it.
	list, err := b.Subscriptions()
	if err != nil {
		t.Fatalf("Subscriptions after add: %v", err)
	}
	if len(list) != 1 || list[0].ID != added.ID {
		t.Fatalf("list = %+v, want the added subscription", list)
	}

	// Duplicate URLs are rejected: the same link would fetch the same nodes.
	if _, err := b.AddSubscription("Copy", "https://provider.example/subscribe?token=abc"); err == nil {
		t.Error("a duplicate URL was accepted")
	} else if pe, ok := err.(*protocol.Error); !ok || pe.Code != "duplicate" {
		t.Errorf("duplicate error = %v, want code duplicate", err)
	}

	// Rename.
	renamed, err := b.UpdateSubscription(added.ID, "My Provider", "", nil)
	if err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if renamed.Name != "My Provider" {
		t.Errorf("name = %q, want %q", renamed.Name, "My Provider")
	}
	// An edit that does not mention the URL must not blank it.
	if renamed.URL != added.URL {
		t.Errorf("URL changed to %q by a name-only edit", renamed.URL)
	}

	// Disable, then re-enable.
	disabled, err := b.SetSubscriptionEnabled(added.ID, false)
	if err != nil {
		t.Fatalf("SetSubscriptionEnabled(false): %v", err)
	}
	if disabled.Enabled {
		t.Error("subscription is still enabled")
	}
	enabled, err := b.SetSubscriptionEnabled(added.ID, true)
	if err != nil {
		t.Fatalf("SetSubscriptionEnabled(true): %v", err)
	}
	if !enabled.Enabled {
		t.Error("subscription is still disabled")
	}

	// Delete, and confirm it is gone.
	if err := b.RemoveSubscription(added.ID); err != nil {
		t.Fatalf("RemoveSubscription: %v", err)
	}
	after, err := b.Subscriptions()
	if err != nil {
		t.Fatalf("Subscriptions after remove: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("got %d subscriptions after delete, want 0", len(after))
	}
}

// TestAddSubscriptionValidation — the backend is the validator, so the Swift
// form and the stored record cannot disagree about what is acceptable.
func TestAddSubscriptionValidation(t *testing.T) {
	b := backendWithConfig(t)

	cases := []struct {
		name string
		url  string
		code string
	}{
		{"empty", "", "bad_request"},
		{"whitespace", "   ", "bad_request"},
		{"not a url", "provider.example/sub", "bad_request"},
		{"wrong scheme", "ftp://provider.example/sub", "bad_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.AddSubscription("x", tc.url)
			if err == nil {
				t.Fatalf("AddSubscription(%q) succeeded", tc.url)
			}
			pe, ok := err.(*protocol.Error)
			if !ok {
				t.Fatalf("error is %T, want *protocol.Error", err)
			}
			if pe.Code != tc.code {
				t.Errorf("code = %q, want %q", pe.Code, tc.code)
			}
		})
	}

	// http:// is accepted too: some providers are plain-HTTP on a LAN.
	if _, err := b.AddSubscription("", "http://192.168.1.10:8080/sub"); err != nil {
		t.Errorf("plain http URL was rejected: %v", err)
	}
}

// TestSubscriptionUnknownID — every per-id operation must reject an unknown id
// rather than silently succeeding, which would hide a stale UI row.
func TestSubscriptionUnknownID(t *testing.T) {
	b := backendWithConfig(t)

	if _, err := b.UpdateSubscription("nope", "x", "", nil); err == nil {
		t.Error("UpdateSubscription accepted an unknown id")
	} else if pe, ok := err.(*protocol.Error); !ok || pe.Code != "not_found" {
		t.Errorf("update error = %v, want not_found", err)
	}

	if err := b.RemoveSubscription("nope"); err == nil {
		t.Error("RemoveSubscription accepted an unknown id")
	}

	if _, err := b.SetSubscriptionEnabled("nope", true); err == nil {
		t.Error("SetSubscriptionEnabled accepted an unknown id")
	}
}

// TestSubscriptionEditingMarksConfigStale — editing sources must not rebuild
// the config silently. The product rule is that a rebuild is the user's
// decision, so the backend flags staleness and the UI offers the reload.
func TestSubscriptionEditingMarksConfigStale(t *testing.T) {
	b := backendWithConfig(t)

	if b.configStale() {
		t.Fatal("a fresh backend reports a stale config")
	}

	added, err := b.AddSubscription("Provider", "https://provider.example/sub")
	if err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}
	if err := b.RemoveSubscription(added.ID); err != nil {
		t.Fatalf("RemoveSubscription: %v", err)
	}

	if !b.configStale() {
		t.Error("editing subscriptions did not mark the config stale; " +
			"the UI would have no way to tell the user a reload is needed")
	}
}

// TestSubscriptionDTOShape pins the wire keys the Swift client decodes.
func TestSubscriptionDTOShape(t *testing.T) {
	raw, err := json.Marshal(toSubscriptionDTO(nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// These are the fields the app reads; a rename breaks it at runtime with no
	// compile error on either side.
	for _, key := range []string{"id", "name", "url", "enabled", "node_count", "max_nodes"} {
		if _, present := m[key]; !present {
			t.Errorf("subscription DTO is missing %q", key)
		}
	}
}

// TestServerDispatchesSubscriptionMethods — every method must be reachable over
// the wire, not merely present on the Backend type.
func TestServerDispatchesSubscriptionMethods(t *testing.T) {
	b := backendWithConfig(t)
	srv := NewServer(b, &bytes.Buffer{})

	for _, method := range []string{
		protocol.MethodListSubscriptions,
		protocol.MethodAddSubscription,
		protocol.MethodUpdateSubscription,
		protocol.MethodRemoveSubscription,
		protocol.MethodSetSubscriptionEnabled,
		protocol.MethodRefreshSubscription,
	} {
		resp := srv.handle(protocol.Request{ID: "1", Method: method})
		if resp.Error != nil && resp.Error.Code == "unknown_method" {
			t.Errorf("%s is not dispatched by the server", method)
		}
	}
}

// TestConfigStaleSurvivesRestart — the dirty markers are in-memory and reset
// when the backend restarts, so staleness must ALSO be derivable from disk.
//
// Without this, adding a subscription, quitting and relaunching would silently
// drop the "reload needed" prompt: the user's new nodes would not be in the
// built config and nothing would say so.
func TestConfigStaleSurvivesRestart(t *testing.T) {
	b := backendWithConfig(t)

	// Give the fixture a built config, older than the state we are about to
	// write, so the timestamps express "state changed after the build".
	configPath := b.ac.FileService.ConfigPath
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"outbounds":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(configPath, past, past); err != nil {
		t.Fatal(err)
	}

	if b.configStale() {
		t.Fatal("config should not be stale before any state edit")
	}

	if _, err := b.AddSubscription("Provider", "https://provider.example/sub"); err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}

	// A brand-new backend over the same layout simulates a relaunch: the
	// in-memory markers are gone, so only the disk comparison can report this.
	fresh := &Backend{ac: b.ac}
	if !fresh.configStale() {
		t.Error("staleness did not survive a restart: a relaunch would hide the reload prompt")
	}
}
