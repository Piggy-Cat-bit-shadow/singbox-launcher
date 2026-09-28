# Interaction audit — defect traceability

**Language**: English

The interaction audit was carried out against a numbered list of 45 reported
defects (`UI-01`…`UI-45`). The list itself lived in the task report and is **not
stored in this repository**, so this file records what can and cannot be tied back
to an individual number.

It exists because the honest answer is not "all 45 are fixed". The mechanisms the
report described are implemented and executed under test; the per-number mapping is
partly unrecoverable, and saying so is more useful than implying otherwise.

## How to read this

| Status | Meaning |
|---|---|
| **Anchored** | The number appears in the code or tests that fix it, so the link is verifiable by searching. |
| **Behavioural** | The defect's subject is implemented and tested, but the code does not carry the number. The mapping is by description, not by marker. |
| **Unverifiable** | The number cannot be matched to anything with confidence. No claim is made about it. |

## Anchored

`UI-01`, `UI-03`, `UI-04`, `UI-05`, `UI-06`, `UI-07`, `UI-15`, `UI-16`, `UI-17`,
`UI-18`, `UI-19`, `UI-20`, `UI-21`, `UI-22`, `UI-23`, `UI-24`, `UI-28`, `UI-30`,
`UI-31`, `UI-35`, `UI-41`, `UI-44`.

These are greppable:

```
grep -rn "UI-01\|UI-03\|..." macos/ backend/ internal/ docs/
```

`UI-30` and `UI-44` were anchored in a later pass, having been fixed without a
marker: the fix for `UI-30` is `b.noteBuildInputsChanged()` in `AddSubscription`,
and `UI-44` is the Add screen's copy, which had promised a first fetch that never
happens.

## Behavioural

These numbers cannot be matched to code, but their **subject matter** is
implemented and covered. Listed with the mechanism, not a claim of per-number
coverage:

| Number(s) | Subject as understood | Where it lives | Test |
|---|---|---|---|
| `UI-09`…`UI-14` | error/failure surfacing, pending indication | `ActionFeedbackStrip`, `PendingRow`, `daemonOperationProgress`, `lastError` | `TestRefreshFailureDoesNotDestroyCommand`, `TestDaemonDestructiveSafety` |
| `UI-25`…`UI-27` | policy extraction, enable/disable matrix | `Models/ActionPolicy.swift` | `TestCoreActionPolicyMatrix`, `TestSubscriptionActionsRequireSomethingToDo` |
| `UI-29` | draft/text-field ownership | `Models/DraftStore.swift` | `TestDraftsSurviveAModelUpdate`, `TestConfirmationDraftsSurviveARender` |
| `UI-32`…`UI-34` | stale responses, generation ownership | `Models/RequestGeneration.swift` | `TestStaleProxyReplyCannotTakeOverTheScreen`, `TestRapidDaemonRefreshDropsOlderResponse` |
| `UI-36`…`UI-40` | single-flight, chained recovery, safety confirmation | `Models/SingleFlight.swift`, `decideDaemonDestructiveActions` | `TestRestartIsSingleFlight`, `TestUpdateThenReloadChain`, `TestDaemonDestructiveSafety` |
| `UI-42`, `UI-43` | feedback visibility, command lifetime | `DaemonCommandLifetime`, `ActionFeedbackStrip` | `TestPreparedCommandSurvivesAReadyDaemon` |

A number appearing in this table means "the subject is covered", **not** "this
specific case was verified". Where the original text described a case these
mechanisms do not reach, it would not have been caught.

## Unverifiable

`UI-02`.

The audit's fifteen dimensions and the named control inventory are all swept and
recorded in [BUTTON_ACTION_MATRIX.md](BUTTON_ACTION_MATRIX.md), with a mechanical
check behind the dimensions that can be checked mechanically (navigation ownership,
icon-only accessibility labels). `UI-02` cannot be placed in either table with any
confidence, and no claim is made about it.

## What this means for the work

- Every defect whose **subject** was identified has a named rule, a named type and
  a test that executes the shipping implementation.
- Each test was verified to fail when its fix is reverted.
- The per-number coverage is partial, and the gap is listed above rather than
  absorbed into a success claim.

If the original numbered list is available, re-running it against this tree is the
way to close the gap: the mechanisms are in place, so any remaining item should be
a matter of mapping rather than of new architecture.
