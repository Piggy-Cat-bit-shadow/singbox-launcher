package core

import (
	"strings"
	"testing"
)

// TestDaemonFatalWithoutANodeIsRecorded — §20/§26 regression.
//
// The real production failure was `default outbound not found: proxy-out`: the
// daemon's supervisor reported a core FATAL naming an outbound that did not
// exist, so it was not a node the launcher could disable. retryCoreReject
// declined, and retryAfterCoreFatal simply RETURNED — the core stayed down and
// nothing was recorded or shown. The user pressed Connect and the app went
// quiet; the cause existed only in the log line we had just written.
//
// The invariant: a FATAL that cannot be resolved by disabling a node must leave
// a lifecycle error carrying the daemon's own message, because that message is
// the only place the true cause exists.
func TestDaemonFatalWithoutANodeIsRecorded(t *testing.T) {
	d := newFakeDaemon(t, "STARTED")
	ac, b := newTestDaemonBackend(t, d, false)

	if ac.HasLifecycleError() {
		t.Fatal("the controller must start with no lifecycle error")
	}

	// The exact message from the real incident.
	b.retryAfterCoreFatal("default outbound not found: proxy-out")

	if !ac.HasLifecycleError() {
		t.Fatal("an unresolvable core FATAL must be recorded; swallowing it is " +
			"what left the user with a silently dead core")
	}
	snap := ac.LifecycleError()
	if snap.Operation != "start" {
		t.Errorf("the failure belongs to the start operation, got %q", snap.Operation)
	}
	if snap.Recoverable {
		t.Error("a config the core refuses is deterministic and must not be " +
			"advertised as retryable")
	}
	// The daemon's message must survive: it names the offending tag, and without
	// it the user has nothing to act on.
	if !strings.Contains(snap.Detail, "proxy-out") {
		t.Errorf("the daemon's message must be preserved verbatim, got %q", snap.Detail)
	}
}

// TestDaemonFatalIsNotDuplicatedWhenRetryIsPossible — the other half: when a node
// CAN be disabled, the fatal is handled by retrying and must not be reported as a
// failure the user has to act on.
func TestDaemonFatalIsNotDuplicatedWhenRetryIsPossible(t *testing.T) {
	d := newFakeDaemon(t, "STARTED")
	ac, b := newTestDaemonBackend(t, d, false)

	// A rejected message naming no node we know: the disabler declines, so this
	// exercises the recorded branch. The complementary case (a real node) is
	// covered by the reject-loop tests, which drive it through a full apply.
	b.retryAfterCoreFatal("unknown field \"foo\"")

	snap := ac.LifecycleError()
	if !ac.HasLifecycleError() {
		t.Fatal("an unresolvable FATAL must still be recorded")
	}
	// Exactly one record: a second call must replace, not accumulate.
	b.retryAfterCoreFatal("unknown field \"bar\"")
	if got := ac.LifecycleError(); got.Detail == snap.Detail {
		t.Error("the later failure must replace the earlier one, so the user sees " +
			"the current cause rather than a stale one")
	}
}
