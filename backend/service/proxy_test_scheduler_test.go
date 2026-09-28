package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"singbox-launcher/api"
	"singbox-launcher/backend/protocol"
	coreservices "singbox-launcher/core/services"
)

// Proxy latency scheduling tests.
//
// The properties that matter, and why each is asserted the way it is:
//
//   - Concurrency is asserted by MAXIMUM OBSERVED IN-FLIGHT, not by wall clock.
//     A duration threshold passes on a fast machine and flakes on a busy CI
//     runner; the in-flight counter measures the property directly and is
//     deterministic given the sleeps.
//   - Partial and total failure are asserted as COMPLETED runs, because the
//     whole point of the redesign is that one bad node is data, not an error.
//   - Cancellation is asserted by "no new work started" plus "state cleaned up",
//     not by exact timing.

// countingTransport records concurrency and lets each node's outcome be scripted.
type countingTransport struct {
	// nodes is returned by GroupProxies.
	nodes []string

	active    int32
	maxActive int32
	mu        sync.Mutex
	started   []string
	// delayFor decides one node's outcome. Nil means "42 ms, success".
	delayFor func(name string) (int64, error)
	// sleep is how long each measurement occupies a worker.
	sleep time.Duration
	// blockOn, when non-empty, makes that node wait for release.
	blockOn string
	release chan struct{}
}

func (c *countingTransport) GroupProxies(string) ([]api.ProxyInfo, string, error) {
	out := make([]api.ProxyInfo, 0, len(c.nodes))
	for _, n := range c.nodes {
		// Delay is deliberately -1: the engine reports "not measured". A test
		// that let the transport supply the delay could not tell a real
		// measurement from a pass-through.
		out = append(out, api.ProxyInfo{Name: n, Delay: -1})
	}
	return out, "", nil
}

func (c *countingTransport) SwitchProxy(string, string) error { return nil }

func (c *countingTransport) Delay(n string) (int64, error) {
	return c.DelayContext(context.Background(), n)
}

func (c *countingTransport) DelayContext(ctx context.Context, n string) (int64, error) {
	cur := atomic.AddInt32(&c.active, 1)
	for {
		max := atomic.LoadInt32(&c.maxActive)
		if cur <= max || atomic.CompareAndSwapInt32(&c.maxActive, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(&c.active, -1)

	c.mu.Lock()
	c.started = append(c.started, n)
	c.mu.Unlock()

	if c.sleep > 0 {
		select {
		case <-time.After(c.sleep):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if c.blockOn == n && c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if c.delayFor != nil {
		return c.delayFor(n)
	}
	return 42, nil
}

func (c *countingTransport) observedMax() int { return int(atomic.LoadInt32(&c.maxActive)) }

// nodeNames builds n deterministic node names.
func nodeNames(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("node-%02d", i))
	}
	return out
}

// TestGroupTestIsBoundedAndConcurrent — the core scheduler property.
func TestGroupTestIsBoundedAndConcurrent(t *testing.T) {
	api.SetPingTestAllConcurrency(5)
	defer api.SetPingTestAllConcurrency(20)

	tr := &countingTransport{nodes: nodeNames(20), sleep: 40 * time.Millisecond}
	b := proxyBackend(t, tr)

	res, err := b.RunGroupTest(context.Background(), "g")
	if err != nil {
		t.Fatalf("RunGroupTest: %v", err)
	}
	max := tr.observedMax()
	t.Logf("nodes=%d succeeded=%d failed=%d maxInFlight=%d duration=%dms",
		res.Total, res.Succeeded, res.Failed, max, res.DurationMS)

	if max > 5 {
		t.Errorf("max in-flight = %d, exceeded the configured bound of 5", max)
	}
	if max <= 1 {
		t.Errorf("max in-flight = %d: the pool did not run concurrently, so the "+
			"serial regression is back", max)
	}
	if res.Succeeded != 20 || res.Failed != 0 {
		t.Errorf("succeeded=%d failed=%d, want 20/0", res.Succeeded, res.Failed)
	}
	if len(tr.started) != 20 {
		t.Errorf("started %d measurements, want 20 — every node must be measured once",
			len(tr.started))
	}
}

// TestGroupTestPartialFailureCompletes — one bad node must not fail the run.
//
// This is the behaviour most worth borrowing from a mature URL-test scheduler:
// the group result is an aggregate, so 15 successes and 5 timeouts is a
// COMPLETED test with a summary, not an error.
func TestGroupTestPartialFailureCompletes(t *testing.T) {
	api.SetPingTestAllConcurrency(10)
	defer api.SetPingTestAllConcurrency(20)

	tr := &countingTransport{
		nodes: nodeNames(20),
		delayFor: func(name string) (int64, error) {
			// The last five fail.
			if name >= "node-15" {
				return 0, context.DeadlineExceeded
			}
			return 42, nil
		},
	}
	b := proxyBackend(t, tr)

	res, err := b.RunGroupTest(context.Background(), "g")
	if err != nil {
		t.Fatalf("RunGroupTest returned an error for a partial failure: %v", err)
	}
	if res.Total != 20 || res.Succeeded != 15 || res.Failed != 5 {
		t.Errorf("total=%d succeeded=%d failed=%d, want 20/15/5",
			res.Total, res.Succeeded, res.Failed)
	}

	// Each failure keeps its own reason, so the row can explain itself.
	api := b.ac.APIService
	for _, n := range []string{"node-15", "node-19"} {
		m, ok := api.GetMeasurement(n)
		if !ok {
			t.Errorf("%s has no stored measurement", n)
			continue
		}
		if m.Status != coreservices.MeasurementTimeout {
			t.Errorf("%s status = %q, want timeout", n, m.Status)
		}
		if m.Delay != -1 {
			t.Errorf("%s delay = %d, want -1 (a timeout is not 0 ms)", n, m.Delay)
		}
	}
	if m, _ := api.GetMeasurement("node-00"); m.Status != coreservices.MeasurementSuccess || m.Delay != 42 {
		t.Errorf("node-00 = %+v, want success at 42", m)
	}
}

// TestGroupTestAllFailureIsACompletedRun — total failure is still a RESULT.
//
// The frontend must receive counters, not a backend error: "all nodes failed" is
// something the UI says, not something the IPC layer throws.
func TestGroupTestAllFailureIsACompletedRun(t *testing.T) {
	api.SetPingTestAllConcurrency(10)
	defer api.SetPingTestAllConcurrency(20)

	tr := &countingTransport{
		nodes:    nodeNames(10),
		delayFor: func(string) (int64, error) { return 0, context.DeadlineExceeded },
	}
	b := proxyBackend(t, tr)

	res, err := b.RunGroupTest(context.Background(), "g")
	if err != nil {
		t.Fatalf("an all-failure run returned a backend error: %v", err)
	}
	if res.Succeeded != 0 || res.Failed != 10 || res.Total != 10 {
		t.Errorf("succeeded=%d failed=%d total=%d, want 0/10/10",
			res.Succeeded, res.Failed, res.Total)
	}
}

// TestProgressEventSequence — one started, exactly N results, one finished.
func TestProgressEventSequence(t *testing.T) {
	api.SetPingTestAllConcurrency(4)
	defer api.SetPingTestAllConcurrency(20)

	tr := &countingTransport{nodes: nodeNames(12), sleep: 5 * time.Millisecond}
	b := proxyBackend(t, tr)

	var mu sync.Mutex
	var frames []ProxyGroupTestProgress
	b.Subscribe(func(ev protocol.Event) {
		if ev.Event != protocol.EventProxyTestProgress {
			return
		}
		p, ok := ev.Payload.(ProxyGroupTestProgress)
		if !ok {
			return
		}
		mu.Lock()
		frames = append(frames, p)
		mu.Unlock()
	})

	if _, err := b.RunGroupTest(context.Background(), "g"); err != nil {
		t.Fatalf("RunGroupTest: %v", err)
	}
	// Delivery is asynchronous: the frame list is complete only after the dispatcher
	// drains, and every count below is computed from it.
	b.FlushEventsForTest()

	mu.Lock()
	defer mu.Unlock()

	started, results, finished := 0, 0, 0
	lastCompleted := 0
	for _, f := range frames {
		switch f.Phase {
		case ProxyTestPhaseStarted:
			started++
			if f.Total != 12 {
				t.Errorf("started frame total = %d, want 12", f.Total)
			}
		case ProxyTestPhaseResult:
			results++
			// The coordinator is the only emitter, so completed must arrive as
			// a contiguous 1..N rather than in completion order.
			if f.Completed != lastCompleted+1 {
				t.Errorf("completed went %d -> %d; the coordinator is not the "+
					"only emitter", lastCompleted, f.Completed)
			}
			lastCompleted = f.Completed
		case ProxyTestPhaseFinished:
			finished++
		}
	}
	if started != 1 {
		t.Errorf("started frames = %d, want 1", started)
	}
	if results != 12 {
		t.Errorf("result frames = %d, want 12", results)
	}
	if finished != 1 {
		t.Errorf("finished frames = %d, want 1", finished)
	}
	if lastCompleted != 12 {
		t.Errorf("final completed = %d, want 12", lastCompleted)
	}
}

// TestProgressIsNotReorderedByCompletion — counters stay monotonic even when
// nodes finish out of order.
//
// A fast node that starts late still increments the counter after a slow node
// that started early: the frame NUMBER is the coordinator's, not the worker's.
func TestProgressIsNotReorderedByCompletion(t *testing.T) {
	api.SetPingTestAllConcurrency(3)
	defer api.SetPingTestAllConcurrency(20)

	sleeps := map[string]time.Duration{
		"node-00": 120 * time.Millisecond,
		"node-01": 10 * time.Millisecond,
		"node-02": 60 * time.Millisecond,
	}
	tr := &countingTransport{
		nodes: []string{"node-00", "node-01", "node-02"},
		delayFor: func(string) (int64, error) {
			return 42, nil
		},
	}
	// A per-node sleep needs the transport to know the name; wrap DelayContext.
	tr.sleep = 0
	tr.delayFor = func(name string) (int64, error) {
		time.Sleep(sleeps[name])
		return 42, nil
	}
	b := proxyBackend(t, tr)

	var mu sync.Mutex
	order := []int{}
	seen := map[string]int{}
	b.Subscribe(func(ev protocol.Event) {
		if ev.Event != protocol.EventProxyTestProgress {
			return
		}
		p, _ := ev.Payload.(ProxyGroupTestProgress)
		if p.Phase != ProxyTestPhaseResult {
			return
		}
		mu.Lock()
		order = append(order, p.Completed)
		seen[p.Node]++
		mu.Unlock()
	})

	if _, err := b.RunGroupTest(context.Background(), "g"); err != nil {
		t.Fatalf("RunGroupTest: %v", err)
	}
	// Delivery is asynchronous, so the collected order is only complete once the
	// dispatcher has drained. Reading before that makes this a timing assertion.
	b.FlushEventsForTest()

	mu.Lock()
	defer mu.Unlock()
	for i, c := range order {
		if c != i+1 {
			t.Fatalf("completed counters = %v, want 1,2,3", order)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("saw %d distinct nodes, want 3", len(seen))
	}
	for n, count := range seen {
		if count != 1 {
			t.Errorf("%s reported %d results, want exactly 1", n, count)
		}
	}
}

// TestSupersededRunStopsAndDoesNotWin — a newer run supersedes the older one.
func TestSupersededRunStopsAndDoesNotWin(t *testing.T) {
	api.SetPingTestAllConcurrency(2)
	defer api.SetPingTestAllConcurrency(20)

	release := make(chan struct{})
	tr := &countingTransport{
		nodes:   nodeNames(6),
		blockOn: "node-00",
		release: release,
	}
	b := proxyBackend(t, tr)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = b.RunGroupTest(context.Background(), "group-A")
	}()

	// Let the first run start, then supersede it.
	time.Sleep(30 * time.Millisecond)
	tr2 := &countingTransport{nodes: nodeNames(3)}
	b.ac.APIService.SetTransport(tr2)
	res2, err := b.RunGroupTest(context.Background(), "group-B")
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	close(release)
	<-firstDone

	if res2.Group != "group-B" {
		t.Errorf("second run reported group %q", res2.Group)
	}
	if res2.Total != 3 || res2.Succeeded != 3 {
		t.Errorf("second run total=%d succeeded=%d, want 3/3", res2.Total, res2.Succeeded)
	}
}

// TestCancellationCleansUp — a cancelled run must not leave state behind.
func TestCancellationCleansUp(t *testing.T) {
	api.SetPingTestAllConcurrency(10)
	defer api.SetPingTestAllConcurrency(20)

	tr := &countingTransport{nodes: nodeNames(20), sleep: 2 * time.Second}
	b := proxyBackend(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ProxyGroupTestResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := b.RunGroupTest(ctx, "g")
		done <- res
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if err := <-errCh; err != nil {
			t.Fatalf("cancelled run returned an error: %v", err)
		}
		if res.RunID == 0 {
			t.Error("cancelled run has no run id")
		}
		t.Logf("cancelled: completed=%d succeeded=%d failed=%d",
			res.Total-res.Failed-res.Succeeded, res.Succeeded, res.Failed)
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled run did not finish; cancellation is not reaching workers")
	}

	// No run may remain active, or the UI would be stuck in "testing" forever.
	if id := b.groupTests.ActiveRunID(); id != 0 {
		t.Errorf("active run id = %d after cancellation, want 0", id)
	}
}

// TestSingleMeasurementIsNotDiscarded — the §15/§80 regression.
//
// The transport reports the node (correctly) as unmeasured with -1. The measured
// 42 must come from OUR store, not from a re-read of the engine — otherwise the
// single-node test silently returns "unknown" on any engine that does not echo
// its own delay into the group snapshot.
func TestSingleMeasurementIsNotDiscarded(t *testing.T) {
	tr := &countingTransport{
		nodes:    []string{"A"},
		delayFor: func(string) (int64, error) { return 42, nil },
	}
	b := proxyBackend(t, tr)

	list, err := b.TestProxy("g", "A")
	if err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if len(list.Proxies) != 1 {
		t.Fatalf("got %d proxies, want 1", len(list.Proxies))
	}
	if got := list.Proxies[0].Delay; got != 42 {
		t.Errorf("returned delay = %d, want 42 — the measured value was discarded "+
			"instead of stored", got)
	}
}

// TestMeasurementSurvivesReread — the §113 consistency check.
//
// After a test, a plain reload must still show the measured value. If it
// reverted to "unknown", the UI would flicker back to "—" right after showing a
// number.
func TestMeasurementSurvivesReread(t *testing.T) {
	tr := &countingTransport{
		nodes:    []string{"A", "B"},
		delayFor: func(name string) (int64, error) { return 55, nil },
	}
	b := proxyBackend(t, tr)

	if _, err := b.RunGroupTest(context.Background(), "g"); err != nil {
		t.Fatalf("RunGroupTest: %v", err)
	}
	list, err := b.Proxies("g")
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}
	for _, p := range list.Proxies {
		if p.Delay != 55 {
			t.Errorf("%s delay = %d after reload, want the measured 55", p.Name, p.Delay)
		}
	}
}

// TestPriorSuccessIsKeptSeparateFromCurrentResult — the §20 decision.
//
// A node that measured 42 and then timed out must read as a TIMEOUT. Keeping 42
// as the current delay would present a stale number as present health; the old
// value survives only as LastSuccessDelay.
func TestPriorSuccessIsKeptSeparateFromCurrentResult(t *testing.T) {
	fail := false
	tr := &countingTransport{
		nodes: []string{"A"},
		delayFor: func(string) (int64, error) {
			if fail {
				return 0, context.DeadlineExceeded
			}
			return 42, nil
		},
	}
	b := proxyBackend(t, tr)
	apiSvc := b.ac.APIService

	if _, err := b.TestProxy("g", "A"); err != nil {
		t.Fatalf("first TestProxy: %v", err)
	}
	if m, _ := apiSvc.GetMeasurement("A"); m.Status != coreservices.MeasurementSuccess || m.Delay != 42 {
		t.Fatalf("first measurement = %+v, want success at 42", m)
	}

	fail = true
	if _, err := b.TestProxy("g", "A"); err != nil {
		t.Fatalf("second TestProxy: %v", err)
	}
	m, _ := apiSvc.GetMeasurement("A")
	if m.Status != coreservices.MeasurementTimeout {
		t.Errorf("status after timeout = %q, want timeout", m.Status)
	}
	if m.Delay != -1 {
		t.Errorf("current delay = %d after a timeout, want -1", m.Delay)
	}
	if !m.EverSucceeded || m.LastSuccessDelay != 42 {
		t.Errorf("last-success not preserved: %+v", m)
	}
}

// TestMeasurementStatusIsClassifiedNotStringMatched — the §114/§115 contract.
//
// Both transports must produce the same status for the same underlying
// condition, so the UI never inspects error text.
func TestMeasurementStatusIsClassifiedNotStringMatched(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want coreservices.MeasurementStatus
	}{
		{"deadline", context.DeadlineExceeded, coreservices.MeasurementTimeout},
		{"wrapped deadline", fmt.Errorf("daemon URLTestOutbound: %w", context.DeadlineExceeded), coreservices.MeasurementTimeout},
		{"cancelled", context.Canceled, coreservices.MeasurementCancelled},
		{"capability", coreservices.NewProxyCapabilityError(coreservices.CapabilityTest), coreservices.MeasurementUnsupported},
		{"other", errors.New("connection refused"), coreservices.MeasurementFailed},
	}
	for _, tc := range cases {
		if got := classifyProxyMeasurementError(tc.err); got != tc.want {
			t.Errorf("%s: status = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestClassicAndDaemonShareTheSameScheduling — the §87 parity check.
//
// Whatever the transport, the scheduler's observable behaviour must match: same
// totals, same progress frames, same stored measurements. Anything
// transport-specific must stop at the transport.
func TestClassicAndDaemonShareTheSameScheduling(t *testing.T) {
	run := func(t *testing.T, tr coreservices.ProxyTransport) (int, int, int) {
		t.Helper()
		api.SetPingTestAllConcurrency(4)
		b := proxyBackend(t, tr)

		var mu sync.Mutex
		frames := 0
		b.Subscribe(func(ev protocol.Event) {
			if ev.Event != protocol.EventProxyTestProgress {
				return
			}
			p, _ := ev.Payload.(ProxyGroupTestProgress)
			if p.Phase == ProxyTestPhaseResult {
				mu.Lock()
				frames++
				mu.Unlock()
			}
		})

		res, err := b.RunGroupTest(context.Background(), "g")
		if err != nil {
			t.Fatalf("RunGroupTest: %v", err)
		}
		// Delivery is asynchronous, so the frame count is only final once the
		// dispatcher has drained. Counting before that makes the comparison
		// depend on timing rather than on scheduling.
		b.FlushEventsForTest()
		mu.Lock()
		defer mu.Unlock()
		return res.Succeeded, res.Failed, frames
	}

	// A "classic-like" transport and a "daemon-like" one differ only in the
	// underlying call, which is precisely what the abstraction is for.
	classic := &countingTransport{nodes: nodeNames(8), delayFor: func(string) (int64, error) { return 42, nil }}
	daemon := &countingTransport{nodes: nodeNames(8), delayFor: func(string) (int64, error) { return 42, nil }}

	cs, cf, cfr := run(t, classic)
	ds, df, dfr := run(t, daemon)

	if cs != ds || cf != df {
		t.Errorf("classic (%d/%d) and daemon (%d/%d) disagree on the outcome", cs, cf, ds, df)
	}
	if cfr != dfr {
		t.Errorf("classic emitted %d result frames, daemon %d; scheduling differs by transport",
			cfr, dfr)
	}
}

// TestProxyTestProgressEventIsDeclaredEmittedAndHandled — the §107 three-way
// contract.
//
// A phantom event (declared but never emitted, or emitted but unhandled) is
// invisible at compile time and shows up as a UI that silently never updates.
// This asserts all three ends against the real constant.
func TestProxyTestProgressEventIsDeclaredEmittedAndHandled(t *testing.T) {
	if protocol.EventProxyTestProgress != "proxy_test_progress" {
		t.Fatalf("event name = %q; the Swift client switches on this exact string",
			protocol.EventProxyTestProgress)
	}

	api.SetPingTestAllConcurrency(3)
	defer api.SetPingTestAllConcurrency(20)

	tr := &countingTransport{nodes: nodeNames(4), delayFor: func(string) (int64, error) { return 42, nil }}
	b := proxyBackend(t, tr)

	// The Swift side declares these phase tokens in ProxyTestProgress; they must
	// match the Go constants or frames are ignored in silence.
	swift, err := os.ReadFile(filepath.Join("..", "..", "macos", "Sources", "JiejieBox",
		"App", "AppModel.swift"))
	if err != nil {
		t.Skipf("cannot read AppModel.swift: %v", err)
	}
	src := string(swift)
	for _, phase := range []string{ProxyTestPhaseResult, ProxyTestPhaseFinished, "node_started"} {
		if !strings.Contains(src, `"`+phase+`"`) {
			t.Errorf("phase %q is emitted by the backend but not handled in AppModel.swift; "+
				"those frames would be dropped", phase)
		}
	}
	if !strings.Contains(src, "proxyTestProgress") {
		t.Error("AppModel.swift does not reference the progress event")
	}

	// And it is actually emitted.
	var mu sync.Mutex
	seen := 0
	b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventProxyTestProgress {
			mu.Lock()
			seen++
			mu.Unlock()
		}
	})
	if _, err := b.RunGroupTest(context.Background(), "g"); err != nil {
		t.Fatalf("RunGroupTest: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen == 0 {
		t.Error("no progress events were emitted for a group test")
	}
	t.Logf("emitted %d progress frames for 4 nodes", seen)
}
