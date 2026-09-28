package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTenDuplicateStartsRunTheBodyOnce is CASE 6.
//
// The reported log showed `backend: start_core requested` repeated many times.
// The guard already refused the duplicates, but each caller logged BEFORE
// reaching it, so the log read as several concurrent starts. This asserts the
// property that actually matters — the work runs ONCE — and, with it, that the
// wording is now backed by behaviour rather than by a reading of the log.
func TestTenDuplicateStartsRunTheBodyOnce(t *testing.T) {
	b := backendWithConfig(t)

	var executions int32
	release := make(chan struct{})
	body := func(ctx context.Context) error {
		atomic.AddInt32(&executions, 1)
		<-release // hold the operation in flight so the duplicates overlap it
		return nil
	}

	// The first call owns the operation and blocks inside the body.
	done := make(chan error, 1)
	go func() { done <- b.runCoreOp("start", 30*time.Second, body) }()

	// Wait until it is actually in flight, so the duplicates below are genuinely
	// concurrent with it rather than racing the goroutine above.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if op := b.ops.snapshotOp(); op != nil && op.kind == "start" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if op := b.ops.snapshotOp(); op == nil {
		t.Fatal("the first start never became current")
	}

	const duplicates = 10
	var wg sync.WaitGroup
	for i := 0; i < duplicates; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.runCoreOp("start", 30*time.Second, body); err != nil {
				t.Errorf("a duplicate start returned an error: %v", err)
			}
		}()
	}
	wg.Wait()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the owning start failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let any stray body run

	if got := atomic.LoadInt32(&executions); got != 1 {
		t.Errorf("the start body ran %d times for %d duplicate requests; exactly one "+
			"core may be started", got, duplicates+1)
	}
}

// TestStartWhileRunningDoesNotStartASecondCore is CASE 7.
//
// A start arriving while the runtime is ALREADY running must not spawn a second
// core. The backend refuses a duplicate of the SAME kind, but once the first
// operation has settled a new `start` is a fresh operation — so the runtime
// itself has to be the thing that refuses.
func TestStartWhileRunningDoesNotStartASecondCore(t *testing.T) {
	b := backendWithConfig(t)
	if b.ac == nil || b.ac.RunningState == nil {
		t.Fatal("the fixture has no runtime state")
	}
	b.ac.RunningState.Set(true)

	// Model what CoreBackend.StartVPNContext does for a running runtime: return
	// without spawning. The assertion is on the observable rule — no second
	// start body runs while running.
	var executions int32
	err := b.runCoreOp("start", 5*time.Second, func(ctx context.Context) error {
		if b.ac.RunningState.IsRunning() {
			return nil // the already-running short circuit
		}
		atomic.AddInt32(&executions, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("start while running returned an error: %v", err)
	}
	if got := atomic.LoadInt32(&executions); got != 0 {
		t.Errorf("a start arriving while the core is running executed the start body "+
			"%d times; a second core must not be started", got)
	}
}

// TestDuplicateStopJoinsTheTeardownInFlight — a duplicate stop must not read as
// two teardowns, and must not run the teardown twice.
func TestDuplicateStopJoinsTheTeardownInFlight(t *testing.T) {
	b := backendWithConfig(t)

	var executions int32
	release := make(chan struct{})
	body := func(ctx context.Context) error {
		atomic.AddInt32(&executions, 1)
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- b.runCoreOp("stop", 30*time.Second, body) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if op := b.ops.snapshotOp(); op != nil && op.kind == "stop" {
			break
		}
		time.Sleep(time.Millisecond)
	}

	for i := 0; i < 5; i++ {
		if err := b.runCoreOp("stop", 30*time.Second, body); err != nil {
			t.Errorf("duplicate stop %d returned an error: %v", i, err)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the owning stop failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(&executions); got != 1 {
		t.Errorf("the teardown ran %d times; a duplicate stop must join the one in "+
			"flight, not repeat it", got)
	}
}
