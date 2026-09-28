package service

import (
	"strings"
	"testing"
)

// TestUnpairRejectedWhileActiveDaemonRunning is claim 21.
//
// `UnpairDaemonForget` called straight through to the controller with no guard at
// all. The Swift view disables the control in this situation, but a UI is not a
// safety boundary: a stale frontend, a direct IPC call, a race, or a future layout
// regression all reach the backend with the constraint unenforced. Unpairing while
// the daemon engine is live deletes the client identity, fingerprint and address that
// the running control channel depends on.
func TestUnpairRejectedWhileActiveDaemonRunning(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/daemon.go"))

	idx := indexOf(src, "func (b *Backend) UnpairDaemonForget()")
	if idx < 0 {
		t.Fatal("UnpairDaemonForget not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	// The guard must exist, and it must consult the LIVE runtime rather than only a
	// stored preference.
	hasGuard := strings.Contains(body, "daemonBusyForUnpair") ||
		strings.Contains(body, "IsRunning") ||
		strings.Contains(body, "BackendMode")
	if !hasGuard {
		t.Fatal("unpairing has no backend-side guard: the only protection is a UI that " +
			"disables the button, which is not a safety boundary")
	}
	if !strings.Contains(body, "return") {
		t.Error("the guard does not actually refuse")
	}
}

// TestUnpairRefusesWhenTheDaemonEngineIsActive — the behavioural half of claim 21.
func TestUnpairRefusesWhenTheDaemonEngineIsActive(t *testing.T) {
	b := backendWithConfig(t)

	// A live core: unpairing must be refused regardless of the engine mode, because
	// the running config was built for the identity being deleted.
	b.ac.RunningState.Set(true)
	defer b.ac.RunningState.Set(false)

	if _, err := b.UnpairDaemonForget(); err == nil {
		t.Fatal("unpairing was accepted while the core is running; it deletes the " +
			"client identity the live control channel is using")
	}
}

// TestUnpairClosesActiveDaemonTransport is claim 22.
//
// `PairDaemonWithInvite` reloads the active daemon backend because the connection must
// use the new address, fingerprint and identity. `UnpairDaemon` did NOT — it cleared
// the identity and the settings and returned. The already-established gRPC connection,
// admin client and mTLS transport therefore stayed in memory: deleting a certificate
// from disk does not invalidate an open socket. The UI reported `Paired=false` while
// the old backend could still control the daemon.
func TestUnpairClosesActiveDaemonTransport(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "core/daemon_manager.go"))

	idx := indexOf(src, "func (ac *AppController) UnpairDaemon()")
	if idx < 0 {
		t.Fatal("UnpairDaemon not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	if !strings.Contains(body, "reloadDaemonBackendAfterUnpair") &&
		!strings.Contains(body, "reloadDaemonBackendIfActive") {
		t.Error("unpairing does not tear down or rebuild the active daemon backend, so " +
			"an already-open control channel keeps working after the credentials it " +
			"was built from are deleted")
	}
}

// TestUnpairPersistenceFailureCannotLeaveTornPairingState is claim 23.
//
// The order was: delete the identity, remove the legacy secret, then write settings.
// A failure in the LAST step left the identity gone from disk while settings still
// named an address, fingerprint and secret — a state that looks paired on the next
// launch but has no client certificate, which is very hard to diagnose.
//
// The settings write must therefore come FIRST: failing it leaves everything intact
// and still paired, which is a state the app can describe.
func TestUnpairPersistenceFailureCannotLeaveTornPairingState(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "core/daemon_manager.go"))

	idx := indexOf(src, "func (ac *AppController) UnpairDaemon()")
	if idx < 0 {
		t.Fatal("UnpairDaemon not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	save := indexOf(body, "SaveSettings")
	remove := indexOf(body, "RemoveIdentity")
	if save < 0 || remove < 0 {
		t.Fatal("expected both a settings save and an identity removal")
	}
	if remove < save {
		t.Error("the identity is deleted BEFORE the settings are written; a failed " +
			"settings write then leaves settings claiming a pairing whose credentials " +
			"no longer exist — unpaired on disk, paired in the file")
	}
}
