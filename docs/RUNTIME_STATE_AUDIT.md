# Runtime state machine audit — Classic / Daemon lifecycle and UI sync

**Language**: English
**Baseline**: `4024f804e971c583a0b03a04ddda7b73e345bba6`
**Scope**: every source of "is the core running", the Classic and Daemon start
paths, daemon attach, duplicate `start_core`, RPC capability drift, and the
Proxies screen's verdict.

Every claim below is anchored to a file and line in the shipping source. Where a
suspected defect turned out **not** to exist, that is recorded too — a report that
only lists confirmed bugs cannot be used to decide what still needs watching.

---

## 1. What the lifecycle architecture actually is

```
UI (SwiftUI AppModel)
  → coreOperationBusy / pending marker / awaitCoreSettled
  → IPC (backend/protocol)
  → backend/service/backend.go   runCoreOp (single-flight, settle)
  → core/backend.go   AppController.StartVPN
       ├─ classic: core/process_service.go  Start → startSingBox → startSingBoxPrivileged
       └─ daemon:  core/backend_daemon.go   applyCurrentConfig → applyOnce → ApplyCtx
  → core/controller.go  RunningState  (ONE bool)
  → events.EventBus  VpnStateChanged → IPC event → UI snapshot
```

## 2. Classic start call chain

`StartContext` → rebuild config → TUN check → **privileged** or **direct** spawn.

**Privileged (macOS TUN)** — `process_service.go`:

```
startSingBoxPrivileged(gen)
  gate()                      root-owned copy consistency  (kept)
  deps.start()                platform.StartPrivilegedCore → (scriptPID, singboxPID)
  commitPrivilegedStartLocked → ac.classic.commitPrivileged(...)
                              → ac.RunningState.Set(true)      ← line 918
  writePIDFile(script, singbox)
  go waitExit(scriptPID) → onPrivilegedScriptExited(gen)
  go delayedAutoLoadProxies(gen)
```

**The suspected early return does NOT exist.** Line 592 comments
"startSingBoxPrivileged commits RunningState itself on success", and line 918 does
exactly that. `Set(true)` is reached on the success path.

**Direct (non-TUN)** — line 688 `ac.RunningState.Set(true)`.

## 3. Daemon start call chain

```
StoreCoreOperation → runCoreOp("start")
  → StartCore → runCoreOp → ac.StartVPNContext
  → DaemonBackend.applyCurrentConfigContext → applyOnce
      rebuild → isActive gate → read config.json → pre-launch gate (new)
      → admin.StatusCtx (reachability) → prepareDaemonConfig
      → isActive gate → admin.ApplyCtx
      → clashFallback.setConfigured / expectedGroups
      → log "config applied; waiting for the daemon to report STARTED"
      → return false, nil            ← NO WAIT HAPPENS
```

Running is then published **only** by `consumeStatusStream`, which reacts to a
`ServiceStatus_STARTED` frame.

## 4. Every source of running state

| Source | Type | Location | Authority |
|---|---|---|---|
| `RunningState.running` | `bool` | `core/controller.go:217` | **the** value the UI reads |
| `classic.phase` | `ClassicPhase` | `core/` | start/stop phase, per generation |
| `ClassicGeneration` | `uint64` | `core/` | stale-write guard |
| `Svcb.ProcessState` | OS | direct spawn only | exit detection |
| `SingboxPrivilegedSingboxPID` | int | `controller.go` | privileged core PID |
| PID file | file | `bin/*.pid` | diagnostics |
| `ProxyActionCapabilities` | struct | daemon compat | per-action ability |
| `b.caps` | struct | `daemon_rpc_compat.go` | RPC presence |
| `coreOpState.op` | struct | `backend/service/core_operation.go` | in-flight op |
| `AppModel.pending` | enum | SwiftUI | UI spinner |
| `AppModel.appliedSeq` | uint64 | SwiftUI | stale-snapshot guard |
| `DaemonStatus.ready/reachable` | bool | protocol | daemon transport |

**Conclusion: there is ONE authoritative running flag (`RunningState`)**, not four.
The defect is not competing truth sources. It is that `RunningState` has only
**two values** — so "starting" cannot be represented — and that the daemon path
never *sets* it on attach.

## 5. Duplicate `start_core` — root cause

`backend/service/core_operation.go:152` `beginOp` **already** implements
single-flight: a same-kind operation in flight logs
`already in flight (id=N) — ignoring duplicate request` and returns
`(op, false)`; `runCoreOp:667` then returns `nil`.

So the backend does **not** run concurrent starts. What the log shows is a
different thing:

> `backend: start_core requested` is logged at `backend.go:1207` **before**
> `runCoreOp` reaches the guard.

A burst therefore means the **frontend issued several commands**, and each
printed its line before being folded into one operation. The guard is working;
the noise is upstream. The upstream cause is defect D1 (below): the daemon start
never settles, so the UI keeps believing the start did not happen.

## 6. Confirmed defects

### D1 — Daemon attach to an already-STARTED core can never settle

`applyOnce` ends at the log line `waiting for the daemon to report STARTED` and
returns `false, nil`. It does **not** wait, and it does **not** query the current
state. Running is published only by `consumeStatusStream` on a **transition** to
`STARTED`.

`SubscribeServiceStatus` is a pure **edge** stream: `superviseStatus` opens it
with `&emptypb.Empty{}` and never asks for a snapshot or a replay. So when the
daemon's core is **already** STARTED at subscribe time, the transition that would
publish Running has already happened and is never delivered.

This is the classic **missed edge / lost transition**: subscribe to future
changes, then rely on a change that already occurred. It matches the reported log
exactly — config applied, core running, `attaching without restart`, then
permanent `waiting for the daemon to report STARTED`.

The attach branch at `backend_daemon.go:1377` is inside `consumeStatusStream`
itself (`if running && !wasRunning`), so it only runs when a frame arrives. It
cannot rescue the case it names.

**Available fix input**: `client.StatusCtx()` returns `StatusInfo` with
`Status string` (`idle | started | fatal`) — a synchronous snapshot, already
implemented and already used for reachability. Nothing new is needed on the wire.

### D2 — `Starting` is not representable in the authoritative state

`RunningState.running` is a `bool` with `Set(bool)`, `SetStopped(reason)` and
`SetReasserted()`. There is no `Starting` or `Stopping`.

The UI infers those from `pending`/`coreOperationBusy`, so:
- the authoritative state cannot say "starting";
- a start that never settles leaves the UI in a state the backend cannot
  contradict;
- the Proxies screen, which reads a different projection, concludes "not running"
  and renders **"内核未运行"** while the core is up.

That last one is the reported Proxies defect and it follows directly from this.

### D3 — `finishOp` discards the settlement reason

`core_operation.go:338` — on a **stale** result it returns `settleStale`; `runCoreOp:693`
maps that to a silent `return nil`. The caller cannot distinguish
"settled successfully" from "discarded", so a superseded start looks identical to
a completed one. This is the same class as D1: an operation that never reports,
and therefore never releases the UI.

### D4 — `-race` is not run over the lifecycle packages in the fast path

`ci.yml` runs `go test -race` on a package list; `macos.yml` runs it on a shorter
one. Neither is obviously the superset, and the lifecycle code
(`core`, `backend/service`) is exactly where an unsynchronised state write would
hide. Not a runtime bug — a coverage gap in the evidence for "no race".

## 7. Suspected defects that are NOT real

Recorded because each was investigated and each would otherwise be re-reported:

| Suspicion | Finding |
|---|---|
| Privileged TUN branch returns before committing Running | **False.** `commitPrivilegedStartLocked` → `RunningState.Set(true)` (line 918). |
| Wrapper/script PID used as the core's lifetime | **False.** Both are tracked separately (`SingboxPrivilegedPID` = script, `SingboxPrivilegedSingboxPID` = core) and the PID file carries both. |
| Concurrent `start_core` produces two cores | **False.** `beginOp` single-flight refuses the duplicate. |
| `/admin/apply` marks running before the core is up | **False.** `backend_daemon.go:653` documents the deliberate removal of exactly that. |
| Generation guard missing on late async writes | **False.** `onPrivilegedScriptExited`, `delayedAutoLoadProxies`, `EndDaemonStop` and `retryAfterCoreFatal` all check generation or backend identity before writing. |
| RPC capability drift breaks the runtime state | **No.** `daemon_rpc_compat.go` probes once and records `MissingRPCs`; the missing methods are optional capabilities (`GetChains`, `GetPool`, `GetRules`, `SetChainPositionEnabled`, `SetEndpointEnabled`) and the probe logs them once, not per second. |

The RPC drift is real but is a **capability** gap, not a state-machine fault: the
launcher's proto carries methods the user's daemon build does not implement. It
must stay degraded gracefully, which the existing probe already does.

## 8. Plan

1. **D1** — reconcile the daemon's current state after `Apply`, instead of relying
   on a future edge. Subscribe-then-snapshot-then-reconcile.
2. **D2** — make the authoritative state able to say `Starting`/`Stopping`, and
   derive the Proxies screen's verdict from it rather than from the proxy list.
3. **D3** — surface settlement (settled vs superseded vs joined) so a caller can
   tell the difference, and log duplicates as *joined* rather than as new requests.
4. **D4** — add the lifecycle packages to the race run and cover the fifteen
   required cases.
