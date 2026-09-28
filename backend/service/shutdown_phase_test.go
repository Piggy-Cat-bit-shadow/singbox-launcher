package service

import (
	"sync"
	"testing"
	"time"
)

// TestShutdownStartedSignalClosesBeforeTeardown is statement 2 (§34 name).
//
// `Shutdown` closes its signal in a `defer`, so the channel means "teardown FINISHED"
// while `IsShuttingDown` — and its comment — treat it as "teardown BEGAN". For the whole
// duration of the teardown the backend therefore reports that it is NOT shutting down,
// and everything that consults the signal waits for the teardown instead of being
// cancelled by it.
//
// The one that matters most is the group latency test: its context was cancelled by
// this signal, so a test that should stop IMMEDIATELY when shutdown begins instead ran
// until `GracefulExit` had completely finished — at which point cancelling it is
// meaningless, because the provider is already gone.
func TestShutdownStartedSignalClosesBeforeTeardown(t *testing.T) {
	b := backendWithConfig(t)

	// A teardown that blocks until the test releases it.
	entered := make(chan struct{})
	release := make(chan struct{})
	installControllableLegacy(t, b)
	b.ac.SetExitHookForTest(func() {
		close(entered)
		<-release
	})

	done := make(chan struct{})
	go func() { b.Shutdown(); close(done) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the teardown never began")
	}

	// The teardown is RUNNING right now. From here it must be observable as started.
	started := make(chan struct{})
	go func() {
		for !b.IsShuttingDown() {
			time.Sleep(time.Millisecond)
		}
		close(started)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("IsShuttingDown reported false while the teardown was running; the " +
			"signal means 'finished', so every consumer waits for the teardown " +
			"instead of being cancelled by it")
	}

	// And the run context must already be cancelled: new work must not start.
	select {
	case <-b.runContext().Done():
	case <-time.After(2 * time.Second):
		t.Error("the run context was not cancelled while the teardown was running")
	}

	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not complete after release")
	}
}

// TestShutdownCancelsGroupTestImmediately is the consequence that made claim 2 a P0.
//
// The active latency test's context hung off the shutdown signal, so it was cancelled
// only after `GracefulExit` returned. A test that is supposed to stop the moment
// shutdown begins kept probing a transport that was already being torn down.
func TestShutdownCancelsGroupTestImmediately(t *testing.T) {
	b := backendWithConfig(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	b.ac.SetExitHookForTest(func() {
		close(entered)
		<-release
	})

	cancelled := make(chan struct{})
	var once sync.Once
	b.groupTests.setHookForTest(func() { once.Do(func() { close(cancelled) }) })

	done := make(chan struct{})
	go func() { b.Shutdown(); close(done) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the teardown never began")
	}

	// The teardown is blocked mid-flight. The latency test must ALREADY be cancelled.
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("the active group test was not cancelled when shutdown BEGAN; it waits " +
			"for the teardown to finish, by which time cancelling it is meaningless")
	}

	close(release)
	<-done
}

// TestShutdownDoneSignalIsSeparate — the other half: a caller that needs the teardown to
// be COMPLETE must still be able to wait for that.
func TestShutdownDoneSignalIsSeparate(t *testing.T) {
	b := backendWithConfig(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	b.ac.SetExitHookForTest(func() {
		close(entered)
		<-release
	})

	done := make(chan struct{})
	go func() { b.Shutdown(); close(done) }()
	<-entered

	// Started, but NOT finished.
	if !b.IsShuttingDown() {
		t.Error("shutdown has begun but is not reported as started")
	}
	select {
	case <-b.ShutdownDone():
		t.Fatal("the done signal fired while the teardown was still running")
	default:
	}

	close(release)
	<-done
	select {
	case <-b.ShutdownDone():
	case <-time.After(5 * time.Second):
		t.Fatal("the done signal never fired after the teardown completed")
	}
}
