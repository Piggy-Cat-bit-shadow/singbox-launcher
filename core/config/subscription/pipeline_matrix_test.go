package subscription

import (
	"strings"
	"testing"
)

// TestPipelineCapabilityMatrix — every supported format must survive ALL layers.
//
// This is the check that would have caught the original bug and the WireGuard
// regression that followed it. The failing pattern is always the same: each
// stage passes its own unit tests, but an upstream stage accepts LESS than the
// parser downstream, so a supported body never reaches the code that handles it.
//
// The invariant: if ClassifySubscriptionBody recognises a kind, then
// DecodeSubscriptionContent must pass it through and ParseSubscriptionBody must
// produce entries. A stage may only be narrower than the parser when it has an
// explicit reason, and no stage here does.
func TestPipelineCapabilityMatrix(t *testing.T) {
	const uuid = "00000000-0000-0000-0000-000000000001"

	cases := []struct {
		name      string
		body      string
		wantKind  BodyKind
		wantNodes bool
	}{
		{
			name:      "plain URI list",
			body:      "vless://" + uuid + "@a.example:443#Node A\ntrojan://pw@b.example:443#Node B\n",
			wantKind:  BodyKindURIList,
			wantNodes: true,
		},
		{
			name:      "sing-box outbound",
			body:      `{"type":"vless","tag":"n","server":"a.example","server_port":443,"uuid":"` + uuid + `"}`,
			wantKind:  BodyKindSingboxOutbound,
			wantNodes: true,
		},
		{
			name:      "sing-box config",
			body:      `{"outbounds":[{"type":"vless","tag":"n","server":"a.example","server_port":443,"uuid":"` + uuid + `"}]}`,
			wantKind:  BodyKindSingboxConfig,
			wantNodes: true,
		},
		{
			name:      "sing-box outbound array",
			body:      `[{"type":"vless","tag":"n","server":"a.example","server_port":443,"uuid":"` + uuid + `"}]`,
			wantKind:  BodyKindSingboxOutboundArray,
			wantNodes: true,
		},
		{
			name:      "sing-box config array",
			body:      `[{"outbounds":[{"type":"vless","tag":"n","server":"a.example","server_port":443,"uuid":"` + uuid + `"}]}]`,
			wantKind:  BodyKindSingboxConfigArray,
			wantNodes: true,
		},
		{
			name:      "Xray config",
			body:      `{"outbounds":[{"protocol":"vless","tag":"n","settings":{"vnext":[{"address":"a.example","port":443,"users":[{"id":"` + uuid + `"}]}]}}]}`,
			wantKind:  BodyKindXrayConfig,
			wantNodes: true,
		},
		{
			name:      "Xray array",
			body:      `[{"outbounds":[{"protocol":"vless","tag":"n","settings":{"vnext":[{"address":"a.example","port":443,"users":[{"id":"` + uuid + `"}]}]}}]}]`,
			wantKind:  BodyKindXrayArray,
			wantNodes: true,
		},
		{
			// The case that broke twice. A wg-quick body starts with
			// "[Interface]", so a "starts with '['" test calls it a JSON array
			// and rejects it as malformed. Only the classifier knows better.
			name: "WireGuard .conf",
			body: "[Interface]\nPrivateKey = aGVsbG8=\nAddress = 10.0.0.2/32\n" +
				"[Peer]\nPublicKey = d29ybGQ=\nEndpoint = c.example:51820\n",
			wantKind:  BodyKindWGConf,
			wantNodes: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifySubscriptionBody(tc.body); got != tc.wantKind {
				t.Fatalf("classify = %s, want %s", got, tc.wantKind)
			}

			decoded, err := DecodeSubscriptionContent([]byte(tc.body))
			if err != nil {
				t.Fatalf("decode rejected a %s body: %v", tc.wantKind, err)
			}
			// Compared with surrounding whitespace trimmed: the decoder
			// normalises the edges of every body (a pre-existing behaviour), so
			// a trailing newline may be dropped. What must NOT happen is the
			// decoder altering the content itself.
			if strings.TrimSpace(string(decoded)) != strings.TrimSpace(tc.body) {
				t.Errorf("the decoder rewrote the body\n got: %q\nwant: %q", decoded, tc.body)
			}

			parsed, err := ParseSubscriptionBody(decoded, nil, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.wantNodes && len(parsed.Entries) == 0 {
				t.Error("a recognised format produced no entries")
			}
		})
	}
}

// TestPipelineRejectsOnlyUnsupportedBodies — the other half of the invariant:
// widening acceptance must not make genuinely invalid bodies look supported.
func TestPipelineRejectsOnlyUnsupportedBodies(t *testing.T) {
	cases := []struct{ name, body, wantErr string }{
		{"unknown JSON object", `{"foo":"bar"}`, "unsupported JSON subscription format"},
		{"unknown JSON array", `[{"foo":"bar"}]`, "unsupported JSON subscription format"},
		{"truncated JSON", `{"outbounds":[`, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeSubscriptionContent([]byte(tc.body))
			if err == nil {
				t.Fatal("an unsupported body was accepted")
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
