# Core Flow Audit — Proxies, core lifecycle, and the front-to-back chains

**Language**: English

A core-capability-first audit: the chains a user depends on to actually use the
VPN. The goal was to make the fundamental path solid — core start/stop, node
display, node testing, node switching, and recovery of the node list after a
subscription update or config reload — rather than to polish secondary screens.

---

## 1. Numbers

```
Proxy interactive controls audited:        10   (6 controls + 4 state actions)
Core start/stop/restart actions audited:    6
Backend methods chain-verified:            10
Proxy state paths audited:                 10
Core state paths audited:                   5

Confirmed bugs:                            10
  P0 fixed:                                 4
  P1 fixed:                                 5
  P2 fixed:                                 1
Remaining known core-flow bugs:             0
```

---

## 2. Bugs fixed

### BUG-1 — Every backend error rendered as "error 1" (P0)

**Symptom**: `The operation couldn't be completed. (JijieBox.BackendError error 1.)`

**Root cause**: `BackendError` conformed to `Error` but **not** `LocalizedError`.
Every AppModel catch block reports through `error.localizedDescription`, which for
a non-localised Swift error collapses to the generic Foundation text and throws
away the `message` the backend sent.

This is why the UI could not explain anything: "stop the VPN before switching the
core engine", "start the core before switching proxies" and every other precise
backend message were all replaced by a sentence containing no information.

**Fix**: `extension BackendError: LocalizedError`, returning `message` (falling
back to `code` if a message is ever empty).

**Verification**: reproduced the exact symptom with a minimal program — before
the fix, `localizedDescription` printed the generic text; after, it printed
`stop the VPN before switching the core engine`.

**Regression check**: `TestErrorsCarryAReadableMessage` asserts that every error
the backend can produce has a non-empty message that differs from its code, using
four requests that fail for different reasons.

---

### BUG-2 — Proxies collapsed every "no nodes" cause into one message (P0)

**Root cause**: the screen had a single empty state, `"No nodes in this group."`,
reached whenever `proxies.isEmpty`. A stopped core, an unreachable backend, an
empty config, a stale config and a genuinely empty group are five different
situations needing five different actions, but all five produced that one
sentence — wrong for four of them, and actionable for none.

**Fix**: an explicit `ProxyListState` (`idle`, `loading`, `backendUnavailable`,
`coreStopped`, `noGroups`, `configStale`, `empty`, `noGroupSelected`, `ready`,
`failed`), computed in one place and rendered as exactly one state. Each state
answers what is wrong, why, and what can be done:

| State | Message | Action |
|---|---|---|
| backendUnavailable | Backend Unavailable | Restart Backend |
| coreStopped | Core is not running | — (start it from Home) |
| noGroups | No selector groups | Open Subscriptions / Reload Config |
| configStale | Configuration needs reload | Reload Config |
| empty | This group has no nodes | Update Subscriptions / Open Subscriptions |
| failed | Could not load proxies | Try Again |
| ready (filtered) | No matching nodes | Clear Search |

The action offered depends on whether subscriptions exist, so the user is never
sent to a screen that cannot help them.

---

### BUG-3 — A node-load failure was reported as a group-load failure (P1)

**Root cause**: `loadGroups()` fetched groups and then nodes inside one `do`
block. A failure in the node fetch was caught by the same handler, so the user
was told the groups failed while a perfectly good group list was discarded.

**Fix**: the two steps have separate error handling. Groups load first; only if
that succeeds is the node list requested, and each failure is recorded
separately (`proxyError`). The Proxies screen can then explain its own failure
inline instead of only raising a banner on Home.

---

### BUG-4 — Start/Stop had no pending marker and no mutual exclusion (P1)

**Root cause**: `startCore()` and `stopCore()` used `run` (no pending state),
unlike `restartCore` which used `withPending`. Two consequences:

1. `start_core` returns as soon as the start is **requested** — the transition to
   `running` is asynchronous — so a rapid double click sent two start commands
   before the state had changed to `starting`, with nothing to prevent it.
2. There was a window where the button still read "Start" after being clicked,
   because only the reported state drove the label.

**Fix**: both use `withPending(.startingCore)` / `.stoppingCore`, and the Home
button's label and spinner consider the pending operation as well as the reported
state.

---

### BUG-5 — A config reload reloaded nodes before re-validating the group (P1)

**Root cause**: on `proxies_changed`, the client called `loadProxies(group:)`
first and `loadGroups()` second. A rebuild can rename or remove a selector group,
so loading nodes for a group that no longer exists failed — leaving the stale name
in place and the list empty. The corrective call came afterwards, too late.

**Fix**: `loadGroups()` runs first, which re-picks a valid group when the
remembered one is gone, then loads that group's nodes.

---

### BUG-6 — Quit skipped the EOF fallback when the ACK failed (P0)

**Root cause**: `shutdownGracefully` closed stdin only `if acknowledged`. On a
shutdown timeout or decode failure the pipe stayed **open**, so the helper — whose
read loop blocks on stdin and whose `GracefulExit` cannot stop it — had no way to
learn the frontend was gone. The wait then burned its whole budget and the process
was force-terminated without its graceful teardown ever running.

That inverts the intent: EOF is precisely the fallback for a broken IPC channel,
so it is needed most when the ACK did not arrive. Measured on a helper that only
exits on EOF: **3207 ms with the pipe left open and a force-terminate, versus
155 ms with the EOF fallback and no force-terminate.**

**Fix**: stdin is closed on every path. The ACK ordering still protects the
normal case (the response is written before teardown starts, so it has already
arrived), and on the failure path there is no ACK to protect and the caller's
waiter has already been resumed with the timeout error.

The log line also claimed `"(no ACK; EOF-only path)"` for a route that never
performed an EOF handoff; it now says `"(no ACK; EOF fallback used)"`.

**Regression check**: `TestQuitAlwaysSendsTheEOFFallback` is a static invariant —
closing stdin must not mention the ACK, must precede the force-terminate, and must
follow the shutdown request. Verified to **fail** when the bug is restored.
`TestEOFAloneRunsTheSameTeardownAsTheMethod` proves EOF reaches the full
exactly-once teardown rather than a weaker exit.

---

### BUG-7 — Reload Config was a dead end for a non-wizard config (P1/P2)

**What the audit found**: the empty-state copy told users to reload the config,
so the closure was checked — and it does not close for every user. A rebuild
replays the **wizard state**, and this machine's real configuration (like any
hand-written or externally managed one) has `config.json` but no
`wizard_states/state.json`. Reload therefore failed with
`load state: state: file not found` — naming a file the user has never heard of,
behind a button the UI invites them to press.

**Fix**: `ReloadConfig` checks whether a rebuild is possible and, when it is not,
returns a `not_rebuildable` error that explains the configuration was not created
by the wizard and says what to do instead. The UI's Reload affordance now leads
somewhere informative rather than nowhere.

**Note**: this does not make the configuration rebuildable — it is not. It
replaces a dead end with an accurate explanation, which is the honest outcome for
a config the product does not own.

### BUG-8 — Config provenance was inferred from file existence (P0)

**Root cause**: `configIsRebuildable` returned true when `state.json` existed.
That is not an ownership signal. `AddSubscription` calls `state.New()` and
creates a state file the first time a source is added, so an **externally managed
config acquires a state file** the moment the user subscribes — and the check
flips from correctly refusing to wrongly rebuilding. A reload would then replay
an almost-empty state over a config the launcher never wrote.

**Reproduced**: external config, no state → `not_rebuildable` (correct); add one
subscription → `rebuild_failed` (misjudged as rebuildable).

**Fix**: ownership is recorded explicitly in a marker beside config.json
(`.jiejiebox-config.json`), written **only** after the launcher has actually built
the config. The guard has three cases:

| Case | Decision |
|---|---|
| No config.json | allowed — a rebuild is how the file comes into existence |
| config.json + our marker | allowed — the launcher built it |
| config.json, no marker | **refused** — someone else owns this file |

Modification times were explicitly rejected: a hand edit makes the config newer
than the state, and a rebuild makes it newer still, so mtime says nothing about
ownership.

**Regression check**: `TestExternalConfigStaysUnrebuildableAfterAddSubscription`
drives the exact scenario end to end and asserts the external file is
byte-identical afterwards. `TestMarkerGrantsOwnership` covers the positive path
and that a corrupt marker refuses rather than assuming the permissive answer.

---

### BUG-9 — Proxies nested a vertical ScrollView inside another (P1)

**Root cause**: `PanelScaffold` always wrapped content in a `ScrollView`, and
`ProxiesView` wrapped its node list in another. The inner view therefore had an
unbounded height, so it never scrolled itself; the wheel behaved differently
depending on which view was under the pointer, and the edges double-bounced.

**Fix**: `PanelScaffold` takes `scrollsContent: Bool`. Proxies passes false and
its node list is the page's single vertical scroll owner, bounded by the
scaffold's fixed-height frame. The header stays outside every scroll view, so
Back and Quit remain reachable. Verified: exactly one real `ScrollView` per page.

---

### BUG-10 — A busy proxy row left both its actions clickable (P1)

**Root cause**: `ActionRow` had only a row-wide `disabled`, and the proxy row
passed `disabled: pending != nil && !testing` — so while a node was being
measured, **both** its actions stayed live. The second click was then rejected by
the model's `withPending` guard, and the user saw "Another operation is still
running" for a control that looked available.

**Fix**: `RowAction` gained `isDisabled`, so availability is per action. A busy
row disables both of its actions; across rows the rules follow the operation —
switches are serialised (two would race for the same selection), measurements are
independent and stay usable unless the whole group is tested.

| State | select | measure |
|---|---|---|
| idle | enabled | enabled |
| this node testing / switching | disabled | disabled |
| another node testing | enabled | enabled |
| another node switching | disabled | enabled |
| Test All | disabled | disabled |

`withPending` is retained as the second line of defence rather than the only one.

---

## 3. Front-to-back chain verification

Each core action was traced UI control → closure → AppModel → BackendClient →
IPC method → Go handler → business call → response/event → AppModel → UI, and
exercised against the packaged helper.

| Action | IPC | Verified |
|---|---|---|
| Start core | `start_core` | handler calls `core.StartSingBoxProcess()`, emits core state |
| Stop core | `stop_core` | handler calls `core.StopSingBoxProcess()`, dispatches via the active backend |
| Restart core | `restart_core` | `KillSingBoxForRestart()` — daemon applies in-process, classic lets the supervisor restart |
| List groups | `get_proxy_groups` | reads selector outbounds from the config; works with the core stopped |
| List nodes | `get_proxies` | empty group resolves to the config default |
| Switch node | `switch_proxy` | goes through `SwitchProxyVia`, then **re-reads** the list |
| Test one node | `test_proxy` | measures only that node; never switches |
| Test group | `test_proxy_group` | sequential by design |
| Reload config | `reload_config` | guarded; emits `proxies_changed` |
| Update subscriptions | `update_subscriptions` | per-source result, emits `proxies_changed` |

Measured against a stub Clash API through the packaged helper:

```
groups   : ['proxy-out']
nodes    : selected=['Node Amsterdam']
test     : selected still=['Node Amsterdam']      <- testing does not switch
switch   : response selected=['Node Amsterdam']    <- re-read from the core
event    : proxy_selection_changed {group, name}
re-read  : selected=['Node Amsterdam']             <- persisted
```

A failed switch (stub returning 501) produced `switch_failed` carrying the real
HTTP status, with **no** state change — the failure path reports the truth rather
than appearing to succeed.

---

## 4. What was checked and found sound

- **Runtime truth.** Node selection comes from the core's `now` field
  (`Selected: p.Name == selected`); there is no optimistic local flag, and
  `switch_proxy` returns a freshly-read list rather than what was requested.
- **Config-stale closure — verified closed.** With a buildable state:
  edit → `config_stale = true` → `reload_config` → `config_stale = false`. The
  rebuild additionally refuses to overwrite a working config with an empty one
  ("no nodes parsed from any source"), which is a safeguard rather than a failure:
  it prevents a misconfigured subscription list from destroying a working config.
- **Original semantics preserved.** The UI copy in the empty state promises the
  subscription/reload route, and the chain behind it is real: the SwiftUI app
  switches proxies through the same `SwitchProxyVia` the old UI used, including
  its side effects (active name, per-group last-selected memory, the
  `OnProxySwitched` hook). Nothing was lost in the rewrite.
- **Core-start recovery.** A transition to `running` reloads groups, so the
  Proxies screen becomes usable without leaving and re-entering it.
- **Stop degradation.** `proxyListState` returns `coreStopped` when the core is
  not running, so the screen cannot pretend nodes are switchable; Test All is
  disabled with a reason.
- **Nested controls.** `Menu { Button }` in the group picker is the correct API —
  menu items, not a control inside a label. Node rows use `ActionRow`, whose two
  actions are siblings.
- **Home/Proxies agreement.** Both read the same state: Home's summary from the
  snapshot's `proxy` field, the Proxies screen from the node list. Neither has a
  private copy.

---

## 5. Regression matrix

| # | Chain | Result |
|---|---|---|
| 1 | Start core | entry, pending, disabled while busy |
| 2 | Stop core | entry, pending, disabled while busy |
| 3 | Restart core | entry, pending, disabled while busy |
| 4 | Enter Proxies | loads groups then nodes |
| 5 | Change group | reloads that group's nodes |
| 6 | Search nodes | filters locally, count reads "N of M", clearable |
| 7 | Test one node | measures only that node; selection unchanged (measured); both of that row's actions disabled while it runs (BUG-10) |
| 8 | Test all nodes | sequential, per-row spinners |
| 9 | Switch node | response re-read from the core (measured) |
| 10 | Update subscriptions | per-source result; `proxies_changed` |
| 11 | Reload config | provenance-guarded: refuses a config we did not build (BUG-8) |
| 12 | Return to Proxies | groups reload before nodes (BUG-5) |
| 13 | Home summary | from the snapshot, not page visits |
| 14 | Error readability | real backend message (BUG-1) |
| 15 | Quit | ACK path 66 ms; no-ACK path now uses the EOF fallback (BUG-6) |

---

## 6. Remaining known issues

**None in the core flow.** Two things are recorded as behaviour rather than bugs:

1. **Reload Config deliberately cannot rebuild a hand-written configuration.**
   A rebuild replays wizard state, so a config the launcher did not build is
   refused — now determined by an explicit ownership marker rather than by
   whether a state file happens to exist (BUG-8). Making an external config
   rebuildable is a product decision, not a defect.

   **Resolved.** Rebuildability is now exposed as
   `CoreState.config_rebuildable`, and every Reload affordance is gated on it:
   More, the Proxies stale and no-groups notices, the Subscriptions reload
   prompt, and the Home banner. Where a rebuild is impossible the UI states that
   the config is managed externally and offers **Open Config** instead — a real
   next step rather than a control whose only outcome is an error.
2. **The manual GUI walkthrough is still unrun.** Every claim above is static
   analysis plus IPC exercised against the packaged helper with a stub Clash API.
   Layout, hover states and click feel need a human.
