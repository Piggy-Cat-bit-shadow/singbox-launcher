package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/core/services"
)

// newGateController builds the minimum AppController the gate needs: a
// FileService pointing at a config file we control.
func newGateController(t *testing.T, configJSON string) *AppController {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if configJSON != "" {
		if err := os.WriteFile(path, []byte(configJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &AppController{
		FileService: &services.FileService{ConfigPath: path},
	}
}

// TestPreLaunchGateBlocksTheIncidentConfig is the regression for the delivery
// half of the incident.
//
// This is the exact configuration that produced
// `FATAL: default outbound not found: proxy-out` on real hardware: route.final
// names a tag that no outbound carries. Nothing between the file and the daemon
// looked at it, so it was delivered and the core died on startup.
func TestPreLaunchGateBlocksTheIncidentConfig(t *testing.T) {
	ac := newGateController(t, `{
      "outbounds": [
        {"type": "direct", "tag": "direct-out"},
        {"type": "block", "tag": "block-out"}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)

	err := ac.validateConfigBeforeLaunch()
	if err == nil {
		t.Fatal("a config whose route.final names a missing outbound MUST be refused; " +
			"delivering it is what made the core die with a message the user never saw")
	}

	var sf *StartFailure
	if !errors.As(err, &sf) {
		t.Fatalf("the refusal must be a structured StartFailure so the reason crosses "+
			"IPC, got %T: %v", err, err)
	}
	if sf.Code != StartErrConfigCheckFailed {
		t.Errorf("expected code %q, got %q", StartErrConfigCheckFailed, sf.Code)
	}
}

// TestPreLaunchGateNamesTheProblemAndTheAlternatives — the core says only
// `default outbound not found: proxy-out`. The user cannot act on that: they
// cannot tell whether a subscription failed, a node was filtered, or a group was
// removed. The gate must say which and what IS available.
func TestPreLaunchGateNamesTheProblemAndTheAlternatives(t *testing.T) {
	ac := newGateController(t, `{
      "outbounds": [
        {"type": "direct", "tag": "direct-out"},
        {"type": "block", "tag": "block-out"}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)

	err := ac.validateConfigBeforeLaunch()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	for _, want := range []string{"route.final", "proxy-out", "direct-out", "block-out"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must mention %q so the user can act on it; got:\n%s", want, msg)
		}
	}
}

// TestPreLaunchGatePassesAValidConfig — the gate must not block a sound config.
func TestPreLaunchGatePassesAValidConfig(t *testing.T) {
	ac := newGateController(t, `{
      "outbounds": [
        {"type": "selector", "tag": "proxy-out", "outbounds": ["direct-out"]},
        {"type": "direct", "tag": "direct-out"}
      ],
      "route": {"final": "proxy-out", "rules": []}
    }`)
	if err := ac.validateConfigBeforeLaunch(); err != nil {
		t.Fatalf("a reference-clean config must pass the gate, got: %v", err)
	}
}

// TestPreLaunchGateCatchesOtherDanglingReferences — the incident was route.final,
// but the gate must cover the whole class it belongs to: a selector member, a
// detour, a route rule and a DNS server are the same defect wearing other names.
func TestPreLaunchGateCatchesOtherDanglingReferences(t *testing.T) {
	cases := []struct {
		name   string
		config string
	}{
		{
			name: "selector member",
			config: `{"outbounds":[
              {"type":"selector","tag":"grp","outbounds":["gone-node"]},
              {"type":"direct","tag":"direct-out"}],
              "route":{"final":"grp","rules":[]}}`,
		},
		{
			name: "outbound detour",
			config: `{"outbounds":[
              {"type":"shadowsocks","tag":"n","server":"1.2.3.4","server_port":1,"detour":"gone"},
              {"type":"direct","tag":"direct-out"}],
              "route":{"final":"direct-out","rules":[]}}`,
		},
		{
			name: "route rule outbound",
			config: `{"outbounds":[{"type":"direct","tag":"direct-out"}],
              "route":{"final":"direct-out","rules":[{"domain":["x.test"],"outbound":"gone"}]}}`,
		},
		{
			name: "dns final",
			config: `{"outbounds":[{"type":"direct","tag":"direct-out"}],
              "dns":{"servers":[{"tag":"ok-dns","address":"1.1.1.1"}],"final":"gone-dns"},
              "route":{"final":"direct-out","rules":[]}}`,
		},
		{
			name:   "unparseable",
			config: `{"outbounds": [ this is not json`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := newGateController(t, tc.config)
			if err := ac.validateConfigBeforeLaunch(); err == nil {
				t.Errorf("%s: a dangling reference must be refused before launch", tc.name)
			}
		})
	}
}

// TestPreLaunchGateDoesNotFailOnAMissingFile — a first run may have no config
// yet; the gate must not turn that into a start failure of its own. The engine's
// own path reports a missing config, and two different messages for one cause
// would be worse than one.
func TestPreLaunchGateDoesNotFailOnAMissingFile(t *testing.T) {
	ac := newGateController(t, "") // no file written
	if err := ac.validateConfigBeforeLaunch(); err != nil {
		t.Fatalf("an absent config.json is the start path's business, not the gate's: %v", err)
	}
}
