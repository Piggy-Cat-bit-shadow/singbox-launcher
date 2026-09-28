package service

import (
	"errors"
	"strings"
	"testing"

	"singbox-launcher/api"
	"singbox-launcher/backend/protocol"
	coreservices "singbox-launcher/core/services"
)

// Proxy capability tests.
//
// These pin the three states the Proxies screen must tell apart. Before the fix
// they were collapsed into one: an engine that cannot list proxies at all was
// reported as a hard `proxy_list_failed` error, which surfaced on Home as a red
// banner naming an internal gRPC method:
//
//	cannot read the proxies of group "🌍 国外流量": daemon GetGroups:
//	rpc error: code = Unimplemented desc = unknown method GetGroups
//
// The user could do nothing with that, and it hid the real state of the app.

// stubTransport is a ProxyTransport with programmable behaviour.
type stubTransport struct {
	proxies []api.ProxyInfo
	now     string
	err     error
}

func (s stubTransport) GroupProxies(string) ([]api.ProxyInfo, string, error) {
	if s.err != nil {
		return nil, "", s.err
	}
	return s.proxies, s.now, nil
}

func (s stubTransport) SwitchProxy(string, string) error { return s.err }
func (s stubTransport) Delay(string) (int64, error)      { return 0, s.err }

// proxyBackend builds a backend whose APIService carries the given transport, so
// the proxy path is exercised without a running core or a live daemon.
func proxyBackend(t *testing.T, tr coreservices.ProxyTransport) *Backend {
	t.Helper()
	b := backendWithConfig(t)
	if b.ac.APIService == nil {
		t.Skip("no APIService in this environment")
	}
	b.ac.APIService.SetTransport(tr)
	return b
}

// TestProxyListUnsupportedIsACapabilityNotAnError — the P0 regression from the
// user report.
//
// An engine without the group RPC must produce a SUCCESSFUL response that says
// it is unsupported. Returning an error here is what polluted Home with a
// banner the user could not act on, and it is the difference between "your
// engine cannot do this" and "something went wrong".
func TestProxyListUnsupportedIsACapabilityNotAnError(t *testing.T) {
	b := proxyBackend(t, stubTransport{err: coreservices.ErrProxyListUnsupported})

	// The reported failure path: listing nodes of a named group.
	list, err := b.Proxies("🌍 国外流量")
	if err != nil {
		t.Fatalf("an unsupported engine must not be an error, got: %v", err)
	}
	if list.Supported == nil {
		t.Fatal("supported is absent; the frontend cannot tell this apart from an old backend")
	}
	if *list.Supported {
		t.Error("supported = true for an engine that reported it cannot list proxies")
	}
	if list.UnsupportedReason != "daemon_no_group_rpc" {
		t.Errorf("unsupported_reason = %q, want daemon_no_group_rpc", list.UnsupportedReason)
	}
	if list.Available {
		t.Error("available = true; an engine that cannot list proxies has nothing available")
	}
	if len(list.Proxies) != 0 {
		t.Errorf("got %d proxies from an unsupported engine; fabricating data is not a fix",
			len(list.Proxies))
	}

	// The group-list path must agree: one capability, one answer.
	groups, gerr := b.ProxyGroups()
	if gerr != nil {
		t.Fatalf("ProxyGroups: %v", gerr)
	}
	if groups.Supported == nil || *groups.Supported {
		t.Error("ProxyGroups reports supported while Proxies reports unsupported")
	}
	if groups.Available {
		t.Error("ProxyGroups reports available for an engine that cannot list proxies")
	}
	// The group NAMES still come from the config, so the picker is populated and
	// the user can see what their configuration defines.
	if len(groups.Groups) == 0 {
		t.Error("no groups returned; the names come from config.json and must survive")
	}
}

// TestReportedDaemonScenario — the exact failure from the user report, pinned.
//
// The literal report was:
//
//	cannot read the proxies of group "🌍 国外流量": daemon GetGroups:
//	rpc error: code = Unimplemented desc = unknown method GetGroups …
//
// This test asserts the response shape that replaced it, so a future change
// cannot quietly reintroduce the error string. The group name is the user's own
// (non-ASCII), because the bug report was specifically about a group that exists
// in their config.
func TestReportedDaemonScenario(t *testing.T) {
	b := proxyBackend(t, stubTransport{err: coreservices.ErrProxyListUnsupported})

	list, err := b.Proxies("🌍 国外流量")
	if err != nil {
		t.Fatalf("the reported case is an error again: %v", err)
	}
	if list.Supported == nil || *list.Supported {
		t.Fatal("supported is not false for a daemon without the group RPC")
	}
	if list.UnsupportedReason != "daemon_no_group_rpc" {
		t.Errorf("unsupported_reason = %q, want daemon_no_group_rpc", list.UnsupportedReason)
	}
	if list.Available {
		t.Error("available = true; nothing is available from an engine that cannot list proxies")
	}
}

// TestProxyListSupportedStillWorks — the classic path must not regress.
//
// A working transport reports supported=true and carries real nodes, so the fix
// cannot have been "always say unsupported".
func TestProxyListSupportedStillWorks(t *testing.T) {
	b := proxyBackend(t, stubTransport{
		proxies: []api.ProxyInfo{
			{Name: "n1", ClashType: "vless", Delay: 42},
			{Name: "n2", ClashType: "trojan", Delay: 0},
		},
		now: "n1",
	})

	list, err := b.Proxies("proxy-out")
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}
	if list.Supported == nil || !*list.Supported {
		t.Fatal("supported = false for a working transport")
	}
	if list.UnsupportedReason != "" {
		t.Errorf("unsupported_reason = %q on a supported engine", list.UnsupportedReason)
	}
	if !list.Available {
		t.Error("available = false while a transport is present and answering")
	}
	if len(list.Proxies) != 2 {
		t.Fatalf("got %d proxies, want 2", len(list.Proxies))
	}
	if !list.Proxies[0].Selected {
		t.Error("the selected node is not marked")
	}
	// delay <= 0 means "never measured" and must stay distinct from 0 ms.
	if list.Proxies[1].Delay != -1 {
		t.Errorf("unmeasured delay = %d, want -1 so the UI can show — instead of 0 ms",
			list.Proxies[1].Delay)
	}
}

// TestProxyListGenuineFailureIsStillAnError — the fix must not swallow real
// failures. A transport that breaks for a reason unrelated to capability has to
// keep reporting an error, or the user loses the ability to retry.
func TestProxyListGenuineFailureIsStillAnError(t *testing.T) {
	b := proxyBackend(t, stubTransport{err: errors.New("connection refused")})

	_, err := b.Proxies("proxy-out")
	if err == nil {
		t.Fatal("a genuine transport failure was reported as success")
	}
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("unexpected error type %T", err)
	}
	if pe.Code != "proxy_list_failed" {
		t.Errorf("error code = %q, want proxy_list_failed", pe.Code)
	}
	// And it must still name the group, because that IS actionable context for
	// a real failure.
	if !strings.Contains(pe.Message, "proxy-out") {
		t.Errorf("message %q does not name the group", pe.Message)
	}
}
