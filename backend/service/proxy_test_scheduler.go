package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"singbox-launcher/api"
	"singbox-launcher/backend/protocol"
	coreservices "singbox-launcher/core/services"
	"singbox-launcher/internal/debuglog"
)

// Proxy latency test scheduling.
//
// DESIGN INSPIRATION: Mihomo's URL-test behaviour — a group's nodes are tested
// CONCURRENTLY, every node's outcome is recorded independently, and only the
// whole-group outcome is aggregated, so one dead node costs the budget of one
// node rather than of the whole list.
//
// This is an INDEPENDENT implementation against JijieBox's own ProxyTransport
// abstraction, not a port. Mihomo is GPL-3.0 and none of its code is reproduced
// here; what is borrowed is the shape of the behaviour.
//
// DELIBERATELY NOT ADOPTED from Mihomo: tolerance-based switching, fastest-node
// selection, fallback/load-balance strategies, provider health checks,
// failedTimes auto-recovery and provider caching. Those are core ROUTING
// policies. This launcher only MEASURES; it never changes the user's selected
// node as a side effect of a test.

// proxyNode is one unit of work: a node inside the run's group.
type proxyNode struct {
	Group string
	Name  string
}

// Progress phases.
const (
	ProxyTestPhaseStarted  = "started"
	ProxyTestPhaseResult   = "result"
	ProxyTestPhaseFinished = "finished"
	// ProxyTestPhaseNodeStarted marks one node as actually in flight. Separate
	// from the run-level "started" so the UI can spin only the rows occupying a
	// worker rather than all of them.
	ProxyTestPhaseNodeStarted = "node_started"
)

// ProxyGroupTestProgress is one progress frame of a group test.
//
// Only deltas are sent: a result frame names the single node that finished.
// Pushing the whole node list per event would send megabytes over the IPC
// channel for a test that changed one number.
type ProxyGroupTestProgress struct {
	RunID uint64 `json:"run_id"`
	Group string `json:"group"`
	// Phase is one of started / result / finished.
	Phase string `json:"phase"`
	// Total is the node count snapshotted at start. Fixed for the run, so
	// progress can never read "31/30" if the config changes underneath.
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`

	// Result-phase fields.
	Node       string `json:"node,omitempty"`
	Delay      int64  `json:"delay,omitempty"`
	Status     string `json:"status,omitempty"`
	Error      string `json:"error,omitempty"`
	MeasuredAt string `json:"measured_at,omitempty"`
}

// ProxyGroupTestResult is the final answer of a group test.
type ProxyGroupTestResult struct {
	RunID      uint64             `json:"run_id"`
	Group      string             `json:"group"`
	Total      int                `json:"total"`
	Succeeded  int                `json:"succeeded"`
	Failed     int                `json:"failed"`
	Cancelled  bool               `json:"cancelled,omitempty"`
	DurationMS int64              `json:"duration_ms"`
	Proxies    protocol.ProxyList `json:"proxies"`
}

// groupTestBudget computes the overall ceiling for a group run.
//
// A per-node budget bounds ONE node; it does not bound the RUN. Without an
// overall ceiling a transport that ignores cancellation — or a pathologically
// slow core — could hold the IPC request open indefinitely.
//
// The estimate is the number of concurrency-bounded batches times the node
// budget plus a margin for setup and IPC. It is a safety net, not a schedule: a
// healthy run finishes far sooner.
func groupTestBudget(nodes, concurrency int) time.Duration {
	if concurrency < 1 {
		concurrency = 1
	}
	if nodes < 1 {
		nodes = 1
	}
	batches := (nodes + concurrency - 1) / concurrency
	perNode := time.Duration(api.GetPingTestTimeoutMs()) * time.Millisecond
	required := time.Duration(batches) * perNode
	budget := required + 10*time.Second
	if budget < 15*time.Second {
		budget = 15 * time.Second
	}

	// THE CAP MUST NOT CUT INTO THE WORK.
	//
	// There is a real reason to bound this: past some point the user has moved on and
	// holding the request open serves nobody. But a bound that is SMALLER than the work it
	// describes does not bound the run, it truncates it — the scheduler cancels probes
	// that were still legitimately running and reports timeouts the user never configured.
	//
	// That is reachable through the UI, because the per-node timeout is user-settable up
	// to 60s: 20 nodes at concurrency 4 already needs 5 minutes, five times the old cap.
	//
	// So the cap is now a HARD CEILING placed far above any honest group rather than just
	// above the typical one. Ten minutes covers the entire realistic configuration space
	// with room to spare — the per-node timeout maxes out at 60s, so this accommodates
	// ten full sequential batches at the slowest setting the UI permits — while still
	// guaranteeing that a pathological node count cannot hold a request open for hours.
	const hardCap = 10 * time.Minute
	if budget > hardCap {
		budget = hardCap
	}
	return budget
}

// proxyTestRun is one in-flight group test.
type proxyTestRun struct {
	id      uint64
	group   string
	nodes   []proxyNode
	started time.Time
	// ctx is cancelled when this run is superseded, the core stops, the engine
	// switches, or the backend shuts down.
	ctx    context.Context
	cancel context.CancelFunc
}

// groupTestManager owns the single active group test for this backend.
//
// One run at a time, because the UI can present only one progress state.
// Starting a new run supersedes the old one — the user's latest intent wins —
// and the superseded run's late results are dropped by run id rather than by
// racing on shared state.
type groupTestManager struct {
	mu     sync.Mutex
	nextID uint64
	// nextSingle numbers single-node tests, in a space disjoint from group-run ids.
	nextSingle uint64
	active     *proxyTestRun
	// cancelHook observes cancellations in tests; nil in production.
	cancelHook func()
}

// begin supersedes any active run and registers a new one.
func (m *groupTestManager) begin(group string, nodes []proxyNode, parent context.Context) *proxyTestRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		m.active.cancel()
		m.active = nil
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	m.nextID++
	run := &proxyTestRun{
		id:      m.nextID,
		group:   group,
		nodes:   nodes,
		started: time.Now(),
		ctx:     ctx,
		cancel:  cancel,
	}
	m.active = run
	return run
}

// finish clears the active run and releases its context.
func (m *groupTestManager) finish(id uint64) {
	m.mu.Lock()
	if m.active != nil && m.active.id == id {
		m.active.cancel()
		m.active = nil
	}
	m.mu.Unlock()
}

// isCurrent reports whether id is still the run the UI should care about.
//
// Late results from a superseded run are dropped by this test, which is what
// keeps a slow Classic run from painting numbers onto a UI that has already
// switched to a different group or engine.
func (m *groupTestManager) isCurrent(id uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active != nil && m.active.id == id
}

// ActiveRunID returns the active group test id, or 0.
func (m *groupTestManager) ActiveRunID() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return 0
	}
	return m.active.id
}

// NextSingleTestGeneration allocates an identity for a SINGLE-node test.
//
// It draws from its own space, high above the group-run ids, and that separation is the
// point. A single test previously stored its measurement under `ActiveRunID()`, which is
// either 0 or the id of a group test that is currently running — so a node the user tested
// by hand was recorded as if it were a result of that group run. The two are different
// events with different lifetimes: the group run's results are dropped when it is
// superseded, while the user's deliberate single test must survive.
//
// It does NOT touch the active run, so testing one node never cancels a group test in
// progress.
func (m *groupTestManager) NextSingleTestGeneration() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextSingle++
	// Offset so a single-test generation can never collide with a group-run id, no matter
	// how many of either have happened. The high bit is set.
	return singleTestGenerationBase | m.nextSingle
}

// singleTestGenerationBase separates single-test generations from group-run ids.
//
// Group-run ids start at 1 and increment, so setting the top bit means the two spaces
// cannot overlap for any realistic number of runs, and the distinction is visible in a log
// line or a stored generation.
const singleTestGenerationBase = 1 << 62

// CancelActive cancels the active group test, if any.
//
// Called when the core stops (the transport is gone), when the engine mode
// switches (the transport is about to be replaced) and when the backend shuts
// down. Continuing to probe nodes against a dead or replaced transport would keep
// the process busy during Quit and could report results for an engine that is no
// longer running.
//
// WIRED, and the distinction matters: for a while this method's comment described
// a design that did not exist, and it had no callers at all. See the tests that
// drive a REAL cancellation trigger rather than calling it directly.
func (m *groupTestManager) CancelActive() {
	m.mu.Lock()
	run := m.active
	hook := m.cancelHook
	m.mu.Unlock()
	if run != nil {
		run.cancel()
	}
	if hook != nil {
		hook()
	}
}

// measurementOutcome is one node's finished measurement, as produced by a worker.
type measurementOutcome struct {
	Node       proxyNode
	Delay      int64
	Status     coreservices.MeasurementStatus
	Error      string
	MeasuredAt time.Time
}

// measureProxy is the single measurement primitive.
//
// Single-node test and group test both go through this, so timeout handling,
// capability detection and error classification exist once. Having two paths is
// how they drift: the single-node path would keep one policy while the group
// path grew another.
func measureProxy(ctx context.Context, transport coreservices.ProxyTransport, node proxyNode) measurementOutcome {
	now := time.Now()
	delay, err := transport.DelayContext(ctx, node.Name)
	if err != nil {
		return measurementOutcome{
			Node: node, Delay: -1, Status: classifyProxyMeasurementError(err),
			Error: err.Error(), MeasuredAt: now,
		}
	}
	if delay < 0 {
		// A transport reporting a negative delay has no measurement; treating it
		// as a number would display a bogus row.
		return measurementOutcome{
			Node: node, Delay: -1, Status: coreservices.MeasurementFailed,
			Error: "the engine returned no latency", MeasuredAt: now,
		}
	}
	return measurementOutcome{
		Node: node, Delay: delay, Status: coreservices.MeasurementSuccess, MeasuredAt: now,
	}
}

// classifyProxyMeasurementError maps a transport error to a stable status.
//
// ONE classifier for both transports. Classic reports HTTP failures while
// daemon reports gRPC codes, and their text differs completely — so the UI must
// never see either. It switches on these tokens, which is also why no screen
// contains a string like "deadline exceeded".
func classifyProxyMeasurementError(err error) coreservices.MeasurementStatus {
	if err == nil {
		return coreservices.MeasurementSuccess
	}
	// Cancellation is not a node verdict: the run was stopped, and the node was
	// never judged. Reporting those as failures would mark a whole group dead
	// when the user simply pressed Stop.
	if errors.Is(err, context.Canceled) {
		return coreservices.MeasurementCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, api.ErrPlatformInterrupt) {
		return coreservices.MeasurementTimeout
	}
	if coreservices.IsProxyCapabilityError(err) {
		return coreservices.MeasurementUnsupported
	}
	return coreservices.MeasurementFailed
}

// groupTestSummary aggregates a completed run.
type groupTestSummary struct {
	succeeded int
	failed    int
	cancelled bool
}

// executeGroupTest runs the bounded worker pool and emits progress.
//
// SHAPE: one jobs channel, one results channel, exactly N workers and one
// coordinator. Deliberately NOT "one goroutine per node plus a semaphore":
// spawning 100 goroutines to have 20 do work wastes scheduling and makes
// cancellation ambiguous. Only N workers exist, so the bound is structural
// rather than enforced by a counter that can be got wrong.
//
// ORDERING: workers never emit. They publish to the results channel and the
// coordinator is the only emitter, which is what guarantees completed counts
// arrive as 1,2,3… instead of in completion order. It also means the
// measurement store is written from one goroutine, so no lock ordering question
// arises between the counters and the cache.
func (b *Backend) executeGroupTest(
	ctx context.Context,
	run *proxyTestRun,
	transport coreservices.ProxyTransport,
	nodes []proxyNode,
) groupTestSummary {
	concurrency := api.GetPingTestAllConcurrency()
	if concurrency > len(nodes) {
		concurrency = len(nodes)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	b.emit(protocol.EventProxyTestProgress, ProxyGroupTestProgress{
		RunID: run.id, Group: run.group, Phase: ProxyTestPhaseStarted, Total: len(nodes),
	})

	jobs := make(chan proxyNode, len(nodes))
	results := make(chan measurementOutcome, len(nodes))

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for node := range jobs {
				// Cancellation is checked per node so a stopped run does not
				// start new measurements, and each worker returns promptly
				// rather than draining the queue.
				select {
				case <-ctx.Done():
					return
				default:
				}
				// Tell the UI which node is now actually in flight, so only
				// real in-flight rows show a spinner while the rest keep their
				// previous value. Emitted from the WORKER for this, and only
				// this: it is per-node state that no counter needs, and routing
				// it through the coordinator would reorder it behind results.
				//
				// Guarded by the same currency check as the coordinator: a worker can
				// pick up a job after the run was superseded, and its spinner would
				// otherwise appear on a screen showing a different group.
				if b.groupTests.isCurrent(run.id) {
					b.emit(protocol.EventProxyTestProgress, ProxyGroupTestProgress{
						RunID: run.id, Group: run.group, Phase: ProxyTestPhaseNodeStarted,
						Total: len(nodes), Node: node.Name,
					})
				}
				// A panic in a transport must not take the backend down: this
				// path is driven directly by a UI click.
				outcome := safeMeasure(ctx, transport, node)
				select {
				case results <- outcome:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Feed jobs, then close so workers exit their range.
	go func() {
		defer close(jobs)
		for _, node := range nodes {
			select {
			case jobs <- node:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Closer: workers done implies no more results.
	go func() {
		wg.Wait()
		close(results)
	}()

	summary := groupTestSummary{}
	completed := 0
	outstanding := len(nodes)

	// collect consumes one outcome, updating counters, the store and progress.
	// It runs only on this goroutine.
	collect := func(outcome measurementOutcome) {
		// THE GUARD THAT MAKES `isCurrent` MEAN SOMETHING.
		//
		// A run can be superseded while a probe is in flight — the user switches groups,
		// switches engine, or the core stops — and that probe still returns, possibly
		// seconds later. Writing its result would paint the OLD run's numbers onto rows
		// the UI is now showing for the NEW run, and emitting its progress would move the
		// completed counter for a run the user has already abandoned.
		//
		// Cancelling the run's context stops most in-flight work, but it cannot stop a
		// result that has ALREADY been produced and is sitting in the results channel,
		// and it cannot stop a transport that ignores cancellation. This check is what
		// covers those, and it is why `isCurrent` exists.
		if !b.groupTests.isCurrent(run.id) {
			return
		}
		completed++
		outstanding--
		switch outcome.Status {
		case coreservices.MeasurementSuccess:
			summary.succeeded++
		case coreservices.MeasurementCancelled:
			summary.cancelled = true
		default:
			summary.failed++
		}

		// Store BEFORE emitting: the event tells the UI to re-read, so a
		// measurement that arrived but was not yet stored would make the row
		// flicker back to "unknown".
		b.ac.APIService.SetMeasurement(outcome.Node.Name, coreservices.ProxyMeasurementState{
			Delay:      outcome.Delay,
			Status:     outcome.Status,
			Error:      outcome.Error,
			MeasuredAt: outcome.MeasuredAt,
			Generation: run.id,
		})

		b.emit(protocol.EventProxyTestProgress, ProxyGroupTestProgress{
			RunID: run.id, Group: run.group, Phase: ProxyTestPhaseResult,
			Total: len(nodes), Completed: completed,
			Succeeded: summary.succeeded, Failed: summary.failed,
			Node: outcome.Node.Name, Delay: outcome.Delay,
			Status: string(outcome.Status), Error: outcome.Error,
			MeasuredAt: outcome.MeasuredAt.UTC().Format(time.RFC3339Nano),
		})
	}

	for outstanding > 0 {
		select {
		case outcome, ok := <-results:
			if !ok {
				// Workers exited without producing every result — only possible
				// on cancellation. Stop waiting rather than blocking forever.
				outstanding = 0
				summary.cancelled = true
			} else {
				collect(outcome)
			}
		case <-ctx.Done():
			summary.cancelled = true
			// Drain what the workers already produced so their measurements are
			// not lost, then finish.
			for {
				select {
				case outcome, ok := <-results:
					if !ok {
						outstanding = 0
					} else {
						collect(outcome)
					}
					if outstanding == 0 {
						break
					}
				default:
					outstanding = 0
				}
				if outstanding == 0 {
					break
				}
			}
		}
	}

	// The terminal event is guarded like every other one.
	//
	// A superseded run reaching this point is the NORMAL case, not an exceptional one:
	// superseding cancels the run, the cancellation ends this loop, and control arrives
	// here on the way out. Emitting "finished" then would tell the UI that a run it has
	// already replaced is complete, and the finished phase is what the UI uses to clear
	// its progress state — so the NEW run's progress would be wiped by the OLD run's
	// teardown.
	if b.groupTests.isCurrent(run.id) {
		b.emit(protocol.EventProxyTestProgress, ProxyGroupTestProgress{
			RunID: run.id, Group: run.group, Phase: ProxyTestPhaseFinished,
			Total: len(nodes), Completed: completed,
			Succeeded: summary.succeeded, Failed: summary.failed,
		})
	}
	return summary
}

// safeMeasure runs measureProxy, converting a transport panic into a failed
// measurement for that node.
//
// A panic here must not unwind through the worker goroutine: that would kill the
// process on a UI-driven action. One node reporting "failed" is a far better
// outcome than the whole app dying mid-test.
func safeMeasure(ctx context.Context, transport coreservices.ProxyTransport, node proxyNode) (outcome measurementOutcome) {
	defer func() {
		if r := recover(); r != nil {
			debuglog.WarnLog("backend: latency test for %q panicked: %v", node.Name, r)
			outcome = measurementOutcome{
				Node: node, Delay: -1, Status: coreservices.MeasurementFailed,
				Error: "the latency test failed unexpectedly", MeasuredAt: time.Now(),
			}
		}
	}()
	return measureProxy(ctx, transport, node)
}

// RunGroupTest measures every node of a group with bounded concurrency.
//
// THIS is the entry point the IPC layer calls. The frontend sends one intent and
// consumes progress; it never schedules work itself. Putting concurrency in the
// UI would move cancellation, aggregation and error policy into Swift, where they
// cannot be tested against the transport and would need rewriting per frontend.
func (b *Backend) RunGroupTest(ctx context.Context, group string) (ProxyGroupTestResult, error) {
	// Capability gate FIRST: the UI disables the action, but a stale or
	// hand-rolled request must not reach a transport that cannot serve it.
	caps := b.proxyActionCapabilities()
	if !caps.CanTestGroup {
		return ProxyGroupTestResult{}, capabilityErrorFor(caps)
	}

	list, err := b.Proxies(group)
	if err != nil {
		return ProxyGroupTestResult{}, err
	}
	if !list.Available {
		// The engine is up but has no such group: a legitimate empty answer.
		return ProxyGroupTestResult{Group: group, Proxies: list}, nil
	}
	transport, ok := b.transport()
	if !ok {
		return ProxyGroupTestResult{}, &protocol.Error{
			Code: "core_not_running", Message: "start the core before testing latency",
			Recoverable: true,
		}
	}

	// Snapshot the node list: the run's total must not change mid-flight, or
	// progress becomes unexplainable.
	nodes := make([]proxyNode, 0, len(list.Proxies))
	for _, p := range list.Proxies {
		nodes = append(nodes, proxyNode{Group: group, Name: p.Name})
	}
	if len(nodes) == 0 {
		return ProxyGroupTestResult{Group: group, Proxies: list}, nil
	}

	run := b.groupTests.begin(group, nodes, b.testContext())
	defer b.groupTests.finish(run.id)

	// The caller's context (client disconnect) and the run's own context
	// (supersede / core stop / shutdown) both stop the work.
	runCtx, cancelRun := context.WithCancel(run.ctx)
	defer cancelRun()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			run.cancel()
		case <-stop:
		}
	}()

	budget := groupTestBudget(len(nodes), api.GetPingTestAllConcurrency())
	runCtx, cancelBudget := context.WithTimeout(runCtx, budget)
	defer cancelBudget()

	summary := b.executeGroupTest(runCtx, run, transport, nodes)

	// One authoritative re-read, now that measurements are stored: the returned
	// list carries the values this run just measured, overlaid on whatever the
	// engine reports.
	final, ferr := b.Proxies(group)
	if ferr != nil {
		final = list
	}
	return ProxyGroupTestResult{
		RunID:      run.id,
		Group:      group,
		Total:      len(nodes),
		Succeeded:  summary.succeeded,
		Failed:     summary.failed,
		Cancelled:  summary.cancelled,
		DurationMS: time.Since(run.started).Milliseconds(),
		Proxies:    final,
	}, nil
}

// testContext is the parent context for group tests: cancelled when the backend
// shuts down, so Quit is never held up by a stuck measurement.
//
// It derives from the backend's SHARED run context rather than building its own.
// The previous implementation created a fresh context plus a fresh "watch for
// shutdown and cancel it" goroutine on EVERY call, and returned only the context:
// the watcher had nothing to release it until the whole backend exited, so each
// test leaked a goroutine for the lifetime of the process. Two hundred latency
// tests meant two hundred parked goroutines.
//
// Sharing the parent removes the watcher entirely — cancellation propagates
// through the context tree, so there is nothing left to leak.
func (b *Backend) testContext() context.Context {
	return b.runContext()
}

// setHookForTest records a callback invoked whenever the active run is cancelled.
//
// It exists so the WIRING can be tested: the defect was not that cancellation did
// not work, but that nothing ever asked for it. A test that called CancelActive
// itself would pass with every call site deleted, so the hook lets a test trigger a
// cancellation through its real cause and observe it from here.
func (m *groupTestManager) setHookForTest(fn func()) {
	m.mu.Lock()
	m.cancelHook = fn
	m.mu.Unlock()
}
