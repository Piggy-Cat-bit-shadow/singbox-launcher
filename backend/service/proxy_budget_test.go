package service

import (
	"testing"
	"time"

	"singbox-launcher/api"
)

// TestGroupTestBudgetCoversTheWorkItDescribes is statement 14 (§34 name).
//
// The budget is derived from the work — batches times the per-node timeout — and then
// clamped to 60 seconds. When the derived value EXCEEDS the clamp the budget no longer
// covers the work it was computed from, so a run is cancelled while nodes are still
// legitimately probing and the user sees timeouts they did not configure.
//
// This is reachable through the UI: the per-node timeout is user-settable up to 60s, and
// the default of 5s already needs 15s per batch, so a handful of nodes at the maximum
// setting overruns the clamp.
func TestGroupTestBudgetCoversTheWorkItDescribes(t *testing.T) {
	// The user raises the per-node timeout to the maximum the settings allow.
	prev := api.GetPingTestTimeoutMs()
	api.SetPingTestTimeoutMs(api.MaxPingTestTimeoutMs)
	defer api.SetPingTestTimeoutMs(prev)

	perNode := time.Duration(api.GetPingTestTimeoutMs()) * time.Millisecond

	// A modest group: 20 nodes, concurrency 4 → 5 batches.
	const nodes, concurrency = 20, 4
	batches := (nodes + concurrency - 1) / concurrency

	got := groupTestBudget(nodes, concurrency)

	// The budget must cover the probing it schedules, or the run is cut off mid-flight.
	needed := time.Duration(batches) * perNode
	if got < needed {
		t.Fatalf("the group test budget is %s but the work it schedules needs at least "+
			"%s (%d batches × %s per node). The budget is clamped below the work it was "+
			"derived from, so the run is cancelled while nodes are still probing and the "+
			"user sees timeouts they did not configure", got, needed, batches, perNode)
	}
}

// TestGroupTestBudgetStillBoundsAGiantGroup — the clamp must still exist.
//
// Removing the cap entirely would let a pathological group (thousands of nodes at the
// maximum timeout) hold a request open for an hour. The cap has to be large enough to
// cover honest work and still finite.
func TestGroupTestBudgetStillBoundsAGiantGroup(t *testing.T) {
	prev := api.GetPingTestTimeoutMs()
	api.SetPingTestTimeoutMs(api.MaxPingTestTimeoutMs)
	defer api.SetPingTestTimeoutMs(prev)

	got := groupTestBudget(100000, 1)
	if got > 30*time.Minute {
		t.Fatalf("a pathological group produced a %s budget; the bound has to stay "+
			"finite so a request cannot be held open indefinitely", got)
	}
	if got < time.Second {
		t.Fatalf("budget collapsed to %s", got)
	}
}

// TestGroupTestBudgetIsMonotonicInWork — more work must never mean less budget.
func TestGroupTestBudgetIsMonotonicInWork(t *testing.T) {
	prev := api.GetPingTestTimeoutMs()
	api.SetPingTestTimeoutMs(api.DefaultPingTestTimeoutMs)
	defer api.SetPingTestTimeoutMs(prev)

	small := groupTestBudget(4, 4)
	large := groupTestBudget(64, 4)
	if large < small {
		t.Fatalf("a larger group got a smaller budget (%s for 64 nodes vs %s for 4)", large, small)
	}
}
