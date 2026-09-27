package core

import (
	"os"
	"strings"
	"testing"
)

// TestNoBareUIPortCalls walks the core sources for `ac.uiPort.<method>()` calls
// that are not guarded by a nil check.
//
// The field is nil unless a frontend attaches itself, and the headless backend
// never does. A bare call therefore panics — which is exactly what happened the
// first time the menu bar ran update_subscriptions ("invalid memory address or
// nil pointer dereference" in the UI's error banner). `ac.ui()` returns a no-op
// port instead, so it is the form every call site should use.
//
// This is a source check rather than a behavioural one because the failing paths
// are process-lifecycle code (start, restart, crash supervision) that a unit
// test cannot reasonably drive end to end.
// readCoreFile reads a file from this package directory.
func readCoreFile(name string) (string, error) {
	b, err := os.ReadFile(name)
	return string(b), err
}

func TestNoBareUIPortCalls(t *testing.T) {
	files := []string{
		"config_service.go",
		"config_service_subscriptions.go",
		"process_service.go",
		"rebuild.go",
		"auto_update.go",
		"controller.go",
	}
	for _, name := range files {
		src, err := readCoreFile(name)
		if err != nil {
			t.Skipf("cannot read %s: %v", name, err)
		}
		lines := strings.Split(src, "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if !strings.Contains(trimmed, "ac.uiPort.") &&
				!strings.Contains(trimmed, "svc.ac.uiPort.") {
				continue
			}
			// Assignments and nil comparisons are not calls.
			if strings.Contains(trimmed, "ac.uiPort =") || strings.Contains(trimmed, "ac.uiPort ==") {
				continue
			}
			// A guard within the preceding few lines makes the call safe.
			start := i - 6
			if start < 0 {
				start = 0
			}
			window := strings.Join(lines[start:i+1], "\n")
			if strings.Contains(window, "ac.uiPort != nil") ||
				strings.Contains(window, "ac.hasUI()") ||
				strings.Contains(window, "ac.ui()") {
				continue
			}
			t.Errorf("%s:%d calls uiPort without a nil guard:\n    %s\n"+
				"    Use ac.ui() — the field is nil in the headless backend, and a "+
				"bare call panics.", name, i+1, trimmed)
		}
	}
}
