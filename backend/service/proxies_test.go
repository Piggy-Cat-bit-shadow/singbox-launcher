package service

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/paths"
)

// proxyFixtureConfig is a minimal config.json with two selector groups.
//
// The group discovery path only reads structure (which outbounds are
// selectors, and route.final), so the nodes themselves never have to be real.
const proxyFixtureConfig = `{
  "outbounds": [
    {"type":"selector","tag":"proxy-out","outbounds":["n1","n2"],"default":"n1"},
    {"type":"selector","tag":"backup","outbounds":["n2"],"default":"n2"},
    {"type":"vless","tag":"n1","server":"127.0.0.1","server_port":1},
    {"type":"vless","tag":"n2","server":"127.0.0.1","server_port":2}
  ],
  "route": {"final": "proxy-out"}
}`

// backendWithConfig builds a backend over a throwaway config fixture.
//
// It never touches the user's data directory and never starts sing-box: the
// config is read for structure only, and the Clash API is unreachable in the
// test environment, which is exactly the "core stopped" case the UI must
// handle.
func backendWithConfig(t *testing.T) *Backend {
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
	if err := os.WriteFile(layout.Data.Bin()+"/config.json", []byte(proxyFixtureConfig), 0o644); err != nil {
		t.Fatalf("cannot write the config fixture: %v", err)
	}

	ac, err := core.NewAppController(layout, nil, nil, nil, nil)
	if err != nil {
		t.Skipf("cannot build a controller in this environment: %v", err)
	}
	b := &Backend{ac: ac}
	// Wire the backend exactly as New() does. A bare &Backend{ac: ac} skips
	// watchCoreState, so the runtime transitions the lifecycle state machine
	// depends on would never fire and every test of a transition would be
	// testing a backend that is not the one that ships.
	b.installOwnershipPolicy()
	b.watchCoreState()
	return b
}

// TestProxyGroupsShape pins the group payload the Swift client decodes.
func TestProxyGroupsShape(t *testing.T) {
	b := backendWithConfig(t)

	list, err := b.ProxyGroups()
	if err != nil {
		t.Fatalf("ProxyGroups: %v", err)
	}

	if len(list.Groups) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(list.Groups), list.Groups)
	}
	names := []string{list.Groups[0].Name, list.Groups[1].Name}
	if names[0] != "proxy-out" || names[1] != "backup" {
		t.Errorf("groups = %v, want [proxy-out backup]", names)
	}
	// route.final decides the default group, so the UI can open on the group
	// the traffic actually uses.
	if list.Group != "proxy-out" {
		t.Errorf("default group = %q, want %q", list.Group, "proxy-out")
	}
	// With no Clash API answering, the list is reported as unavailable rather
	// than as an empty-but-healthy group set.
	if list.Available {
		t.Error("Available = true with no transport; the core is not running")
	}

	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"groups", "proxies", "available"} {
		if _, present := m[key]; !present {
			t.Errorf("proxy list is missing %q", key)
		}
	}
	first, ok := m["groups"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatal("groups[0] is not an object")
	}
	for _, key := range []string{"name", "display_name", "count"} {
		if _, present := first[key]; !present {
			t.Errorf("group is missing %q", key)
		}
	}
}

// TestProxyListEmptyGroupUsesDefault — an empty group means "the config
// default", so a client that only wants a node list does not have to discover
// the group name first.
func TestProxyListEmptyGroupUsesDefault(t *testing.T) {
	b := backendWithConfig(t)

	list, err := b.Proxies("")
	if err != nil {
		t.Fatalf("Proxies(\"\"): %v", err)
	}
	if list.Group != "proxy-out" {
		t.Errorf("group = %q, want the route.final default %q", list.Group, "proxy-out")
	}
	if list.Available {
		t.Error("Available = true with no transport")
	}
}

// TestProxyListUnknownGroupIsHonest — an unknown group must not silently
// return the default group's nodes, or the UI would show the wrong list while
// claiming the requested group.
func TestProxyListUnknownGroupIsHonest(t *testing.T) {
	b := backendWithConfig(t)

	list, err := b.Proxies("does-not-exist")
	if err != nil {
		t.Fatalf("Proxies(unknown): %v", err)
	}
	if list.Group != "does-not-exist" {
		t.Errorf("group = %q, want the requested name echoed back", list.Group)
	}
	if len(list.Proxies) != 0 {
		t.Errorf("got %d proxies for an unknown group, want 0", len(list.Proxies))
	}
}

// TestSwitchProxyRequiresTransport — switching without a running core is an
// ordinary, explainable state, not a crash.
func TestSwitchProxyRequiresTransport(t *testing.T) {
	b := backendWithConfig(t)

	_, err := b.SwitchProxy("proxy-out", "n1")
	if err == nil {
		t.Fatal("SwitchProxy succeeded with no transport")
	}
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("error is %T, want *protocol.Error", err)
	}
	if pe.Code != "core_not_running" {
		t.Errorf("code = %q, want %q", pe.Code, "core_not_running")
	}
	if !pe.Recoverable {
		t.Error("the error should be recoverable: starting the core fixes it")
	}
}

// TestSwitchProxyRequiresName — an empty name is a client bug, so it is
// rejected before any transport work.
func TestSwitchProxyRequiresName(t *testing.T) {
	b := backendWithConfig(t)

	_, err := b.SwitchProxy("proxy-out", "  ")
	if err == nil {
		t.Fatal("SwitchProxy accepted a blank name")
	}
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("error is %T, want *protocol.Error", err)
	}
	if pe.Code != "bad_request" {
		t.Errorf("code = %q, want %q", pe.Code, "bad_request")
	}
	if pe.Recoverable {
		t.Error("a blank name is not recoverable by retrying")
	}
}

// TestMaintenanceResultShape pins the reload/update payload.
func TestMaintenanceResultShape(t *testing.T) {
	raw, err := json.Marshal(MaintenanceResult{
		OK:               true,
		Message:          "3 nodes from 2 sources.",
		TotalSources:     2,
		SucceededSources: 2,
		NodesCount:       3,
		CoreSkips:        []string{"1 vless node(s) skipped: unsupported"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"ok", "message", "total_sources", "succeeded_sources",
		"failed_sources", "nodes_count", "core_skips",
	} {
		if _, present := m[key]; !present {
			t.Errorf("maintenance result is missing %q", key)
		}
	}
}

// TestSummaryMessage — the summary must not claim success when nothing worked.
func TestSummaryMessage(t *testing.T) {
	cases := []struct {
		name                     string
		total, ok, failed, nodes int
		want                     string
	}{
		{"all good", 2, 2, 0, 5, "5 nodes from 2 sources."},
		{"singular", 1, 1, 0, 1, "1 node from 1 source."},
		{"partial", 3, 2, 1, 4, "4 nodes from 2 sources; 1 source failed."},
		{"none enabled", 0, 0, 0, 0, "No subscription sources are enabled."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryMessage(tc.total, tc.ok, tc.failed, tc.nodes); got != tc.want {
				t.Errorf("summaryMessage(%d,%d,%d,%d) = %q, want %q",
					tc.total, tc.ok, tc.failed, tc.nodes, got, tc.want)
			}
		})
	}
}

// TestUpdateSubscriptionsWithoutStateIsAnError — the refresh cannot run
// before the wizard has saved state, and the message must say so instead of
// failing silently.
func TestUpdateSubscriptionsWithoutStateIsAnError(t *testing.T) {
	b := backendWithConfig(t)

	_, err := b.UpdateSubscriptions()
	if err == nil {
		t.Fatal("UpdateSubscriptions succeeded with no state.json")
	}
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("error is %T, want *protocol.Error", err)
	}
	if pe.Message == "" {
		t.Error("the error carries no message for the user")
	}
}

// TestServerDispatchesProxyMethods — every new method must be reachable over
// the wire, not merely present on the Backend type.
func TestServerDispatchesProxyMethods(t *testing.T) {
	b := backendWithConfig(t)
	srv := NewServer(b, &bytes.Buffer{})

	for _, method := range []string{
		protocol.MethodGetProxyGroups,
		protocol.MethodGetProxies,
		protocol.MethodSwitchProxy,
		protocol.MethodTestProxy,
		protocol.MethodTestProxyGroup,
		protocol.MethodReloadConfig,
		protocol.MethodUpdateSubscriptions,
	} {
		resp := srv.handle(protocol.Request{ID: "1", Method: method})
		// Either a result or a structured error is fine here; what must never
		// happen is unknown_method, which would mean the dispatch case is
		// missing.
		if resp.Error != nil && resp.Error.Code == "unknown_method" {
			t.Errorf("%s is not dispatched by the server", method)
		}
	}
}
