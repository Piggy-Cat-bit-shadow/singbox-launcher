// Package digestguard holds a cross-layer consistency check that belongs to neither layer.
//
// `core` and `backend/service` each hash config bytes. The duplication is forced by the
// layering — `backend/service` imports `core`, so there is no package both can share a helper
// from — but the two digests ARE compared against each other in practice: the build-revision
// marker is written from one layer and interpreted in the context of the other. If they ever
// diverge, the staleness check reports a config as changed when it is not, or as unchanged
// when it is, and both failures are silent.
//
// Convention cannot be enforced by the compiler here, so this package enforces it by test.
// It exists as its own package precisely because a test inside either layer would have to
// import the other and create a cycle.
package digestguard_test

import (
	"testing"

	"singbox-launcher/backend/service"
	"singbox-launcher/core"
)

func TestConfigDigestsAgreeAcrossLayers(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"empty object", []byte("{}")},
		{"a real config", []byte(`{"outbounds":[{"type":"direct","tag":"A"}]}`)},
		{"key order matters", []byte(`{"b":1,"a":2}`)},
		{"bytes that exercise hex rendering", []byte{0x00, 0xff, 0x10, 0x7f, 0x80}},
		{"non-ascii", []byte(`{"а":"кириллица","emoji":"🎯"}`)},
		{"a lone newline", []byte("\n")},
	}

	for _, tc := range cases {
		got := core.ConfigContentDigest(tc.data)
		want := service.HashConfigBytes(tc.data)
		if got != want {
			t.Errorf("%s: the two config digests disagree:\n  core:            %s\n  backend/service: %s\n"+
				"They are compared against each other, so a divergence makes the staleness "+
				"check report a change that did not happen — or miss one that did",
				tc.name, got, want)
		}
		if got == "" {
			t.Errorf("%s: the digest is empty; an empty digest reads as 'no revision "+
				"recorded', which silently disables the check it exists for", tc.name)
		}
	}
}
