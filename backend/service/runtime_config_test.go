package service

import (
	"os"
	"path/filepath"
	"testing"

	"singbox-launcher/core"
	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/paths"
)

// TestRunningCoreReportsTheConfigItActuallyLoaded is statement 10 (§34 name).
//
// The Clash API answers about the config the RUNNING core was started with, while the
// group picker reads config.json from disk. After a rebuild those are different
// documents: the picker lists groups the live core does not have, and switching to one
// fails with an error naming a group the user can see on screen.
//
// Nothing compared the two, so the divergence was invisible and lasted until the user
// happened to restart. The identity of the live config is now captured at the running
// transition and the disagreement is reported.
func TestRunningCoreReportsTheConfigItActuallyLoaded(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[]}`)

	// Nothing is running, so there is nothing to diverge from.
	if matches, running := b.ConfigMatchesRuntime(); running || matches {
		t.Fatalf("a stopped core reported running=%v matches=%v; with nothing live there "+
			"is no runtime config and callers must fall back to the file", running, matches)
	}

	// The core starts with what is on disk.
	b.recordRunningConfig()
	matches, running := b.ConfigMatchesRuntime()
	if !running {
		t.Fatal("a recorded runtime config did not report a running core, so no caller " +
			"can ever detect a divergence")
	}
	if !matches {
		t.Fatal("the config that was just recorded does not match itself")
	}

	// A rebuild rewrites config.json while the core keeps running the old document.
	if err := os.WriteFile(b.ac.FileService.ConfigPath,
		[]byte(`{"outbounds":[{"type":"selector","tag":"NewGroup"}]}`), 0o644); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}

	matches, running = b.ConfigMatchesRuntime()
	if !running {
		t.Fatal("the core is still running")
	}
	if matches {
		t.Fatal("config.json was rewritten after the core started, but the runtime config " +
			"still reports a match — the app cannot tell that the live core is serving the " +
			"previous document, so the picker offers groups the core does not have")
	}
	if !b.RuntimeConfigDiverged() {
		t.Fatal("the divergence is not reported, so nothing can offer the user a restart")
	}
}

// TestRuntimeConfigClearedWhenCoreStops — a stopped core has no runtime identity, so the
// stale hash must not keep reporting a divergence forever.
func TestRuntimeConfigClearedWhenCoreStops(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[]}`)
	b.recordRunningConfig()

	if err := os.WriteFile(b.ac.FileService.ConfigPath, []byte(`{"outbounds":[1]}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !b.RuntimeConfigDiverged() {
		t.Fatal("a divergence while running was not detected")
	}

	// The core stops. On the next start it will load whatever is on disk, so there is
	// nothing to be inconsistent with.
	b.runtimeCfg.clear()
	if b.RuntimeConfigDiverged() {
		t.Fatal("a stopped core still reports a divergent runtime config; the flag would " +
			"never clear and the UI would offer a restart forever")
	}
}

// TestConfigMatchesRuntimeDetectsAPathChange — the same bytes at a different path is still
// a different runtime config, because the core was told to load the other one.
func TestConfigMatchesRuntimeDetectsAPathChange(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[]}`)
	b.recordRunningConfig()

	_, running := b.ConfigMatchesRuntime()
	if !running {
		t.Fatal("expected a running core")
	}

	// Simulate the layout moving the config underneath the recorded identity.
	body, rerr := os.ReadFile(b.ac.FileService.ConfigPath)
	if rerr != nil {
		t.Fatalf("read config: %v", rerr)
	}
	b.runtimeCfg.set("/some/other/config.json", hashConfigBytes(body), body)
	if matches, running := b.ConfigMatchesRuntime(); !running || matches {
		t.Fatal("a runtime config recorded at a different path reported a match; the core " +
			"was started with the other file")
	}
}

// TestProxyListReportsRuntimeRestartRequired is statement 11 (§34 name).
//
// The group names come from config.json and the live selection comes from the Clash API.
// When those describe different documents the response must SAY so, because a list that
// silently mixes them is a list the user cannot trust.
func TestProxyListReportsRuntimeRestartRequired(t *testing.T) {
	b := newTestBackendWithConfig(t, `{
		"outbounds":[{"type":"selector","tag":"Proxy","outbounds":["a"]}],
		"route":{"final":"Proxy"}
	}`)

	// Not running: no divergence, and the file is the truth.
	list, err := b.ProxyGroups()
	if err != nil {
		t.Fatalf("ProxyGroups while stopped: %v", err)
	}
	if list.RuntimeRestartRequired {
		t.Error("a stopped core reported that a restart is required; with nothing running " +
			"the file on disk is what will be loaded next")
	}

	// The core starts, then the config changes underneath it.
	b.recordRunningConfig()
	if err := os.WriteFile(b.ac.FileService.ConfigPath, []byte(`{
		"outbounds":[{"type":"selector","tag":"Proxy","outbounds":["a"]}],
		"route":{"final":"Proxy"},
		"experimental":{"clash_api":{"external_controller":"127.0.0.1:9090"}}
	}`), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	list, err = b.ProxyGroups()
	if err != nil {
		t.Fatalf("ProxyGroups: %v", err)
	}
	if !list.RuntimeRestartRequired {
		t.Fatal("config.json changed after the core started, but the proxy list does not " +
			"report that a restart is required — the UI will present groups read from a " +
			"document the live core never loaded")
	}
}

// TestDivergenceIsNotReportedWhenContentIsEquivalent — a re-serialised but identical
// config must not trigger a restart prompt.
func TestDivergenceIsNotReportedWhenContentIsEquivalent(t *testing.T) {
	b := newTestBackendWithConfig(t, `{"outbounds":[],"route":{"final":"direct"}}`)
	b.recordRunningConfig()

	// Same document, different formatting.
	if err := os.WriteFile(b.ac.FileService.ConfigPath,
		[]byte("{\n  \"outbounds\": [],\n  \"route\": {\"final\": \"direct\"}\n}\n"), 0o644); err != nil {
		t.Fatalf("reformat: %v", err)
	}

	if b.RuntimeConfigDiverged() {
		t.Error("a reformatted but equivalent config was reported as divergent; the user " +
			"would be told to restart for a change that does not exist")
	}
}

// --- helpers ---

// newTestBackendWithConfig builds a backend whose config.json holds the given document.
//
// Mirrors backendWithConfig, but for a document the test chooses: the divergence tests
// need to rewrite the config AFTER the core started, so the fixture has to be under their
// control rather than a fixed string.
func newTestBackendWithConfig(t *testing.T, config string) *Backend {
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
	if err := os.WriteFile(filepath.Join(layout.Data.Bin(), "config.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("cannot write the config fixture: %v", err)
	}

	ac, err := core.NewAppController(layout, nil, nil, nil, nil)
	if err != nil {
		t.Skipf("cannot build a controller in this environment: %v", err)
	}
	b := &Backend{ac: ac}
	b.installOwnershipPolicy()
	b.watchCoreState()
	return b
}
