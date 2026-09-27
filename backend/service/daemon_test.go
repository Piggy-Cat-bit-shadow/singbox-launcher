package service

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"singbox-launcher/backend/protocol"
)

// TestDaemonStatusShape — the Daemon screen is driven entirely by this payload,
// so its keys are a contract. A rename would silently turn every status row
// into a blank line at runtime.
func TestDaemonStatusShape(t *testing.T) {
	b := backendWithConfig(t)

	status, err := b.DaemonStatus()
	if err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}

	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"supported", "service", "installed", "paired", "reachable", "ready",
		"active_mode", "core_supports_lxd", "needs_install", "needs_start",
		"persists_after_quit",
	} {
		if _, present := m[key]; !present {
			t.Errorf("daemon status is missing %q", key)
		}
	}

	// The pairing secret must never cross the IPC boundary in either
	// direction. Its absence is a security property, so it is asserted.
	for _, forbidden := range []string{"secret", "daemon_secret", "DaemonSecret"} {
		if _, present := m[forbidden]; present {
			t.Errorf("daemon status exposes %q to the frontend", forbidden)
		}
	}
}

// TestDaemonStatusIsNotReadyOnAFreshInstall — activation must be gated on real
// readiness, because that gate is what stops the engine switch from being
// attempted against a service that was never installed.
func TestDaemonStatusIsNotReadyOnAFreshInstall(t *testing.T) {
	b := backendWithConfig(t)

	status, err := b.DaemonStatus()
	if err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}
	if status.Ready {
		t.Errorf("a fresh install reports ready=true (service=%q paired=%v reachable=%v)",
			status.Service, status.Paired, status.Reachable)
	}
	if status.ActiveMode {
		t.Error("a fresh install reports the daemon engine active")
	}
}

// TestDaemonCommandsAreOfferedButNeverRun — the setup steps return a command
// for the user to run. They must never execute a privileged operation
// themselves: that is what made selecting the engine look like a freeze.
func TestDaemonCommandsAreOfferedButNeverRun(t *testing.T) {
	b := backendWithConfig(t)

	steps := []struct {
		name string
		call func() (DaemonCommandResult, error)
		op   string
	}{
		{"install", b.DaemonInstall, "install"},
		{"start", b.DaemonStart, "start"},
		{"repair", b.DaemonRepair, "fresh_invite"},
		{"uninstall", func() (DaemonCommandResult, error) { return b.DaemonUninstall(false) }, "uninstall"},
		{"remove all", func() (DaemonCommandResult, error) { return b.DaemonUninstall(true) }, "uninstall"},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			result, err := step.call()
			if err != nil {
				t.Fatalf("%s returned an error: %v", step.name, err)
			}
			if result.Operation != step.op {
				t.Errorf("operation = %q, want %q", result.Operation, step.op)
			}
			// Available commands must be non-empty and marked as needing admin
			// rights, so the UI can warn before opening a terminal.
			if result.Available {
				if result.Command == "" {
					t.Error("command is marked available but is empty")
				}
				if !result.NeedsAdmin {
					t.Error("a service command should be marked as needing administrator rights")
				}
				if result.Message == "" {
					t.Error("an available command has no guidance message")
				}
			}
			// Status is refreshed alongside the command, so the page does not
			// need a second round trip.
			if result.Status.Service == "" && result.Status.Supported {
				t.Error("the command result carries no refreshed status")
			}
		})
	}
}

// TestPairDaemonValidatesInvite — a malformed paste must produce a clear
// message rather than an opaque parser error, and must not be attempted at all.
func TestPairDaemonValidatesInvite(t *testing.T) {
	b := backendWithConfig(t)

	cases := []struct {
		name   string
		invite string
		code   string
	}{
		{"empty", "", "bad_request"},
		{"whitespace", "   ", "bad_request"},
		{"not an invite", "hello world", "bad_invite"},
		{"too few parts", "127.0.0.1:19091#abc", "bad_invite"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := b.PairDaemon(tc.invite)
			if err == nil {
				t.Fatalf("PairDaemon(%q) succeeded", tc.invite)
			}
			pe, ok := err.(*protocol.Error)
			if !ok {
				t.Fatalf("error is %T, want *protocol.Error", err)
			}
			if pe.Code != tc.code {
				t.Errorf("code = %q, want %q", pe.Code, tc.code)
			}
			if pe.Message == "" {
				t.Error("the error carries no message for the user")
			}
		})
	}
}

// TestDaemonKeepRunningIsInvertedOnDisk — the UI says "Keep VPN Running After
// Quit"; storage says DaemonStopVPNOnExit. The conversion lives in the backend
// so the frontend never has to know about the inverted field, and getting it
// backwards would silently invert the user's choice.
func TestDaemonKeepRunningIsInvertedOnDisk(t *testing.T) {
	b := backendWithConfig(t)

	// Default: the VPN keeps running after quit, so stop-on-exit is false.
	status, err := b.DaemonStatus()
	if err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}
	if !status.PersistsAfterQuit {
		t.Error("the default should be to keep the VPN running after quit")
	}

	if err := b.SetDaemonKeepRunningAfterQuit(false); err != nil {
		t.Fatalf("SetDaemonKeepRunningAfterQuit(false): %v", err)
	}
	status, err = b.DaemonStatus()
	if err != nil {
		t.Fatalf("DaemonStatus after change: %v", err)
	}
	if status.PersistsAfterQuit {
		t.Error("keep-running=false did not persist; the flag mapping is inverted")
	}

	if err := b.SetDaemonKeepRunningAfterQuit(true); err != nil {
		t.Fatalf("SetDaemonKeepRunningAfterQuit(true): %v", err)
	}
	status, _ = b.DaemonStatus()
	if !status.PersistsAfterQuit {
		t.Error("keep-running=true did not persist")
	}
}

// TestDaemonCommandResultShape pins the wire keys the Swift client decodes.
func TestDaemonCommandResultShape(t *testing.T) {
	raw, err := json.Marshal(DaemonCommandResult{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"operation", "command", "available", "message", "needs_admin", "follow_up", "status",
	} {
		if _, present := m[key]; !present {
			t.Errorf("daemon command result is missing %q", key)
		}
	}
}

// TestServerDispatchesDaemonMethods — every method must be reachable over the
// wire, not merely present on the Backend type.
func TestServerDispatchesDaemonMethods(t *testing.T) {
	b := backendWithConfig(t)
	srv := NewServer(b, &bytes.Buffer{})

	for _, method := range []string{
		protocol.MethodGetDaemonStatus,
		protocol.MethodDaemonInstall,
		protocol.MethodDaemonStart,
		protocol.MethodDaemonRepair,
		protocol.MethodDaemonUninstall,
		protocol.MethodPairDaemon,
		protocol.MethodUnpairDaemon,
		protocol.MethodSetDaemonKeepRunning,
	} {
		resp := srv.handle(protocol.Request{ID: "1", Method: method})
		if resp.Error != nil && resp.Error.Code == "unknown_method" {
			t.Errorf("%s is not dispatched by the server", method)
		}
	}
}

// TestDaemonRepairProducesAPairingInvite — the "Pair Service" UI step calls
// this, so its contract is what makes the pairing flow linear.
//
// A first-time user with a running but unpaired service must be able to reach
// pairing from the Daemon screen. That works only if repair yields an invite
// command and points at pairing as the follow-up.
func TestDaemonRepairProducesAPairingInvite(t *testing.T) {
	b := backendWithConfig(t)

	result, err := b.DaemonRepair()
	if err != nil {
		t.Fatalf("DaemonRepair: %v", err)
	}

	// The operation name is what the UI branches on to reveal "Continue to
	// Pair", so it is part of the contract rather than an internal label.
	if result.Operation != "fresh_invite" {
		t.Errorf("operation = %q, want %q", result.Operation, "fresh_invite")
	}
	if result.FollowUp != "pair" {
		t.Errorf("follow_up = %q, want %q", result.FollowUp, "pair")
	}
	if !result.NeedsAdmin {
		t.Error("producing an invite needs administrator rights and should say so")
	}

	// When the core supports the daemon, the command must be real. The exact
	// path is deliberately not asserted: it varies by install.
	if result.Available {
		if !strings.Contains(result.Command, "lxd client add") {
			t.Errorf("command %q does not look like a pairing-invite command", result.Command)
		}
		if !strings.Contains(result.Command, "--name") {
			t.Errorf("command %q does not name the client", result.Command)
		}
		if result.Message == "" {
			t.Error("an available command has no guidance for the user")
		}
	}
}

// TestDaemonRepairMatchesInstallFollowUp — install and repair must agree on the
// step after them, or the guided sequence would point in two directions.
func TestDaemonRepairMatchesInstallFollowUp(t *testing.T) {
	b := backendWithConfig(t)

	install, err := b.DaemonInstall()
	if err != nil {
		t.Fatalf("DaemonInstall: %v", err)
	}
	repair, err := b.DaemonRepair()
	if err != nil {
		t.Fatalf("DaemonRepair: %v", err)
	}

	if install.FollowUp != repair.FollowUp {
		t.Errorf("install follow_up = %q but repair follow_up = %q; "+
			"the guided sequence would be inconsistent",
			install.FollowUp, repair.FollowUp)
	}
}
