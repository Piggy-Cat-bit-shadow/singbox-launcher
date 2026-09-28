package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
	coreservices "singbox-launcher/core/services"
)

// TestSupersededRunDoesNotWriteMeasurements is statement 15 (§34 name).
//
// `isCurrent` documents precisely the right behaviour — "late results from a superseded
// run are dropped by this test, which is what keeps a slow Classic run from painting
// numbers onto a UI that has already switched to a different group or engine" — and had
// ZERO callers. The coordinator wrote every measurement into the shared store and emitted
// every progress event without ever asking whether its run was still the one the UI cared
// about.
//
// The visible result: the user starts a test on group A, changes their mind and starts one
// on group B, and A's slow probes land afterwards, overwriting B's rows with A's numbers.
func TestSupersededRunDoesNotWriteMeasurements(t *testing.T) {
	b := backendWithConfig(t)

	// A first run that blocks inside one node's measurement, so it can be superseded
	// while genuinely in flight rather than after it has already finished.
	release := make(chan struct{})
	transport := &countingTransport{
		nodes:   []string{"node-a"},
		blockOn: "node-a",
		release: release,
	}

	first := b.groupTests.begin("GroupA",
		[]proxyNode{{Group: "GroupA", Name: "node-a"}}, context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.executeGroupTest(first.ctx, first, transport, first.nodes)
	}()

	// Wait until the first run is genuinely inside its measurement.
	deadline := time.Now().Add(5 * time.Second)
	for {
		transport.mu.Lock()
		n := len(transport.started)
		transport.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first run never started measuring")
		}
		time.Sleep(time.Millisecond)
	}

	// The user supersedes it with a run on another group.
	second := b.groupTests.begin("GroupB",
		[]proxyNode{{Group: "GroupB", Name: "node-b"}}, context.Background())
	defer b.groupTests.finish(second.id)

	// The newer run records its own measurement.
	b.ac.APIService.SetMeasurement("node-b", coreservices.ProxyMeasurementState{
		Delay:      777,
		Status:     coreservices.MeasurementSuccess,
		MeasuredAt: time.Now(),
		Generation: second.id,
	})

	// Now the superseded run's probe finally returns.
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded run never finished")
	}

	measurements := b.ac.APIService.GetMeasurements()
	if m, ok := measurements["node-a"]; ok {
		t.Errorf("a SUPERSEDED run wrote a measurement for node-a (delay=%d, status=%s); "+
			"late results from a run the UI has moved past must be dropped, not stored",
			m.Delay, m.Status)
	}
	if m, ok := measurements["node-b"]; !ok || m.Delay != 777 {
		t.Errorf("the CURRENT run's measurement was lost or overwritten: %+v", m)
	}
}

// TestSupersededRunDoesNotEmitProgress is statement 15's second half.
//
// Dropping the STORE write is not enough: a superseded run's progress events still reach
// the UI, where they move the completed counter and the per-node spinner for a run the
// user has abandoned. The events are keyed by RunID, so a client that checks it could
// filter them — but the backend must not require the client to undo its work.
func TestSupersededRunDoesNotEmitProgress(t *testing.T) {
	b := backendWithConfig(t)

	release := make(chan struct{})
	transport := &countingTransport{
		nodes:   []string{"node-a"},
		blockOn: "node-a",
		release: release,
	}

	var mu sync.Mutex
	// Only collect events emitted AFTER the supersede point, so the pre-existing
	// "started" event for the first run does not count against it.
	var collect bool
	var seen []uint64
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event != protocol.EventProxyTestProgress {
			return
		}
		p, ok := ev.Payload.(ProxyGroupTestProgress)
		if !ok {
			return
		}
		mu.Lock()
		if collect {
			seen = append(seen, p.RunID)
		}
		mu.Unlock()
	})
	defer unsub()

	first := b.groupTests.begin("GroupA",
		[]proxyNode{{Group: "GroupA", Name: "node-a"}}, context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.executeGroupTest(first.ctx, first, transport, first.nodes)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		transport.mu.Lock()
		n := len(transport.started)
		transport.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first run never started measuring")
		}
		time.Sleep(time.Millisecond)
	}

	second := b.groupTests.begin("GroupB",
		[]proxyNode{{Group: "GroupB", Name: "node-b"}}, context.Background())
	defer b.groupTests.finish(second.id)

	mu.Lock()
	collect = true
	mu.Unlock()

	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded run never finished")
	}

	mu.Lock()
	defer mu.Unlock()
	for _, id := range seen {
		if id == first.id {
			t.Fatalf("a superseded run (id=%d) emitted a progress event after run %d "+
				"superseded it; the UI moves its completed counter and its per-node "+
				"spinner for a run the user has abandoned", first.id, second.id)
		}
	}
}
