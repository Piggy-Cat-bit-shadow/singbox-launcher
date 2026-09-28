package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Fallback identity verification must FAIL CLOSED.
//
// The launcher may fall back to a local Clash API when a gRPC capability is
// absent. That endpoint is reached over loopback and carries the user's proxy
// control, so proving it belongs to OUR core is a security boundary — and the
// proof used to be vacuous in exactly the case where it mattered most.

// fallbackServer serves a Clash-shaped /proxies response.
func fallbackServer(t *testing.T, proxies map[string]any, wantAuth string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != "Bearer "+wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"proxies": proxies})
	}))
}

// TestFallbackIdentityFailsWithEmptyProof is claim T.
//
// With no expected groups the old loop checked nothing and `checkIdentity`
// returned true. Any Clash-shaped API answering on that port therefore passed
// identity verification — and when the config's secret was empty there was no
// auth proof either, so the conjunction was empty and satisfied.
//
// An empty proof is not a satisfied proof.
func TestFallbackIdentityFailsWithEmptyProof(t *testing.T) {
	srv := fallbackServer(t, map[string]any{
		"SomeGroup": map[string]any{"type": "Selector", "all": []string{"a", "b"}},
	}, "", 0)
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	cfg := DaemonClashFallbackConfig{Enabled: true, BaseURL: srv.URL}

	if p.checkIdentityProof(context.Background(), cfg, identityProof{}) {
		t.Fatal("an EMPTY proof was accepted; any Clash-compatible API on that port " +
			"would be treated as the user's own core, and the launcher would drive " +
			"its proxy state")
	}
}

// TestFallbackWithEmptySecretRequiresStrongIdentity is claim U's security half.
//
// Without a secret, auth proves nothing, so the structural evidence must carry
// the whole claim: the endpoint must present the group's MEMBERS as the config
// we sent defines them.
func TestFallbackWithEmptySecretRequiresStrongIdentity(t *testing.T) {
	// A foreign core that happens to have a group with our group's NAME but
	// different members must be rejected.
	srv := fallbackServer(t, map[string]any{
		"proxy-out": map[string]any{"type": "Selector", "all": []string{"stranger-1", "stranger-2"}},
	}, "", 0)
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	cfg := DaemonClashFallbackConfig{Enabled: true, BaseURL: srv.URL}

	proof := identityProof{
		Groups:    []string{"proxy-out"},
		Members:   map[string][]string{"proxy-out": {"node-a", "node-b"}},
		HasSecret: false,
	}
	if p.checkIdentityProof(context.Background(), cfg, proof) {
		t.Fatal("a foreign core with our group NAME but different members was accepted; " +
			"group names are common enough to coincide by accident, and with no secret " +
			"there is no other proof")
	}
}

// TestFallbackAcceptsAGenuineEndpoint — the checks must not reject the real
// thing, or the fallback would simply never work.
func TestFallbackAcceptsAGenuineEndpoint(t *testing.T) {
	srv := fallbackServer(t, map[string]any{
		"proxy-out": map[string]any{"type": "Selector", "all": []string{"node-a", "node-b"}},
	}, "s3cret", 0)
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	cfg := DaemonClashFallbackConfig{Enabled: true, BaseURL: srv.URL, Token: "s3cret"}

	proof := identityProof{
		Groups:    []string{"proxy-out"},
		Members:   map[string][]string{"proxy-out": {"node-a", "node-b"}},
		HasSecret: true,
	}
	if !p.checkIdentityProof(context.Background(), cfg, proof) {
		t.Fatal("a genuine endpoint with matching groups and members was rejected")
	}
}

// TestFallbackMemberComparisonIsOrderInsensitive — Clash reports members in its
// own order, and the claim is about the SET.
func TestFallbackMemberComparisonIsOrderInsensitive(t *testing.T) {
	srv := fallbackServer(t, map[string]any{
		"proxy-out": map[string]any{"type": "Selector", "all": []string{"node-b", "node-a"}},
	}, "", 0)
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	cfg := DaemonClashFallbackConfig{Enabled: true, BaseURL: srv.URL}
	proof := identityProof{
		Groups:    []string{"proxy-out"},
		Members:   map[string][]string{"proxy-out": {"node-a", "node-b"}},
		HasSecret: false,
	}
	if !p.checkIdentityProof(context.Background(), cfg, proof) {
		t.Fatal("member order changed the verdict; the comparison must be over sets")
	}
}

// TestFallbackRejectsAMissingGroup — a genuine endpoint that lacks a group we
// sent is not running our config.
func TestFallbackRejectsAMissingGroup(t *testing.T) {
	srv := fallbackServer(t, map[string]any{
		"other": map[string]any{"type": "Selector", "all": []string{"x"}},
	}, "", 0)
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	cfg := DaemonClashFallbackConfig{Enabled: true, BaseURL: srv.URL}
	proof := identityProof{
		Groups:    []string{"proxy-out"},
		Members:   map[string][]string{"proxy-out": {"node-a"}},
		HasSecret: true,
	}
	if p.checkIdentityProof(context.Background(), cfg, proof) {
		t.Fatal("an endpoint lacking our group was accepted")
	}
}

// TestFallbackRejectsUnauthorized — a mismatched token means something else is
// listening.
func TestFallbackRejectsUnauthorized(t *testing.T) {
	srv := fallbackServer(t, map[string]any{
		"proxy-out": map[string]any{"type": "Selector", "all": []string{"node-a"}},
	}, "expected-token", 0)
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	cfg := DaemonClashFallbackConfig{Enabled: true, BaseURL: srv.URL, Token: "wrong-token"}
	proof := identityProof{
		Groups:    []string{"proxy-out"},
		Members:   map[string][]string{"proxy-out": {"node-a"}},
		HasSecret: true,
	}
	if p.checkIdentityProof(context.Background(), cfg, proof) {
		t.Fatal("an endpoint that rejected our credentials was accepted as our core")
	}
}

// TestPreparedConfigCarriesMembers — the proof must come from the bytes that were
// actually sent, or it proves nothing about the running core.
func TestPreparedConfigCarriesMembers(t *testing.T) {
	config := []byte(`{
		"outbounds": [
			{"type":"selector","tag":"proxy-out","outbounds":["node-a","node-b"]},
			{"type":"urltest","tag":"auto","outbounds":["node-a","node-b","node-c"]},
			{"type":"shadowsocks","tag":"node-a"}
		]
	}`)

	prepared, err := prepareDaemonConfig(config, t.TempDir(), daemonPlatformPrepOptions())
	if err != nil {
		t.Fatalf("prepareDaemonConfig: %v", err)
	}

	if len(prepared.SelectorMembers) != 2 {
		t.Fatalf("SelectorMembers = %v, want the two selector/urltest groups",
			prepared.SelectorMembers)
	}
	if got := prepared.SelectorMembers["proxy-out"]; len(got) != 2 {
		t.Errorf("proxy-out members = %v, want [node-a node-b]", got)
	}
	if got := prepared.SelectorMembers["auto"]; len(got) != 3 {
		t.Errorf("auto members = %v, want three entries", got)
	}
}

// TestSelectorMembersOnAConfigWithoutSelectorsIsEmpty — the helper must not
// invent members, because an invented member set would make the proof wrong
// rather than merely weak.
func TestSelectorMembersOnAConfigWithoutSelectorsIsEmpty(t *testing.T) {
	config := []byte(`{"outbounds":[{"type":"shadowsocks","tag":"only"}]}`)
	prepared, err := prepareDaemonConfig(config, t.TempDir(), daemonPlatformPrepOptions())
	if err != nil {
		t.Fatalf("prepareDaemonConfig: %v", err)
	}
	if len(prepared.SelectorMembers) != 0 {
		t.Fatalf("SelectorMembers = %v, want empty when there are no selectors",
			prepared.SelectorMembers)
	}
}
