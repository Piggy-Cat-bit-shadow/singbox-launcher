package service

import (
	"sync"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
)

// TestConcurrentEmitDeliveredInSequenceOrder is statement 1 (§34 name).
//
// `emit` assigned the sequence under `b.mu` but delivered to subscribers AFTER
// releasing it. Two goroutines could therefore hand out 10 and 11 in order and then
// deliver them in the opposite order. The frontend's rule is "discard any event whose
// seq is <= my high-water mark", which is exactly right for a duplicated or replayed
// event and catastrophically wrong here: the lower number is discarded as stale, so
// the transition it carried is lost until something unrelated happens to re-emit it.
//
// Not theoretical: `core_state_changed` (seq 10) overtaken by `settings_changed`
// (seq 11) loses the core transition from the UI entirely.
func TestConcurrentEmitDeliveredInSequenceOrder(t *testing.T) {
	b := backendWithConfig(t)

	const (
		emitters   = 16
		perEmitter = 60
		total      = emitters * perEmitter
	)

	var mu sync.Mutex
	observed := make([]int64, 0, total)
	got := make(chan struct{}, total)

	unsub := b.Subscribe(func(ev protocol.Event) {
		mu.Lock()
		observed = append(observed, ev.Seq)
		mu.Unlock()
		select {
		case got <- struct{}{}:
		default:
		}
	})
	defer unsub()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < emitters; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start // release them together, to maximise interleaving
			for j := 0; j < perEmitter; j++ {
				b.emit(protocol.EventCoreStateChanged, map[string]any{
					"emitter": n, "n": j,
				})
			}
		}(i)
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	deadline := time.After(60 * time.Second)
	for {
		mu.Lock()
		n := len(observed)
		mu.Unlock()
		if n >= total {
			break
		}
		select {
		case <-got:
		case <-done:
			mu.Lock()
			n = len(observed)
			mu.Unlock()
			if n < total {
				t.Fatalf("only %d of %d events delivered", n, total)
			}
		case <-deadline:
			t.Fatalf("timed out after %d events", n)
		}
		if n >= total {
			break
		}
	}

	mu.Lock()
	seqs := append([]int64(nil), observed...)
	mu.Unlock()

	// Drained whatever else arrived so the count is exact.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	seqs = append([]int64(nil), observed...)
	mu.Unlock()

	// THE INVARIANT: the order subscribers observed IS the order of the sequence
	// numbers. A single inversion is a lost event at the frontend.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("delivery order violates sequence order at index %d: "+
				"observed ...%d, %d... — the lower number is discarded as stale by "+
				"the client, so the transition it carried is lost",
				i, seqs[i-1], seqs[i])
		}
	}
	if len(seqs) != total {
		t.Fatalf("delivered %d events, want %d", len(seqs), total)
	}
	if seqs[0] != 1 {
		t.Errorf("first delivered sequence is %d, want 1", seqs[0])
	}
}

// TestConcurrentEmitIsStrictlyMonotonicUnderHeavyContention — the same invariant with
// subscribers that do work, so a fast emitter cannot accidentally serialise itself.
func TestConcurrentEmitIsStrictlyMonotonicUnderHeavyContention(t *testing.T) {
	b := backendWithConfig(t)

	var mu sync.Mutex
	var last int64
	var inversions int
	var count int

	unsub := b.Subscribe(func(ev protocol.Event) {
		mu.Lock()
		if last != 0 && ev.Seq <= last {
			inversions++
		}
		last = ev.Seq
		count++
		mu.Unlock()
		// A subscriber that yields: makes any "they usually serialise" assumption fail.
		time.Sleep(20 * time.Microsecond)
	})
	defer unsub()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				b.emit(protocol.EventCoreStateChanged, map[string]any{"j": j})
			}
		}()
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if inversions != 0 {
		t.Fatalf("%d out-of-order deliveries observed; sequence order and delivery "+
			"order must be guaranteed by the same mechanism", inversions)
	}
	if count != 320 {
		t.Fatalf("delivered %d events, want 320", count)
	}
}

// TestEmitDoesNotHoldTheStateLockDuringDelivery — the reason the fix is a dispatcher
// rather than a wider lock.
//
// A subscriber commonly calls back into the backend (reading a snapshot, for example).
// Delivering under `b.mu` would deadlock the moment it does, so the sequence and the
// delivery must be serialised by a mechanism that is NOT the state mutex.
func TestEmitDoesNotHoldTheStateLockDuringDelivery(t *testing.T) {
	b := backendWithConfig(t)

	delivered := make(chan struct{})
	unsub := b.Subscribe(func(ev protocol.Event) {
		// Re-enters the backend while holding whatever the emitter holds.
		_ = b.Snapshot()
		_ = b.IsShuttingDown()
		select {
		case <-delivered:
		default:
			close(delivered)
		}
	})
	defer unsub()

	go b.emit(protocol.EventCoreStateChanged, map[string]any{"x": 1})

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscriber never ran, or it deadlocked re-entering the backend " +
			"while the emitter held a lock")
	}
}
