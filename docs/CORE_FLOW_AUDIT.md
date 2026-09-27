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

Confirmed bugs:                             6
  P0 fixed:                                 2
  P1 fixed:                                 3
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

### BUG-6 — Reload Config was a dead end for a non-wizard config (P1/P2)

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
- **Config-stale closure.** Editing a subscription sets `config_stale` and it
  survives a restart; the Proxies screen surfaces it with a Reload action.
  (The reload itself is gated per BUG-6.)
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
| 7 | Test one node | measures only that node; selection unchanged (measured) |
| 8 | Test all nodes | sequential, per-row spinners |
| 9 | Switch node | response re-read from the core (measured) |
| 10 | Update subscriptions | per-source result; `proxies_changed` |
| 11 | Reload config | guarded; explains when unrebuildable |
| 12 | Return to Proxies | groups reload before nodes (BUG-5) |
| 13 | Home summary | from the snapshot, not page visits |
| 14 | Error readability | real backend message (BUG-1) |
| 15 | Quit | unaffected; 66 ms path from the previous round |

---

## 6. Remaining known issues

**None in the core flow.** Two things are recorded as behaviour rather than bugs:

1. **Reload Config cannot rebuild a hand-written configuration.** A rebuild
   replays wizard state; a config with no state file is not rebuildable. The UI
   now says so precisely instead of failing obscurely. Making it rebuildable is a
   product decision, not a defect.
2. **The manual GUI walkthrough is still unrun.** Every claim above is static
   analysis plus IPC exercised against the packaged helper with a stub Clash API.
   Layout, hover states and click feel need a human.
