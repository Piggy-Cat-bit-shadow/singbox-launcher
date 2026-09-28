package service

import (
	"sync"
	"testing"

	"singbox-launcher/backend/protocol"
)

// TestOneBackendHasExactlyOneSession — the session id must be minted once and be the same on
// every path.
//
// It was a plain field with TWO lazy initialisers under two DIFFERENT locks: `emit` minted it
// under `eventQueueMu` while `SessionID()` did so under `mu`. That is a data race (reported by
// `-race`), and the worse outcome is not the race but the disagreement: a client that saw
// session X on an event and session Y on a status read discards one of them as belonging to a
// different process, which is exactly the ambiguity the session id exists to remove.
//
// The race detector is the real proof here — run this under `-race`, where the old code
// reports "Write by SessionID() / Previous read by emit()". This test states the invariant
// that has to hold even on a build without the detector.
func TestOneBackendHasExactlyOneSession(t *testing.T) {
	b := backendWithConfig(t)

	// Concurrently: read the session, and emit events (which ALSO reads it, and used to be
	// the second writer). A zero-value Backend exercises the lazy path for real.
	const readers = 16
	var wg sync.WaitGroup
	sessions := make([]string, readers)
	for i := 0; i < readers; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			sessions[i] = b.SessionID()
		}(i)
		go func(i int) {
			defer wg.Done()
			b.emit("test.session", map[string]any{"i": i})
		}(i)
	}
	wg.Wait()
	b.FlushEventsForTest()

	first := sessions[0]
	if first == "" {
		t.Fatal("the session id is empty, which compares equal to every other empty session " +
			"and silently disables the restart defence it exists for")
	}
	for i, got := range sessions {
		if got != first {
			t.Fatalf("concurrent readers saw different session ids: %q at index 0, %q at "+
				"index %d. The id is minted in two places under two different locks, so one "+
				"process can present two identities", first, got, i)
		}
	}

	// And every delivered event must carry that same session.
	var seen []string
	b.Subscribe(func(ev protocol.Event) { seen = append(seen, ev.Session) })
	b.emit("test.session", nil)
	b.FlushEventsForTest()
	for _, got := range seen {
		if got != first {
			t.Fatalf("an event carried session %q while SessionID() reports %q; a client "+
				"would discard one of them as a different process's", got, first)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no event was delivered, so the session stamping was never exercised")
	}
}
