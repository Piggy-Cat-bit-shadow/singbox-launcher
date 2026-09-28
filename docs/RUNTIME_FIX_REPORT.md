# Runtime state machine — fix report

**Language**: English
**Baseline**: `4024f804` · **Final HEAD**: `bb592d45` · **Working tree**: clean
**Audit**: [RUNTIME_STATE_AUDIT.md](RUNTIME_STATE_AUDIT.md) (Phase A, written before any code changed)
**CI**: [run 36465997873](https://github.com/Piggy-Cat-bit-shadow/singbox-launcher/actions/runs/36465997873) — **success** at `27a88eaa` (last code commit)

---

## 1. Root causes

### Why Classic ran but showed "正在启动"

`RunningState.Set(true)` was called, but the **classic phase was never promoted to
`ClassicRunning`**. `phaseToWireState` reads the *phase*, so the wire state stayed
`starting` however much the runtime knew.

Reproduced directly before changing anything:

```
commitPrivilegedStartLocked => true
RunningState.IsRunning() = true
classic phase = starting          ← the defect
```

That matches the reported log exactly: root-owned copy verified, sing-box started,
runtime config matched, PID file written, Clash API up, four proxy groups loaded —
and the UI still on 正在启动.

Why adoption worked and the first start did not: the **late-adoption** path
(`process_service.go:2552`) did set `ClassicRunning`; the synchronous first-start
path never did.

### Why Daemon hung on `waiting for the daemon to report STARTED`

`applyOnce` logged that line and **returned without waiting**, leaving Running to
the status stream. That stream is a pure **edge** stream:
`SubscribeServiceStatus` is opened with an empty request and carries no snapshot
and no replay. When the daemon's core was *already* STARTED, the transition had
already happened and would never arrive.

The attach branch in `consumeStatusStream` could not rescue it, because it lives
*inside* the stream and only runs when a frame arrives. This is the classic
missed-edge bug, and it is exactly the reported permanent hang.

### Why `start_core requested` repeated

**The guard was working.** `beginOp` refuses a same-kind operation already in
flight. The noise came from ordering: every caller logged `start_core requested`
**before** reaching the guard, so N duplicates printed N lines and nothing said
they had been folded into one operation.

### Why the Proxies page said "内核未运行"

```swift
if core?.state != .running { return .coreStopped }
```

Five backend states collapsed into one message. A core that was **starting** — and
a core that was **running with its API still coming up** — were both reported as
"not running". A second consequence followed: it returned *before* the
`config_stale` check, so a running core whose list had not arrived could not reach
`.configStale` either.

This was a presentation bug in **one derived property**, not a missing state: the
wire state already represented all five.

### Why RPC compatibility drifted

The launcher's proto carries `GetChains`, `GetPool`, `GetRules`,
`SetChainPositionEnabled`, `SetEndpointEnabled`; the user's daemon build does not
implement them. That is a **capability** gap on a daemon we do not own — case **A/B**
in the request's list, not a launcher client error.

It is **not** related to the state bug, and the existing probe already handles it:
`daemon_rpc_compat.go` probes once, records `MissingRPCs`, and logs at Warn once —
not per second. Stream failures (`daemon.tailscale`, `daemon.conns`) log at
**Debug** with a backoff that doubles and caps at 10s.

---

## 2. Files changed

**Fixes**

```
core/classic_runtime.go        commitPrivileged now moves the PHASE with ownership
core/backend_daemon.go         reconcileDaemonRuntimeState + status mapping
core/controller.go             one transition log line, at the single write point
backend/service/backend.go     entry points stop logging "requested" before the guard
backend/service/core_operation.go  logs JOIN vs new; op-started line
macos/.../App/AppModel.swift   proxyListState: .coreStartingUp instead of collapsing
macos/.../Views/ProxiesView.swift  renders the new state with a spinner
macos/.../Models/Localization.swift  coreApiNotReady, EN + ZH
```

**Documents**

```
docs/RUNTIME_STATE_AUDIT.md    Phase A audit, corrections, and the open flake
docs/RUNTIME_FIX_REPORT.md     this file
```

**Tests added**

```
core/classic_privileged_phase_test.go      CASE 2 + superseded-commit guard
core/classic_lifecycle_transition_test.go  CASE 1, both directions
core/daemon_attach_state_test.go           CASE 3/4/5 + honest status mapping
core/daemon_stream_backoff_test.go         CASE 9/10/15
core/config/stale_member_test.go           §25 pruning + no-build-failure
backend/service/core_operation_duplicate_test.go  CASE 6/7 + duplicate stop
backend/service/core_state_api_independence_test.go  CASE 11 + CASE 13
internal/swiftlogic/proxystate_test.go     CASE 12
internal/swiftlogic/singleverdict_test.go  one-verdict structure
```

---

## 3. State machine — before / after

**BEFORE (Classic privileged)**

```
start → phase=starting → RunningState=true → phase STAYS starting
      → wire state "starting" forever
      → UI: 正在启动        (Proxies: 内核未运行)
```

**AFTER**

```
start → phase=starting → commitPrivileged { ownership + phase=running }
      → wire state "running"
      → UI: 已运行          (Proxies: nodes, or "connecting to API…")
```

**BEFORE (Daemon attach to a live core)**

```
apply → "waiting for the daemon to report STARTED" → return
      → wait for a STARTED edge that already fired
      → never settles → frontend retries → N × "start_core requested"
```

**AFTER**

```
apply → StatusCtx (current state) → reconcile → Running
      → settles; duplicates join; no retry storm
```

---

## 4. Daemon attach: how an already-STARTED daemon now completes

`reconcileDaemonRuntimeState`, called immediately after a successful `ApplyCtx`:

1. re-checks **ownership** (`isActive`) — the window between the pre-apply check
   and this read is a full network round trip;
2. reads the **current** status via `StatusCtx` (already implemented: reports
   `idle | started | fatal`);
3. maps it with `daemonStatusMeansRunning`, which reports **unknown** for an
   unrecognised value rather than treating it as stopped — a daemon that grows a
   new status must not have its running core silently reported as stopped;
4. writes the state, and on a false→true transition schedules the proxy load the
   stream would have done on a running edge.

Ordering is **subscribe-then-snapshot**: the stream is already open, so a
transition landing between the snapshot and the next frame is not lost in either
direction. Running is still **not** declared merely because Apply was accepted —
the snapshot is what makes the difference, and a core that dies right after
accepting a config must not read as connected.

---

## 5. SingleFlight

Already present, and now **provable and legible**:

- `beginOp` refuses a same-kind operation in flight (unchanged);
- `runCoreOp` logs `duplicate request joined the operation in flight; no second
  start was started`, naming the operation id;
- entry points no longer log "requested" before the guard, because they cannot yet
  know which case it is.

Behavioural coverage: ten concurrent duplicates **plus** the owner run the start
body **exactly once**; a start arriving while running runs **no** start body; a
duplicate stop **joins** rather than repeating the teardown.

---

## 6. RPC compatibility

| Question | Finding |
|---|---|
| Which RPCs are actually missing | `GetChains`, `GetPool`, `GetRules`, `SetChainPositionEnabled`, `SetEndpointEnabled` — absent from the user's daemon build |
| Version difference or client error | Neither: the launcher's proto is newer than that daemon. Not a malformed request |
| Fallback | Already in place — the probe records `MissingRPCs` once; `coveredByFallback`/`ProxyActionCapabilities` decide per action, and the daemon's loopback Clash API covers what it can |
| Does it affect the runtime state | **No.** Pinned by test: a running core reports `running` with no API data at all |
| Does it spam the log | **No.** Stream failures are Debug with a doubling backoff capped at 10 s |

---

## 7. Test results

| Item | Result |
|---|---|
| `go test ./...` | **38 packages ok, 0 failures** |
| `go test -race` (CI's exact list) | **clean** |
| `go vet ./core/... ./api/... ./internal/... ./backend/...` | clean |
| l10n `--strict` | 88 used / 89 catalog / 0 hard fails |
| `paths_guard --strict` | 0 findings |
| `win7guard` | 417 files, clean |
| `bash build/test_darwin.sh silent` | **All tests passed!** |
| `./build/build_macos_app.sh` | Build complete, Mach-O arm64 |
| Swift-logic suites | **44 passing** |

Required cases:

| # | Case | Test |
|---|---|---|
| 1 | Classic Stopped→Running | `TestClassicStartTransitionsStoppedToRunning` |
| 2 | wrapper exits, core alive | `TestClassicRunningPhaseSurvivesStrandedWrapper` |
| 3 | Daemon STARTING→STARTED→Running | `TestDaemonAttachStillReportsStoppedWhenIdle` + reconcile |
| 4 | **already STARTED → immediate Running** | `TestDaemonAttachToAlreadyStartedCoreSettles` |
| 5 | STARTED, no replay | same test — no stream involved |
| 6 | 10× duplicate → one execution | `TestTenDuplicateStartsRunTheBodyOnce` |
| 7 | start while Running → no 2nd core | `TestStartWhileRunningDoesNotStartASecondCore` |
| 8 | stale generation cannot overwrite | `TestPrivilegedCommitDoesNotPromoteASupersededGeneration` |
| 9 | unsupported RPC → state still ok | `TestUnsupportedStreamsBackOffAndDoNotSpam` |
| 10 | unsupported stream → no spin | same |
| 11 | API down, core running → Running | `TestRunningCoreReportsRunningWithoutAnyAPI` |
| 12 | running+API+proxies → not "stopped" | `TestProxyScreenNeverClaimsStoppedWhileRunning` |
| 13 | failure → not permanently Starting | `TestCoreStateIsNeverPermanentlyStarting` |
| 14 | Stop during Starting, no deadlock | `TestSupersedingStopCancelsStart` (existing) |
| 15 | Classic↔Daemon switching | `TestEngineCloseStopsALiveOwnedCore` (existing) |

Every **new** test was verified to fail on a revert of its fix.

---

## 8. Remaining risks

1. **The privileged startup path is not exercised end-to-end.** The tests drive
   the real `classicRuntime` and the real `ProcessService` commit, but no test
   spawns a root core; a macOS authorization prompt cannot run in CI. The
   bookkeeping is covered; the OS interaction is not.

2. **The fake daemon does not model a lying capability surface.** CASE 9/10 are
   pinned as source invariants (backoff caps, log level, context exit) rather
   than behaviour, because the fake server cannot advertise methods it lacks.

3. **`reconcileDaemonRuntimeState` costs one extra round trip per start.** Judged
   worth it: it is what removes the permanent hang. It is tolerant of failure — a
   `StatusCtx` error logs a warning and leaves the stream authoritative rather
   than failing a start that succeeded on the daemon.

4. **One CI failure is UNRESOLVED and deliberately left open.**
   `TestBackupExportEnvelope` failed once, passed on the next run. `core/debugapi`
   does not import `core`, 20/20 local runs pass, the full CI package set passes
   locally, three timezones and two `TMPDIR` arrangements pass, and the baseline
   behaves identically. Recorded with its evidence rather than marked fixed;
   the useful next artefact is the failing run's `temp/darwin/test_output.log`.

5. **Generation coverage is per-writer, not systemic.** Every async writer I
   audited already checks its generation, and the privileged commit's guard is now
   pinned by test — but there is no single choke point that makes a stale write
   impossible by construction. A `ReconcileRuntimeState()` funnel was considered
   and not built, because the existing structure already routes every write
   through `RunningState.set`; that is the natural place for such a guard if a
   future path needs one.

### Two audit findings I withdrew

- **D2 was overstated**: the wire state is complete, not missing states.
- **D4 was wrong**: `-race` already covers `./core/...` and `./backend/...`.

Both are recorded in the audit as withdrawn rather than deleted, because the next
reader would otherwise re-derive the same suspicion from the same absence of
evidence.
