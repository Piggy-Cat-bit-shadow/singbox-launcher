package swiftlogic_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"singbox-launcher/internal/swiftlogic"
)

// TestEveryScreenReadsTheOneRuntimeState pins the single-truth-source rule.
//
// The failure this guards against is not one wrong verdict but the STRUCTURE that
// produces them: each screen keeping its own idea of whether the core is running.
// The 首页 said "正在启动" while the 代理页 said "内核未运行" precisely because two
// screens answered the same question differently.
//
// The rule: a view derives the runtime verdict from `model.core?.state` (the one
// wire value the backend publishes) or from a model property that does, and never
// from a locally maintained flag.
func TestEveryScreenReadsTheOneRuntimeState(t *testing.T) {
	// A repo-relative glob, resolved from the SOURCE FILE's location rather than
	// the working directory: a compiled test binary can be run from anywhere, and
	// a check that silently globs nothing passes while checking nothing.
	root, err := swiftlogic.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	views, err := filepath.Glob(filepath.Join(root, "macos/Sources/JiejieBox/Views/*.swift"))
	if err != nil || len(views) == 0 {
		t.Fatal("no view files found; this check would pass vacuously")
	}

	// A local re-derivation looks like a stored boolean about the core.
	//
	// The leading group tolerates PROPERTY WRAPPERS (`@State private var …`),
	// which is how such a flag is actually written in SwiftUI. Omitting it made
	// this check pass against an injected `@State private var isCoreRunning` —
	// a checker that cannot see the shape it forbids protects nothing.
	suspect := regexp.MustCompile(`(?m)^\s*(@\w+(\([^)]*\))?\s+)*(private\s+|fileprivate\s+)?(var|let)\s+(isCoreRunning|coreRunning|vpnRunning|isRunning)\b`)
	offenders := 0
	for _, path := range views {
		code := swiftCode(readFileOrFail(t, path))
		if m := suspect.FindString(code); m != "" {
			t.Errorf("%s declares a local running flag (%q). Every screen must derive "+
				"the verdict from model.core?.state, or screens disagree — which is "+
				"exactly how 首页 showed 正在启动 while 代理页 showed 内核未运行",
				filepath.Base(path), strings.TrimSpace(m))
			offenders++
		}
	}
	if offenders == 0 {
		t.Log("no view keeps a private running flag")
	}

	// And the screens that answer the question must reach the shared value —
	// either directly, or through ONE derived property that does.
	//
	// ProxiesView reads `model.proxyListState` rather than `model.core` directly,
	// and that is correct layering rather than a second source: the derivation
	// lives in AppModel and is covered by its own test. Requiring the raw field in
	// every view would forbid the indirection that keeps the rule in one place.
	direct := map[string]string{
		"HomeView.swift":        "model.core",
		"CoreDetailsView.swift": "model.core",
	}
	for name, want := range direct {
		code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/"+name))
		if !strings.Contains(code, want) {
			t.Errorf("%s does not read %s, so it cannot be reporting the authoritative "+
				"runtime state", name, want)
		}
	}
	// The one screen that goes through a derived property must use a property
	// that itself consults the wire state.
	proxies := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/ProxiesView.swift"))
	if !strings.Contains(proxies, "model.proxyListState") {
		t.Error("ProxiesView reads neither model.core nor model.proxyListState, so it " +
			"has no route to the authoritative runtime state at all")
	}
}

// TestRuntimeVerdictComesFromTheWireState — the Proxies verdict in particular
// must be a function of the published state, never of the proxy list.
//
// `proxies.isEmpty => core stopped` is the exact inference the reported defect
// made. A core with no loaded nodes is a perfectly normal running core.
func TestRuntimeVerdictComesFromTheWireState(t *testing.T) {
	code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/App/AppModel.swift"))
	idx := strings.Index(code, "var proxyListState: ProxyListState {")
	if idx < 0 {
		t.Fatal("proxyListState is missing")
	}
	body := code[idx:]
	if end := strings.Index(body[1:], "\n    /// "); end > 0 {
		body = body[:end+1]
	}

	// The verdict must consult the published core state.
	if !strings.Contains(body, "core?.state") && !strings.Contains(body, "coreState") {
		t.Error("the Proxies verdict never consults the core's published state, so it " +
			"is guessing about the one fact the screen exists to report")
	}
	// And it must NOT treat an empty list as proof the core is down.
	emptyIsStopped := regexp.MustCompile(`proxies\.isEmpty[^\n]*\{[^\n]*coreStopped`)
	if emptyIsStopped.MatchString(body) {
		t.Error("the Proxies verdict treats an empty node list as proof the core is " +
			"stopped; a running core with no loaded nodes is normal")
	}
}

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
