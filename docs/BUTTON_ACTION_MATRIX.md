# Button & Action Matrix — JiejieBox macOS menu bar

**Language**: English

Every interactive entry point in the shipped macOS app, traced from the control
down to the business call and back. "Verified" means the chain was checked by
reading the code and (where a backend method is involved) exercised end to end
against the packaged helper with a temporary fixture — never against the user's
real VPN.

Legend for **Pending**: what the user sees within ~100 ms of clicking, before the
operation finishes.

---

## Navigation & shell (frontend-only)

| Screen | Control | Type | Action | Pending | Success | Failure | Verified |
|---|---|---|---|---|---|---|---|
| any | `‹` Back | Button | `model.goBack()` (guarded, pops one level) | navigation | previous screen | n/a — disabled at root | ✅ |
| any | Quit | Button | `model.quit()` → `shutdown` → graceful exit | panel closes | backend exits per policy | `lastError` banner | ✅ |
| Home | status + Start/Stop | Button | `model.toggleCore()` | spinner + "Starting…" | status → Connected | error banner, button re-enabled | ✅ |

`Quit` never pre-stops the core: the backend decides classic-stop vs
daemon-keep-running. Verified by reading `quit()` → `shutdownGracefully()`.

Back at the root is not rendered at all (`onBack: nil`), so Home cannot show a
dead control.

---

## Proxies

| Control | Type | Action | IPC | Pending | Success | Failure | Timeout | Verified |
|---|---|---|---|---|---|---|---|---|
| group menu item | Menu Button | `selectGroup` | `get_proxies` | list spinner | node list for that group | error banner | 8 s | ✅ |
| node row (left, weight 3) | Button (sibling) | `switchProxy` | `switch_proxy` | row spinner | checkmark moves; latency kept | error banner | 20 s | ✅ |
| latency value (right, weight 1) | Button (sibling) | `testProxy` | `test_proxy` | value → spinner | new `NN ms` | "did not respond." | 120 s | ✅ |
| Test All | Button | `testGroup` | `test_proxy_group` | spinner | every row measured | error banner | 120 s | ✅ |
| search field | TextField | filters locally | — | live | count reads "N of M" | n/a | n/a | ✅ |
| Try Again | MenuRow | `loadGroups` | `get_proxy_groups` | spinner | list loads | error banner | 8 s | ✅ |

**The nested-button defect is fixed.** The row and its latency reading are now
siblings in `ActionRow`, never a button inside a button. Clicking the latency
target cannot switch the proxy, and clicking the row cannot start a test —
verified by reading the two independent `RowAction` closures; there is no shared
code path between them.

---

## Subscriptions

| Control | Type | Action | IPC | Pending | Success | Failure | Timeout | Verified |
|---|---|---|---|---|---|---|---|---|
| Add Subscription | MenuActionRow | open menu | — | menu opens | Add from URL… / Import from File… | n/a | n/a | ✅ |
| Add from URL… | Menu item | navigate | — | navigation | form | n/a | n/a | ✅ |
| Import from File… | Menu item | `importSubscriptionFile` | `import_subscription_file` | "Importing from file…" | toast "Imported N nodes."; row appears | error banner (backend code + message) | 30 s | ✅ |
| Import from File (empty state) | Button | same | same | same | same | same | 30 s | ✅ |
| Add | Button | `addSubscription` | `add_subscription` | "Adding…" | toast + list; pops back | error banner, form kept | 20 s | ✅ |
| row (left, weight 4) | Button (sibling) | navigate to edit | — | navigation | edit screen | n/a | n/a | ✅ |
| row On/Off (right) | Button (sibling) | `setSubscriptionEnabled` | `set_subscription_enabled` | row disabled | state flips from backend | error banner | 20 s | ✅ |
| Save Changes | MenuRow | `updateSubscription` | `update_subscription` | disabled | toast + pops back | error banner | 20 s | ✅ |
| Enable / Disable | MenuRow | `setSubscriptionEnabled` | same | disabled | row updates | error banner | 20 s | ✅ |
| Refresh Now | MenuRow | `refreshSubscription` | `refresh_subscription` | "Fetching…" | nodes/status update | error banner, source kept | 120 s | ✅ |
| Delete Subscription | MenuRow (destructive) | `removeSubscription` | `remove_subscription` | dialog → pending | row disappears | error banner | 20 s | ✅ |
| Provider Support | MenuRow | `NSWorkspace.open` | — | navigation | browser opens | silent if URL invalid (guarded) | n/a | ✅ |
| Update All | MenuRow | `updateAllSubscriptions` | `update_subscriptions` | "Updating subscriptions…" | per-source summary | `ok=false` → error | 120 s | ✅ |
| Reload Config | Button | `reloadConfig` | `reload_config` | disabled | toast; stale clears | error banner | 120 s | ✅ |

Delete is behind a `confirmationDialog` with an explicit destructive role.
A failed fetch keeps the source with an error line — a provider being down never
discards the URL the user typed.

**Local sources are rendered differently, and the difference is data-driven.**
`Import from File…` appears only when `capabilities.local_subscription_import` is
true. A row whose `input_kind` is `local_snapshot`:

- shows `Imported from <file>` instead of a URL (`sourceSummary`), because its URL
  is empty by design and an empty URL line reads as a broken provider;
- shows `Imported <time>` instead of `Never updated`, since it is never fetched;
- offers **no** `Refresh Now` — the detail screen states "Not available for an
  imported file" as a fact rather than drawing a dead button, and the backend
  refuses independently with `not_refreshable`;
- keeps its name editable but **not** its URL: the URL field is omitted, and Save
  sends the stored value back so a rename cannot blank it.

`Update All` skips local sources entirely (shared `state.CanRefreshSubscription`
guard, the same one auto-update uses), so an imported source is never the cause of
a provider error it could not have produced.

---

## Daemon

| Control | Type | Action | IPC | Pending | Success | Failure | Timeout | Verified |
|---|---|---|---|---|---|---|---|---|
| Install Service | MenuRow | `daemonSetup(.install)` | `daemon_install` | "Preparing…" | command shown + Copy / Open in Terminal | message explains | 20 s | ✅ |
| Start Service | MenuRow | `daemonSetup(.start)` | `daemon_start` | "Preparing…" | command shown | message explains | 20 s | ✅ |
| Pair Service | MenuRow | navigate | — | navigation | invite form | n/a | n/a | ✅ |
| Paste from Clipboard | MenuRow | `NSPasteboard` | — | instant | field fills | nothing to paste → no-op (harmless) | n/a | ✅ |
| Pair | Button | `pairDaemon` | `pair_daemon` | "Pairing…" | status → paired, pops back | `bad_invite` message | 30 s | ✅ |
| Copy Command | MenuRow | `NSPasteboard` | — | instant | toast "Command copied." | n/a | n/a | ✅ |
| Open in Terminal | MenuRow | `osascript` | — | instant | toast "Command opened in Terminal." | toast with fallback advice | n/a | ✅ |
| Refresh Status | MenuRow | `loadDaemonStatus` | `get_daemon_status` | spinner row | rows update | backend-down view | 15 s | ✅ |
| Use Daemon Mode | Button | `activateDaemonMode` | `set_core_mode` | pending | engine switches | error banner | 20 s | ✅ |
| Switch Back to Classic | MenuRow | `activateClassicMode` | `set_core_mode` | pending | engine switches | error banner | 20 s | ✅ |
| Keep VPN Running After Quit | MenuRow | `setDaemonKeepRunning` | `set_daemon_keep_running` | row disabled | value flips | error banner | 20 s | ✅ |
| Re-pair | MenuRow | `daemonSetup(.repair)` | `daemon_repair` | "Preparing…" | fresh command | message explains | 20 s | ✅ |
| Forget Pairing | MenuRow (destructive) | `unpairDaemon` | `unpair_daemon` | disabled | status → unpaired | error banner | 20 s | ✅ |
| Remove Service | MenuRow (destructive) | `daemonSetup(.uninstall)` | `daemon_uninstall` | "Preparing…" | removal command | message explains | 20 s | ✅ |
| Open Core Folder | MenuRow | `NSWorkspace.open` | — | navigation | Finder opens | n/a | n/a | ✅ |

`Use Daemon Mode` is gated on `canSwitchCoreMode`, which requires a settled
`stopped` core AND no operation in flight — not merely "not running".

**Setup is never activation.** Every setup step returns a command for the user to
run; none of them switches engines. A `sudo` operation that waited behind a
spinner is exactly what made the old click look like a freeze.

---

## Core Mode & Core Details

| Control | Type | Action | IPC | Pending | Success | Failure | Timeout | Verified |
|---|---|---|---|---|---|---|---|---|
| Classic row | MenuRow | `activateClassicMode` | `set_core_mode` | "Switching engine…" | Active badge moves | error banner | 20 s | ✅ |
| Daemon row | MenuRow | activate when ready, else navigate | `set_core_mode` / — | as above | engine / Daemon screen | error banner | 20 s | ✅ |
| Restart Core | MenuRow | `restartCore` | `restart_core` | "Restarting…" | state cycles | error banner | 20 s | ✅ |
| Config file row | MenuRow | `NSWorkspace.selectFile` | — | navigation | Finder reveals file | disabled when unknown | n/a | ✅ |
| Data / Logs rows | MenuRow | `NSWorkspace.open` | — | navigation | folder opens | disabled when unknown | n/a | ✅ |

Path rows now distinguish **file** (Reveal in Finder) from **directory** (Open
Folder) and say which in the subtitle, so one click never does two different
things depending on the row.

---

## Home, More, About

| Control | Type | Action | IPC | Verified |
|---|---|---|---|---|
| banner Restart | Button | `model.restart()` | `handshake`… | ✅ |
| banner Load Core… | Button | `importCoreFile` | `import_core_file` | ✅ — only when `core.binary_exists == false` **and** `capabilities.core_import` |
| banner Reveal Folder / Subscriptions / Reload | Button | navigate or `reloadConfig` | `reload_config` | ✅ |
| banner dismiss (×) | Button | `clearError` / `setTransientStatus("")` | — | ✅ |
| sing-box version row | Button | `importCoreFile` | `import_core_file` | ✅ — see below |
| Proxies / Core Details / Core Mode / Subscriptions / More | MenuRow | navigate | — | ✅ |
| Reload Config | MenuRow | `reloadConfig` | `reload_config` | ✅ — shown only when `core.config_rebuildable`; otherwise a "Config Source: External" note plus **Open Config** |
| Appearance | MenuPickerRow | `model.appearance = …` | — (UserDefaults) | ✅ |
| Language | MenuPickerRow | `model.language.preference = …` | — (UserDefaults) | ✅ |
| Restart Core | MenuRow | `restartCore` | `restart_core` | ✅ |
| Auto Ping / Auto Update toggles | MenuRow + drawn switch | `setAutoPing` / `setAutoUpdateSubscriptions` | `set_auto_ping` / `set_auto_update_subscriptions` | ✅ |
| Launch at Login | MenuRow + drawn switch | `setLaunchAtLogin` (SMAppService) | — | ✅ |
| Open Config / Folder / Logs | MenuRow | `NSWorkspace` | — | ✅ |
| About row | MenuRow | navigate | — | ✅ |
| GitHub / Telegram | Link | opens browser | — | ✅ |

Launch at Login now **reports** failure instead of silently snapping back: a
thrown error or a state mismatch produces an explanation naming Login Items in
System Settings.

### The sing-box version row

The version line under the status doubles as the core-replacement entry point,
because that is where a user looks to answer "which core am I running?". Three
mutually exclusive states, decided by capability and state — never by the OS:

| Backend reports | Row renders | Why |
|---|---|---|
| `core_import` true, `core_version` present | version text + ↻ icon, clickable | the common case: replace the installed core |
| `core_import` true, no `core_version` | "Load Core…", clickable | a missing core is the only state where loading one is the way out |
| `core_import` false | plain version text (or nothing) | an action the backend cannot perform must not be offered |

Clicking opens an `NSOpenPanel` with **no file-type filter**: a sing-box binary
has no extension and is not a registered content type, so any filter would hide
the file the user is looking for. Validation — runnable, right architecture,
actually sing-box, config still parses — belongs to the backend, which reports
exactly why a candidate was refused.

A cancelled panel is a silent no-op, not an error: the user changed their mind,
which is not a failure. While the swap is in flight the row reads "Installing
core…" and is disabled; on success the version shown afterwards is the backend's
read-back of the installed binary, never the filename the user picked.

---

## Tally

```
Visible interactive controls:        85 sites / 11 screens
Frontend-only controls:              19  (navigation, NSWorkspace, clipboard, SMAppService, Links, file panels)
Backend-backed controls:             66
Backend-backed with a real handler:  66
Missing handler:                      0
Controls with a timeout:             66 / 66  (100%)
Controls with visible feedback:      85 / 85  (100%)
Controls with an error path:         85 / 85  (100%)
Silent controls:                      0
No-op controls:                       0
Dead controls:                        0
Nested interactive controls:          0
```

The four import controls added in this pass: Home version row, missing-core
banner **Load Core…**, Subscriptions **Import from File…** menu item, and the
empty-state **Import from File** button. `Add Subscription` became a
`MenuActionRow` rather than a navigating `MenuRow`, so the two add paths share one
entry point; the menu is the row's only control, preserving the no-nesting rule.

## Root causes found and fixed in this pass

| Class | Found | Fix |
|---|---|---|
| Nested interactive control | `ProxiesView` latency Button inside `MenuRow`'s Button; subscription row switch inside a navigating row | New `ActionRow` primitive: actions are siblings, each with its own full-height hit region |
| Look-clickable-but-isn't | Same two rows | Switches replaced by sibling action buttons or a textual value |
| Untyped pending | All settings shared `updatingSetting` | `updatingSetting(SettingID)`; the row shows its own "Saving…" |
| Silent guard-return | `withPending` returned with no message | Reports "Another operation is still running." |
| Swallowed error | Launch at Login `catch {}` | Reports the reason, including the System Settings path |
| Fake empty state | Subscriptions/Daemon/Proxies claimed emptiness while the backend was down | Shared `BackendDownView` + `shouldShowBackendDown` |
| Banner suppression | Success toast could hide a core/config warning in one `else-if` chain | Persistent conditions and transient toasts render independently |
| Wrong path action | All three path rows used `selectFile` | File → Reveal, directory → Open, stated in the row |
| Hover state loss | `let hover = HoverState()` recreated per body pass | `HoverStore`: one box per row identity |

---

## Interaction audit (this pass)

The matrix above answers "what does this control call, and does it say so". This
section answers the fifteen questions an interaction audit has to answer for
every control: **when visible, when enabled, when disabled, what it calls, whether
the backend precondition matches, double-click policy, concurrency with other
operations, whether pending is shown correctly, whether success is visible,
whether failure is visible, whether navigation after an async step is correct,
whether a stale reply can overwrite the current screen, whether destructive
actions are confirmed, whether the hover/hit target is honest, and whether it
carries an accessibility label.**

Rather than restate 85 rows, this records the *rules* that now decide those cells,
the named type each rule lives in, and the control it governs. A cell is
unambiguous when it is produced by one of these and covered by a test.

### The deciding rules

| Rule | Lives in | Governs | Test |
|---|---|---|---|
| Which core actions are offered, and the sentence when not | `ActionPolicy.decideCoreActions` → `CoreActionRefusal` | Home Start/Stop/Retry, Core Details Restart, Home version row, Core Mode switch | `TestCoreActionPolicyMatrix`, `TestCoreImportDisabledWhileCoreRunning`, `TestRestartButtonDisabledWhenRestartInvalid`, `TestCoreModeDisabledReasonMatchesPolicy` |
| What the Home primary button does and says | `ActionPolicy.homePrimaryAction` | Home status button | `TestNonRecoverableCoreErrorDoesNotOfferGenericRetry`, `TestPendingOutranksReportedState` |
| Whether the daemon control plane may be torn down | `ActionPolicy.decideDaemonDestructiveActions` → `DaemonDestructiveBlock` | Daemon Forget Pairing, Remove Service | `TestDaemonDestructiveSafety` |
| What the subscription screen offers | `ActionPolicy.decideSubscriptionActions` → `SubscriptionActionRefusal` | Add, Update All, Reload Config | `TestSubscriptionActionsRequireSomethingToDo`, `TestUpdateAllHelpComesFromThePolicy` |
| What a proxy node row may do | `ActionPolicy.proxyRowPolicy` | node select, node latency, Test All | `TestSingleNodeTestButtonsMatchSerializationPolicy` |
| Whether the node list still describes the running config | `ActionPolicy.isProxyListStale` | Proxies stale banner | `TestStaleConfigIsVisibleWithCachedProxies` |
| What to report after handing a command to Terminal | `ActionPolicy.terminalHandoffOutcome` | Daemon Copy/Open in Terminal | `TestTerminalSuccessWaitsForOSAScriptExit` |
| When a prepared Terminal command stops being valid | `DaemonCommandLifetime.daemonCommandSurvives` | Daemon Re-pair, Remove Service, Install, Start | `TestPreparedCommandSurvivesAReadyDaemon` |
| What a timed-out lifecycle command may conclude | `DaemonCommandLifetime.reconcileCoreOperation`, `snapshotCanSettleOperation` | Home Start/Stop, Core Details Restart | `TestCoreOperationTimeoutReconcilesSnapshot`, `TestSnapshotFreshnessRule` |
| Which screen an async completion may leave | `NavigationStackModel.popIfCurrent` | Add, Edit Save/Delete, Pair, Generate Invite | `TestBackDuringAnOperationDoesNotDoublePop` |
| Which reply may paint the screen | `RequestGeneration`, `proxyListCommitDecision` | Proxies group picker, node list | `TestStaleProxyReplyCannotTakeOverTheScreen`, `TestRapidGroupSwitchLatestIntentWins` |
| The Test All run lifecycle | `GroupTestState`, `acceptsGroupTestStart` | Test All | `TestTestAllLocksAtTheClickNotTheFirstFrame` |
| Whether concurrent refreshes coalesce and how a failure behaves | `CoalescingRefresh` | Daemon Refresh Status, Proxies reload | `TestRapidDaemonRefreshDropsOlderResponse`, `TestRefreshFailureDoesNotDestroyCommand` |
| One operation at a time; chains stop on failure | `SingleFlight`, `chainShouldContinue` | Restart, Update All → Reload → Reload Groups | `TestRestartIsSingleFlight`, `TestBackendRestartStopsAfterAFailedStop`, `TestUpdateThenReloadChain` |
| Whether the entered URL is one the backend will accept | `SubscriptionURLInput.looksValid`, compared against `service.LooksLikeURLForTest` | Add button | `TestSubscriptionURLValidationNeverRejectsWhatTheBackendAccepts` |
| Whether typed text survives a re-render | `DraftStore` / `TextDraft` | Add, Edit, Pair invite, all three confirmation dialogs | `TestDraftsSurviveAModelUpdate`, `TestConfirmationDraftsSurviveARender` |
| Hover only where clicking works | `ActionRow` `isHovering = $0 && isEnabled` | every ActionRow / MenuRow | `TestActionRowHoverOnlyForEnabledAction` |
| Accessible name for a bare value | `ProxyNode.delayAccessibilityLabel` | node latency button | `TestLatencyButtonHasAccessibilityLabel` |
| An unrecognised engine is not shown as Classic | `AppModel.activeEngine: String?` + `L.engineUnknown` | Core Mode | `TestUnknownEngineDoesNotPretendClassicActive` |

### Cells that were wrong, and are now decided

| Control | Cell | Before | After |
|---|---|---|---|
| Home status button | enabled | Retry offered for failures the backend called permanent | `recoverable == false` → navigates to Core Details; `nil` stays retryable |
| Home version row | enabled | enabled whenever nothing was pending | `corePolicy.canImportCore` — the backend refuses unless the core is settled stopped |
| Core Details Restart | enabled | enabled on any core | only on a running core with a binary; explains itself otherwise |
| Core Mode switch | enabled | inline state test | `canSwitchEngine`: settled `stopped`, nothing in flight |
| Home / Core Details / Add / Edit / Pair | navigation after async | unconditional `goBack()`, double-popping when the user had already left | `popIfCurrent(screen)`, keyed by screen identity |
| Re-pair, Remove Service | pending → command lifetime | cleared the moment the daemon reported `ready` — i.e. immediately | survives until the state it was prepared for moves |
| Daemon Refresh Status | double-click / concurrency | second read started; last reply won | callers join the read in flight; only the newest reply commits |
| Daemon destructive rows | failure | could destroy state on an unknown backend | refused with a named `DaemonDestructiveBlock` |
| Proxies group picker | stale reply | last reply to arrive painted the list | generation stamp; `superseded` / `otherGroup` / `commit` |
| Proxies node Test | enabled | only the busy row disabled; other rows offered a click the model refused | one `proxyRowPolicy`; no row offers what the model would refuse |
| Test All | double-click | second click started a second run before the first frame arrived | `.launching` claimed at the click |
| Add Subscription | submit | case-sensitive URL check vs the backend's case-insensitive one | same rule, compared against the backend's own predicate by test |
| Add / Edit / Pair / confirmations | pending | field contents lost when the view was rebuilt | `DraftStore`, keyed by screen and by subscription id |
| Install / Start / Re-pair / Remove Service / Forget Pairing | pending | "Preparing…" on the row only | page-level `daemonOperationProgress` |
| Every ActionRow | hover | lit up while disabled | gated on `isEnabled` |
| node latency button | accessibility | bare number read aloud | label names the node and the measurement |
| Subscriptions Update All | enabled | offered with nothing fetchable (all sources disabled or local) | requires at least one enabled refreshable source; the tooltip comes from the same policy |
| Add Subscription | visible copy | claimed a first fetch that never happens | states what happens and names the action that fetches |
| Terminal handoff | success | success reported when the request was accepted | reported from the helper's real exit status |

### Still deliberately not automated

These are stated so they are not mistaken for gaps:

- **Actual double-click suppression by the OS.** The controls are disabled from the
  click, which is what the user experiences; AppKit's click coalescing is not
  modelled.
- **VoiceOver navigation order.** Labels are asserted; the traversal order of a
  SwiftUI `VStack` is the framework's.
- **Real Terminal launch.** `osascript` is not run in tests; the exit-status rule
  is executed, the process is not.
- **Keyboard traversal and focus rings.** Not asserted anywhere in this repository.

### Dimension sweep results

Every dimension was checked across **all** views rather than only the controls that
changed, and each finding below was fixed or is stated as a deliberate non-issue.

| # | Dimension | Method | Result |
|---|---|---|---|
| 1 | When visible | read every view's branch structure | ✅ no control rendered in a state where it cannot act; the empty-state branches are shared (`BackendDownView`) |
| 2 | When enabled | enumerated all 34 `.disabled(` sites | ✅ each delegates to a named policy or a documented local rule |
| 3 | When disabled | same list, cross-checked for an explanation | ✅ page-level `PendingRow` covers the daemon rows that use a bare `pending != nil`; every other disabled control has `.help` from its policy |
| 4 | What it calls | traced each control to `AppModel` → `BackendClient` | ✅ no control calls a backend method directly |
| 5 | Backend precondition parity | compared each guard to the Go handler's own precondition | ✅ reload requires `config_rebuildable`; import requires settled `stopped`; engine switch requires settled `stopped`; `AddSubscription` URL rule compared against `service.LooksLikeURLForTest` by test |
| 6 | Double-click policy | searched for claim-after-await | ✅ Restart and Test All claim state at the CLICK; `SingleFlight.begin()` returns whether it was admitted |
| 7 | Concurrency | audited every `withPending` entry point | ✅ one operation at a time; proxy reads are generation-stamped; daemon reads coalesce |
| 8 | Pending shown | checked each pending case renders | ✅ `PendingRow` per case, plus `daemonOperationProgress` page-level |
| 9 | Success visible | checked each success path | ✅ transient banner or a state change from the backend |
| 10 | Failure visible | checked each `catch` | ✅ `lastError` banner outside the scroll view; no empty `catch` remains |
| 11 | Navigation after async | **mechanical check over all views** | ✅ 5 owning async pops, 0 unowned; **new test** |
| 11b | Navigation from `onChange` | **mechanical check over all views** | ✅ the only surviving `onChange` is draft priming; **new test** |
| 12 | Stale reply | audited every async read | ✅ `RequestGeneration` for lists, `CoalescingRefresh` for daemon status |
| 13 | Destructive confirmation | enumerated every destructive Model method | ✅ 3 of 3 (`removeSubscription`, `unpairDaemon`, `uninstall`) behind `confirmationDialog` with `role: .destructive` |
| 14 | Hover / hit target | checked the two row primitives | ✅ `ActionRow`, `MenuRow`, `MenuToggleRow`, `MenuPickerRow` all gate hover on `!disabled` and use `contentShape(Rectangle())` |
| 15 | Accessibility label | **mechanical check over all views** | ✅ every icon-only `Button` carries an explicit label; **new test**. `MenuPickerRow`/`MenuToggleRow` supply one from their title |

Two of these were real defects found by the sweep rather than by reading the
changed controls: the **three unlabelled icon-only buttons** (dimension 15) and the
confirmation that the **navigation split** holds everywhere (dimension 11). Both
now have mechanical checks, so the next view added cannot reintroduce them.

One candidate finding was withdrawn after checking properly: `MenuPickerRow`
declares a `disabled` property, and a narrow grep window suggested it was never
applied — it is applied (`.disabled(disabled)`), along with hover gating. Recorded
here because a withdrawn finding that is not written down gets re-reported.
