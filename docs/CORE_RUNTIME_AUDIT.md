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
