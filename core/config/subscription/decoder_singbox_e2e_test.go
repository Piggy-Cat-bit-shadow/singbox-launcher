package subscription

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// singboxConfigFixture is a realistic "sing-box 版" subscription body: a whole
// config carrying service outbounds, a selector, a urltest and real nodes.
//
// It is deliberately a full config rather than a bare node list, because that is
// the shape the decoder used to reject outright. Credentials and hosts are
// placeholders — no real URL, token or server appears here.
const singboxConfigFixture = `{
  "log": {"level": "info"},
  "dns": {"servers": [{"address": "local"}]},
  "inbounds": [{"type": "tun", "tag": "tun-in"}],
  "outbounds": [
    {"type": "selector", "tag": "proxy-out", "outbounds": ["auto", "Node A"], "default": "auto"},
    {"type": "urltest", "tag": "auto", "outbounds": ["Node A", "Node B"]},
    {"type": "vless", "tag": "Node A", "server": "a.example", "server_port": 443,
     "uuid": "00000000-0000-0000-0000-000000000001"},
    {"type": "trojan", "tag": "Node B", "server": "b.example", "server_port": 443,
     "password": "placeholder"},
    {"type": "direct", "tag": "direct"},
    {"type": "block", "tag": "block"},
    {"type": "dns", "tag": "dns-out"}
  ],
  "route": {"final": "proxy-out"}
}`

// TestSingboxConfigSubscriptionEndToEnd drives the whole chain the refresh path
// uses: HTTP response -> decode -> parse -> materialised entries.
//
// Testing only the decoder would miss the point of the bug: the failure was that
// the decoder refused a body the PARSER supports, so the fix is only proven by
// showing nodes actually come out the other end.
func TestSingboxConfigSubscriptionEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Profile-Title", "Singbox Config Sub")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(singboxConfigFixture))
	}))
	defer srv.Close()

	res, err := FetchSubscriptionWithMeta(srv.URL)
	if err != nil {
		t.Fatalf("FetchSubscriptionWithMeta: %v", err)
	}

	decoded, err := DecodeSubscriptionContent(res.RawBody)
	if err != nil {
		t.Fatalf("DecodeSubscriptionContent rejected a supported config: %v", err)
	}
	if string(decoded) != singboxConfigFixture {
		t.Error("the decoder modified the body")
	}
	if kind := ClassifySubscriptionBody(string(decoded)); kind != BodyKindSingboxConfig {
		t.Fatalf("classified as %s, want %s", kind, BodyKindSingboxConfig)
	}

	parsed, err := ParseSubscriptionBody(decoded, nil, 0)
	if err != nil {
		t.Fatalf("ParseSubscriptionBody: %v", err)
	}
	if len(parsed.Entries) == 0 {
		t.Fatal("a supported sing-box config produced no entries")
	}

	// The real nodes must be present, so the fix delivers usable proxies and not
	// merely "no error".
	nodes := map[string]string{} // tag -> scheme
	groups := map[string]bool{}
	for _, e := range parsed.Entries {
		if e.Node == nil {
			continue
		}
		if string(e.Node.Scheme) == "group" {
			groups[e.RawTag] = true
			continue
		}
		nodes[e.RawTag] = string(e.Node.Scheme)
	}

	for tag, wantScheme := range map[string]string{"Node A": "vless", "Node B": "trojan"} {
		got, ok := nodes[tag]
		if !ok {
			t.Errorf("node %q is missing from the parse result (got %v)", tag, nodes)
			continue
		}
		if got != wantScheme {
			t.Errorf("node %q scheme = %q, want %q", tag, got, wantScheme)
		}
	}

	// Service outbounds are not proxies and must never be shown as nodes: a user
	// seeing "direct" or "block" in the proxy list would be misled into selecting
	// something that cannot carry traffic.
	for _, service := range []string{"direct", "block", "dns-out"} {
		if _, isNode := nodes[service]; isNode {
			t.Errorf("service outbound %q was materialised as a node", service)
		}
	}

	// selector / urltest keep the importer's existing semantics: they become
	// groups, not nodes.
	for _, groupTag := range []string{"proxy-out", "auto"} {
		if !groups[groupTag] {
			t.Errorf("group %q is missing; selector/urltest handling changed", groupTag)
		}
		if _, isNode := nodes[groupTag]; isNode {
			t.Errorf("%q became a node instead of a group", groupTag)
		}
	}
}

// TestSingboxConfigFixtureIsNotLeakingSecrets — the fixture is committed, so it
// must never carry a real credential, endpoint or subscription URL.
//
// Checked mechanically rather than by eye, because a fixture copied from a live
// response is exactly how a token ends up in the repository.
func TestSingboxConfigFixtureIsNotLeakingSecrets(t *testing.T) {
	for _, forbidden := range []string{
		"http://", "https://", "token", "subscribe?", "uuid=", "password=",
	} {
		if contains(singboxConfigFixture, forbidden) {
			t.Errorf("the fixture contains %q; it must stay clearly synthetic", forbidden)
		}
	}
}
