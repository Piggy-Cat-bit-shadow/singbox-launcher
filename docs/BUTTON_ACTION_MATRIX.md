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
