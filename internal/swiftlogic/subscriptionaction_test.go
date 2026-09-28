package swiftlogic_test

import (
	"regexp"
	"strings"
	"testing"
)

// TestSubscriptionActionsRequireSomethingToDo covers UI-45's remaining policies.
//
// "Update All" needs something it can actually update. A disabled source, or one
// whose input is a local snapshot with no URL, cannot be fetched — offering the
// control anyway promises a network read that cannot happen, and the user gets
// either a silent no-op or an error about a URL they never had.
func TestSubscriptionActionsRequireSomethingToDo(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// The normal case: one enabled remote source, a rebuildable config, nothing busy.
let ok = decideSubscriptionActions(busy: false, configRebuildable: true,
                                   refreshableEnabledCount: 1, totalCount: 1)
check("Update All is offered when something can be fetched", ok.canUpdateAllSubscriptions)
check("a rebuildable config can be reloaded", ok.canReloadConfig)
check("adding is offered when idle", ok.canAddSubscription)
check("nothing to explain", ok.updateAllReason == nil)

// Every source is a local snapshot: there IS a list, but nothing can be fetched.
let localOnly = decideSubscriptionActions(busy: false, configRebuildable: true,
                                          refreshableEnabledCount: 0, totalCount: 2)
check("UI-45 Update All is withheld when nothing is refreshable",
      !localOnly.canUpdateAllSubscriptions)
check("UI-45 and the reason names refreshability",
      localOnly.updateAllReason == .nothingRefreshable)
// Reload is still available: rebuilding the config does not need a fetch.
check("reload is unaffected by refreshability", localOnly.canReloadConfig)

// An EMPTY list: no subscriptions at all. The empty state accounts for the screen,
// so there is nothing to explain — a reason here would be noise.
let empty = decideSubscriptionActions(busy: false, configRebuildable: true,
                                      refreshableEnabledCount: 0, totalCount: 0)
check("Update All is withheld on an empty list", !empty.canUpdateAllSubscriptions)
check("an empty list explains nothing", empty.updateAllReason == nil)

// A busy app: everything is withheld and the reason is the operation.
let busy = decideSubscriptionActions(busy: true, configRebuildable: true,
                                     refreshableEnabledCount: 3, totalCount: 3)
check("a busy app withholds Update All", !busy.canUpdateAllSubscriptions)
check("a busy app withholds reload", !busy.canReloadConfig)
check("a busy app withholds adding", !busy.canAddSubscription)
check("a busy app names itself as the reason", busy.updateAllReason == .busy)

// A HAND-WRITTEN CONFIG. The backend refuses to overwrite it, so a reload control
// would offer an action that is guaranteed to fail.
let handWritten = decideSubscriptionActions(busy: false, configRebuildable: false,
                                            refreshableEnabledCount: 1, totalCount: 1)
check("UI-45 a non-rebuildable config cannot be reloaded", !handWritten.canReloadConfig)
check("UI-45 and reload says why",
      reloadConfigRefusal(busy: false, configRebuildable: false) == .configNotRebuildable)
// Updating subscriptions is unaffected: fetching does not touch the config file.
check("updating is still offered for a hand-written config",
      handWritten.canUpdateAllSubscriptions)

// The busy reason outranks the refreshability reason, so a user mid-operation is
// told about the operation rather than about their sources.
let busyAndEmpty = decideSubscriptionActions(busy: true, configRebuildable: false,
                                             refreshableEnabledCount: 0, totalCount: 2)
check("busy is reported before refreshability", busyAndEmpty.updateAllReason == .busy)

// The exhaustive invariant: whenever the app is busy, NOTHING is offered. This is
// the property that makes the controls and the model's one-at-a-time guard agree.
var violations = 0
for rebuildable in [true, false] {
    for refreshable in [0, 1, 5] {
        for total in [0, 1, 5] {
            let p = decideSubscriptionActions(busy: true, configRebuildable: rebuildable,
                                              refreshableEnabledCount: refreshable,
                                              totalCount: total)
            if p.canReloadConfig || p.canUpdateAllSubscriptions || p.canAddSubscription {
                violations += 1
            }
        }
    }
}
check("UI-45 a busy app offers nothing", violations == 0)

// And a withheld Update All with sources present ALWAYS carries a reason, so the
// user is never shown a disabled control with nothing to read.
var unexplained = 0
for refreshable in [0, 1, 5] {
    for total in [0, 1, 5] {
        let p = decideSubscriptionActions(busy: false, configRebuildable: true,
                                          refreshableEnabledCount: refreshable,
                                          totalCount: total)
        if !p.canUpdateAllSubscriptions && total > 0 && p.updateAllReason == nil {
            unexplained += 1
        }
    }
}
check("UI-45 a withheld Update All is explained when a list is shown", unexplained == 0)
`)
}

// TestUpdateAllHelpComesFromThePolicy pins that the control's tooltip is derived
// from the policy that disables it.
//
// A locally-chosen string keeps saying "nothing to refresh" after the rule starts
// refusing for a different reason — a busy app, an unrebuildable config. The user
// then reads an explanation for a condition that is not the one stopping them,
// which is worse than no explanation: it points at the wrong remedy.
func TestUpdateAllHelpComesFromThePolicy(t *testing.T) {
	code := swiftCode(repoFile(t, "macos/Sources/JiejieBox/Views/SubscriptionsView.swift"))

	if !strings.Contains(code, "updateAllHelp") {
		t.Fatal("the Update All row does not read a help value from the policy")
	}
	// The tooltip must be built by switching on the policy's reason, not by testing
	// the enabled flag directly.
	if regexp.MustCompile(`\.help\(\s*model\.canUpdateAllSubscriptions`).MatchString(code) {
		t.Error("the Update All tooltip is chosen from the enabled flag instead of the " +
			"policy's reason, so it can explain a condition that is not the one refusing")
	}
	// Every reason must map to a distinct message: collapsing them would tell a
	// busy user to check their subscriptions.
	region := code[strings.Index(code, "private var updateAllHelp"):]
	if end := strings.Index(region, "\n    }"); end > 0 {
		region = region[:end]
	}
	if !strings.Contains(region, "updateAllReason") {
		t.Error("updateAllHelp does not consult the policy's reason")
	}
	for _, c := range []string{".busy", ".nothingRefreshable", ".configNotRebuildable"} {
		if !strings.Contains(region, c) {
			t.Errorf("updateAllHelp does not handle %s, so that refusal has no message", c)
		}
	}
}
