package swiftlogic_test

import (
	"strings"
	"testing"
)

// TestProxyScreenDoesNotCallAStartingCoreStopped pins the reported defect.
//
// The screen reached "core is not running" through `core?.state != .running`,
// which collapses five states into one message. The user's log shows a core that
// was verifiably up — root-owned copy verified, PID written, Clash API answering,
// four groups loaded — while this screen said "内核未运行".
//
// A core that is STARTING and a core that is RUNNING-with-a-slow-API are the two
// cases that were misreported, and both are reachable: the first from the phase
// fix's own window, the second from any cold Clash API.
func TestProxyScreenDoesNotCallAStartingCoreStopped(t *testing.T) {
	code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/App/AppModel.swift"))

	// The exact collapsing expression must be gone.
	if strings.Contains(code, "if core?.state != .running { return .coreStopped }") {
		t.Fatal("the Proxies state still collapses every non-running core state into " +
			".coreStopped; a starting core and a slow API were reported to the user " +
			"as \"内核未运行\" — the opposite of the truth")
	}
	// The distinct case must exist and be reached for the transient states.
	if !strings.Contains(code, "case coreStartingUp") {
		t.Error("the ProxyListState enum must distinguish a core that is UP from one that is not")
	}
	for _, state := range []string{"case .starting, .stopping:", "return .coreStartingUp"} {
		if !strings.Contains(code, state) {
			t.Errorf("the derivation must route %q to .coreStartingUp", state)
		}
	}
}

// TestProxyScreenNeverClaimsStoppedWhileRunning — the invariant, stated directly.
//
// Whatever else changes, a core the backend reports as `running` must never
// produce the "core is not running" notice. That is the one thing this screen is
// for.
func TestProxyScreenNeverClaimsStoppedWhileRunning(t *testing.T) {
	code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/App/AppModel.swift"))

	idx := strings.Index(code, "var proxyListState: ProxyListState {")
	if idx < 0 {
		t.Fatal("proxyListState is missing")
	}
	body := code[idx:]
	if end := strings.Index(body[1:], "\n    /// "); end > 0 {
		body = body[:end+1]
	}

	// Within the running branch, the derivation must NOT return .coreStopped.
	runIdx := strings.Index(body, "case .running:")
	if runIdx < 0 {
		t.Fatal("the derivation no longer handles .running explicitly")
	}
	// The running arm's own body, up to the switch's closing brace.
	arm := body[runIdx:]
	if end := strings.Index(arm, "\n        }"); end > 0 {
		arm = arm[:end]
	}
	if strings.Contains(arm, "return .coreStopped") {
		t.Error("the .running arm returns .coreStopped; a running core must never be " +
			"reported as not running")
	}
}

// TestProxyStoppedAndStartingUseDifferentCopy — the two conditions need different
// words, because the user's next action differs: a stopped core needs Start, a
// core that is up needs a moment.
func TestProxyStoppedAndStartingUseDifferentCopy(t *testing.T) {
	view := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/ProxiesView.swift"))
	if !strings.Contains(view, "case .coreStartingUp:") {
		t.Fatal("the view does not handle .coreStartingUp, so the new state cannot render " +
			"its own message")
	}
	if !strings.Contains(view, "L.coreApiNotReady.tr(language)") {
		t.Error("the starting-up case must use its own copy, not the stopped-core copy")
	}

	loc := repoFile(t, "macos/Sources/JiejieBox/Models/Localization.swift")
	if !strings.Contains(loc, "case coreApiNotReady") {
		t.Fatal("the coreApiNotReady key is missing")
	}
	if n := strings.Count(loc, "case .coreApiNotReady:"); n < 2 {
		t.Errorf("coreApiNotReady has %d translations; both EN and ZH are required", n)
	}
	// The English string must not claim the core is down.
	for _, line := range strings.Split(loc, "\n") {
		if !strings.Contains(line, "case .coreApiNotReady:") {
			continue
		}
		if strings.Contains(line, "The installed service") {
			continue
		}
		if strings.Contains(line, "not running") || strings.Contains(line, "未运行") {
			t.Errorf("the coreApiNotReady copy still says the core is not running: %s",
				strings.TrimSpace(line))
		}
	}
}
