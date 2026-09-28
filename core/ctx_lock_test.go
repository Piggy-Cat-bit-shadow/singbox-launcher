package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"strings"
)

// acquireWithContext is small, subtle and load-bearing: it decides ownership of
// a mutex between two goroutines with no blocking on either side. Reasoning
// alone is not evidence for that, so it is tested directly and hard.
//
// The invariant every case must preserve: EXACTLY ONE side owns the lock. An
// abandoned acquisition that leaks the lock deadlocks the engine permanently; a
// double release panics with an unrecoverable "unlock of unlocked mutex".

// TestAcquireWithContextTakesAFreeLock — the ordinary case.
func TestAcquireWithContextTakesAFreeLock(t *testing.T) {
	var mu sync.Mutex
	if !acquireWithContext(context.Background(), &mu) {
		t.Fatal("a free lock must be acquired")
	}
	// We own it: an immediate TryLock must fail, and Unlock must not panic.
	if mu.TryLock() {
		t.Fatal("the lock was reported acquired but is actually free")
	}
	mu.Unlock()
}

// TestAcquireWithContextRefusesAnAlreadyCancelledContext — no lock is taken, and
// the caller is told not to unlock.
func TestAcquireWithContextRefusesAnAlreadyCancelledContext(t *testing.T) {
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if acquireWithContext(ctx, &mu) {
		t.Fatal("a cancelled context must not acquire the lock")
	}
	// The lock must still be free, or the refusal leaked it.
	if !mu.TryLock() {
		t.Fatal("a refused acquisition left the lock held; the engine would deadlock")
	}
	mu.Unlock()
}

// TestAcquireWithContextGivesUpWhileBlocked is the abandoned-wait case, and the
// one the whole design exists for: the lock is held by someone else, our context
// expires, and we must neither block nor leak.
func TestAcquireWithContextGivesUpWhileBlocked(t *testing.T) {
	var mu sync.Mutex
	mu.Lock() // someone else holds it

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- acquireWithContext(ctx, &mu) }()

	select {
	case got := <-done:
		if got {
			t.Fatal("acquired a lock that was already held")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquireWithContext did not give up when its context expired; the " +
			"caller is blocked with no way out, which is the whole thing the " +
			"operation timeout was supposed to prevent")
	}
}

// TestAcquireWithContextAbandonedDoesNotLeakTheLock is the race that matters.
//
// The helper goroutine may acquire the lock at the exact moment the caller gives
// up. Whichever way that resolves, the lock must end up FREE once everything
// settles — otherwise every later apply blocks forever, and the launcher's core
// silently stops responding.
func TestAcquireWithContextAbandonedDoesNotLeakTheLock(t *testing.T) {
	for i := 0; i < 300; i++ {
		var mu sync.Mutex
		mu.Lock() // force the helper to block, so it acquires only after the cancel

		ctx, cancel := context.WithCancel(context.Background())

		result := make(chan bool, 1)
		go func() { result <- acquireWithContext(ctx, &mu) }()

		// Cancel immediately: this lands inside the helper's pending Lock in a
		// different place on each iteration, which is the point.
		cancel()
		<-result

		// The helper may still be about to acquire. Release our hold, then wait
		// for the lock to become free; if the abandoned acquisition leaked it,
		// this never succeeds.
		mu.Unlock()
		waitFree(t, &mu, i)
	}
}

// TestAcquireWithContextNeverDoubleUnlocks — a double release is an
// UNRECOVERABLE runtime error, not a panic that can be caught.
func TestAcquireWithContextNeverDoubleUnlocks(t *testing.T) {
	for i := 0; i < 300; i++ {
		var mu sync.Mutex
		held := make(chan struct{})
		release := make(chan struct{})

		go func() {
			mu.Lock()
			close(held)
			<-release
			mu.Unlock()
		}()
		<-held

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan bool, 1)
		go func() { done <- acquireWithContext(ctx, &mu) }()
		cancel()
		<-done

		close(release)
		waitFree(t, &mu, i)
	}
}

// TestAcquireWithContextUnderContention — many waiters, one lock, mixed
// cancellations. Nothing may leak and nothing may double-release.
func TestAcquireWithContextUnderContention(t *testing.T) {
	var mu sync.Mutex
	var holders atomic.Int32
	var maxHolders atomic.Int32

	const workers = 24
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(),
				time.Duration(n%5)*time.Millisecond)
			defer cancel()

			if !acquireWithContext(ctx, &mu) {
				return
			}
			// We own the lock. Prove mutual exclusion held.
			if n := holders.Add(1); n > 1 {
				if n > maxHolders.Load() {
					maxHolders.Store(n)
				}
			}
			holders.Add(-1)
			mu.Unlock()
		}(i)
	}

	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()

	select {
	case <-waitDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the contention test deadlocked; an acquisition leaked the lock")
	}

	if maxHolders.Load() > 1 {
		t.Fatalf("mutual exclusion was violated: %d holders at once", maxHolders.Load())
	}

	// Give any ABANDONED acquisition time to finish releasing.
	//
	// A worker whose context was already expired returns false immediately, and
	// its helper goroutine may still be acquiring the lock to release it. That is
	// the designed behaviour, not a leak — so the check below waits rather than
	// sampling at one instant and calling a transient state a leak. The point of
	// the assertion is that the lock becomes free and STAYS free, which
	// waitFree confirms by taking it.
	waitFree(t, &mu, -1)

	// And it must still be free after a further settle, proving nothing is
	// holding it in the background.
	time.Sleep(20 * time.Millisecond)
	if !mu.TryLock() {
		t.Fatal("the lock was free and then became held again; an abandoned " +
			"acquisition is running after the fact and the engine would deadlock")
	}
	mu.Unlock()
}

// waitFree waits for mu to become acquirable, then releases it.
func waitFree(t *testing.T, mu *sync.Mutex, iteration int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mu.TryLock() {
			mu.Unlock()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("iteration %d: the lock was never released; an abandoned acquisition "+
		"leaked it", iteration)
}

// TestDaemonStopWaitIsCancellable — a stop must not be blocked past its deadline
// by whatever apply it queues behind.
//
// `StopVPNContext` took a plain `applyMu.Lock()`. An apply can take tens of
// seconds, so a caller's deadline bounded nothing: the stop had not started when
// the timeout expired, and the reply described a teardown that never began. The
// daemon RPC had the same problem — a service that accepts a stop and then does
// not answer held the caller indefinitely.
func TestDaemonStopWaitIsCancellable(t *testing.T) {
	src := stripCommentsForTest(readCoreSource(t, "core/backend_daemon.go"))

	idx := strings.Index(src, "func (b *DaemonBackend) StopVPNContext(")
	if idx < 0 {
		t.Fatal("StopVPNContext not found")
	}
	end := strings.Index(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	if strings.Contains(body, "b.applyMu.Lock()") {
		t.Error("StopVPNContext waits for applyMu with a plain Lock; a stop queued " +
			"behind a long apply ignores its own deadline entirely")
	}
	if !strings.Contains(body, "acquireWithContext(ctx, &b.applyMu)") {
		t.Error("StopVPNContext does not acquire applyMu with a context")
	}
	if strings.Contains(body, "b.admin.Stop()") {
		t.Error("StopVPNContext calls the non-contextual daemon Stop, so a daemon " +
			"that accepts the request and never answers holds the caller forever")
	}
	if !strings.Contains(body, "StopCtx(ctx)") {
		t.Error("StopVPNContext does not pass its context to the daemon stop RPC")
	}
}
