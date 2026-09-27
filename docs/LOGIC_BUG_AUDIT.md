# Logic Consistency Audit — JiejieBox macOS menu bar

**Language**: English

A horizontal sweep for *recurrences* of bug patterns that this project has
already produced real defects from. The goal was not to add features or
redesign anything: it was to find the same class of mistake elsewhere, before a
user finds it with a screenshot.

Every finding below was confirmed by reading the code, not inferred from a
pattern match. Patterns that turned up clean are recorded too, because "checked
and clean" is the useful part of an audit.

---

## 1. Numbers

```
Interactive controls audited:      79
Disabled conditions audited:       35
Guard-return paths audited:        14
Async backend methods audited:     30
Navigation routes audited:         10
State sources audited:             13

Confirmed logic bugs:               5
  P0 fixed:                         0
  P1 fixed:                         4
  P2 fixed:                         1
Remaining known logic bugs:         0
```

Nothing found was P0: the P0 classes (dead end, silent click, wrong runtime
state, infinite pending, lifecycle, dangerous mutation, data loss) had already
been closed in the preceding work, and this sweep confirmed they stayed closed.

---

## 2. Confirmed bugs

### BUG-1 — Navigation disabled by a runtime condition (P1)

**Pattern**: A / S (a `.disabled` that gates an *entry* rather than a *mutation*).

**Occurrences checked**: all 35 `.disabled` sites. Of these, 3 wrap a row that
navigates. Two were correct (`pending != nil` is transient; an empty path means
there is nothing to open). One was wrong.

**Files**: `Views/HomeView.swift` (Network → Proxies row).

**Root cause**: `.disabled(model.core?.state != .running)` on a row whose action
is `path.append(.proxies)`. With the core stopped the row was greyed out, so the
user could not open the screen — and that screen is precisely where the message
"Start the core to list and switch proxies." lives. The entry was disabled by
the very condition it exists to explain.

**Fix**: the row is always navigable. `ProxiesView` already distinguishes
"backend down", "core stopped" and "no nodes", so opening it with the core
stopped shows a useful explanation instead of an error.

**Regression check**: re-ran the navigates-under-`.disabled` scan; only the two
correct cases remain. This is the same mistake as the earlier daemon-row bug, so
it is now a recorded rule: **not being able to change something is never a reason
to hide its status.**

---

### BUG-2 — An unrelated row reported another control's save (P1)

**Pattern**: R / mislabelled pending state.

**Occurrences checked**: every `PendingOperation` case and every row that reads
`model.pending`.

**Files**: `App/AppModel.swift` (`SettingID`, `setSubscriptionEnabled`).

**Root cause**: enabling or disabling a subscription source reused
`.updatingSetting(.autoUpdateSubscriptions)`. Two consequences, both wrong: the
**Auto Update Subscriptions** row in More displayed "Saving…" for a save it was
not performing, and the shared `pending` guard could block a genuine auto-update
toggle while a subscription toggle was in flight.

**Fix**: a dedicated `SettingID.subscriptionEnabled(String)`. The Subscriptions
row now reports *its own* progress ("Saving…" with a spinner) instead of the
whole list going inert with no explanation of what was happening.

**Regression check**: grep for `updatingSetting(` shows each caller using its own
id; no cross-assignment remains.

---

### BUG-3 — Backend events declared but never handled (P1)

**Pattern**: O / dead protocol surface — the mirror image of "a button with no
backend method".

**Occurrences checked**: all 11 event names against every emit site and every
client handler.

**Files**: `backend/protocol/protocol.go`, `Models/Protocol.swift`,
`App/AppModel.swift`.

**Root cause**: three problems in one family.

1. `daemon_changed` was **emitted and ignored**, so the Daemon screen kept
   whatever status it loaded on entry. Pairing or removing a service from
   elsewhere left stale rows, and a prepared install/invite command lingered as
   if still valid.
2. `subscriptions_changed` was **emitted and ignored**, so a background refresh
   left the Subscriptions screen and Home's count stale.
3. `handshake_ready`, `log_line` and `error` were **declared on both sides and
   never emitted at all** — leftovers from the deleted diagnostics view. For
   `error` this was checked specifically: every backend failure today is
   returned as the response to a request, so there is no out-of-band failure to
   attach it to, and a client waiting for it would wait forever.

**Fix**:
- `daemon_changed` → `loadDaemonStatus()`, and a prepared command is dropped once
  the daemon reports ready (a command is only valid for the state that produced
  it).
- `subscriptions_changed` → `loadSubscriptions()`.
- `handshake_ready`, `log_line` and `error` **deleted** from both sides, since
  nothing emits them. A phantom event is worse than a missing one: it invites a
  future client to wait for something that never arrives. Everything the backend
  can fail at is already reported through a request's response.

**Regression check**: the event surface is now exactly symmetric — 8 events,
each emitted by the backend, declared in Swift, and handled by the client, with
no phantom entries in either direction. Queries emit nothing, so the reload
handlers cannot feed back on themselves.

---

### BUG-4 — Home depended on visiting another screen (P1)

**Pattern**: P / stale summary because data loads lazily.

**Occurrences checked**: every value Home renders against where it is loaded.

**Files**: `App/AppModel.swift` (`performStart`).

**Root cause**: Home shows a subscription count and a Core Mode subtitle, but
subscriptions and daemon status were loaded **only** when More, Subscriptions or
the Daemon screen was opened. On a fresh launch Home therefore reported
"Subscriptions: None" and an unhelpful daemon label even with sources
configured — and only corrected itself after the user visited those pages.

This is the same defect as the earlier proxy-summary bug, which was fixed by
putting the summary in the snapshot. These two had been missed.

**Fix**: bootstrap primes both after the snapshot, before `connection = .ready`.
Both are cheap local reads, so the first render is already correct.

**Regression check**: every value Home displays is now loaded during bootstrap or
carried in the snapshot. Home has no remaining dependency on page visits.

---

### BUG-5 — Stale comment describing removed protocol (P2)

**Pattern**: T / documentation contradicting the code.

**Files**: `backend/protocol/protocol.go`.

**Root cause**: the `HandshakeResult` comment still claimed the type "is also
emitted as EventHandshakeReady", after that constant was deleted in BUG-3.

**Fix**: the comment now states there is deliberately no handshake event and why
(the snapshot seq already guarantees a consistent view).

---

## 3. Patterns checked and found clean

| Pattern | Scope | Result |
|---|---|---|
| C — nested interactive controls | Button/Toggle/Picker/NavigationLink/TextField/Link inside another's label | **0** |
| F — saved preference used as runtime truth | Active/Connected/Running/Selected/Enabled/Paired/Ready | clean |
| N — silent guard-return | all 14 guard-returns | all backed by `.disabled` or non-user paths |
| I — async request without a timeout | 30 methods | **30/30 explicit**; none falls through to the default |
| J — timeout that leaks the continuation | `abandon` | resumes exactly once; verified by probe |
| K — view lifetime owning backend lifetime | bootstrap | model-owned task, survives panel close (verified) |
| L — expected termination reported as a crash | Quit + Restart + crash | intent distinguishes; only unexpected notifies |
| M — shutdown executed twice | IPC + EOF | `sync.Once`; guard proven load-bearing |
| O — capability claiming a removed feature | `remote`, `configurator` | both false; `subscriptions` added |
| R — leftover command/banner/pending after success | invite, transient, pending | cleared on success; token-guarded expiry |
| §12 — Empty / Loading / Error confusion | Proxies, Subscriptions, Daemon | shared `BackendDownView`; emptiness only after a confirmed reply |
| §15 — double trigger from response + event | edit paths | queries emit nothing; no feedback loop |
| §17 — more than one event consumer | bootstrap | old stream cancelled before a new one starts |
| §20 — toggle left in the wrong position on failure | 5 toggles | server-backed ones re-read from the backend; Launch at Login re-reads SMAppService and reports failure |
| §21 — destructive action without confirmation | Delete / Forget / Remove | delete confirmed; daemon destructive gated on `active_mode && running` with a reason |
| §26 — full hit area | all rows | `MenuRow`/`ActionRow` set `maxWidth: .infinity` + `contentShape(Rectangle())` |
| §27 — empty placeholder reserving layout | banners | rendered as a real conditional branch, never `.opacity(0)`/`.hidden()` |
| §29 — core state machine too permissive | Start/Stop/Mode switch | `canSwitchCoreMode` requires a settled `stopped` core, not `!= running` |

---

## 4. State-machine review

**Daemon** — `supported → core_supports_lxd → installed/needs_install →
needs_start → paired → reachable → ready`. Every combination has an exit: the
Daemon screen shows exactly one next step, plus `Refresh Status` in all states
and `Open Core Folder` when the core cannot host the service. A `ready` daemon
offers activation; an active one offers the status rows, the keep-running
setting and `Switch Back to Classic`.

**Subscription** — `no source → added → needs refresh → refreshing →
success | failed → disabled | enabled → config stale`. The empty state offers
Add; a failed fetch keeps the source with its error and a `Refresh Now`; a stale
config is reported with `Reload Config`. No state is a dead end.

**Proxy** — `core stopped | API unavailable | groups loading | no groups | group
selected | nodes loading | no nodes | node active | latency pending | backend
error`. These are now distinct: "backend down" is its own view, "core stopped"
has its own message and a `Try Again`, and "never measured" is `—` rather than a
false `0 ms`.

**Core** — only a settled `stopped` permits a mode switch; `starting`/`stopping`
are treated as busy rather than as "not running". `canSwitchCoreMode` is the one
shared gate for Classic, Daemon activation and the mode rows.

---

## 5. What was deliberately not changed

- `core/**` — untouched. No business-logic problem was found; the two backend
  edits were confined to `backend/service` and `backend/protocol`.
- The pairing protocol, mTLS, the LXD CLI contract, `CoreSupportsLxd` and the
  daemon lifecycle — all verified byte-identical to the start of this audit.
- No second source of truth was introduced for any fix: the subscription row reads
  backend state, the daemon command stays derived from the backend's own
  `operation`, and the Active badge still reads runtime `core.backend`.
