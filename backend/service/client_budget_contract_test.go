package service

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"singbox-launcher/api"
)

// TestClientTimeoutExceedsEveryBackendBudget is statement 18 (§34 name).
//
// The client aborts a request after a per-method timeout, while the backend runs the same
// operation under its own budget. If the client's number is the smaller one it abandons
// work that is still healthy and reports a failure — so the invariant is that the client
// always waits LONGER than the backend can take.
//
// This is checked from the Go side because the Swift client cannot be unit-tested in this
// toolchain (no XCTest links), and it is exactly the kind of cross-language constant pair
// that drifts: the backend budget was just raised to ten minutes while the client still
// aborted at two.
func TestClientTimeoutExceedsEveryBackendBudget(t *testing.T) {
	root := repoRootForTest(t)
	src, err := os.ReadFile(filepath.Join(root, "macos/Sources/JiejieBox/Services/BackendClient.swift"))
	if err != nil {
		t.Fatalf("read BackendClient.swift: %v", err)
	}
	client := string(src)

	// The group-test branch's seconds value, which is the number under test.
	groupCase := indexOfStr(client, "case BackendMethod.testProxy, BackendMethod.testProxyGroup,")
	if groupCase < 0 {
		t.Fatal("could not find the proxy-test timeout case in BackendClient.swift")
	}
	// Take the next `seconds = N` after the case label.
	rest := client[groupCase:]
	m := regexp.MustCompile(`seconds = ([0-9]+(?:\.[0-9]+)?)`).FindStringSubmatch(rest)
	if m == nil {
		t.Fatal("could not find the seconds value for the proxy-test case")
	}
	clientSeconds, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("parse client seconds: %v", err)
	}
	clientTimeout := time.Duration(clientSeconds * float64(time.Second))

	// The backend budgets this test could plausibly hit. The group test is the one that
	// scales with node count and with the user's per-node timeout, so it is the binding
	// constraint.
	prev := api.GetPingTestTimeoutMs()
	api.SetPingTestTimeoutMs(api.MaxPingTestTimeoutMs)
	defer api.SetPingTestTimeoutMs(prev)

	// A large-but-honest group at the slowest user-settable per-node timeout.
	worstGroup := groupTestBudget(200, 1)
	if clientTimeout <= worstGroup {
		t.Fatalf("the client aborts a group test after %s but the backend's budget for "+
			"200 nodes at the maximum per-node timeout is %s. The client would abandon a "+
			"run that is still doing useful work and present it as a failure; the client "+
			"timeout must sit ABOVE the backend's budget", clientTimeout, worstGroup)
	}

	// And the documented comment must not contradict the value.
	if strings.Contains(client, "request timeout (120s") && clientSeconds != 120 {
		t.Logf("note: the doc comment still says 120s while the value is %v", clientSeconds)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root")
	return ""
}

func indexOfStr(haystack, needle string) int {
	return strings.Index(haystack, needle)
}
