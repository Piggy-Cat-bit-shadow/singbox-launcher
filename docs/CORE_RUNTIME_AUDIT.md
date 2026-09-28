# Core Runtime Audit — Classic lifecycle, Daemon, config generation, SwiftUI IPC

**Language**: English · [Русский](CORE_RUNTIME_AUDIT.ru.md)

A second full pass over the runtime that decides whether the VPN is up: the
Classic (privileged child process) lifecycle, the Daemon engine, the config
builder, and the IPC that carries all of it to SwiftUI. The starting point was a
real incident — the daemon applied a config and the core died with
`default outbound not found: proxy-out` — and the audit was scoped to the whole
CLASS of defects that produced it, not the single line.

The rule applied throughout: a race is not fixed by adding a flag or a `sleep`,
and a lifecycle error is not fixed by letting the UI guess. Where the old code
had special cases, an invariant replaced them.

---

## 1. Numbers

```
Runtime surfaces audited:                 6   (Classic spawn, Classic stop,
                                               termination, Daemon apply,
                                               config build, IPC/state)
Confirmed root-cause categories:          8
  P0 (breaks the VPN or lies about it):   6
  P1 (wrong behaviour / no recovery):     2

New source files:                         5
New test files:                           4
Regression tests added:                  ~80
Files changed:                           31
```

All P0 and P1 categories below are fixed and covered by tests that were verified
non-vacuous: disabling the fix makes the test fail with the original symptom.

---

## 2. The incident, and the class it belongs to

The daemon reported `core FATAL: default outbound not found: proxy-out`. The
on-disk `config.json` had `route.final = "proxy-out"` while its ten outbound tags
contained no such outbound: the template variable `route_final` still carried a
literal default of `proxy-out`, and the state no longer produced a group with that
tag.

The narrow reading is "one config builder emitted one stale tag". The actual
class is broader:

> The runtime could hold a configuration whose internal references do not
> resolve, and NOTHING in the pipeline was responsible for noticing.

`CleanDanglingOutboundsInRouteRules` rewrote `route.rules[*].outbound` and used
`route.final` only as a fallback source — it never validated `route.final`
itself. So a dangling final target was structurally invisible to every check the
launcher performed, right up to the moment the core refused it.

### Fix — one reference-integrity validator (`core/build/ref_integrity.go`)

A single pass over the assembled config resolves every reference the core will
resolve:

| Reference | Checked against |
|---|---|
| `outbounds[*].detour` | outbound tags |
| selector / urltest / chain members | outbound tags |
| `route.rules[*].outbound` | outbound tags |
| `route.final` | outbound tags |
| `dns.servers[*].detour` | outbound tags |
| `dns.rules[*].server` | DNS server tags |
| `route.default_domain_resolver` | DNS server tags |
| `route.rules[*].rule_set` | declared rule-sets |

`route.final` is additionally *repaired* when it dangles, by choosing the first
declared group that can carry traffic, then a direct outbound, then refusing. The
choice is by declaration order, not map order, so the result is deterministic —
asserted by running the repair thirty times and requiring the same target.

Design points worth stating, because each one was a false positive that had to be
eliminated before the validator could be trusted:

- `domain_resolver` is a **DNS server** tag, not an outbound. It is validated in a
  second pass against server tags, which also makes forward references legal.
- Validation is skipped for preview builds, mirroring the existing
  `CleanDanglingOutboundsInRouteRules` skip: a preview is a draft of an incomplete
  config, and failing it would block the wizard.
- A config with **no** outbounds is not an error. The lx-only template is a real,
  valid shape and has no outbound tags to reference.

The validator decodes through `jsonc.ToJSON`, not `encoding/json`. This is not
incidental: every generated config contains `//` comments, so a plain JSON decode
fails on every real config — the first version of this code did exactly that.

Wired into `core/build/build.go` as step 4, after assembly. A dangling reference
is now an `ErrInvalidInputs`, which means it is caught at BUILD time rather than
by the core.

---

## 3. Classic lifecycle — ownership by generation

### 3.1 The model

The old lifecycle was a set of independent booleans (`RunningState`,
`StoppedByUser`, `RestartRequestedByUser`, `SingboxPrivilegedMode`, …) that
different goroutines read and wrote at different times. Every race in this area
was a consequence of that: a stale goroutine could not tell whether "the runtime"
still meant the runtime it was started for.

`core/classic_runtime.go` replaces it with a single mutex-guarded state:

```
                      beginOperation(ClassicStarting)
        ┌──────────┐ ──────────────────────────────▶ ┌───────────┐
        │ stopped  │                               │ starting  │
        └──────────┘ ◀──────────────────────────── │           │
             ▲            early exit / failure     └───────────┘
             │                                        │  ▲
             │  terminate confirmed                   │  │ readiness window
             │                                        ▼  │ survived
        ┌──────────┐ ◀──────────────────────────── ┌───────────┐
        │ stopping │ ───── superseded ───────────▶ │  running  │
        └──────────┘                               └───────────┘
             │
             ▼
        ┌──────────┐
        │  failed  │
        └──────────┘
```

Every operation takes a **generation**. `isCurrent(gen)` gates each state write,
so a goroutine that has been superseded cannot change the phase, clear ownership,
or touch the crash counter. `beginOperation` refuses a start while one is in
flight, which is the invariant that makes concurrent starts impossible by
construction rather than by convention.

Two subtleties that the tests pinned down:

- The **zero value** of `ClassicPhase` must count as settled. It is the phase of a
  runtime that has never run, and treating it as busy refused the first start of
  the process's life.
- `wireState` maps the zero value to `"stopped"`, so a never-started runtime
  reports stopped rather than an empty string the frontend cannot decode.

### 3.2 Termination with confirmation (`core/terminate.go`)

`KillPrivilegedProcess` sent `-TERM`, deleted the PID file, and returned `nil` —
without ever confirming the process had exited. A `Stop` could therefore report
success while the core was still running and still holding the TUN.

The replacement is one primitive used by every kill path:

```
signal TERM ──▶ wait up to 5s ──▶ still alive? ──▶ signal KILL ──▶ wait up to 3s
     │                                   │                              │
     └── confirmed exit ─────────────────┘                              │
                                                                        ▼
                                              confirmed exit  /  ERROR: still alive
```

Every probe verifies the process's **executable path**, not just that the PID
exists. A recycled PID is reported as already gone rather than signalled: killing
whatever inherited the number is the worst possible outcome, and the audit found
this hazard in every one of the kill paths.

### 3.3 Readiness (`§15`)

`exec.Start` succeeding means a process exists, not that a VPN is up. The old code
promoted the state to `running` immediately, so a core that rejected its config
produced a "Connected" flash and then "Stopped" — read by the user as a glitch,
not as a failure with a cause — and the exit was handled by the crash monitor, so
a first-run config error was classified as a crash and auto-restarted.

The phase now stays `starting` until the process survives a one-second readiness
window, and a death inside that window is reported as a **start failure** with its
classified reason. The window is not a sleep standing in for a readiness probe: it
is the shortest interval that separates "still coming up" (a rejecting core exits
in tens of milliseconds) from "already dead", and the authoritative signal remains
the process's own exit. An unreadable process table assumes *alive*, because
reporting a death that did not happen is worse than reporting it late.

### 3.4 Adoption (`§9`)

Taking over a core from a previous session recorded its PID and reported it
running. An adopted process is not our child: there is no `exec.Cmd` to `Wait` on,
so nothing reported its exit, and the UI kept saying "running" for a process that
no longer existed while Stop and Restart both aimed at a ghost.

`watchAdoptedCore` polls the process's existence, identity-verified on every tick,
and corrects the state on death. It is deliberately not a fabricated `Wait`:
macOS cannot wait on a non-child, and a fake `Cmd` would break the first time it
was used. Adoption also now **refuses** a PID whose executable cannot be verified,
since recording ownership without one would let a later `Stop` signal a stranger.

---

## 4. Config generation

### 4.1 Pipeline

```
  state.json ─┐
              ├─▶ build snapshot ─▶ assemble config ─▶ finalizeReferences
  template  ──┘                          │                    │
                                         │                    ├─ dangling? ─▶ repair route.final
                                         │                    └─ else ─────▶ ErrInvalidInputs
                                         ▼
                              write candidate (.candidate)
                                         │
                                    sing-box check
                                    ┌────┴────┐
                                 reject     accept
                                    │          │
                    keep config.json as-is    atomic promote
```

`finalizeReferences` is skipped for previews, matching the existing behaviour for
route-rule cleanup.

### 4.2 Build serialization (`§31`)

A rebuild is a read-modify-write on ONE file. Nothing serialized it, so a rebuild
from a settings change could interleave with the rebuild a Start performs, and the
loser's candidate could be promoted after the winner's — leaving on disk a config
rendered from an already-stale state snapshot. The daemon applies whatever is on
disk, so the damage was real and silent.

`buildMu` now serializes builds. It is a real mutex rather than a busy flag: the
second caller **waits** for a correct config instead of being refused one, because
refusing would make "change a setting, press Start" fail for no reason the user
could see. It is taken first and never held while another launcher lock is
acquired, so it cannot invert with `CmdMutex`, and the existing
"cache incomplete → Update" re-entrancy is safe because that path already passes
`triggerRebuild=false`.

### 4.3 Preflight

Verified already present and correct: a candidate is written to
`config.json.candidate`, checked by `sing-box check`, and only then promoted. An
invalid candidate leaves the working config untouched, and the candidate is
removed. `TestRejectLoopNoPromoteLeavesConfigUntouched` pins all three.

---

## 5. Daemon

The daemon engine's apply path already had the right shape — `applyMu`, a
structured error returned rather than shown — but two paths defeated it.

**A FATAL that cannot be resolved was swallowed.** When the daemon reported a core
FATAL, `retryAfterCoreFatal` tried to disable the node the core named and retried.
When that declined — the retry cap was reached, or the FATAL named something that
is not a node, which is exactly the `proxy-out` case — the function simply
returned. Nothing recorded, nothing shown, core down. The user pressed Connect and
the app went quiet. The FATAL is now recorded with the daemon's own message
verbatim, because that message is the only place the true cause exists.

**A failed stop was reported only to a dialog nobody was watching.** `StopVPN`
routed its error to the Fyne UI port, which the headless backend never has. A stop
that failed — the tunnel still up — reached nobody.

### Handover

```
   Classic ──switch──▶ Daemon          Daemon ──switch──▶ Classic
   ─────────────────────────           ─────────────────────────
   1. refuse if !ClassicSettled()      1. refuse if !ClassicSettled()
   2. renewGeneration()                2. renewGeneration()
        (abandons the old runtime's         (stale watchers stop)
         watchers and ownership)        3. setBackend(classic)
   3. setBackend(daemon)
```

`renewGeneration` BEFORE the backend swap is what makes the handover atomic from
the lifecycle's point of view: after it, no goroutine belonging to the old engine
can write state, so there is no window in which two engines both believe they own
the runtime. A switch is refused while a start, stop or restart is in progress
rather than queued, because there is no correct way to hand over mid-operation.

---

## 6. IPC and the SwiftUI client

### 6.1 One error source (`§12`, `§25`, `§28`)

The macOS app talks to a **headless** backend. Almost every core failure was
reported only to the Fyne UI port, and with no port attached `showErrorUI` was
log-only, `showDeterministicExitDialog` returned immediately, and the
privileged-copy dialog returned before saying anything. The SwiftUI client was
structurally incapable of learning that a start had failed.

One store now holds it — `(code, operation, message, detail, recoverable,
config_error)` — read by `coreState()` and therefore delivered both live and in
the snapshot, so a failure survives a GUI restart instead of vanishing with the
notification. Producers: the rebuild path, `showErrorUI` (covering every existing
caller), the deterministic-exit and restart-exhausted paths, the privileged-copy
gate, the daemon FATAL, and the daemon stop.

Retryability is decided at the source rather than guessed by the UI. An occupied
port, missing permissions, a cancelled authorization and a refused config are
deterministic and are **not** advertised as retryable: telling a user to retry a
failure that will always recur is what makes them click Start in a loop.

### 6.2 Pending converges on the backend (`§13`, `§14`, `§32`)

```
   OLD ────────────────────────────────────────────────────────
   click Start ─▶ IPC reply (ACCEPTED) ─▶ pending = nil
                                          ↑ spinner gone, button
                                            back to "Start"
   ...core still rebuilding / authorizing / applying...

   NEW ────────────────────────────────────────────────────────
   click Start ─▶ IPC reply ─▶ pending HELD
                                 │
                    core_state_changed (settled) ─▶ pending = nil
                                 │
                    deadline (75s) ─▶ pending = nil
```

The IPC reply means *accepted*, not *finished*. Clearing the marker there is
precisely the "I pressed Start and it reverted to Start" report. A rejected
command still clears immediately and shows the reason, so a failed operation never
leaves the UI spinning.

### 6.3 Actionable text (`§33`)

Each failure code maps to a sentence saying what to DO — an occupied port and a
missing protected copy need different actions from the user. Config failures
additionally state the fact that decides the user's situation: *the previous
working config is still in use*. That reassurance is gated on the backend's
`config_error` flag, because after a spawn failure it would be untrue.

---

## 7. Defects found in the fixes themselves

Writing the regression matrix surfaced three bugs in the new code. They are
recorded here rather than quietly fixed, because the second pass exists to find
them.

1. **The zero `ClassicPhase` counted as busy**, so the first start of the
   process's life was refused.
2. **`onPrivilegedScriptExited` returned while `CmdMutex` was released**, with its
   deferred `Unlock` still armed — unlocking a mutex the goroutine did not hold,
   which Go treats as a *fatal runtime error*, not a recoverable one.
3. **`CoreLogPath` and the Linux capability probe dereferenced `FileService`
   without a check.** Both run on background goroutines, where a partially
   constructed controller turned a startup race into a process-wide crash.

---

## 7a. Third pass — adversarial re-audit of the state machine

The second pass concluded that lifecycle paths were sound. That conclusion was too
generous, and the way it was wrong is worth recording: the state machine's own
bookkeeping was correct, but the **invariant failed at the boundaries it does not
own** — process ownership, the running-state belief, and the runtime transitions it
consumes.

An adversarial pass was run against the shipped code with the instruction to
disprove it. It found five real defects, all reproduced from the source before
being fixed, each now pinned by a test that fails when the fix is reverted:

1. **An engine released a live core.** `SwitchBackendMode` required
   `!RunningState.IsRunning()`, which reads like "no core is running" and is not:
   the crash path clears that flag when the exit is OBSERVED, and a process can
   outlive the observation. `Close()` then only renewed the generation —
   bookkeeping — so switching after a crash left a live classic core holding the
   TUN while the daemon's core started beside it. Two engines, one VPN.
2. **A retired backend repaired the config it no longer owned.**
   `retryAfterCoreFatal` called `retryCoreReject` — which persists a node as
   disabled — before checking `isActive()`, so a status frame arriving after an
   engine switch still removed a node from the user's working set.
3. **A stop trusted a single event.** `Running == false` is produced by a user
   stop, a restart's teardown and a crash, and they need opposite responses. A
   stop superseded by a restart was settled as SUCCESS moments before the restart
   spawned a fresh core.
4. **The reported state could say `stopped` for a running VPN.**
   `coreLifecycleState` decided from a phase that does not describe whether a
   *process* exists; an adopted root core records ownership without passing
   through a start operation, so the UI drew a clean Start button over an actively
   routed machine.
5. **A classification read another generation's log window.** Every sibling
   callback takes its generation as a parameter; `classifyCoreExitReason` looked up
   the current one, so a monitor winning a race against a renew could classify a
   transient crash as a deterministic failure — which stops auto-restart.

A sixth finding was in the fixes themselves: the runtime transition began carrying
its reason, which exposed that `RunningState.Set` dedups a no-op write. A stop
completing while the flag was already false published no event, so the operation
sat at `stopping` forever. Stressing the existing stop tests showed 13 failures in
30 runs. A successful contextual stop now settles its own operation.

Two lessons the pass made concrete:

- **A test that cannot distinguish the bug from the fix is not a test.** The first
  version of the engine-abandonment test passed against the broken code, because a
  fresh controller's generation is 0 and re-reading it happened to agree with the
  argument. It became evidence only once the fixture made the two values differ.
- **Substituting the wrong fake proves nothing.** The acceptance tests for the
  original "Start reports success while nothing has spawned" defect replaced
  `runCoreOp`'s closure, which shows only that `runCoreOp` awaits what it is given.
  The actual bug was that the production closure returned immediately. They now
  drive the real chain — `Backend.StartCore` → `AppController.StartVPNContext` →
  `LegacyBackend` → the process operation — and fake only the process step.

## 7b. Third pass, second round — the fixes audited in turn

The third pass's own fix for the restart-versus-stop confusion (a "teardown reason"
recorded on the controller and read when the running flag dropped) was itself
audited adversarially, and it was **wrong by design**. The asymmetry that made it
wrong is worth stating plainly: four call sites recorded a reason while seventeen
flipped the flag, and the two steps were non-atomic on a single shared slot.

The concrete failure it admitted, in order:

1. a user stops the core, recording "user stop";
2. the following state write is a **dedup no-op** — the flag is already false — so
   nothing is published, and the recorded reason is left behind;
3. the core later comes up and dies in a **crash**;
4. the crash transition reads the leftover reason and is reported as a **completed
   user stop** — settling an operation that never happened, and hiding the crash.

On top of that, three paths where the user genuinely did ask for the core to go
down carried **no** reason (the daemon's stop-on-exit; the adopted-core watcher,
which polls on a timer and can observe a death during a `Stop`; and `GracefulExit`
exiting with an operation still registered), so those stops could never settle. And
`publishTeardown` — the helper written to clear the slot — was dead code whose
non-atomic clear could silently destroy a concurrent write.

The fix is structural rather than a patch: **the reason is a parameter of the state
write** (`SetStopped(reason)`), so the note and the flip are one operation and there
is no slot to go stale. A no-op write now records nothing because it records nothing
at all. The three unlabelled paths were labelled or made to consult the user's
recorded intent, and the exit path settles any operation still in flight.

The audit's most useful criticism was of the tests. Four of them were **vacuous**:
two were source greps that never execute a line, one injected the reason itself
through a test-only seam (so it would pass with every production call deleted), and
one re-implemented the production logic inside its own fake. Only one was
load-bearing. The replacement includes a **behavioural** test that drives the real
`RunningState` and the real subscriber through the exact four-step sequence above
and fails on the original symptom when the shared slot is reintroduced —
`a CRASH was reported as a completed USER STOP`.

## 8. Test strategy

Every new test is deterministic: no public network, no real sing-box, no real
VPN, nothing that depends on machine speed.

| Mechanism | Used for |
|---|---|
| Scripted process table (`aliveChecker`) | readiness, termination, adoption |
| Recording signal function | escalation to KILL, survivor handling |
| Generation/ownership assertions | supersession, stale watchers, handover |
| Fake daemon server | daemon FATAL, apply, stop |
| Go tests reading Swift sources | pending lifecycle, error-code coverage |
| Real call chain + faked process op | "Start awaits a real commit", refusal propagation, timeout reaching the work |
| Cancel-injection at every boundary | `acquireWithContext` ownership (300 iterations each, under `-race`) |
| In-process HTTP fixture | fallback identity proof (empty proof refused, members compared) |
| Real `RunningState` + real subscriber | teardown-reason attribution, no-op-write leaks (behavioural) |

The last row deserves a note. There is no XCTest harness in this environment, so
Swift invariants are enforced from the Go side by reading the Swift sources: a
test proves the three core lifecycle commands do not use the helper that clears
pending on reply, and another proves every error code the backend can emit has a
branch in the UI. These are text assertions, and they are honest about it — they
check the shape of the code, not its runtime behaviour.

Non-vacuity was checked for each behavioural fix by disabling it and confirming
the test fails with the original symptom. Two are worth quoting, because the
failure text is the bug:

```
an early exit must settle the runtime as failed, got "running"
an unresolvable core FATAL must be recorded; swallowing it is what left
the user with a silently dead core
```

---

## 9. Known limits

- **Config provenance is genuinely unknown on this machine.** The real
  `config.json` has no marker while `state.json` exists, and `state.json` has no
  `vars`, so the Clash API secret is regenerated per build and the on-disk config
  is not byte-reproducible from state. Adoption is therefore reported as UNKNOWN
  rather than assumed either way. This is a real limit, not a fallback: the
  explicit "Let JiejieBox manage it" action does not depend on reproducibility.
- **`GOOS=windows go build ./...` fails at baseline** in `core` — unrelated to
  this audit and verified pre-existing.
- **The readiness window is a heuristic.** It is bounded and its result is
  corrected by the process's real exit, but it is not an API-level readiness
  probe. A core that stays alive while failing to serve would still be reported
  running.
- **The build mutex is per-controller.** Two launcher processes writing the same
  `config.json` are still not coordinated; the daemon model assumes one client.

---

# Second pass — the class survived the first fix

The first pass closed the incident by adding a reference-integrity validator. A
deliberate second pass then re-attacked the SAME invariant from the outside,
asking of each corrected defect: *which other paths share this mistake?* Eight
more root causes fell out, six of them reachable in normal use. They are recorded
here because the pattern matters more than the individual bugs: a validator is
only as good as the set of shapes it walks, and a lifecycle fix on one engine is
not a lifecycle fix on the other.

## P0-A — the validator walked only the TOP LEVEL of every rule list

`ref_integrity.go` iterated `route["rules"]` and `dns["rules"]` without
recursing. sing-box's logical rules — `{"type":"logical","mode":"or","rules":[…]}` —
nest arbitrarily, and this repo generates them. Reproduced before the fix:

```
{"type":"logical","mode":"or","rules":[{"domain":"a.com","outbound":"ghost-out"}]}
  ValidateConfigReferences → 0 issues
  CleanDanglingOutboundsInRouteRules → byte-identical, 0 warnings
```

That is the production failure exactly, one nesting level down, passing every
check the launcher had. Fixed by extracting `walkRuleRefs` / `walkDNSRuleRefs`,
which recurse and carry the full path, and making the cleaner recurse identically
so the two can never disagree about the same rule. Tests:
`TestNestedLogicalRuleTargetIsCaught`, `TestDeeplyNestedRuleTargetIsCaught`,
`TestNestedDNSRuleServerIsCaught`, `TestCleanerRepairsNestedRule`.

## P0-B — the cleaner silently deleted user routing rules

A dangling target with no valid fallback was DROPPED, with a warning in the log.
`bank.com → ru-out` simply stopped applying and fell through to `route.final`,
and nothing in the UI said so. This also contradicted the project's own
fail-closed policy for a dangling `detour`, which drops the node rather than
silently rerouting it. A log line is not informed consent. Now a build error
naming the rule and the tag. Tests:
`TestDanglingWithoutFallbackFailsRatherThanDroppingTheRule`,
`TestClean_DanglingWithoutFallbackFailsTheBuild`.

## P0-C — the cleaner and the validator disagreed about sentinel literals

`outboundSentinelLiterals` (`direct`/`block`/`dns-out`/`reject`/`drop`) are names
the core resolves, and the cleaner deliberately left them alone. The validator had
no sentinel concept, so `"outbound":"direct"` was reported `missing_target` and
the BUILD FAILED on a config the core accepts — a false positive dressed as a
dangling reference. Both passes now share `isSentinelOutbound`. Test:
`TestSentinelLiteralsAreNotDangling`.

## P0-D — the daemon reported "stopped" on an accepted request (2nd engine, same lie)

The first pass taught the CLASSIC engine that a sent signal is not an exited
process. The daemon engine kept the weaker belief: `/admin/stop` is an ordinary
POST (`internal/lxdclient/client.go:246`) that returns 200 on ACCEPTANCE, and the
code treated that as proof and cleared `RunningState` immediately. Reproduced with
a daemon that accepts the stop while its core keeps running:

```
BUG PROVEN: RunningState=false while /admin/status still reports the core as
started — the launcher claims the VPN is down while the tunnel is up
```

Worse, this was downstream of a two-cores path: mode switching is gated on
`RunningState`, so a falsely-false flag permitted an engine switch while the
daemon core was still up. The daemon is now polled until it confirms the core is
gone (`awaitDaemonStopped`), unknown ≠ down, and on timeout the failure is
recorded and `RunningState` keeps saying "running". The same correction is applied
to `OnAppExit`, which additionally used to return `true`, making `GracefulExit`
enter a wait loop whose first check its own eager write had already satisfied.
Tests: `TestDaemonStopIsNotReportedUntilTheCoreIsGone`,
`TestDaemonStopReportsStoppedOnceTheCoreIsConfirmedGone`,
`TestDaemonStopThatIsConfirmedLaterSettlesLate`,
`TestDaemonStopRecordsAFailureWhenItCannotBeConfirmed`.

Self-inflicted bug found while testing this: the first version of the confirmed
branch never cleared the running flag, so a successful stop would have shown
"connected" forever. Caught by the settle test, not by review.

## P0-E — a retired daemon backend could still apply config and disable user nodes

`retryAfterCoreFatal` runs on a goroutine nobody owns and called
`applyCurrentConfig`, which passed `context.Background()` rather than `b.ctx`.
`Close()` therefore could not cancel it: after a daemon→classic switch an
abandoned backend could still push a config to the daemon the user had left —
and `retryCoreReject` disables user nodes on the way, before the `isActive()`
guard that was supposed to contain it. Now bounded by `b.ctx`.

Related: `isActive()` is a pointer comparison against `ac.Backend()`, and
`setBackend` calls `prev.Close()` while `ac.backend` still points at `prev` — so
for the whole handover a dying backend looked live, and a status frame decoded
microseconds earlier could publish state after the new engine was active. The
daemon engine has no generation counter; `closed atomic.Bool` is its equivalent.
Tests: `TestClosedDaemonBackendIsNotActive`.

## P1 — reserved names, inbound targets, and a headless nil-deref

- A user outbound tagged `direct`/`block`/`dns-out` shadows a core built-in and
  makes every reference to it ambiguous, yet validated as clean. Now
  `RefReservedTag`. Test: `TestReservedTagCollisionIsCaught`.
- `route.rules[*].inbound` was never checked, though template branches can drop an
  inbound. Now validated at every depth, and skipped when no inbounds are declared
  at all. Tests: `TestInboundReferenceIsCaught`,
  `TestInboundNestedInLogicalRuleIsCaught`, `TestNoInboundsDeclaredSkipsInboundCheck`.
- `dns.final` lacked the "no targets declared" guard its `route.final` twin has,
  so a minimal template was rejected where the route case was tolerated. Tests:
  `TestDNSFinalWithoutServersIsNotReported`, `TestDNSFinalWithServersIsStillCaught`.
- `refreshUI()` dereferenced `ac.ui()` with no `hasUI()` guard, unlike every
  sibling call in the same file — a nil-deref on the headless backend.

## P1 — shutdown force-kill bypassed process identity

`GracefulExit` waited 2s and then fell back to `ac.SingboxCmd.Process.Kill()`: a
raw PID signal with no identity check (a recycled PID means killing an unrelated
process in the last seconds of shutdown), which also cannot reach a privileged
root core at all — `SingboxCmd` is nil there, so "Forcing kill" logged a line and
left the TUN up. Replaced with `ForceStopOwnedCore()`, which goes through the same
identity-verified terminate primitive as a normal stop.

## What the second pass confirmed as already correct

Stated with evidence, because a review that only lists problems is not a review:

- **Build/validate ordering.** `ValidateConfigReferences` runs on the re-rendered,
  repaired bytes and `res.ConfigJSON` is assigned from that same object; no filter
  runs after validation. The emitted bytes ARE the validated bytes.
- **Candidate preflight.** `promoteCandidate` is reachable only when `sing-box
  check` returned nil, the candidate lives beside `config.json`, and promotion is
  an atomic `os.Rename`. A rejected candidate cannot destroy the last working
  config. Now pinned by `TestRejectedCandidateNeverReplacesTheGoodConfig`.
- **In-graph filtering.** `sanitizeOutboundGraph` runs a mutating fixpoint that
  updates detour carriers, chain positions, group members, nested chains and
  cycles, and mutates `finalTags` so later sections see the final set.
- **Preview builds.** No non-test caller sets `ForPreview: true`; every real
  producer goes through the validated path, so a preview cannot mask a defect.
- **The classic generation guard**, `applyOnce` commit ordering, `applyMu`, and the
  supervisor reconnect/backoff were each read and found sound.

## P0 — the second pass's own fixes crashed the process, twice

Found by an adversarial review of the changeset rather than by a failing test,
which is the point of having one. Both are `fatal error: sync: unlock of unlocked
mutex` — an UNRECOVERABLE runtime error: no `recover()` catches it, no dialog
appears, the launcher dies. Both were on the exact race paths this audit set out
to fix.

**`onPrivilegedScriptExited`** armed `defer ac.CmdMutex.Unlock()` and also released
the mutex by hand in the user-restart branch. The supersede guard that follows then
returned **before** the compensating re-acquire — so pressing Restart and switching
engines in that window killed the process. Reproduced 3/3.

**`Monitor`** had the same shape, and this one is worse: the comment at the guard
reads *"THE MODE-SWITCH WINDOW, closed"*, but the guard that closes it returned
through an armed defer. The fix converted the two-cores race it was written for
into a fatal crash in precisely that case. Both guarded returns after the manual
unlock were affected.

The root cause is structural, not incidental: **every function that mixes
`defer Unlock` with a manual `Unlock` has now produced this bug twice**, while the
functions that manage the balance explicitly (`KillForRestart`,
`ForceStopOwnedCore`, `Stop`) have never had it. Both functions were converted to
the explicit discipline — no defer, and each return stating whether it holds the
lock — rather than patching the two returns.

`TestNoDeferredUnlockWhereTheLockIsReleasedMidBody` asserts the shape on the
source (and says so: it proves the fragile pattern is absent, not that the manual
accounting is right). The two behavioural tests reproduce the original crash when
the defer is restored, which is how they were verified.

## P1 — a retired daemon backend could still settle a stop

`Close()` sets `closed` and `isActive()` honours it, but the stop path never
consulted either: it called `EndDaemonStop`, which wrote `RunningState`
unconditionally. A stop polls for up to 20s, so a user switching engines mid-stop
left the finishing goroutine writing the state of an engine it no longer owned —
the user would see "stopped" for a classic core that was starting, or "running"
for a daemon they had just left. `BeginDaemonStop`/`EndDaemonStop` now take the
backend performing the stop and refuse to act once it is no longer current: the
daemon engine's equivalent of the Classic generation rule.

---

# Fourth pass — IPC session, disk transactions, and request scheduling

The third pass fixed the lifecycle. This one is about the layers the lifecycle talks
through, and it found defects that made the previous round's work unreachable in the
shipped app.

## P0 — the IPC read loop was serialised behind each handler

`Serve` read a line, ran the handler to completion, and only then read the next one.
`start_core` can legitimately block for the whole operation budget — a config build, a
password prompt, a daemon apply. While it did, the server did not read the pipe at all,
so a `stop_core` the user sent next sat unread.

**"Stop cannot supersede start" was not a lifecycle defect.** The command never reached
the controller. Every ordering guarantee the previous round established was correct and
irrelevant, because the second command was never dispatched.

The loop now hands each request to its own goroutine and returns to reading
immediately. Ordering is kept where it means something — each domain serialises its own
mutations — and nowhere else, because the request that must interrupt another is exactly
the one that must not queue behind it. Responses carry their ids, so out-of-order
replies were already expressible. `Serve` drains in-flight handlers before returning, so
"when Serve returns, every accepted request was answered" still holds.

## P0 — a restarted backend looked silent to the frontend

`seq` is a per-process counter; the frontend's high-water mark lives as long as the
frontend does. After a helper restart the new process counted from 1 while the client
still held (say) 137, so **every** event from the new helper compared as stale and was
dropped. Core state, traffic, selections, subscriptions and daemon state all froze until
the new helper emitted more events than the old one ever had.

A sequence number is only meaningful within the session that issued it, so events and
snapshots now carry one. A client scopes its mark to the session, re-baselines when it
changes, and **drops** an event from a session it is not following — the two mistakes
here are opposite (dropping the live backend's events, applying the dead one's) and both
came from the same ambiguity.

## P0 — the snapshot could roll a newer event backwards

The stream opens before the snapshot is requested, so an event can arrive while the
snapshot is in flight. Applying it immediately and then applying the snapshot on top is
a rollback: the snapshot was composed at an earlier sequence and describes the older
state, and the event has already been consumed, so the UI stays wrong until something
unrelated happens.

Events that race the baseline are now buffered and replayed after it, filtered against
its sequence. The backend's `Snapshot` had the mirror problem — it read the sequence,
released the lock, and only then built the content, so the number described a moment
**before** anything in the snapshot was captured. Both are now captured in one critical
section, which makes the number a real version point.

## P0 — an old helper's exit could tear down the new one

Termination handlers run asynchronously, after the app has moved on, and consulted
client-wide state. During a restart the old helper's handler therefore judged the old
process's exit by the **new** process's intent — so a normal exit was reported as a
crash — and then cleared the new helper's process, stdin and read task. A live backend
was reported as dead with its event stream closed.

Every async continuation now captures the generation it was created for, and a
superseded one may touch nothing. Related: `shutdown` terminated, slept a fixed
interval, and released ownership **without checking whether the helper had exited**, so
`restart()` could start a second helper over a live first one — two processes sharing
the state files, the config, the core and the daemon channel. It now escalates and
confirms, and reports failure rather than pretending.

## P0 — provenance proved the past, not the present

The marker recorded `managed: true`, which says the launcher built a config here at
some time. It does not say the file present now **is** that config. A user who edited
config.json by hand left the marker untouched, so the next rebuild was authorised to
delete their work.

The marker is now bound to content: it carries the SHA-256 of the bytes it describes,
and ownership holds only while the file still matches. A mismatch is UNKNOWN rather than
EXTERNAL — the launcher cannot tell the user's own edit from a foreign tool, and
accusing the user of being someone else is the mistake this file already made once.

The hole was not one missing call. Only the maintenance reload and the explicit adoption
recorded ownership; every path that actually **builds** the config recorded nothing, and
on a fresh install the launcher disowned the file it had just created. So the promotion
point itself now announces what it promoted, which makes the write and its description
one transaction: a path that promotes a config and forgets is no longer expressible.

## P0 — state.json had no single transaction model

The subscription CRUD paths saved the whole file without taking the lock the refresh
path used. Two unsynchronised whole-file writers is worse than a lost update — the
concurrent test observed state.json left **unparseable**, because each save is a
truncate-and-write the other can walk into. All mutations now go through one helper that
holds the lock across the entire read-modify-write, **including the load**: locking only
the save still lets a refresh write back a snapshot it read before the user's edit.

Update-all loaded before locking, and the repair is deliberately not a bigger lock. The
sweep fetches every subscription over the network, so holding the lock there would block
every subscription edit behind the slowest provider. It now snapshots source **IDs**
under the lock, fetches outside it, and merges under a fresh lock that re-reads the
current state and copies only fetch-owned fields. A user's rename, URL or enabled flag is
never written from a fetch.

## P0 — core import raced the lifecycle it was checking

`coreIsStoppedForReplacement` consulted the running flag and the button state, both of
which stay false while a start is **in flight**. An import arriving in that window was
permitted to rename the core binary while the start goroutine was about to exec the old
path — version and config validated against one file, the process running another.

Two changes, both needed: the check now consults the lifecycle (where "stopping" is not
stopped), and replacement takes a **lease** from the operation record, because a check
and a rename are two steps and only mutual exclusion closes the window between them. The
lease refuses where an ordinary operation supersedes — superseding a start to install a
core would cancel the user's start and then rename the binary it was about to exec,
which is the race dressed up as success.

## P0 — unpairing had no backend guard and outlived its own credentials

`UnpairDaemonForget` called straight through to the controller. The Swift view disables
the control while the daemon is in use, but a UI is not a safety boundary. It is now
refused while the core runs or an operation is in flight.

The live transport also survived: deleting a certificate from disk does not invalidate
an open socket, so the UI reported `Paired=false` while the old backend could still
control the daemon. Unpairing now tears that transport down.

And the write order was backwards — the identity was deleted before the settings were
saved, so a failed save left settings naming a pairing whose credentials no longer
existed. Settings are written first now, so failing leaves everything intact and still
paired: a state the app can describe.

## P1 — the rest

- **`CancelActive` had zero callers** despite a comment describing exactly when it was
  called, so a core killed externally, a daemon FATAL, a mode switch and shutdown all
  left a latency test probing a dead transport. Wired to those three real triggers, with
  tests that drive the **triggers** rather than the method — a test that calls it
  directly passes with every call site deleted.
- **`testContext` leaked a goroutine per test**: it built a context *and* a
  shutdown-watcher goroutine and returned only the context. Two hundred tests, two
  hundred parked goroutines. It now derives from one shared run context.
- **A write failure was only a log line.** The backend kept generating events into a
  pipe nobody read while a Classic core kept running with no frontend. It now fails the
  connection and finishes the read loop so the normal teardown runs.
- **A successful refresh hid a failed rebuild.** The core logged the rebuild failure and
  returned `nil`, so the user was told "N nodes from M sources" while config.json had
  not been touched. Both phases now travel out on the result.
- **`MethodSubscribe` was a fake handshake**: events began at connection time regardless,
  while the handler acknowledged a subscription that did not exist. The asymmetry is
  documented rather than implied, and the ordering guarantee moved to the snapshot
  barrier, which is real.
- **Enabling/disabling or repointing a subscription** neither marked the config stale nor
  invalidated the previous provider's nodes and metadata; a URL change now clears the
  materialisation, and a duplicate URL is refused on edit as well as on add.

## What this pass confirmed as already correct

`subscription_import.go` already took the lock before loading state. `writeCandidate`
already used the candidate-then-rename pattern. The remaining `RunningState.IsRunning()`
and `GetVPNButtonState` uses are display derivations, not safety gates — the two safety
gates now consult the operation record. The operation state machine, the engine-switch
transaction and the fallback identity proof from the previous rounds hold up.

## The review of this pass, and the two regressions it found in it

Every round in this document was followed by a review of the round itself. This pass's
review found ten defects — two of them introduced by the pass being reviewed, which is
the honest reason to record it here rather than only in the commit.

**The sweep was disconnected.** Restructuring the subscription merge, the call site was
replaced with `_ = sweepIDs` and the call never re-added. Every subscription silently
stopped refreshing while `UpdateSubscriptions` still reported `RefreshOK: true` — lying
in exactly the direction the two-phase result had just been built to prevent, in the
same round that built it. Its own test could not see it, because that test asserted "no
fetch happens inside the lock", which is trivially true of code that fetches nothing.
The test now asserts the sweep is *called*.

**The merge lost its lock.** The sweep re-reads state under a brief lock and the caller
no longer wraps it, but the read-modify-write needs the lock itself; otherwise it is a
second unsynchronised whole-file writer beside the CRUD paths the same round had just
serialised. `state.Load` is also not a pure read — it can `Save` — so a "fetch results"
merge could rewrite the user's outbound config as a side effect.

Three further defects predated the pass and were only visible once it was examined
closely: the maintenance lease's check and take were in two critical sections (so a
start could be *superseded* by an import rather than refusing it); unpair deleted
credentials it had failed to stop using, because the engine switch ran after the wipe
and its error was swallowed; and the provenance hook was installed *after* the
construction-time build it was meant to describe, leaving the hole open on the first
build of every launch.

### A test helper that was editing its own subject

`stripGoComments` cut every line at the first `//`, which is a comment only outside a
string literal. Go source is full of `json:"http://…"`, so every struct field after one
silently vanished from the text under test. Two tests were disabling themselves —
including the P0 assertion for the client half of session identity, which checked the
same literal twice and never tested the snapshot at all.

The lesson is not "grep tests are bad" but that **a helper which transforms the subject
must be tested at least as carefully as the assertions that use it**. A source-shape
test is only as strong as the fidelity of the text it is handed, and a lossy helper
converts a passing assertion into no assertion at all while still reporting success.
Fixing the stripper immediately failed four tests whose assertions had been passing
against text their own helper had already deleted.

### The race that the review prompted

`SendEvent` writes the stream from subscriber goroutines that `drain()` did not track,
and `defer unsubscribe()` ran *after* the drain — so `Serve` could return while an event
was still being written, and a caller reading the stream raced a live writer. Three
races in 25 stress runs; zero once unsubscribe moved before the drain. Found only
because the review asked what concurrent dispatch had made reachable, and the answer
was "a write path nobody was counting".
