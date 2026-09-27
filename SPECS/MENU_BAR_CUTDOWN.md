# MENU BAR CUTDOWN — Phase 0 audit

Baseline: `3eecb7863926b58ece79d426e4929269f2eef9cd` (main, clean).
Last green CI: run `36324879028`.
All figures below are measured from the repository, not estimated.

## 1. New product definition

A native macOS **menu-bar controller** for sing-box, Apple Silicon only.

- No main window, no sidebar, no dashboard, no Dock icon (`LSUIElement`).
- Lives in the status bar; clicking the icon opens a compact SwiftUI window.
- High-frequency actions only: start/stop, proxy group + node switching,
  latency, subscription update, config reload, core mode.
- Complex configuration stays in the files the user already edits.
- Go is a headless helper process; Fyne is removed entirely.

## 2. Measured baseline

| Metric | Value |
|---|---|
| Fyne GUI executable | 39 081 264 bytes |
| Last Fyne ZIP | 17 675 261 bytes |
| Backend binary (Phase 1) | 46 599 778 bytes |
| Fyne packages in backend dep graph | **41** |

The backend is *larger* than the GUI binary and still pulls 41 Fyne packages.
That is the intermediate state this task must end.

## 3. Dependency analysis — what actually reaches Fyne

The backend's full `singbox-launcher` dependency set was enumerated. Exactly
two edges pull Fyne in:

| Edge | Fyne files | Used by |
|---|---|---|
| `core/uiservice` | 353 lines, imports `fyne.io/fyne/v2`, `app`, `widget`, `fyne.io/systray` | only `core/controller.go` (+ `ui/`, `main.go`) |
| `internal/dialogs` | 1 file with Fyne | `core/controller.go`, `backend_daemon.go`, `elevation.go`, `process_service.go`, `tray_menu.go`, `classic_privileged_*`, `rebuild.go`, `config_service.go` |

Already Fyne-free and reusable as-is:
`internal/traffic` (0 Fyne files), `core/config/**`, `core/state/**`,
`core/build/**`, `core/template/**`, `core/services/**`, `internal/lxdclient`,
`internal/daemonpb`, `internal/platform`, `internal/paths`.

`core/debugapi` has only 2 Fyne-touching files out of 19 — it is close to
headless already and is a candidate to keep for the proxy/core commands.

**Conclusion:** removing Fyne from the backend is a bounded change — delete
`core/uiservice`, make the UI callbacks optional/nullable in `core`, and
remove the Fyne dialog calls (replacing user-visible dialogs with either
backend events or nothing). It is not a rewrite of `core`.

## 4. Feature classification

### KEEP (backend + SwiftUI)

| Feature | Notes |
|---|---|
| Core lifecycle: start / stop / restart / state | Phase 1 IPC already has start/stop; add restart |
| Core backend mode: classic / daemon | Existing semantics unchanged |
| Core version, binary exists, config exists | In snapshot already |
| Config path / dir, log dir / paths | `get_paths` |
| Config reload / rebuild | Existing rebuild path, no GUI |
| Proxy: selector groups, node list, current proxy, switch | Clash API, already in `core/services` |
| Latency: test one proxy, test group | Existing ping logic |
| Subscriptions: update, parser, node processing | Keep; drop the GUI editor |
| Auto ping after connect, auto update subscriptions | Two toggles |
| Lightweight traffic: upload/download rate | Reuse `internal/traffic` if cheap; otherwise drop |
| Logs: open log file / log dir (Swift `NSWorkspace`) | Backend returns paths only |
| Graceful quit; daemon persist policy | Unchanged semantics |
| Launch at Login | Swift-only, `SMAppService` |

### DELETE (product feature removed)

| Feature | Scope of deletion |
|---|---|
| **Remote machines** | `ui/machine_*.go`, `ui/clash_remote.go`, `ui/lxd_remote_override.go`, `ui/remote_*.go`, `ui/local_remote_tabs.go`; `core/services/lxd_remote_*.go`, `lxd_transport_pool.go`; remote registry / pairing / invite / deploy / telemetry / mTLS GUI |
| **Configurator / Wizard** | Entire `ui/configurator/**` (158 files). `configurator/business` and `models` are Fyne-free — audit for logic the runtime needs, migrate that, delete the rest |
| **Full traffic profiler** | `ui/traffic/**`, profiler sessions, connection tracing, charts, dialogs |
| **Diagnostics GUI** | `ui/diagnostics_tab.go`, STUN GUI, IP-check GUI, Mesa toggle, clean-rulesets GUI |
| **Settings page** | `ui/settings_*.go`, Appearance/Language/Storage pages, portable-mode UI |
| **Help / About page** | `ui/help_tab.go`; replaced by a small About sheet |
| **All Fyne UI** | `ui/**`, `internal/fynewidget/**`, `ui/design/**`, `ui/icons/**`, Fyne packaging |

### MIGRATE (logic survives, presentation dies)

| From | To |
|---|---|
| `ui/configurator/business` + `models` (Fyne-free) | `backend/` package if the runtime needs it; otherwise delete |
| Proxy group/node formatting in `ui/servers_*` | Backend DTO fields |
| Latency thresholds / colour decisions | Backend returns the number; Swift decides the colour |
| `core/debugapi` proxy + core commands | Reuse as the backend's command implementation |

### AUDIT LATER (keep only if cheap and genuinely used)

Saved states/profiles · Debug HTTP API · portable mode · autostart core ·
subscription scheduler · core download/update.

Rule: if a feature is not needed for daily proxy control and the backend is
not already mature for it, delete it rather than carry the complexity.

## 5. Environment constraint (verified)

`xcodebuild` fails — *"requires Xcode, but active developer directory is a
CommandLineTools instance"* — and there is no `/Applications/Xcode.app`.
Swift 6.4 / macOS 27.0 / SDK 27.0. SwiftUI builds and runs via **SwiftPM**,
and a scripted `.app` passes `codesign -v` (verified in the previous wave).

Therefore: **`macos/Package.swift`**, not `.xcodeproj`. GitHub's macOS runner
has a full toolchain, so CI is unaffected.

## 6. Risks

1. **Daemon persist policy.** A menu-bar app quits differently from a windowed
   app. The existing rule (core may outlive the GUI in daemon mode) must stay;
   closing the window must not stop the core, and `Quit` must follow the
   current graceful-exit semantics.
2. **User data.** `DataDir` = `~/Library/Application Support/singbox-launcher`,
   `config.json`, subscriptions, custom core, settings, daemon certificates.
   The menu-bar build must reuse the same layout. Bundle ID stays
   `com.piggycat.jiejiebox`.
3. **Feature loss is intentional.** Remote and the Configurator are being
   removed by explicit decision; the report will state it plainly rather than
   presenting the cutdown as a pure win.
4. **Backend size.** Deleting the Fyne edge should shrink the backend
   substantially; the report will record before/after rather than claim it.

## 7. Phases

0. This audit.
1. Remove the `core/uiservice` + `internal/dialogs` Fyne edges; prove the
   backend dependency graph has zero Fyne packages.
2. SwiftPM menu-bar skeleton: `MenuBarExtra`, backend launch, handshake,
   snapshot, start/stop, quit.
3. Backend API: proxy groups, nodes, switch, latency, core mode, paths,
   subscription update, config reload.
4. Swift proxy navigation: groups, nodes, search, test.
5. Core mode + config + subscription actions.
6. Lightweight traffic + More menu.
7. Launch at Login, About, error presentation.
8. Delete Remote, Configurator, profiler, diagnostics GUI, dead packages.
9. Delete all Fyne; `go mod tidy`.
10. CI / packaging / docs cutover.
11. Owner verification on the Action artifact.

---

# Phase 1 RESULT — Fyne removed from the backend

Baseline `3eecb786` → this wave.

## What was done

`core` no longer imports any GUI toolkit. The concrete `AppController.UIService`
field was replaced by `internal/uiport.Port` — an interface owned by a leaf
package, so `core` and the presentation layer can both reference it without a
cycle. The headless backend leaves it nil; a `Headless` implementation absorbs
message calls.

Because the Fyne UI referenced `AppController.UIService` in ~380 places, the
whole Fyne presentation layer was deleted in the same wave (owner decision):
`ui/**`, `internal/fynewidget/**`, `core/uiservice/**`, `internal/dialogs/**`,
`internal/nodewarn/**`, `main.go`, plus the Windows UAC dialog orchestration.

## Verified

| Check | Before | After |
|---|---|---|
| `fyne.io` imports in the repo | many | **0** |
| `fyne` entries in go.mod | 5 | **0** |
| Fyne packages in backend dep graph | 41 | **0** |
| `fyne.io` strings in the backend binary | 3 906 | **0** |
| Backend binary size | 46 599 778 B | 26 615 890 B (−43%) |
| Fyne GUI binary | 39 081 264 B | deleted |
| Go files deleted | — | 382 files, 92 731 lines |

`go build ./...` clean, `go vet ./core` clean, **all 34 test packages pass**,
`paths_guard` and `win7guard` clean. The backend still answers the IPC
protocol correctly (verified by running it).

## Consequences recorded honestly

- **Windows elevation/daemon UI is gone.** `core/elevation.go` and
  `core/classic_privileged_windows.go` keep their logic but their Fyne dialogs
  became `uiport.UIAction` descriptions. Windows was already out of product
  scope.
- **The Configurator, Remote, Diagnostics and Settings GUIs are deleted.**
  `core/config`, `core/state`, `core/build`, `core/template` and
  `core/services/*` survive intact — the config domain was never inside
  `ui/configurator`; the dependency ran the other way.
- **Three registry `refs/go` entries pointed at deleted UI files** and were
  removed; the WireGuard rule text no longer cites a deleted path. Contract
  version left at 1.1.84 because no schema or dictionary semantics changed.
- **Debug API lost its Fyne canvas inspector**, so its UI group no longer
  exists. The proxy and core command surface is untouched.
- Not yet done: the SwiftUI menu-bar app (Phase 2+), CI/packaging cutover,
  docs rewrite. The repo currently has **no runnable GUI**.

---

# KEEP FEATURE ENTRY AUDIT (verified against code, not intent)

Every row below was checked by reading the sources. "Entry exists" means an
interactive SwiftUI primitive is present — not that a feature was planned.

Method columns were produced by grepping `Method*` constants in
`backend/protocol/protocol.go` and the dispatch switch in
`backend/service/server.go`. Control columns come from grepping
`Button(` / `Toggle(` / `Picker(` / `actionRow(` across the Swift views.

## Backend surface — complete inventory

Implemented and dispatched: `handshake`, `get_app_snapshot`, `subscribe`,
`start_core`, `stop_core`, `shutdown`. **Six methods. That is all of them.**

## Swift surface — complete inventory

Every interactive control in the shipped app:

| # | Control | File | Calls |
|---|---|---|---|
| 1 | Start / Stop | `HomeView.swift` | `start_core` / `stop_core` |
| 2 | Restart (backend) | `HomeView.swift` | restart the helper process |
| 3 | Quit | `HomeView.swift` | `shutdown` + terminate |
| 4 | Reveal Config | `HomeView.swift` | `NSWorkspace` |
| 5 | Core Mode row | `HomeView.swift` | navigate to CoreModeView |
| 6 | More row | `HomeView.swift` | navigate to MoreView |
| 7 | Open Config / Folder / Logs | `MoreView.swift` | `NSWorkspace` |
| 8 | Launch at Login | `MoreView.swift` | `SMAppService` |
| 9 | Appearance | `MoreView.swift` | `UserDefaults` |
| 10 | About row | `MoreView.swift` | navigate to AboutView |

## Feature coverage

| Feature | Backend method | SwiftUI entry | Location | Status |
|---|---|---|---|---|
| Core start | `start_core` | Start button | Home | **COVERED** |
| Core stop | `stop_core` | Stop button | Home | **COVERED** |
| Core state + version | `get_app_snapshot` | status line, version | Home | **COVERED** |
| Core restart | — | — | — | **MISSING BOTH** |
| Core mode (read) | `get_app_snapshot` | Core Mode row shows mode | Home | **PARTIAL** |
| Core mode (switch) | — | — | — | **MISSING BOTH** |
| Config path | `get_app_snapshot` | Reveal Config | Home | **COVERED** |
| Config folder | `get_app_snapshot` | Open Config Folder | More | **COVERED** |
| Config reload | — | — | — | **MISSING BOTH** |
| Logs | — (path derived in Swift) | Open Logs | More | **PARTIAL — hardcoded path** |
| Proxy groups | — | — | — | **MISSING BOTH** |
| Proxy list | — | — | — | **MISSING BOTH** |
| Proxy switch | — | — | — | **MISSING BOTH** |
| Latency test | — | — | — | **MISSING BOTH** |
| Update subscriptions | — | — | — | **MISSING BOTH** |
| Auto ping after connect | — | — | — | **MISSING BOTH** |
| Auto update subscriptions | — | — | — | **MISSING BOTH** |
| Traffic rate | — | — | — | **MISSING BOTH** |
| Launch at Login | frontend-only | Toggle | More | **COVERED** |
| Appearance | frontend-only | Picker | More | **COVERED** |
| About | frontend-only | About row | More | **COVERED** |
| Quit | `shutdown` | Quit button | Home | **COVERED** |

## Report

```
Total KEEP features:            22
With a visible entry:            9   (41%)
Missing entry:                  13
Backend methods without entry:   0   (every method has an entry)
Dead Swift entries:              0   (every control reaches code)
KEEP features missing BOTH sides: 10
```

## What this actually says

The SPEC's §118 reverse audit (backend methods with no UI entry) returns
**zero**: all six methods are reachable. That is the good news, and it is only
good because the backend is small.

The real gap is the opposite direction, and it is the one that matters:
**ten KEEP features have neither a method nor an entry.** They are not
"implemented but hidden" — they do not exist in either half yet.

The §129 cutover rule ("only remove Fyne when missing entry = 0") therefore
**cannot be met**, because the Fyne path is already deleted — it was removed in
`8bc50113` as an explicit owner decision before this audit existed. The
consequence is now concrete rather than theoretical: **the shipped app cannot
select a proxy, cannot update subscriptions, and cannot reload config.** Those
are daily-use features, not polish.

Two smaller findings worth recording:

- **Open Logs hardcodes the log directory** in Swift
  (`~/Library/Logs/singbox-launcher`) instead of asking the backend. It works
  today only because that path happens to be correct; it will silently break
  if the layout changes. This is exactly the "backend returns paths" rule the
  cutdown audit set.
- **Core Mode is a read-only display.** The row implies a choice but the view
  cannot change anything, which is the "looks clickable, does nothing"
  pattern §114 forbids.

## Honest status

The architecture is done and verified; the product is roughly 40% migrated by
feature count. Calling this "cutover complete" would be false.

---

# Entry audit — follow-up fixes applied

Two defects the audit found were fixed in the same wave:

1. **Open Logs no longer hardcodes the path.** `SettingsState` gained a
   `logs_dir` field, populated from the resolved layout, and the view reads it
   instead of constructing `~/Library/Logs/...` in Swift. The button is
   disabled when the backend reports no path. This restores the cutdown rule
   that the backend reports locations and the frontend only opens them.

2. **Core Mode no longer pretends to be a choice.** It had a checkmark list
   that looked selectable but had no `set_core_mode` behind it. The rows are
   now plain content, the active mode is labelled "Active" rather than with a
   checkmark, and the footer states that changing the mode is not available.
   Honest about the gap instead of simulating the control.

Still open and not addressed: the ten KEEP features with no backend method and
no entry (restart core, mode switching, config reload, proxy groups/list/
switch/latency, subscriptions update, auto-ping, auto-update, traffic rate).
They are the remaining migration work, not polish.

---

# KEEP FEATURE ENTRY AUDIT — FINAL (post-implementation)

This section supersedes the audit above, which was measured when the backend
still had six methods and the SwiftUI app had ten controls. That snapshot is
kept for the record; the numbers below are the ones that describe the shipped
product.

Measured by grep against the sources, not by intent:

- methods: `Method*` constants in `backend/protocol/protocol.go`
- dispatch: `protocol.Method*` cases in `backend/service/server.go`
- client: `BackendMethod.*` call sites in `Services/BackendClient.swift`
- entries: `model.*` call sites in `Sources/JiejieBox/Views/*.swift`

## Backend surface — 17 methods, all dispatched

`handshake`, `get_app_snapshot`, `subscribe`, `start_core`, `stop_core`,
`restart_core`, `shutdown`, `set_core_mode`, `set_auto_ping`,
`set_auto_update_subscriptions`, `get_proxy_groups`, `get_proxies`,
`switch_proxy`, `test_proxy`, `test_proxy_group`, `reload_config`,
`update_subscriptions`.

Every one appears in the server dispatch switch and is called from the Swift
client. There is no method implemented but unreachable, and no client call to a
method the server does not handle.

## Event surface — 9 events, all decoded

`handshake_ready`, `core_state_changed`, `settings_changed`,
`proxies_changed`, `proxy_selection_changed`, `traffic_rate`, `log_line`,
`error`, `shutting_down`.

## Feature coverage

| Feature | Backend method | SwiftUI entry | Location | Status |
|---|---|---|---|---|
| Core start | `start_core` | Start button | Home | COVERED |
| Core stop | `stop_core` | Stop button | Home | COVERED |
| Core restart | `restart_core` | Restart Core row | More | COVERED |
| Core state + version | `get_app_snapshot` / `core_state_changed` | status line, version | Home | COVERED |
| Core mode (read) | `get_app_snapshot` | Core Mode row + Core Details | Home | COVERED |
| Core mode (switch) | `set_core_mode` | Mode picker (disabled while running) | Core Mode | COVERED |
| Config path | `get_app_snapshot` | Reveal Config, Open Config | Home, More | COVERED |
| Config folder | `get_app_snapshot` | Open Config Folder | More | COVERED |
| Config reload | `reload_config` | Reload Config row | More | COVERED |
| Logs | `get_app_snapshot` (`logs_dir`) | Open Logs | More | COVERED |
| Proxy groups | `get_proxy_groups` | Group menu | Proxies | COVERED |
| Proxy list | `get_proxies` | Node list + search | Proxies | COVERED |
| Proxy switch | `switch_proxy` | Node row (full-width hit target) | Proxies | COVERED |
| Latency test (one) | `test_proxy` | Latency button per row | Proxies | COVERED |
| Latency test (group) | `test_proxy_group` | Test All | Proxies | COVERED |
| Update subscriptions | `update_subscriptions` | Update Subscriptions row | More | COVERED |
| Auto ping after connect | `set_auto_ping` | Toggle row | More | COVERED |
| Auto update subscriptions | `set_auto_update_subscriptions` | Toggle row | More | COVERED |
| Traffic rate | `traffic_rate` event | Live ↓/↑ readout | Home | COVERED |
| Launch at Login | frontend-only (`SMAppService`) | Toggle row | More | COVERED |
| About | frontend-only | About row | More | COVERED |
| Quit | `shutdown` | Quit button | Home footer | COVERED |

## Report

```
Total KEEP features:             22
With a visible entry:            22   (100%)
Missing entry:                    0
Backend methods without entry:    0
Dead Swift entries:               0
KEEP features missing BOTH sides:  0
```

The §129 cutover rule ("only remove Fyne when missing entry = 0") is now
satisfied on its own terms. The Fyne tree was in fact removed earlier, in
`8bc50113`, as an explicit owner decision; the gap that removal opened is
closed by the table above rather than by the rule being waived.

## Deliberate deletions (KEEP → DELETE, with reason)

Recorded so "zero missing" is a measurement rather than an omission:

| Removed | Reason |
|---|---|
| Remote Machines (whole feature) | Owner decision: single-machine menu bar. `Capabilities.remote` is still reported by the backend, but no UI is offered; the capability flag exists for protocol honesty, not as a hidden entry. |
| Configurator / Wizard GUI | Writing config.json is the backend's job; the menu bar links to the file and folder instead. |
| Traffic Profiler (sessions, DNS chains, per-process) | Replaced by the lightweight `traffic_rate` readout. The full profiler needs a window, not a menu bar. |
| Diagnostics GUI, log viewer | Logs are opened in the system viewer; no log parsing UI. |
| Settings page, Help/About pages (Fyne) | Reduced to the rows in More + a single About view. |
| Appearance picker | Frontend-only preference; still present as a preference, not as a "feature". |
| Core-mode stop-switch-restart automation | Not deleted but deliberately *not* implemented: the core refuses to switch while running, so the UI disables the control and explains why. See the mode section below. |

## Semantics that were recovered rather than invented

- **Quit does not stop the core.** `shutdown` reaches `GracefulExit`, and the
  backend decides: classic stops the process, daemon leaves it running unless
  `DaemonStopVPNOnExit` is set. The SwiftUI footer calls `quit()`, which calls
  `shutdownGracefully()` — it never calls `stop_core` first. This is the same
  policy the Fyne build had, reached through the protocol instead of through a
  Fyne callback.
- **Core Mode refuses while running.** `SwitchBackendMode` returns "stop the VPN
  before switching the core engine"; the backend maps it to `mode_locked` and
  the UI disables the row and explains. The switch is a config change, not a
  live migration.

## Test coverage added in this wave

`backend/service/contract_test.go` and `backend/service/proxies_test.go` pin
the wire shapes by raw JSON key, because a rename would break the Swift client
at runtime with no compile error on either side:

- core-state events reach subscribers on a *real* `RunningState` transition,
  with no command involved, and no-op transitions do not emit
- proxy group discovery, default-group selection from `route.final`, and the
  unavailable-vs-empty distinction while the core is stopped
- switch/test refusal codes (`core_not_running`, `bad_request`) and that every
  new method is dispatched rather than answering `unknown_method`
- traffic sampler: core totals preferred over summed connections, counter-reset
  reads as zero, first sample emits no rate, start/stop are idempotent

All fixtures are temporary directories; none touch the user's data directory,
and no test starts or kills sing-box.

## Data-layout regression found and fixed in this wave

Moving the Go half from `Contents/MacOS/` (the old Fyne app) to
`Contents/Helpers/jiejiebox-backend` silently broke bundle detection:
`paths.IsAppBundle` recognised only `<X>.app/Contents/MacOS/…`, so the helper
no longer looked like part of a bundle. The consequence was severe and quiet —
the helper would resolve its data directory to the bundle itself and report the
user's existing `config.json`, subscriptions and custom core as **missing**.

Measured before the fix, running the packaged helper:

```
data_dir:    .../JiejieBox.app/Contents/Helpers
config_path: .../JiejieBox.app/Contents/Helpers/bin/config.json
config_exists: false
```

And after, with the same packaged binary:

```
data_dir:    /Users/jie/Library/Application Support/singbox-launcher
config_path: /Users/jie/Library/Application Support/singbox-launcher/bin/config.json
config_exists: true
core_version: 1.15.0-jiejie-masquerade.5
logs_dir:    /Users/jie/Library/Logs/singbox-launcher
```

`IsAppBundle` now treats anything under `Contents/` as inside the bundle, which
is the property the layout actually depends on. The three call sites (data
directory, layout switch, purge) all benefit, and `TestIsAppBundle` covers the
helper path plus the negative cases.

This is worth recording because it is the class of bug the menu-bar rewrite
invites: the Go half kept working, every test passed, and the failure would
only have appeared when a real user's config was treated as absent.

---

# Navigation, Subscriptions and Daemon completion

Real-device use exposed three product-completeness defects that the earlier
entry audit could not see, because it counted *features* rather than *paths
through the app*.

## The three defects

1. **A page with no way out.** NavigationStack's automatic back button is a
   toolbar item, and a borderless `MenuBarExtra(.window)` panel has nowhere to
   draw it. More (and every other subpage) could be entered and then had no
   dependable exit.
2. **"Update Subscriptions" with no way to enter a subscription.** The KEEP list
   included subscriptions, but the only control was a button that refreshed
   sources the user had no way to create.
3. **Selecting Daemon looked like a freeze.** `set_core_mode("daemon")` was a
   single click that tried to construct a daemon client immediately. The daemon
   needs an installed launchd service, a paired identity and a reachable control
   plane; with none of those present the click stalled instead of explaining.

## What changed

### Navigation

`PanelScaffold` is now the only chrome, used by all 11 screens:

```
┌──────────────────────────────────────┐
│ ‹  Title                         Quit │  ← fixed, never scrolls
├──────────────────────────────────────┤
│  scrollable page content             │
└──────────────────────────────────────┘
```

- **Back is an explicit control**: `model.goBack()`, guarded by
  `canGoBack`. No reliance on the system affordance, and the navigation bar is
  hidden so a second back arrow can never appear.
- **Quit is on every screen**, including Home, from one shared `QuitButton`.
  It calls `model.quit()` → `shutdown` → backend graceful exit, and never
  pre-stops the core, so daemon keep-running semantics are preserved.
- Home passes no `onBack` and shows no back control.

### Subscriptions — a complete loop

| Action | Method | Entry |
|---|---|---|
| list | `list_subscriptions` | Subscriptions screen |
| add | `add_subscription` | Add Subscription |
| edit | `update_subscription` | row → edit screen |
| enable / disable | `set_subscription_enabled` | row switch |
| delete | `remove_subscription` | edit screen, confirmed |
| refresh one | `refresh_subscription` | edit screen |
| update all | `update_subscriptions` | Subscriptions screen |

Records live in the canonical **v8 state source tree** via `state.NewSubscriptionSource`
and `State.Save`. There is no `subscriptions.json` and no second truth. The
frontend receives a compact `SubscriptionDTO`, never a `state.Source`.

An empty state offers Add rather than an update button that does nothing, and a
missing `state.json` is treated as a fresh install (the state package's own
documented meaning of `ErrNotFound`) so the first subscription can be added
without visiting the wizard.

### Daemon — setup before activation

`get_daemon_status` returns a `DaemonStatusDTO` built from the existing
`DaemonStatusSnapshot` and the `DaemonServiceCheck` classifier. It carries no
secret in either direction.

The flow is deliberately two-step:

1. **Setup** (never switches engines): install → start → pair, each returning a
   quoted shell command from the existing `DaemonInstallCommand` /
   `DaemonBootstrapCommand` / `DaemonRepairCommand` / `DaemonUninstallCommand`
   builders. Privileged steps are handed to Terminal instead of running behind a
   spinner, because a `sudo` operation that waits cannot look like anything but
   a hang.
2. **Activate**: `Use Daemon Mode` appears only when status reports
   `installed && paired && reachable`.

The screen offers exactly **one** next step at a time, so it never presents a
checklist of parallel actions. Keeping the VPN alive after quit is phrased
positively in the UI and converted to the stored `DaemonStopVPNOnExit` in the
backend, so Swift never sees the inverted field.

### No endless pending

`BackendClient` now bounds every request with a per-method timeout (8 s for
state reads, 15 s for daemon status, 20 s for engine commands, 120 s for network
work such as provider fetches and latency tests), so a lost response surfaces as
"Operation timed out (…)" and the button returns to its normal state.

### Two bugs found while verifying

- **`toSubscriptionDTO(nil)` panicked.** A nil source now yields an empty DTO.
- **Staleness did not survive a restart.** The dirty markers are in-memory, so
  adding a subscription, quitting and relaunching dropped the "reload needed"
  prompt. `config_stale` now also compares `state.json` against `config.json`'s
  mtime, which is durable. The product rule is unchanged: the backend reports
  staleness and never rebuilds on its own.

## Final audit

Subscriptions:

```
[x] list          [x] add            [x] edit
[x] enable        [x] disable        [x] delete
[x] refresh one   [x] update all
```

Daemon:

```
[x] status        [x] install/update [x] start service
[x] pair          [x] re-pair        [x] mode activate
[x] switch back to classic           [x] keep running after quit
[x] error recovery (one next step + refresh, plus Open Core Folder
    when the installed core has no lxd subcommand)
```

Navigation:

```
[x] every child page has Back       [x] every page has Quit
[x] no dead-end route               [x] no duplicate navigation controls
```

Verified mechanically: all 10 `Screen` cases have at least one entry
(`grep path.append`), all 10 subpages call `model.goBack()`, all 11 screens use
`PanelScaffold`, and all 31 backend methods are dispatched and called by the
client except `subscribe`, which the transport handles by opening the event
stream.
