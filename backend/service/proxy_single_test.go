package service

import (
	"strings"
	"testing"
)

// TestSingleProxyTestIsCancellable is statement 17 (§34 name).
//
// `TestProxy` runs its measurement under `context.Background()`, so nothing can stop it:
// not the core stopping, not an engine switch, not backend shutdown. The group test is
// carefully cancellable on all three, which makes the single test the one path that can
// hold Quit open on a wedged socket — and Quit is exactly when the user has decided they
// are done waiting.
//
// It also BORROWS the active group run's id as its generation via ActiveRunID(). That id
// belongs to a different run: it is either 0 (no group test running, so the measurement is
// attributed to no run at all) or the id of a group test that is currently in flight and
// whose own results are keyed by it. Storing a single-node measurement under a group run's
// generation makes the two indistinguishable — the superseded-run check cannot tell the
// user's deliberate single test from a late result of the group run it was attributed to.
func TestSingleProxyTestIsCancellable(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/proxies.go"))

	idx := indexOf(src, "func (b *Backend) TestProxy(group, name string)")
	if idx < 0 {
		t.Fatal("TestProxy not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	if strings.Contains(body, "context.Background()") {
		t.Error("the single proxy test measures under context.Background(), so nothing can " +
			"cancel it — not a core stop, not an engine switch, not shutdown — and Quit " +
			"can be held open by a measurement that will never return")
	}
	if !strings.Contains(body, "b.testContext()") {
		t.Error("the single proxy test does not derive its context from the backend's run " +
			"context, so it is outside the cancellation tree the group test is inside")
	}
	if strings.Contains(body, "ActiveRunID()") {
		t.Error("the single proxy test borrows the ACTIVE GROUP RUN's id as its generation. " +
			"That id identifies a different run: it is 0 when no group test is running, and " +
			"otherwise it is a group test's id, so a single-node measurement is stored as " +
			"if it belonged to that group run")
	}
}

// TestSingleProxyTestHasItsOwnGeneration — the replacement for the borrowed id.
//
// The measurement needs an identity of its own so it is distinguishable from a group run's
// results, and so it can be cancelled without touching a running group test.
func TestSingleProxyTestHasItsOwnGeneration(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/proxies.go"))

	idx := indexOf(src, "func (b *Backend) TestProxy(group, name string)")
	if idx < 0 {
		t.Fatal("TestProxy not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	if !strings.Contains(body, "single") && !strings.Contains(body, "Generation") {
		t.Error("the single proxy test records no generation of its own")
	}
}

// TestSingleTestGenerationIsDisjointFromGroupRuns — the property that makes the two
// distinguishable.
//
// A single test's generation must never equal a group run's id, or a stored measurement
// cannot be attributed: the superseded-run logic would drop the user's deliberate single
// test as a late result of some group run.
func TestSingleTestGenerationIsDisjointFromGroupRuns(t *testing.T) {
	var m groupTestManager

	groupIDs := map[uint64]bool{}
	for i := 0; i < 50; i++ {
		run := m.begin("g", []proxyNode{{Name: "n"}}, nil)
		groupIDs[run.id] = true
		m.finish(run.id)
	}

	for i := 0; i < 50; i++ {
		got := m.NextSingleTestGeneration()
		if got == 0 {
			t.Fatal("a single test got generation 0, which is the 'no run' sentinel")
		}
		if groupIDs[got] {
			t.Fatalf("a single test's generation (%d) collides with a group run's id, so "+
				"the two cannot be told apart in the measurement store", got)
		}
	}

	// Two single tests must differ, or a newer one cannot supersede an older one.
	a := m.NextSingleTestGeneration()
	b := m.NextSingleTestGeneration()
	if a == b {
		t.Fatal("two single tests got the same generation")
	}
}
