package service

import (
	"sync"
	"sync/atomic"
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
			// The EMITTERS are finished, which is not the same as the events being
			// DELIVERED: delivery is asynchronous, so wait for the dispatcher to catch up
			// before concluding anything. Reading here directly is what made this look like
			// loss when it was only a race between finishing and draining.
			b.FlushEventsForTest()
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

	// Delivery is asynchronous by design, so wait for the dispatcher rather than guessing
	// with a sleep: the count below is exact only once everything numbered has been handed
	// to the subscribers.
	b.FlushEventsForTest()
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

	// FLUSH, DO NOT SLEEP. Delivery is asynchronous, and a fixed sleep turns this into a
	// race against machine speed: it passed locally for a long time and then failed on the
	// CI runner, which is exactly the failure mode a timing assumption produces. The flush
	// is an exact synchronisation point — it returns once every event emitted BEFORE it has
	// been delivered — so the assertions below read a settled state rather than a hopeful one.
	b.FlushEventsForTest()

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

// TestABlockedSubscriberCannotWedgeTheEmitter is the liveness half of the ordering fix.
//
// The requirement is only that DELIVERY order equals SEQUENCE order. An earlier fix got that
// by holding a mutex across the subscriber calls — correct for ordering, and dangerous for
// liveness, because the production subscriber writes to the client's pipe. A client that
// stops draining its stdin blocks that write forever, so it would block every other emit
// behind it. `Shutdown` announces itself through the same function, so the teardown itself
// would be the thing that never runs: the backend could not even report that it was going
// away, which is exactly when the client most needs to hear it.
//
// Delivery is therefore ordered by a single consumer goroutine rather than by a lock held
// across delivery, and this test pins that: a subscriber that blocks must not stop an
// independent emitter from making progress.
func TestABlockedSubscriberCannotWedgeTheEmitter(t *testing.T) {
	b := &Backend{}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventTrafficRate {
			once.Do(func() { close(entered) })
			<-release // simulate a client that has stopped draining its stdin
		}
	})
	defer unsub()

	b.emit(protocol.EventTrafficRate, map[string]any{"up": 1})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking subscriber was never entered; the test would be vacuous")
	}

	// A DIFFERENT emitter must complete while the first subscriber is still blocked.
	//
	// This is the assertion that fails when delivery happens under a lock held by the
	// emitter: the second call cannot return until the first one's subscriber does, so the
	// emitter — and with it `Shutdown` — is wedged by a client that is not reading.
	emitted := make(chan struct{})
	go func() {
		b.emit(protocol.EventCoreStateChanged, map[string]any{"state": "running"})
		close(emitted)
	}()

	select {
	case <-emitted:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a blocked subscriber PREVENTED an unrelated emit from completing. The " +
			"emitter is wedged behind a client that is not draining its pipe, so " +
			"`shutting_down` cannot be announced and teardown never begins")
	}
	close(release)
}

// TestASubscriberMayEmitWithoutDeadlocking — the reentrancy the design claims to allow.
//
// `emit`'s documentation justified moving off the state lock by saying a subscriber may
// re-enter the backend. With a non-reentrant mutex held across delivery, a subscriber that
// calls `emit` deadlocks permanently — and the test that shipped alongside claimed to cover
// reentrancy while only exercising read-only accessors.
//
// THE SINGLE-CHILD VERSION WAS NOT ENOUGH, and the reason is the whole point. A subscriber
// runs ON the dispatcher goroutine, which is the queue's only consumer. Emitting one child
// finds room in the buffer and returns; emitting MORE THAN `eventQueueSize` children fills it,
// and the send then waits for a drain that cannot happen until the subscriber returns — a
// permanent deadlock, reachable by any subscriber that reacts to an event by emitting a burst
// (a state change that fans out per-source, say). The fix is that the emitter recognises
// re-entrancy and delivers inline rather than queueing.
func TestASubscriberMayEmitWithoutDeadlocking(t *testing.T) {
	b := &Backend{}

	const children = eventQueueSize + 10
	var delivered atomic.Int64

	inner := make(chan struct{})
	var innerOnce sync.Once
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventCoreStateChanged {
			// Re-entering the emitter from inside a subscriber must not deadlock — and must
			// keep working past the buffer size, which is where a naive queue send wedges.
			for i := 0; i < children; i++ {
				b.emit(protocol.EventSettingsChanged, map[string]any{"i": i})
			}
			innerOnce.Do(func() { close(inner) })
			return
		}
		delivered.Add(1)
	})
	defer unsub()

	done := make(chan struct{})
	go func() {
		b.emit(protocol.EventCoreStateChanged, map[string]any{"state": "running"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("a subscriber that emitted %d events deadlocked the backend. It runs on "+
			"the dispatcher goroutine, which is the queue's ONLY consumer, so once the "+
			"buffer fills the send waits for a drain that cannot happen until the "+
			"subscriber returns", children)
	}

	select {
	case <-inner:
	case <-time.After(20 * time.Second):
		t.Fatal("the re-entrant emits were never delivered")
	}

	// Every child must actually arrive: delivering inline must not become "drop on
	// re-entrancy", which would make the deadlock disappear by losing events instead.
	b.FlushEventsForTest()
	if got := delivered.Load(); got != children {
		t.Errorf("delivered %d of %d re-entrant events; a subscriber's own emits must not "+
			"be dropped", got, children)
	}
}

// TestAFullQueueDoesNotFreezeTheBackend — the blast radius of the queue bound.
//
// Holding `mu` across the send made the bound a whole-backend wedge: with the queue full
// behind a stuck subscriber, every OTHER emitter blocked on `mu`, and so did `Snapshot`,
// `Subscribe` and `FlushEventsForTest`. The queue exists to cap a wedged client, so that is
// exactly the situation it must not make worse — and `Shutdown` announces itself through
// `emit`, so teardown could not even begin.
//
// The send has its own lock now, and `mu` is released before it, so a full queue blocks
// emitters only.
func TestAFullQueueDoesNotFreezeTheBackend(t *testing.T) {
	b := &Backend{}

	release := make(chan struct{})
	entered := make(chan struct{})
	var enteredOnce sync.Once
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventCoreStateChanged {
			enteredOnce.Do(func() { close(entered) })
			// Block the ONLY consumer, so the queue fills behind this.
			<-release
		}
	})
	defer unsub()
	defer close(release)

	// Fill the queue from a goroutine that will legitimately block.
	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		for i := 0; i < eventQueueSize+64; i++ {
			b.emit(protocol.EventCoreStateChanged, map[string]any{"i": i})
		}
	}()

	<-entered // the consumer is now stuck and the backlog is growing

	// WAIT FOR THE QUEUE TO ACTUALLY BE FULL, which is the entire precondition.
	//
	// The first version asserted only that the subscriber had been entered, and then read.
	// Filling 65536 slots takes noticeably longer than entering the first subscriber, so it
	// checked the backend with roughly 100 of 65536 events queued — nowhere near the
	// condition. Verified: it PASSED with `b.mu` held across the blocking send, which is the
	// exact defect it is named for. A test whose precondition has not happened yet cannot
	// observe the behaviour that precondition causes.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if len(b.eventQueueCh) == cap(b.eventQueueCh) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the queue never filled (len %d of %d), so the saturated-queue "+
				"condition this test is about was never reached",
				len(b.eventQueueCh), cap(b.eventQueueCh))
		}
		time.Sleep(time.Millisecond)
	}

	// READERS MUST STILL WORK. This is the assertion the fix is about: with `mu` held across
	// the send, this call never returns.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_ = b.Snapshot()
		b.mu.Lock()
		_ = len(b.subscribers)
		b.mu.Unlock()
	}()

	select {
	case <-readDone:
	case <-time.After(10 * time.Second):
		t.Fatal("a full event queue froze the backend's readers. The send must not hold " +
			"the state lock: the queue's bound exists to cap a wedged client, and holding " +
			"`mu` across it turns that bound into a deadlock for Snapshot, Subscribe and " +
			"FlushEventsForTest — the shutdown announcement goes through the same path, so " +
			"teardown itself could not begin")
	}
}

// TestConcurrentFlushesEachWakeTheirOwnCaller — the single-slot clobbering.
//
// `flushSignal` was one slot, so two concurrent flushes overwrote each other: A installed its
// closure, B replaced it, A's own sentinel then fired B's closure, and A blocked on `<-done`
// forever. Keyed by the sequence each caller waits for, every flush is released by its own
// boundary.
func TestConcurrentFlushesEachWakeTheirOwnCaller(t *testing.T) {
	b := &Backend{}

	// A slow subscriber, so the two flushes genuinely overlap and the sentinels queue up
	// behind real work rather than being consumed instantly.
	unsub := b.Subscribe(func(ev protocol.Event) {
		time.Sleep(200 * time.Microsecond)
	})
	defer unsub()

	for i := 0; i < 4; i++ {
		b.emit(protocol.EventCoreStateChanged, map[string]any{"i": i})
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.FlushEventsForTest()
		}()
	}

	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("two concurrent flushes did not both return; with a single signal slot one " +
			"call's closure is clobbered by the other and its caller waits forever")
	}
}
