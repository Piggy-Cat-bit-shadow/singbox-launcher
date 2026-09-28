package service

import (
	"testing"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
	"singbox-launcher/internal/locale"
)

// Engine IDENTITY and persistence POLICY are independent axes, and conflating
// them produced two bugs that pointed the same way: with the daemon selected and
// "keep running after quit" OFF, the app reported itself as the CLASSIC engine.
//
// The cause was one shared mistake in two places — asking
// `CorePersistsAfterAppExit()` ("will the core outlive the app?") when the
// question was "which engine is serving right now?".
//
// These tests pin the two axes apart. They are written against a controller whose
// active backend is switched to the daemon, because that is the only state in
// which the two answers differ.

// daemonEngineBackend returns a backend whose active engine is the daemon, with
// the stop-on-exit policy set as requested.
func daemonEngineBackend(t *testing.T, stopOnExit bool) *Backend {
	t.Helper()
	b := backendWithConfig(t)

	settings := locale.LoadSettings(b.ac.FileService.Layout.Data.Bin())
	settings.DaemonStopVPNOnExit = stopOnExit
	settings.CoreBackendMode = string(core.BackendDaemon)
	// A configured address is enough to CONSTRUCT the daemon engine; nothing
	// here connects to it. The identity under test is a property of the backend
	// object, so the test must not skip merely because no daemon is running —
	// a skipped test is not evidence, and this claim is exactly the kind that
	// would otherwise go unverified in CI.
	settings.DaemonAddress = "127.0.0.1:1"
	settings.DaemonSecret = "test-secret"
	if err := locale.SaveSettings(b.ac.FileService.Layout.Data.Bin(), settings); err != nil {
		t.Fatalf("cannot persist settings: %v", err)
	}

	// Switch to the daemon engine through the REAL handover, not by poking a
	// field: the identity under test is read from whichever backend the handover
	// published, so a test that installed one by hand would not exercise the
	// production path at all.
	if err := b.ac.SwitchBackendMode(core.BackendDaemon); err != nil {
		t.Skipf("cannot bring up the daemon engine in this environment: %v", err)
	}
	if b.ac.BackendMode() != core.BackendDaemon {
		t.Fatalf("the handover did not take: mode = %q", b.ac.BackendMode())
	}
	return b
}

// TestCoreBackendFieldDoesNotDependOnKeepRunning is claim A.
//
// The daemon engine is active and the user has chosen to STOP the VPN when the
// app quits. `CorePersistsAfterAppExit()` is therefore false — and the old code
// read that as "the engine is classic", which is a different question entirely.
//
// The frontend consumes this field as `activeEngine`, so the wrong value shows
// the classic engine while the daemon is serving: the mode screen mislabels
// itself and every engine-dependent control acts on the wrong engine.
func TestCoreBackendFieldDoesNotDependOnKeepRunning(t *testing.T) {
	b := daemonEngineBackend(t, true) // stop the VPN on quit

	if got := b.coreState().Backend; got != string(core.BackendDaemon) {
		t.Fatalf("CoreState.Backend = %q with the daemon active and keep-running OFF; "+
			"want %q. Backend identity must come from the active engine, never "+
			"from the persistence policy.", got, core.BackendDaemon)
	}
}

// TestCoreBackendFieldIsDaemonRegardlessOfKeepRunning states the invariant as a
// matrix, so a future change cannot satisfy one row by breaking another.
func TestCoreBackendFieldIsDaemonRegardlessOfKeepRunning(t *testing.T) {
	for _, stopOnExit := range []bool{true, false} {
		name := "keep-running"
		if stopOnExit {
			name = "stop-on-exit"
		}
		t.Run(name, func(t *testing.T) {
			b := daemonEngineBackend(t, stopOnExit)
			if got := b.coreState().Backend; got != string(core.BackendDaemon) {
				t.Fatalf("CoreState.Backend = %q, want daemon (stopOnExit=%v)",
					got, stopOnExit)
			}
		})
	}
}

// TestDaemonActiveModeDoesNotDependOnKeepRunning is claim B.
//
// `DaemonStatus.ActiveMode` gates the destructive actions on the daemon screen
// (`active_mode && core.running`). Deriving it from the persistence policy meant
// that a user who turned OFF "keep running after quit" had that protection
// lifted while a real daemon core was running — precisely the state the guard
// exists for, and precisely the actions (unpair, remove service) that must not
// be offered then.
func TestDaemonActiveModeDoesNotDependOnKeepRunning(t *testing.T) {
	b := daemonEngineBackend(t, true)

	st, err := b.DaemonStatus()
	if err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}
	if !st.ActiveMode {
		t.Fatal("ActiveMode = false with the daemon engine selected; the mode is an " +
			"IDENTITY fact and must not follow the keep-running policy")
	}
	if st.PersistsAfterQuit {
		t.Fatal("PersistsAfterQuit = true with stop-on-exit configured; the POLICY " +
			"field must report the policy, independently of identity")
	}
}

// TestDaemonActiveModeAndPersistenceAreIndependent pins both fields in both
// directions. One axis moving must never move the other.
func TestDaemonActiveModeAndPersistenceAreIndependent(t *testing.T) {
	cases := []struct {
		name         string
		stopOnExit   bool
		wantActive   bool
		wantPersists bool
	}{
		{"daemon + keep running", false, true, true},
		{"daemon + stop on exit", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := daemonEngineBackend(t, tc.stopOnExit)
			st, err := b.DaemonStatus()
			if err != nil {
				t.Fatalf("DaemonStatus: %v", err)
			}
			if st.ActiveMode != tc.wantActive {
				t.Errorf("ActiveMode = %v, want %v", st.ActiveMode, tc.wantActive)
			}
			if st.PersistsAfterQuit != tc.wantPersists {
				t.Errorf("PersistsAfterQuit = %v, want %v", st.PersistsAfterQuit, tc.wantPersists)
			}
		})
	}
}

// TestClassicEngineIdentityIsClassic — the classic direction, so the fix cannot
// be "always report daemon".
func TestClassicEngineIdentityIsClassic(t *testing.T) {
	b := backendWithConfig(t)
	// A fresh controller starts on the classic engine, so no handover is needed;
	// assert the starting mode rather than assuming it.
	if b.ac.BackendMode() != core.BackendClassic {
		t.Fatalf("a fresh controller must start on the classic engine, got %q", b.ac.BackendMode())
	}

	if got := b.coreState().Backend; got != string(core.BackendClassic) {
		t.Fatalf("CoreState.Backend = %q with classic active, want classic", got)
	}
	st, err := b.DaemonStatus()
	if err != nil {
		t.Fatalf("DaemonStatus: %v", err)
	}
	if st.ActiveMode {
		t.Fatal("ActiveMode = true while the classic engine is active")
	}
}

// TestBackendIdentitySurvivesMissingLayout — the identity helper must not panic
// or guess when the controller is not fully wired, and a nil controller is
// classic by definition (the zero value of the mode).
func TestBackendIdentitySurvivesMissingLayout(t *testing.T) {
	if got := backendIdentity(nil); got != string(core.BackendClassic) {
		t.Fatalf("backendIdentity(nil) = %q, want classic", got)
	}
}

// TestBackendIdentityIsAStableWireToken guards the string the Swift enum decodes.
// A value outside this set would fail decoding and blank the engine display.
func TestBackendIdentityIsAStableWireToken(t *testing.T) {
	b := backendWithConfig(t)
	valid := map[string]bool{
		string(core.BackendClassic): true,
		string(core.BackendDaemon):  true,
	}
	got := b.coreState().Backend
	if !valid[got] {
		t.Fatalf("CoreState.Backend = %q, which is not a token the frontend can "+
			"decode (want %q or %q)", got, core.BackendClassic, core.BackendDaemon)
	}
	_ = protocol.CoreStateStopped // the token is a protocol.CoreState field
}
