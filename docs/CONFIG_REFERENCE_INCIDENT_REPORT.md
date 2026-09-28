# Incident report — `default outbound not found: proxy-out`

**Language**: English
**Baseline**: `0afd096c1cda04b5689a993e9d449846dd605c4e`
**Failing Action**: [run 36453455061](https://github.com/Piggy-Cat-bit-shadow/singbox-launcher/actions/runs/36453455061)
**Fixed HEAD**: `c9d07ec3ea4bf438cee2fc87d747277e8f600ad9`
**Analysis**: [CONFIG_REFERENCE_INCIDENT.md](CONFIG_REFERENCE_INCIDENT.md)

---

## 1. Root cause

The reported hypothesis was that outbound generation deletes the required
`proxy-out` selector, that repair mutates a temporary object, and that a later
rebuild overwrites the fix.

**Measured, none of those is what happens.** Three separate defects combined:

| # | Defect | Effect |
|---|---|---|
| 1 | The **zero-node guard** aborted generation before the validity pass | `proxy-out` never existed to be kept when no node was usable |
| 2 | `RepairRouteFinal` reset a vanished GROUP to `direct-out` | a config that started and silently sent all traffic direct |
| 3 | **Nothing validated the config on the delivery path** | a stale/broken `config.json` reached the daemon unchecked |

Defect 1 is the root cause of the reported symptom; 2 and 3 are why it was
possible for a broken or silently-wrong config to reach a running system.

## 2. Why `proxy-out` disappeared

Not the selector logic. The three-pass validity computation was already correct —
a `required: true` selector with `addOutbounds: ["direct-out"]` counts that
constant and stays valid.

What killed it was **earlier**: `GenerateOutboundsFromParserConfig` returned
`no nodes parsed from any source` from the zero-node guard, before pass 2 ran at
all. With an empty subscription there was no `proxy-out` to keep, so
`route.final` (which names it) was left dangling.

Measured: with one node the generator emits `proxy-out` containing
`[proxy-out-auto, direct-out, node-a]`; with that node disabled it refused
outright.

**And `required` was read by nobody.** `template.RequiredOutboundTags()` existed —
including a legacy `wizard.required: 1` fallback — with **zero production
callers**. The marker was parsed, carried, and consulted by nothing.

## 3. Why reference repair did not take effect

**It did.** This part of the report is a correction:

- `RepairRouteFinal` mutates the **decoded config object**, and
  `finalizeReferences` **re-emits the bytes from that repaired object**
  (`b.Reset(); b.Write(out)`), so the file on disk is the validated one.
- The reject-loop rebuild calls the **whole** `BuildConfig`, repair included, so a
  second build does not restore the old value.

The real problem was that the repair produced a **wrong-but-valid** config:
`route.final → direct-out`. That starts, validates, and disables the tunnel
silently, with one warning line. Worse than refusing, because the user believes
the VPN is on.

## 4. Multiple sources of truth

**No.** There is one canonical input (persisted state + `state.json`) and one
generated artefact (`config.json`). That design is sound.

The defect was that `config.json` **on disk** was treated as authoritative at
launch, and it can predate the state it was built from — because the pre-start
rebuild **declines to run** when the config is unmanaged or `state.json` is
absent (`rebuildConfigBeforeStart` returns `nil`), and then the file is used
as-is.

## 5. Canonical state design

Unchanged, deliberately. The missing piece was a **gate**, not a second model:

```
state.json (canonical user intent)
    → Generate  (required selectors honoured)
    → Normalize
    → Repair    (only when the launcher OWNS the output)
    → Validate  (ValidateConfigReferences)
    → Serialize
    → Atomic write
    → [GATE: validate the exact bytes]  ← was missing
    → Launch
```

## 6. Files changed

```
core/config/outbound_generator.go    zero-node guard exception for a self-sufficient required outbound
core/config/outbound_validity.go     hasSelfSufficientRequiredOutbound + documented constant credit
core/config/required_outbound_test.go NEW — required-selector lifecycle
core/build/ref_integrity.go          group-aware repair refusal; download_detour; report helpers
core/build/build.go                  pass the template's declared groups into the repair
core/build/ref_integrity_test.go     repair-refusal + download_detour tests
core/build/node_sections_build_test.go  fixture made consistent with the generator
core/backend_daemon.go               shared pre-launch gate on the delivery path
core/process_service.go              the same gate on the classic start path
core/prelaunch_gate_test.go          NEW — gate behaviour + Classic Mode contract
core/exit_reason.go                  dangling reference is a CONFIG failure, not a missing file
core/exit_reason_test.go             incident-message classification tests
backend/cmd/jiejiebox-backend/checkconfig.go  NEW — `-check-config` pipeline smoke test
backend/cmd/jiejiebox-backend/main.go         the flag
backend/service/maintenance.go       RebuildConfigForCheck / ConfigPath
build/check_macos_artifact.sh        runs the smoke test on the packaged artifact
macos/.../Models/Localization.swift  stale-service explanation (EN + ZH)
macos/.../Views/DaemonView.swift     stale service explains itself
internal/swiftlogic/staleservice_test.go  NEW — the above, executed
docs/CONFIG_REFERENCE_INCIDENT.md    root cause analysis
```

## 7. Tests added / changed

**Added**

- `TestRequiredSelectorSurvivesAnEmptySubscription` — the incident's first half
- `TestRequiredSelectorWithNoNodesStillRoutes`
- `TestNonRequiredEmptySelectorStillDrops` — pins that the fix credits the
  DECLARATION, not disabling the empty-selector rule
- `TestRequiredCreditIsNotInvented` — no invented members
- `TestRepairRefusesToRedirectAGroupToDirect` — the incident's second half
- `TestRepairStillRedirectsANonGroupNode` — a vanished server is still repairable
- `TestRepairPrefersASurvivingGroupOverTheDeclarationCheck`
- `TestDeclaredGroupTagsReadsRequiredGroups` — the previously-dead accessor is wired
- `TestPreLaunchGateBlocksTheIncidentConfig` / `…NamesTheProblemAndTheAlternatives`
  / `…PassesAValidConfig` / `…CatchesOtherDanglingReferences` (5 sub-cases) /
  `…DoesNotFailOnAMissingFile` / `…NeverRewritesTheConfig` /
  `…ReportsRatherThanRepairsUnrepairable`
- `TestIncidentExitMessageIsAConfigFailure` / `TestMissingFileStillClassifiedAsResource`
  / `TestConfigReferenceFailureDoesNotRestart`
- `TestRemoteRuleSetDownloadDetourIsChecked`
- `TestStaleServiceExplainsItself` / `TestStaleServiceCopyExistsInBothLanguages`

**Changed**

- `ref_integrity_test.go`: the six existing `RepairRouteFinal` calls now pass
  `nil` for the declaration — the unknown-declaration path, whose behaviour is
  unchanged. **No existing test was deleted or weakened.**
- `node_sections_build_test.go`: the hand-written cache now includes the required
  selector the generator emits, so the fixture models a state the app can
  actually produce.

Every new test was verified to FAIL on a revert of its fix.

## 8. Classic Mode

**Affected and now covered — with the opposite policy, by design.**

Classic runs a config the user wrote. The shared gate **validates and reports**
and never repairs: silently rewriting a hand-written config would change the
user's routing without telling them, which is the same class of harm as the
original defect pointed the other way. `TestPreLaunchGateNeverRewritesTheConfig`
asserts the bytes on disk are identical after a refusal.

Generated configs may still be repaired, because there the launcher owns the
output. The difference is **ownership**, and both halves are pinned by test.

Classic also inherits the gate on its own start path (`ProcessService.StartContext`).

## 9. daemon/service staleness

Investigated as a **separate** issue and deliberately **not** conflated with this
incident.

Detection already existed (`DaemonServiceCheck.NeedsInstall` covers `unsafe`,
`stale` and `process_stale`, with `ServiceState`/`ServiceDetail` on the wire) — but
the SwiftUI side **decoded `service_detail` and displayed it nowhere**, so a
first-time install and a service that no longer matches the app showed the same
generic subtitle. The install step now derives its subtitle from the backend's own
reason.

The `default outbound not found` error was **not** caused by a stale daemon. They
are independent, as the request asked to be checked.

## 10. Restart policy

Already correct, for the **wrong reason**. Deterministic failures already stopped
auto-restart — but `reMissingFile` matched a bare `not found`, which swallowed
every dangling-reference message. The incident's fatal was classified
`exitReasonMissingResource`, so the user was told to check their **file paths**
for a fault in `route.final`.

The verdict was deterministic by accident, which is why nobody noticed: the loop
stopped and only the remedy was wrong. Named reference phrases now have their own
pattern, checked first, and `reMissingFile` is narrowed to filesystem-shaped
messages. The test asserts the **reason**, not merely "deterministic" — asserting
the latter would have passed against the bug.

## 11. CI runtime smoke test

`backend/.../checkconfig.go` adds `-check-config`, which runs the real pipeline
with no core started:

```
load the bundled template → build config.json → validate references
  → assert route.final resolves, by name → sing-box check (when a core is given)
```

`build/check_macos_artifact.sh` runs it on the packaged artifact. Three design
points, each of which could have made it useless:

- it uses the **shipping template**, not a fixture (a fixture agrees with the code
  by construction and could never catch template/code drift);
- it seeds a one-node subscription so the required-selector path actually runs;
- it seeds global outbounds into `state.Directions` — writing
  `ParserConfig.Outbounds` *looks* right and silently does nothing, and the smoke
  test caught that in its own seeding.

`sing-box check` is optional and its absence is **reported**, never assumed away.

## 12. `go test ./...`

Clean. 38 packages ok.

## 13. `go vet ./...`

Clean (`./core/... ./api/... ./internal/... ./backend/...`, as CI runs it).
`go test -race` on the CI package set: clean. Guards: l10n 0 hard fails,
paths_guard 0 findings, win7guard 417 files.

## 14. macOS Action URL

https://github.com/Piggy-Cat-bit-shadow/singbox-launcher/actions/runs/36459244649

## 15. Action result

**success** — for `c9d07ec3`. The artifact acceptance log contains:

```
--- config pipeline smoke test ---
PASS  config pipeline: generated config passes reference integrity
      references: OK
      route.final: resolves
      sing-box check: SKIPPED (no -core given)
ALL CHECKS PASSED
```

## 16. Final HEAD SHA

`c9d07ec3ea4bf438cee2fc87d747277e8f600ad9`

## 17. Working tree

Clean (only the untracked, gitignored `.build/`).

---

## Required question A

> If a user deletes the outbound that `route.final` points at, what happens now?

**Generated (Wizard) configs — the group is kept, or the build refuses:**

1. If the deleted outbound was a `required: true` selector with its own
   `addOutbounds`, it is **not** deleted from the config: it survives holding its
   declared fallback (`direct-out`) and any remaining nodes. `route.final` keeps
   resolving. **The core starts and routes.**
2. If the required selector has no fallback and no nodes, generation fails with an
   explicit error — no config is written.
3. If a non-required group vanishes and no group survives, `RepairRouteFinal`
   **refuses** (rather than pointing the catch-all at `direct-out`), and the build
   fails naming the tag and listing the available outbounds.

**Any config, at launch:** the shared gate re-reads the file and refuses to
deliver it, reporting the path, the missing tag and the alternatives:

```
Configuration is invalid, so it was not sent to the daemon.
route.final: missing_target "proxy-out": the catch-all outbound does not exist…

Available outbounds:
- direct-out
- block-out
- 🌍 国外流量
```

It does **not** auto-restart (`config invalid` is deterministic).

## Required question B

> Why can "CI green but Start fails with `default outbound not found`" not happen again?

Four independent reasons, each of which would have to fail simultaneously:

1. **The class is fixed at its source.** A required outbound survives an empty
   node set, so the tag `route.final` names is not silently dropped.
2. **The repair can no longer paper over it.** It refuses rather than redirecting
   traffic to `direct-out`, so a wrong config cannot be traded for a silent one.
3. **Validation runs where the config is delivered**, not only where it is built —
   on the exact bytes about to be used, on both engines. The pre-start rebuild can
   be skipped; the gate cannot.
4. **CI now produces a configuration.** Artifact acceptance runs the real pipeline
   on the packaged artifact and fails if it cannot build a reference-sound config.
   A green build is no longer accepted as evidence that the app can start.

The residual risk is honest and narrow: `sing-box check` is skipped in CI because
no core is shipped, so **the core's own schema acceptance is not verified in CI** —
only internal reference integrity is. The flag accepts `-core`, so wiring a test
core in would close it.
