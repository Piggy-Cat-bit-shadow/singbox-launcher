package core

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"singbox-launcher/internal/lxdclient"
)

// Regression tests for the DAEMON side of "stopped means confirmed exit".
//
// The classic engine was taught this invariant first: a signal that was sent is
// not a process that exited. The daemon engine kept the old, weaker belief — that
// an accepted request is a completed one — so `/admin/stop` returning HTTP 200
// was treated as proof the tunnel was down. These tests pin the corrected
// contract on both the user-initiated stop and the exit path.

// stopProofDaemon is a daemon whose stop ACKNOWLEDGES but does not take effect.
//
// It models the dangerous case directly: the request is accepted (200) while the
// core keeps running. The existing fakeDaemon helper cannot express this, because
// it flips its status to idle synchronously inside the /admin/stop handler — so
// every test built on it passes whether or not the launcher verifies anything.
type stopProofDaemon struct {
	mu        sync.Mutex
	status    string
	stops     int
	stopTaken bool // when false, /admin/stop is accepted but the core survives
}

func newStopProofDaemon(t *testing.T, status string, stopTaken bool) (*stopProofDaemon, *httptest.Server) {
	t.Helper()
	d := &stopProofDaemon{status: status, stopTaken: stopTaken}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		switch r.URL.Path {
		case "/admin/status":
			st := d.status
			d.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"` + st + `"}`))
			return
		case "/admin/stop":
			d.stops++
			if d.stopTaken {
				d.status = "idle"
			}
		}
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return d, srv
}

func (d *stopProofDaemon) setStatus(s string) {
	d.mu.Lock()
	d.status = s
	d.mu.Unlock()
}

func (d *stopProofDaemon) stopCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stops
}

// newDaemonBackendForStop wires a DaemonBackend to an arbitrary admin endpoint.
func newDaemonBackendForStop(t *testing.T, srv *httptest.Server) (*AppController, *DaemonBackend) {
	t.Helper()
	ac := &AppController{RunningState: &RunningState{}}
	ac.RunningState.controller = ac
	ac.RunningState.Set(false)
	installTestController(t, ac)
	b := &DaemonBackend{ac: ac, admin: lxdclient.New(lxdclient.Config{
		Addr: strings.TrimPrefix(srv.URL, "http://"),
	})}
	ac.backend = b
	return ac, b
}

// TestDaemonStopIsNotReportedUntilTheCoreIsGone — the reported lie.
//
// `admin.Stop()` is an ordinary POST. The daemon answering 200 means the request
// was accepted; it says nothing about whether the core exited. Claiming stopped
// on that basis left the UI offering "Start" while the tunnel was still carrying
// traffic.
func TestDaemonStopIsNotReportedUntilTheCoreIsGone(t *testing.T) {
	d, srv := newStopProofDaemon(t, "started", false) // stop accepted, core survives
	ac, b := newDaemonBackendForStop(t, srv)
	ac.RunningState.Set(true)

	b.StopVPN()

	// Wait past the point where the eager write would have happened.
	time.Sleep(600 * time.Millisecond)
	if !ac.RunningState.IsRunning() {
		t.Fatal("reported stopped while the daemon still reports the core as started: " +
			"the launcher would show an idle VPN over a live tunnel")
	}
	if d.stopCount() == 0 {
		t.Fatal("the stop request was never sent")
	}
}

// TestDaemonStopReportsStoppedOnceTheCoreIsConfirmedGone — the honest case must
// still settle, and settle on the daemon's own word.
func TestDaemonStopReportsStoppedOnceTheCoreIsConfirmedGone(t *testing.T) {
	d, srv := newStopProofDaemon(t, "started", true) // stop takes effect
	ac, b := newDaemonBackendForStop(t, srv)
	ac.RunningState.Set(true)

	b.StopVPN()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ac.RunningState.IsRunning() {
		time.Sleep(20 * time.Millisecond)
	}
	if ac.RunningState.IsRunning() {
		t.Fatalf("a confirmed stop must settle to stopped (stop requests sent: %d)", d.stopCount())
	}
}

// TestDaemonStopThatIsConfirmedLaterSettlesLate — the core goes idle a moment
// after the request, which is the normal (asynchronous) teardown. The launcher
// must keep waiting rather than either lying immediately or giving up.
func TestDaemonStopThatIsConfirmedLaterSettlesLate(t *testing.T) {
	d, srv := newStopProofDaemon(t, "started", false)
	ac, b := newDaemonBackendForStop(t, srv)
	ac.RunningState.Set(true)

	b.StopVPN()
	time.Sleep(300 * time.Millisecond)
	if !ac.RunningState.IsRunning() {
		t.Fatal("gave up before the daemon confirmed the teardown")
	}
	// The core finishes going down.
	d.setStatus("idle")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ac.RunningState.IsRunning() {
		time.Sleep(20 * time.Millisecond)
	}
	if ac.RunningState.IsRunning() {
		t.Fatal("a teardown that completed later must still settle to stopped")
	}
}

// TestDaemonStopRecordsAFailureWhenItCannotBeConfirmed — a stop that never takes
// effect must produce an error the headless frontend can see, not just a silent
// non-transition. This is the §12 requirement: no Fyne-only error path.
func TestDaemonStopRecordsAFailureWhenItCannotBeConfirmed(t *testing.T) {
	_, srv := newStopProofDaemon(t, "started", false)
	ac, b := newDaemonBackendForStop(t, srv)
	ac.RunningState.Set(true)
	if ac.HasLifecycleError() {
		t.Fatal("precondition: no lifecycle error before the stop")
	}

	b.StopVPN()

	// The confirmation budget is seconds, not milliseconds; poll for the record.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !ac.HasLifecycleError() {
		time.Sleep(50 * time.Millisecond)
	}
	if !ac.HasLifecycleError() {
		t.Fatal("an unconfirmable stop must be recorded; otherwise the user is " +
			"left with a VPN that is still up and an app that says nothing is wrong")
	}
	snap := ac.LifecycleError()
	if snap.Operation != "stop" {
		t.Errorf("the failure belongs to the stop operation, got %q", snap.Operation)
	}
}

// TestDaemonStoppingStateIsPublished — §13: the protocol has always defined
// `stopping`, but the daemon engine never produced it, because its only signal
// was RunningState (legitimately still true while the core is being torn down).
func TestDaemonStoppingStateIsPublished(t *testing.T) {
	_, srv := newStopProofDaemon(t, "started", false)
	_, b := newDaemonBackendForStop(t, srv)

	if b.Stopping() {
		t.Fatal("precondition: no stop in flight")
	}
	b.SetStopping(true)
	if !b.Stopping() {
		t.Fatal("the stopping marker must be observable, since it is what the wire state reads")
	}
	b.SetStopping(false)
	if b.Stopping() {
		t.Fatal("clearing the marker must clear it")
	}
}

// TestClosedDaemonBackendIsNotActive — §23/§P1-4: `isActive` is a pointer
// comparison against ac.Backend(), and setBackend runs prev.Close() while
// ac.backend STILL points at prev. So during the handover a dying backend looks
// active, and a status frame decoded microseconds earlier could publish state
// after the new backend went live.
func TestClosedDaemonBackendIsNotActive(t *testing.T) {
	_, srv := newStopProofDaemon(t, "started", true)
	ac, b := newDaemonBackendForStop(t, srv)

	if !b.isActive() {
		t.Fatal("precondition: a published backend is active")
	}
	b.Close()
	// ac.backend still points at b, exactly as it does inside setBackend.
	if ac.Backend() != CoreBackend(b) {
		t.Fatal("precondition: the pointer comparison would still call this backend live")
	}
	if b.isActive() {
		t.Fatal("a closed backend must never look active; otherwise its callbacks " +
			"can write state after the engine has been replaced")
	}
}
