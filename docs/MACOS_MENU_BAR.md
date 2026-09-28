# JiejieBox — macOS menu bar app

**Language**: English

JiejieBox is a native macOS menu bar controller for the sing-box core. It
replaced the previous Fyne desktop UI entirely: there is no Fyne dependency
anywhere in the build, and no second frontend.

```
MenuBarExtra (SwiftUI)  ──JSON over stdio──▶  jiejiebox-backend (Go, headless)
                                                      │
                                                      ▼
                                              sing-box core
```

- The **frontend** is a `MenuBarExtra` window (`LSUIElement` = true: no Dock
  icon, no main window). It renders state and forwards clicks.
- The **backend** is the same Go core logic that used to sit behind Fyne, now
  built with `-tags headless`, which drops the GUI toolkit from the binary
  entirely.

---

## 1. What the app does

| Screen | Purpose |
|---|---|
| **Home** | Status, live speed, Start/Stop, banners, and entries to everything else |
| **Proxies** | Pick a group, search nodes, switch the active node, measure latency |
| **Core Details** | Read-only runtime facts: versions, paths, backend, PID |
| **Core Mode** | Switch between the classic and daemon engines |
| **Subscriptions** | List, add, edit, enable, refresh and delete subscription sources |
| **Daemon** | Set up and diagnose the system-service engine, then activate it |
| **More** | Reload config (when JiejieBox owns it), appearance, automation toggles, open files |
| **About** | Versions and links |

**Navigation invariant.** Every screen draws the same header: `‹ Title … Quit`.
Back is an explicit control, not the system affordance — inside a borderless
`MenuBarExtra` window the system back button is a toolbar item with nowhere to
draw, which is how a page could previously be entered with no dependable exit.
Quit is present on every screen including Home. See `PanelScaffold.swift`.

Deliberately removed: Remote Machines, the configurator/wizard GUI, the full
traffic profiler, the diagnostics GUI, and the Fyne settings/help pages. See
`SPECS/MENU_BAR_CUTDOWN.md` for the audit that records each removal and its
reason.

---

## 2. Building

Requirements: macOS 14+, Swift 6.x command line tools, Go 1.20+.

```bash
# Frontend only (fast iteration)
cd macos && swift build

# Full app bundle
./build/build_macos_app.sh          # SwiftUI frontend → JiejieBox.app/Contents/MacOS
./build/package_macos.sh arm64      # + Go helper, signing, .zip/.dmg

# Verify the shipped artifact
./build/check_macos_artifact.sh dist/<zip> arm64
```

### There is no `.xcodeproj`

The frontend is a **SwiftPM** package (`macos/Package.swift`). `xcodebuild` is
not used and not required; the build machine needs only the Command Line Tools.

### Toolchain constraint: no SwiftUI macros

This matters if you edit the Swift code. The Command Line Tools SwiftPM ships
the Observation macro plugin but **not** the SwiftUI one, so these do **not**
compile here:

```
@State  @Environment  @EnvironmentObject  @Published  @StateObject  @AppStorage
```

The app therefore uses `@Observable` classes with plain stored properties and
explicit `Binding(get:set:)` where a control needs one. Hover state uses a small
`@Observable final class HoverState`. Do not reintroduce the macros above.

---

## 3. Data compatibility

The app reads the **same data directory** as the upstream launcher:

```
~/Library/Application Support/singbox-launcher
├── bin/config.json          sing-box config
├── bin/…                    subscriptions, custom core, wizard state
└── …
~/Library/Logs/singbox-launcher   logs
```

Existing config, subscriptions and custom cores keep working; nothing is
migrated or rewritten on first launch. The bundle identifier stays
`com.piggycat.jiejiebox`.

> **Layout note.** The Go helper lives in `Contents/Helpers/`, not
> `Contents/MacOS/`. `paths.IsAppBundle` therefore accepts anything under
> `Contents/` — recognising only `Contents/MacOS` would make the helper treat
> the bundle as its data directory and report the user's real config as
> missing. `TestIsAppBundle` guards this.

---

## 4. Behaviour worth knowing

- **Quit does not always stop the core.** In classic mode it stops sing-box; in
  daemon mode it leaves it running unless "stop VPN on exit" is configured. The
  backend owns this decision — the UI never pre-stops the core, because doing so
  would defeat daemon persistence.
- **Core mode switches require a stopped core.** Switching engines is a config
  change, not a live migration: a running classic process cannot be handed to
  the daemon. The mode rows are disabled while running and explain why.
- **Latency "—" means never measured.** It is not 0 ms. 0 ms is a valid reading.
- **Test All is sequential.** Firing hundreds of simultaneous handshakes starves
  in-flight traffic, so group testing measures nodes one at a time and can take
  a while on a large subscription.
- **Speed readout only while connected.** The sampler starts and stops with the
  core, so a stopped core costs nothing.
- **Subscriptions are managed in place.** The records live in the canonical v8
  `state.json` source tree — the same records the wizard and the backup importer
  use. Adding a source does **not** rebuild the config: the app says the config
  needs a reload and offers the action, because rebuilding is the user's
  decision.
- **Reload Config is offered only for a config JiejieBox built.** A rebuild
  replays the wizard state, so a config written by hand or by another tool cannot
  be rebuilt here — the app says so and offers "Open Config" instead. Ownership
  is recorded in a marker beside `config.json`, not inferred from the presence of
  a state file.
- **Appearance is a frontend-only preference** (System / Light / Dark), stored in
  `UserDefaults`. It never reaches the backend, and it survives a relaunch.
- **Language is a frontend-only preference** (Follow System / 简体中文 / English),
  stored in `UserDefaults` under `appLanguage` and applied live — switching
  re-renders the open panel without a relaunch. "Follow System" is a distinct
  choice rather than an alias: it resolves against `Locale.preferredLanguages`
  each time, so an app left on it follows a later change to the system language.

  Interface text lives in `Models/Localization.swift` as one `enum L` with two
  exhaustive `switch` tables. Two consequences are deliberate: a missing
  translation is a **compile error** rather than a blank label, and the frontend
  owns only its OWN strings. Backend-authored prose (build reports, daemon
  messages) arrives over IPC already localized by the Go side's locale package —
  re-translating it here would need the backend's context and would drift from it.

  Fonts are system fonts, always: no bundled CJK family, no per-language family
  switch. macOS selects PingFang SC for Chinese and SF Pro for Latin with matched
  optical sizes, and a custom family would break SF Symbols alignment. Tolerance
  for longer translated strings is handled where it belongs — bounded `lineLimit`
  plus `fixedSize(vertical:)` on subtitles — not by changing type.
- **The More page is grouped by kind**, in the order a user looks for things:
  Automation (toggles), Application (appearance, language), Files (actions),
  About. Rows within a section share one primitive each — `MenuToggleRow`,
  `MenuPickerRow`, `MenuRow` — so height, icon column and trailing-control
  position cannot drift apart. A picker row and a toggle row are the same shape,
  which is why Appearance and Language read as one group.
- **Typography and spacing are tokens**, not per-call-site choices. `Typography`
  (in `Views/Typography.swift`) holds the text scale and extends the existing
  `Metrics` with the spacing values. Section headers are inset by `rowPaddingH`
  so `contentInset + sectionHeaderInset` is, by construction, the column the row
  icons occupy — a group label that starts left of its own content reads as a
  stray caption. The Go suite enforces this: a semantic font (`.caption`,
  `.callout`) or a raw point size in a view fails `TestSwiftViewsUseTypographyTokens`.
- **Daemon is set up before it is activated.** The engine needs an installed
  launchd service, a paired identity and a reachable control plane. The Daemon
  screen walks those steps one at a time and only offers "Use Daemon Mode" once
  status says ready. Privileged steps open Terminal, so a `sudo` prompt never
  looks like a frozen app.
- **Proxy listing depends on the engine, and says so.** The Proxies screen reads
  groups and nodes through whichever transport the active engine provides:
  `ClashTransport` (HTTP to the core) in classic mode, `daemonProxyTransport`
  (gRPC) in daemon mode. A daemon built without the group RPC answers
  `Unimplemented`; that is a CAPABILITY, not a failure, and it is reported as
  `supported: false` with a reason rather than as an error. The screen then
  explains the limit and points at Core Mode instead of offering a Retry that
  could never succeed. See [PROXY_DAEMON_AUDIT.md](PROXY_DAEMON_AUDIT.md).

  Three states are kept apart, because they need three different screens:
  `supported=false` (engine cannot), `supported=true, available=false` (engine is
  not up), and `supported=true, available=true` with an empty list (no nodes).
- **Errors are graded by what the user can do about them.** Home shows only
  blocking conditions — core start/stop failure, unreadable config, backend
  unavailable, missing or incompatible core binary. Feature-level failures
  (a proxy read, a latency test, a subscription refresh) stay on the screen they
  belong to and carry their own Retry. The mechanism is that `lastError` is the
  Home banner and the background/derived loaders do not write to it; nothing is
  suppressed, since the owning screen still shows the message.
- **Requests are time-bounded.** Every backend call has a per-method timeout, so
  a lost response surfaces as "Operation timed out" rather than a button stuck
  on "Updating…" forever.

---

## 5. Verifying a build

The automated checks that exist:

```bash
cd macos && swift build                      # frontend compiles
go build ./...                               # backend compiles
go test ./backend/... ./internal/paths/      # protocol + layout contracts
go run ./tools/l10n/l10n_check --strict      # localisation
go run ./tools/l10n/hardcoded_check --strict # hardcoded UI strings
go run ./tools/paths_guard --strict          # path handling rules
go run ./tools/win7guard                     # Win7 toolchain safety
./build/check_macos_artifact.sh <zip> arm64  # bundle contents, arch, size
```

CI runs all of these plus a check that the backend dependency graph contains no
`fyne.io/` package.

### Manual acceptance

Automated screenshot testing is intentionally not used. The owner runs this
matrix by hand against a real core:

| Area | Check |
|---|---|
| Home | Status text, speed readout, Start/Stop enables and disables at the right times |
| Home | Each banner appears and its button works |
| Core | Restart shows progress and the state returns to Connected |
| Core | Mode rows disabled while connected; explanation visible |
| Proxy | Group menu lists groups; selecting one loads its nodes |
| Proxy | Node row switches; checkmark moves; latency button re-measures |
| Proxy | Search filters; count reads "N of M" |
| Proxy | Test All fills every latency; unreachable nodes stay "—" |
| More | Reload Config reports a result; toggles persist across a restart |
| Subscriptions | Add a real URL → appears in the list; duplicate URL is refused |
| Subscriptions | Edit name/URL, toggle enable, refresh one, Update All |
| Subscriptions | Delete asks for confirmation and the row disappears |
| Subscriptions | Empty state offers Add rather than a dead update button |
| Subscriptions | After adding, "Configuration needs reload" appears with Reload |
| Daemon | Status rows match reality (service / pairing / connection) |
| Daemon | Install opens Terminal with a quoted sudo command; Refresh updates |
| Daemon | Pair rejects a malformed invite with a clear message |
| Daemon | "Use Daemon Mode" is absent until status is ready |
| Navigation | Every subpage shows Back; Back returns one level; Home has none |
| Navigation | Quit is visible on every page and never stops a daemon VPN |
| Interaction | No operation can leave a button pending indefinitely |
| Interaction | Every row: hover highlights, click anywhere in the row works, disabled rows look disabled |
| Interaction | No control smaller than ~24 pt; nothing looks clickable but isn't |
