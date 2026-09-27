# Import flow audit — Custom Core Import & Local Subscription Import

**Language**: English

End-to-end trace of the two import features, from the Swift control down to the
state write and back. Every claim below is marked with how it was established:

- **read** — verified by reading the code path;
- **IPC** — exercised end to end against the packaged backend helper with a
  throwaway `SINGBOX_LAUNCHER_DATA_DIR` and temporary fixtures;
- **test** — pinned by a named Go test that fails if the property regresses.

Nothing here was verified against the user's live VPN, and no test or manual step
started a real `sing-box run`.

---

## 1. The contract both features share

The frontend selects a file and does nothing else. It does not read the bytes,
guess a format, count nodes, check an architecture, copy a file, or write state.
Every decision about what the selected file *is* is made by the backend, because
a frontend that decided any of it would become a second source of truth.

```
Swift: NSOpenPanel → path (and nothing else)
   │  BackendClient.importCoreFile(path:)           45 s budget
   │  BackendClient.importSubscriptionFile(path:)   30 s budget
   ▼
IPC  { "method": "import_core_file",         "params": { "path": "…" } }
     { "method": "import_subscription_file", "params": { "path": "…" } }
   ▼
Go   Backend.ImportCoreFile / ImportSubscriptionFile
   │   validate → parse/verify → mutate state under the shared lock → respond
   ▼
Swift: renders the RESULT DTO's fields; refreshes from backend state
```

Capability gating is asked, never assumed:

| Capability | Wire key | Swift accessor | Gates |
|---|---|---|---|
| Core import | `core_import` | `model.coreImportAvailable` | Home version row, missing-core banner |
| Local subscription import | `local_subscription_import` | `model.localSubscriptionImportAvailable` | `Import from File…` menu item, empty state |

Both are optional on the Swift side (`Bool?`, defaulting to false) so a backend
predating this feature decodes cleanly and simply does not offer the controls.

---

## 2. Custom Core Import

### 2.1 The transaction

`ImportCoreFile` is ordered so that the installed core is **never** damaged. Each
step either produces a fully validated staging file or returns before anything at
the destination is opened for writing.

| # | Step | Failure code |
|---|---|---|
| 1 | Path checks: non-empty, exists, regular file, size ≤ 256 MiB | `bad_path`, `file_not_found`, `not_regular_file`, `file_too_large` |
| 2 | Platform support | `unsupported` |
| 3 | Core is settled-stopped (not "not running") | `core_busy` |
| 4 | Mach-O architecture (fat slice or thin) | `wrong_architecture` |
| 5 | `SINGBOX_LAUNCHER_CORE` override inactive | `core_override_active` |
| 6 | Target path resolved | `install_failed` |
| 7 | Selected path is not the installed path | `already_installed` |
| 8 | Stage a copy next to the target | `install_failed` |
| 9 | Probe `<staged> version` (3 s cap) parses a version | `invalid_core` |
| 10 | `<staged> check -c config.json` (5 s cap) | `core_config_incompatible` |
| 11 | Atomic rename over the target | `install_failed` |
| 12 | Invalidate the three binary-derived caches, re-resolve | — |
| 13 | Re-read the installed version | *(warning, not fatal)* |

**Step 3 is "settled stopped", not "not running".** A `starting` core is about to
be running and a `stopping` one has not released its process; replacing the
binary underneath either would leave the process and the file disagreeing. The UI
disables the control first; the backend enforces it independently, because hiding
a button is not a safety boundary. *read*

**Steps 4 and 7 are ordered deliberately.** The architecture check runs first, so
a text file selected by mistake is rejected on its own merits (`wrong_architecture`)
rather than reaching the same-path comparison. *read, test*

**Step 5 exists because success would be a lie.** With the environment override
set, installing into the Data core does not change the binary that runs. The
refusal names the variable so the user can act on it. *IPC*

### 2.2 Verified non-destructiveness

The decisive property: a rejected import leaves the installed core **byte-identical**.
Checked by SHA-256 before and after, for every rejection class.

| Scenario | Result | How |
|---|---|---|
| Empty path | `bad_path`, core unchanged | test |
| Missing file | `file_not_found`, core unchanged | test |
| A directory | `not_regular_file`, core unchanged | test |
| Not a Mach-O | `wrong_architecture`, core unchanged | test |
| A valid Mach-O that is not sing-box | `invalid_core`, core unchanged | test |
| The installed path itself | `already_installed`, core unchanged | test |
| Override active | `core_override_active`, core unchanged | test |
| Any failure | no staging leftovers in `bin/` | test |

The `invalid_core` case is the one that cannot be skipped: there is no marker in
a Mach-O that says "sing-box", so the only reliable test is to run the candidate
and look for a version banner. A binary that runs but prints nothing recognisable
is refused. *test*

### 2.3 Cache invalidation

Three caches are derived from the core binary. One of them
(`installedCoreVersionCache`) lives for the whole session and does **not**
self-expire on an mtime change, so a swap that did not invalidate it would leave
the UI reporting the previous version indefinitely.

Verified in a single long-lived backend process, alternating two genuine cores:

```
state#1  core_version=1.14.2-lx.4
import#2 old=1.14.2-lx.4                  new=1.15.0-jiejie-masquerade.6
state#3  core_version=1.15.0-jiejie-masquerade.6
import#4 old=1.15.0-jiejie-masquerade.6   new=1.14.2-lx.4
state#5  core_version=1.14.2-lx.4
```

Both directions track the swap, in-process, with no restart. *IPC*

`core_state_changed` and `daemon_changed` are emitted after a successful import,
so any other observer of the same backend refreshes too. *IPC*

### 2.4 Known unverified path

**No automated test installs a working core.** Doing so would require shipping a
real sing-box binary into the test tree, which the standing rules forbid touching
and which would add ~76 MB to the repository. The success path is therefore
verified **by IPC against the packaged helper** using genuine cores
(`1.15.0-jiejie-masquerade.6` and `1.14.2-lx.4`) and a throwaway data directory,
and what the automated tests pin is every rejection plus non-destructiveness.

This is the honest boundary: *validate, reject, and never damage* are covered by
tests; *install and observe the new version* is covered by manual IPC only.

### 2.5 Config compatibility is a gate, with one deliberate exception

Step 10 runs `check -c config.json` against the staged candidate **before** the
swap and returns `config_checked` / `config_compatible`.

An **incompatible** config is fatal (`core_config_incompatible`, and the message
says the installed core was left unchanged). Installing a core that cannot run
the user's config would only surface at the next Start, when the whole VPN fails
and the cause is no longer obvious — so it is refused up front, while the working
core is still in place. *read*

A **missing** config is not a failure: a fresh install may legitimately import a
core first and add subscriptions afterwards. In that case `config_checked` is
false and the install proceeds. *read*

After the rename nothing is fatal. If the version cannot be read back from the
newly installed binary, that becomes a **warning** on the result rather than an
error: the swap already happened, so reporting failure would be false. The same
path also warns when something outranks the Data core (the active binary is not
the one just installed) and sets `daemon_update_required` when the root-owned
daemon copy is now behind. *read*

---

## 3. Local Subscription Import

### 3.1 Reuse of the existing pipeline

The import runs `DecodeSubscriptionContent` → `MaterializeSubscriptionBody`
(which calls `ParseSubscriptionBody`) — the same two calls a network refresh
makes. Only the byte source differs. *read*

Parsing happens **before** the state lock is taken: a multi-megabyte file can take
a moment, and holding the subscription lock for that long would stall a concurrent
background refresh for no reason. The lock is taken only around
load-mutate-save, and state is re-read inside it so a concurrent writer's changes
are not lost. *read*

### 3.2 A local source is a snapshot

The design decision with the widest consequences. An imported source is a
**snapshot**, not a reference:

| Property | Value | Why |
|---|---|---|
| `input_kind` | `local_snapshot` | the discriminator |
| `url` | *(empty)* | storing a path here would make every existing URL check treat a file as a provider |
| `local_filename` | original basename | display only |
| `kind` | `subscription` | unchanged — it participates in config build like any other source |
| file watching | none | deferred, deliberately |
| persisted absolute path | none | the file may be deleted or moved |

Verified: after importing and then **deleting the source file**, the source still
reports its nodes. *IPC, test*

Legacy records with no `input_kind` read as `remote`, so no migration is needed
and no existing source changes behaviour. *read*

### 3.3 Refreshability has exactly one truth

`state.CanRefreshSubscription` is the single source of truth. Every network fetch
path asks it, rather than testing `url != ""` locally — which would give the right
answer for remote sources and a silently wrong one for local, sending a local
snapshot to the network with an empty URL and producing provider errors the user
never caused. *read*

| Path | Behaviour for a local source | Verified |
|---|---|---|
| `RefreshSubscription` (manual) | refused: `not_refreshable` | IPC, test |
| `Update All` | skipped; reports success, no fetch attempted | IPC |
| auto-update | skipped (2 guard sites) | read |
| startup warm-up / retry | skipped | read |
| config build | **included** — nodes are emitted normally | read |

The `not_refreshable` refusal survives a **rename**, which is the case where a
naive implementation would regress: if the kind were recomputed from the URL,
renaming would still work and the source would quietly become refreshable again.
*IPC, test*

### 3.4 Rejections and their codes

Every rejection leaves `state.json` exactly as it was — no half-created source
showing "0 nodes, error" for the user to find and delete. *test*

| Input | Code |
|---|---|
| Empty path | `bad_path` |
| Missing file | `file_not_found` |
| A directory | `not_regular_file` |
| File over the size cap | `file_too_large` |
| Empty file | `decode_failed` |
| Not decodable as a subscription | `unsupported_format` |
| Decodes but does not parse | `parse_failed` |
| Parses but yields no usable nodes | `no_nodes` |
| State write fails | `save_failed` |

The malformed-input suite — missing, directory, empty path, empty file, non-JSON,
valid JSON with no nodes — passes with zero sources left behind in every case.
*test*

### 3.5 Config ownership is untouched

Import marks the config **stale** (`config_stale: true`) and never rebuilds.
Rebuilding is the user's decision, and for an externally-managed config it must
never happen at all. `config_rebuildable` is passed through unchanged, so a user
whose config is external sees the ownership warning rather than a Reload button
whose only outcome is a refusal. *IPC, read*

The user's own config is external (`config_rebuildable: false`); nothing in this
feature changes that. *IPC*

---

## 4. Defect found and fixed during this audit

**Blank source label for local snapshots.** Two message sites printed `src.URL` as
the source's name:

- `core/rebuild_snapshot.go` — the "source degraded, built without it" warning;
- `core/build_report_feed.go` — the build report's entry subject.

For a local snapshot the URL is empty by design, so both produced a message naming
nothing (`subscription  has no nodes yet`) — precisely the case a user would need
to identify. Both now call the shared `state.SourceLabel`, which falls back
name → URL → filename → id. *read, test*

The fields were also not projected into the DTO: `toSubscriptionDTO` now emits
`input_kind`, `can_refresh` and `filename`, without which the Swift row would fall
back to "remote" and render an empty URL. *IPC, test*

### 4.1 The portable backup format dropped local snapshots

Found by a pre-existing reflection test (`TestSource10CoversStateSourceKeys`) that
requires every `state.Source` field to either travel in the `1.0` backup format or
be declared an exception with a reason. It failed on the two new keys, which
exposed three linked problems rather than one:

1. **The keys were not in the file.** `input_kind` and `local_filename` are
   properties of the *source*, not of the machine that made the backup. Without
   them a snapshot would arrive at the destination as an ordinary subscription
   with an empty URL — a source the UI would offer to refresh with no provider to
   ask. They are now exported and imported; an empty `input_kind` still reads as
   `remote`, so files written before this feature are unaffected.

2. **The nodes were not in the file.** This was the more serious half. The export
   deliberately drops `nodes[]` for subscriptions, because for a *remote* source
   that array is a machine-local provider cache which refills on the first update
   at the destination. A snapshot has no provider: the drop would have produced a
   source that can never be filled. Local snapshots therefore export their nodes,
   while remote subscriptions keep dropping their cache (both behaviours are now
   pinned by tests).

3. **The import ignored them anyway.** `import10` handled
   `SourceKindSubscription` on the assumption that `nodes[]` is always empty in
   the file, routing only the disabled marks into `PendingDisabled`. Even with
   export fixed, a restore would have silently discarded the nodes. The local
   snapshot branch now imports its node list the same way a folder imports its
   members.

The failure mode was the worst kind — a backup that looks restored and is not.
Round-tripping a snapshot through export → parse → import into an empty state now
preserves the nodes, the filename and the non-refreshability, and a remote
subscription still exports without its cache. *test*

---

## 5. Swift side — what each control does

| Control | Gate | IPC | Budget | Success | Failure |
|---|---|---|---|---|---|
| Home version row | `core_import` | `import_core_file` | 45 s | toast + version read-back | error banner |
| Banner **Load Core…** | `core_import` **and** `!binary_exists` | same | 45 s | as above | as above |
| Subscriptions **Import from File…** | `local_subscription_import` | `import_subscription_file` | 30 s | toast "Imported N nodes."; list reloads | error banner, list unchanged |
| Empty-state **Import from File** | same | same | 30 s | as above | as above |

**Budgets.** The backend's worst case is bounded but additive — validate, copy up
to 256 MiB, 3 s version probe, 5 s config check among them. A timeout below that
sum is worse than merely tight: it can fire while the backend is mid-swap, making
the UI report a failure for a swap that actually succeeded. Both imports therefore
carry explicit budgets well above the internal caps, and
`TestSwiftTimeoutBudgets` fails if either loses its explicit entry. *test*

**State after import.** A successful core import calls `refreshCoreState()` and
`loadDaemonStatus()`; a successful subscription import calls `loadSubscriptions()`.
Neither assumes the outcome — both re-read from the backend. *read*

**A cancelled panel is a silent no-op**, not an error: the user changed their mind.
*read*

**No nested interactive controls.** `Add Subscription` became a `MenuActionRow`
whose root *is* the `Menu` — the same rule `MenuPickerRow` follows — rather than a
`Menu` inside `MenuRow`'s `Button`, which would make the inner control
hit-disabled. *read*

---

## 6. Deferred, explicitly

Not implemented, and not implied by anything above:

- watching an imported file for changes, and re-importing on change;
- persisting a security-scoped bookmark or the original path;
- a **Replace from File** action for an existing source;
- downloading a core, or offering a version picker;
- any configurator or diagnostics UI.

---

## 7. Verification summary

| Area | read | IPC | test |
|---|---|---|---|
| Core: validate / reject matrix | ✅ | ✅ | ✅ |
| Core: non-destructive on failure | ✅ | ✅ | ✅ |
| Core: no staging leftovers | ✅ | ✅ | ✅ |
| Core: override guard | ✅ | ✅ | ✅ |
| Core: cache invalidation in-process | ✅ | ✅ | — |
| Core: successful install + version read-back | ✅ | ✅ | *(see §2.4)* |
| Core: config check gates an incompatible config | ✅ | ✅ | — |
| Sub: pipeline reuse | ✅ | ✅ | — |
| Sub: snapshot survives file deletion | ✅ | ✅ | ✅ |
| Sub: refresh refused, incl. after rename | ✅ | ✅ | ✅ |
| Sub: Update All / auto-update skip | ✅ | ✅ | — |
| Sub: malformed input leaves no source | ✅ | ✅ | ✅ |
| Sub: DTO fields for the row | ✅ | ✅ | ✅ |
| Sub: config ownership preserved | ✅ | — | — |
| Sub: survives a backup/restore round trip (nodes + kind) | ✅ | — | ✅ |
| Both: dispatch reachable over IPC | ✅ | ✅ | ✅ |
| Both: Swift covered + budgeted | ✅ | — | ✅ |

Reproduce the automated half:

```
go test ./backend/service/ -run 'TestCoreImport|TestLocalImport|TestServerDispatchesImportMethods|TestSwiftClientCoversEveryMethod|TestSwiftTimeoutBudgets' -count=1
go test ./core/backup/ -run 'TestSource10CoversStateSourceKeys|TestExportCarriesLocalSnapshot|TestLocalSnapshotSurvivesRestore|TestExportStillDropsRemoteSubscriptionCache' -count=1
```
