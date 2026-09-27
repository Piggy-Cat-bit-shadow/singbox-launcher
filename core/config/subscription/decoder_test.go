package subscription

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNormalizeSubscriptionTextLine(t *testing.T) {
	in := "  vless://x@y?a=1&amp;b=2  "
	got := NormalizeSubscriptionTextLine(in)
	want := "vless://x@y?a=1&b=2"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if NormalizeSubscriptionTextLine("") != "" {
		t.Fatal("empty in should be empty out")
	}
}

// TestDecodeSubscriptionContent tests the DecodeSubscriptionContent function
func TestDecodeSubscriptionContent(t *testing.T) {
	tests := []struct {
		name        string
		content     []byte
		expectError bool
		checkResult func(*testing.T, []byte)
	}{
		{
			name:        "Base64 URL encoded content",
			content:     []byte(base64.URLEncoding.EncodeToString([]byte("vless://test\nvmess://test"))),
			expectError: false,
			checkResult: func(t *testing.T, decoded []byte) {
				if !strings.Contains(string(decoded), "vless://test") {
					t.Error("Expected decoded content to contain 'vless://test'")
				}
			},
		},
		{
			name:        "Base64 standard encoded content",
			content:     []byte(base64.StdEncoding.EncodeToString([]byte("vless://test\nvmess://test"))),
			expectError: false,
			checkResult: func(t *testing.T, decoded []byte) {
				if !strings.Contains(string(decoded), "vless://test") {
					t.Error("Expected decoded content to contain 'vless://test'")
				}
			},
		},
		{
			name:        "Plain text content",
			content:     []byte("vless://test\nvmess://test"),
			expectError: false,
			checkResult: func(t *testing.T, decoded []byte) {
				if !strings.Contains(string(decoded), "vless://test") {
					t.Error("Expected decoded content to contain 'vless://test'")
				}
			},
		},
		{
			name:        "Empty content",
			content:     []byte(""),
			expectError: true,
		},
		{
			name:        "Whitespace only",
			content:     []byte("   \n\t  "),
			expectError: false,
			checkResult: func(t *testing.T, decoded []byte) {
				if len(decoded) == 0 {
					t.Error("Expected decoded content to be returned even if whitespace")
				}
			},
		},
		{
			name:        "JSON array subscription (Xray-style)",
			content:     []byte(`[ {"remarks":"a","outbounds":[]} ]`),
			expectError: false,
			checkResult: func(t *testing.T, decoded []byte) {
				if !strings.HasPrefix(string(decoded), "[") {
					t.Errorf("expected JSON array, got %q", string(decoded))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decoded, err := DecodeSubscriptionContent(tt.content)
			if tt.expectError {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if tt.checkResult != nil {
				tt.checkResult(t, decoded)
			}
		})
	}
}

// TestDecodeSubscriptionContentPassesStructuredBodies — the decoder must not
// second-guess the formats the importer supports.
//
// It previously rejected every body starting with '{' before the parser ran, so
// a subscription returning a complete sing-box config failed with "returned JSON
// configuration instead of subscription list" even though the importer handles
// exactly that shape. ClassifySubscriptionBody is the single source of truth for
// formats, and the decoder now asks it.
func TestDecodeSubscriptionContentPassesStructuredBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"single sing-box outbound",
			`{"type":"vless","tag":"node-a","server":"a.example","server_port":443}`},
		{"full sing-box config",
			`{"outbounds":[{"type":"vless","tag":"n","server":"a.example","server_port":443}],"route":{"final":"n"}}`},
		{"sing-box outbound array",
			`[{"type":"vless","tag":"a"},{"type":"trojan","tag":"b"}]`},
		{"sing-box config array",
			`[{"outbounds":[{"type":"vless","tag":"a"}]},{"outbounds":[{"type":"trojan","tag":"b"}]}]`},
		{"single Xray config",
			`{"outbounds":[{"protocol":"vless","tag":"x"}]}`},
		{"Xray config array",
			`[{"outbounds":[{"protocol":"vless","tag":"x"}]}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeSubscriptionContent([]byte(tc.body))
			if err != nil {
				t.Fatalf("structured body rejected: %v", err)
			}
			// Pass-through must be byte-identical: the decoder's job is to
			// unwrap and reject, never to rewrite a body the parser will read.
			if string(got) != tc.body {
				t.Errorf("body was modified\n got: %s\nwant: %s", got, tc.body)
			}
		})
	}
}

// TestDecodeSubscriptionContentRejectsUnknownJSON — valid JSON that no importer
// recognises must be refused with an accurate message.
//
// The old wording ("returned JSON configuration instead of subscription list")
// became false once JSON configurations were supported, so it named a problem
// that no longer existed. It must also NOT fall through to the URI branch: a
// JSON blob is not a link list, and treating it as one yields zero nodes with no
// explanation.
func TestDecodeSubscriptionContentRejectsUnknownJSON(t *testing.T) {
	cases := []struct{ name, body string }{
		{"unknown object", `{"foo":"bar"}`},
		{"unknown array", `[{"foo":"bar"}]`},
		{"empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeSubscriptionContent([]byte(tc.body))
			if err == nil {
				t.Fatal("unrecognised JSON was accepted")
			}
			if !strings.Contains(err.Error(), "unsupported JSON subscription format") {
				t.Errorf("error %q should explain that the JSON shape is unsupported", err)
			}
			// The retired wording must be gone: it described JSON configs as
			// unsupported, which is no longer true.
			if strings.Contains(err.Error(), "instead of subscription list") {
				t.Errorf("error %q still uses the retired wording", err)
			}
		})
	}
}

// TestDecodeSubscriptionContentKeepsURIAndBrokenBodies — the fix must not widen
// acceptance. A URI list still passes through, and a truncated JSON body is
// still an error rather than an empty subscription.
func TestDecodeSubscriptionContentKeepsURIAndBrokenBodies(t *testing.T) {
	uri := "vless://uuid@a.example:443#Node A\ntrojan://pw@b.example:443#Node B\n"
	got, err := DecodeSubscriptionContent([]byte(uri))
	if err != nil {
		t.Fatalf("URI list rejected: %v", err)
	}
	if string(got) != uri {
		t.Error("URI list was modified")
	}

	if _, err := DecodeSubscriptionContent([]byte(`{"outbounds":[`)); err == nil {
		t.Error("truncated JSON was accepted")
	}
}
