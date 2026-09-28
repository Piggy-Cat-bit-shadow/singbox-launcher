# PROXY_LATENCY_AUDIT

Group latency testing was serial. It is now a bounded-concurrency scheduler with
live progress, a single measurement primitive shared by single-node and group
tests, and a capability model that distinguishes listing from switching from
measuring.

---

## 1. What was wrong

### 1.1 The group test was a `for` loop

`backend/service/proxies.go`:

```go
for _, p := range list.Proxies {
    if _, derr := transport.Delay(p.Name); derr != nil { ... }
}
```

Every node was measured after the previous one finished. With the default
per-node budget of 5000 ms, a group of 40 nodes containing 20 dead nodes could
take a hundred seconds, during which the UI offered no progress and no way to
tell "working" from "hung".

### 1.2 The concurrency setting existed but was never called

`api.GetPingTestAllConcurrency()` and `normalizePingTestAllConcurrency()`
(1/5/10/20/50/100, default 20) have been in `api/clash_delay.go` since the legacy
ping-all path. A repository-wide search found **no caller**: the constant was
defined, documented and dead. The macOS backend never consulted it.

This is the single most concrete finding: the product already had the right
policy and the new backend path silently bypassed it.

### 1.3 The single-node test threw its own result away

```go
delay, err := transport.Delay(name)
...
_ = delay          // the measured number, discarded
return b.Proxies(group)   // re-read, hoping the core echoes it back
```

The value was measured and then dropped, and the reply was rebuilt from whatever
the engine reported next. Under Classic this sometimes coincided. Under daemon it
is not a contract at all: `URLTestOutbound` returning 42 ms says nothing about
what the next group snapshot contains, so the row could show "—" immediately
after a successful test.

### 1.4 Capability was one boolean for three different actions

Listing, switching and measuring were collapsed into a single
`ErrProxyListUnsupported`. An engine that could list and switch but not measure
had its whole proxy screen disabled, and the user could not tell which ability
was missing or act on it.

---

## 2. What was built

### 2.1 One measurement primitive

`measureProxy(ctx, transport, group, name)` is the only place latency is
measured. `TestProxy` and the group runner both call it, so timeout handling,
capability detection and error classification exist once. Two paths is how they
drift: one grows a policy the other never learns.

### 2.2 Bounded worker pool, one coordinator

```
jobs chan proxyNode ──► N workers ──► results chan measurementOutcome ──► coordinator
```

* **N workers exist**, not "a goroutine per node behind a semaphore". The bound
  is structural; there is no counter to get wrong and no thundering herd of
  parked goroutines.
* `N = min(api.GetPingTestAllConcurrency(), len(nodes))`, so the existing
  setting and its normalization rules are reused rather than duplicated.
* **Workers never emit.** They publish to `results`; the coordinator is the only
  emitter. That is what guarantees `completed` arrives as 1,2,3… regardless of
  which node finishes first, and it means the measurement store is written from
  one goroutine — no lock ordering question between the counters and the cache.

### 2.3 Run identity and supersede

Each run gets a monotonic `run_id`. Starting a new run cancels the previous one;
late results from a superseded run are recognised by run id and dropped. This is
the same class of problem as the existing `AutoLoadGeneration`, solved the same
way rather than with a new ad-hoc flag.

### 2.4 Delta progress events

`proxy_test_progress` carries one frame per transition:

| phase | payload |
|---|---|
| `started` | run_id, group, total |
| `node_started` | run_id, group, node |
| `result` | run_id, group, node, delay, status, error, counters |
| `finished` | run_id, group, counters |

Only deltas: a result frame names the single node that finished. Sending the full
node list per event would push megabytes over the IPC channel for a test that
changed one number.

`node_started` exists so only nodes actually occupying a worker show a spinner.
Without it the only options are "no feedback" or "every row spins", and the
latter misrepresents what the app is doing.

### 2.5 Measurement store

`proxyScopeState.Measurements map[string]ProxyMeasurementState` records delay,
status, error, `MeasuredAt`, generation, and a separate `LastSuccessDelay`.

* Stored **before** the event is emitted, so a re-read triggered by the event
  cannot see a value that has not landed yet.
* Scoped per `ScopeLocal`/`ScopeRemote`, so remote and local results cannot
  contaminate each other.
* **Runtime only** — never persisted. The core's own history still supplies
  initial values.

`proxies.go` overlays it on the engine's value: a value measured this session wins,
because the engine's snapshot is not a reliable echo of a test that just ran.

### 2.6 Capability, per action

```
ProxyActionCapabilities{ can_list, can_switch, can_test_single, can_test_group,
                         list_reason, switch_reason, test_reason }
```

Classic reports all three by construction — it drives the core's Clash-compatible
HTTP API, whose endpoints are part of the core's contract, not of this launcher's
generated client. Daemon answers from the runtime probe.

Reasons are stable tokens (`engine_lacks_rpc`, `core_stopped`, `unknown`), never
prose, so the UI localizes them and no screen parses an English sentence or
inspects the backend mode.

The sentinels were split (`ErrProxyListUnsupported`,
`ErrProxySwitchUnsupported`, `ErrProxyLatencyUnsupported`) and wrapped in a
`ProxyCapabilityError` carrying the action, so `errors.Is` and a structured
switch both work.

### 2.7 Context reaches the transport

`ProxyTransport.DelayContext(ctx, name)`; `Delay(name)` remains as a delegating
wrapper so the legacy Fyne targets and existing callers compile unchanged, and
there is still exactly **one** HTTP delay implementation
(`api.GetDelayContext`, with `api.GetDelay` delegating to it).

Two deadlines stay separate on purpose:

* **Per-node budget** (`GetPingTestTimeoutMs`, default 5000 ms) — "this node is
  too slow". Travels in the request (`timeout` query param / `Timeout` field).
* **Run context** — "this run is over". Cancels in-flight work on supersede,
  core stop, engine switch, or app quit.

Merging them would make a cancelled run indistinguishable from a slow node.

### 2.8 Failure policy

| condition | treatment |
|---|---|
| one node times out | that node's result; others continue |
| some nodes fail | run COMPLETES with `succeeded`/`failed` counters |
| all nodes fail | run COMPLETES with `succeeded=0`; UI says so |
| engine cannot measure | `can_test_group=false`; Test All disabled and explained |
| core stops / mode switches / app quits | run cancelled, no result written for unmeasured nodes |
| transport panics | that node reports "failed"; the process survives |

`context.Canceled` maps to `cancelled`, not `failed`: a stopped run did not judge
the node, and marking a whole group dead because the user pressed Stop would be a
lie.

---

## 3. Relationship to Mihomo

**Design inspiration:** Mihomo's concurrent URL-test behaviour — nodes tested
concurrently, each outcome recorded independently, outcome aggregated at group
level, and a group test that completes despite individual failures.

**Implementation:** independent, written against JijieBox's `ProxyTransport`.
Mihomo is GPL-3.0; no code was copied, and only the behavioural shape was taken.

**Deliberately NOT adopted:** tolerance-based switching, fastest-node selection,
fallback and load-balance strategies, provider health checks, `failedTimes`
auto-recovery, provider caching. Those are core *routing* policies. This launcher
only **measures**; it never changes the user's selected node as a side effect of
a test. That is a hard boundary, asserted by the absence of any switch call in
the scheduler.

---

## 4. Behaviour matrix

| scenario | list | switch | test | group result |
|---|---|---|---|---|
| Classic, core running | ✓ | ✓ | ✓ | completes, counters |
| Daemon (current), all RPCs | ✓ | ✓ | ✓ | completes, counters |
| Daemon, no `URLTestOutbound` | ✓ | ✓ | ✗ | Test All disabled, explained |
| Daemon, no `SelectOutbound` | ✓ | ✗ | ✓ | nodes measured, switching disabled |
| Daemon, no `GetGroups` | ✗ | ✗ | ✗ | explanation state (unchanged) |
| Core stopped | list unavailable | — | — | "start the core" |
| Partial failure | ✓ | ✓ | ✓ | `succeeded=N-k failed=k` |
| All failure | ✓ | ✓ | ✓ | `succeeded=0 failed=N` |
| Engine switch mid-run | — | — | — | run cancelled, stale results dropped |
| App quit mid-run | — | — | — | run cancelled, Quit not delayed |
| Platform sleep | — | — | — | run cancelled, unmeasured nodes keep prior state |

---

## 5. Verification

Deterministic tests with a fake transport; CI stays fully offline (no sing-box,
no real daemon, no network).

| property | test |
|---|---|
| bounded and concurrent | `TestGroupTestIsBoundedAndConcurrent` — **max in-flight**, not wall clock |
| partial failure completes | `TestGroupTestPartialFailureCompletes` |
| all failure completes | `TestGroupTestAllFailureIsACompletedRun` |
| event sequence | `TestProgressEventSequence` — 1 started, N results, 1 finished, completed 1..N |
| no reordering | `TestProgressIsNotReorderedByCompletion` |
| supersede | `TestSupersededRunStopsAndDoesNotWin` |
| cancellation | `TestCancellationCleansUp` — no active run left |
| measured value not discarded | `TestSingleMeasurementIsNotDiscarded` |
| reload preserves value | `TestMeasurementSurvivesReread` |
| prior success ≠ current result | `TestPriorSuccessIsKeptSeparateFromCurrentResult` |
| status classified, not string-matched | `TestMeasurementStatusIsClassifiedNotStringMatched` |
| classic/daemon parity | `TestClassicAndDaemonShareTheSameScheduling` |
| latency unsupported ≠ list unsupported | `TestLatencyUnsupportedIsIndependentOfListing` |
| switch unsupported ≠ test unsupported | `TestSwitchUnsupportedIsIndependentOfTest` |
| sentinels stay distinct | `TestSentinelSplitKeepsTheActionsDistinct` |
| no capability claimed before probe | `TestCapabilitiesUnknownBeforeProbe` |

Concurrency is asserted by **maximum observed in-flight**, not by duration: a
duration threshold passes on a fast machine and flakes on a busy CI runner.

`go test -race` on the scheduler, store and coordinator: **clean**.

---

## 6. Performance

Measured with 40 nodes at 100 ms each (fake transport):

```
serial theoretical : 4.00s
bounded actual     : 202ms      (maxInFlight = 20)
speedup            : 19.8x
```

The bad-node case improves more, because the budget is no longer paid serially:
40 nodes with 20 timeouts at 5 s each is ~2 batches (~10 s) instead of ~100 s.

Recorded for the audit only — not asserted in CI, where wall-clock thresholds
are a known source of flakes.

---

## 7. Honest caveats

- **The 19.8× figure is a fake-transport benchmark.** It measures the scheduler,
  not a real network. Real speedup is bounded by the core's own parallelism and
  by how many nodes are genuinely slow.
- **Auto Ping after connect was not re-routed through this scheduler.** It is a
  separate legacy path; leaving it alone keeps this change's blast radius at the
  proxy screen. The measurement primitive is shared where the new flow is used,
  but Auto Ping and Test All are not yet single-flighted against each other.
- **No explicit Cancel button.** Supersede, core stop, mode switch and quit all
  cancel, which covers the specified cases; a user-facing Cancel is not built.
- **The remote-daemon transport is not exercised end to end** — it has no UI
  entry point (`capabilities.Remote` is false). It implements the new interface
  so it still compiles, and its cancellation merge is untested at runtime.
- **`MeasuredAt` is recorded but not displayed.** The data is there for a future
  freshness indicator; the UI shows nothing time-based yet.
- **No manual GUI walkthrough.** The progress text, per-row updates and spinner
  behaviour are verified by build plus the state-machine tests, not by eye.
- **The Chinese copy was not reviewed by a native speaker.**
- **Where the group snapshot's node order conflicts with a measurement**, the
  engine's order is preserved and the measurement is overlaid by name; a node
  present in the store but absent from the engine's list is simply not shown.
