package swiftlogic_test

import "testing"

// TestBackDuringAnOperationDoesNotDoublePop covers UI-04 and the interaction
// sequences (C) the audit asks for.
//
// Each case is the SAME shape: an operation is started from a screen, the user
// presses Back while it is in flight, and the operation then succeeds. Before the
// fix the success path called `goBack()` unconditionally, so the user's own exit
// and the operation's completion each popped a screen — one save moved them two
// levels, which is how a user ends up somewhere they never navigated to.
//
// The operation now names the screen it expects to leave, and gives up if the
// user has already gone.
func TestBackDuringAnOperationDoesNotDoublePop(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// --- Add Subscription: the user presses Back while the add is in flight. ---
var nav = NavigationStackModel()
nav.push(.subscriptions)
nav.push(.addSubscription)
nav.goBack()                       // the user's own Back
check("the user is back on Subscriptions", nav.currentScreen == .subscriptions)
// The add completes. It must NOT pop again.
_ = nav.popIfCurrent(.addSubscription)
check("UI-04 a completed add does not pop the user's screen",
      nav.currentScreen == .subscriptions)
check("UI-04 the stack is still one deep", nav.path.count == 1)

// The happy path still pops: without this the fix would have broken the normal
// case, which is the failure mode of an over-eager guard.
var happy = NavigationStackModel()
happy.push(.subscriptions)
happy.push(.addSubscription)
_ = happy.popIfCurrent(.addSubscription)
check("a completed add returns to Subscriptions", happy.currentScreen == .subscriptions)

// --- Edit Subscription: same shape, for Save and for Delete. ---
var edit = NavigationStackModel()
edit.push(.subscriptions)
edit.push(.editSubscription("s1"))
edit.goBack()
_ = edit.popIfCurrent(.editSubscription("s1"))
check("UI-04 a completed save does not pop twice", edit.currentScreen == .subscriptions)

var del = NavigationStackModel()
del.push(.subscriptions)
del.push(.editSubscription("s1"))
del.goBack()
_ = del.popIfCurrent(.editSubscription("s1"))
check("UI-04 a completed delete does not pop twice", del.currentScreen == .subscriptions)

// --- Pair: the operation navigates on success, and it is the one that had a
// separate onChange pop as well. ---
var pair = NavigationStackModel()
pair.push(.daemon)
pair.push(.daemonPair)
pair.goBack()
_ = pair.popIfCurrent(.daemonPair)
check("UI-04 a completed pair does not pop twice", pair.currentScreen == .daemon)

// --- The identity check must be EXACT, not positional. ---
// An edit screen popped from the wrong subscription must not match: two edit
// screens are different screens.
var other = NavigationStackModel()
other.push(.subscriptions)
other.push(.editSubscription("s2"))
check("UI-04 popIfCurrent does not match a different subscription",
      !other.popIfCurrent(.editSubscription("s1")))
check("UI-04 the stack is untouched after a non-match", other.currentScreen == .editSubscription("s2"))

// A screen buried below the top must not be poppable by identity either: the
// operation would otherwise remove a screen the user is not looking at.
var buried = NavigationStackModel()
buried.push(.daemon)
buried.push(.subscriptions)
check("UI-04 popIfCurrent does not reach below the top",
      !buried.popIfCurrent(.daemon))
check("UI-04 nothing was removed", buried.path.count == 2)

// An empty stack is safe: no crash, no pop.
var empty = NavigationStackModel()
check("UI-04 popIfCurrent on an empty stack is a no-op", !empty.popIfCurrent(.addSubscription))
check("goBack on an empty stack is safe", { empty.goBack(); return empty.path.isEmpty }())
`)
}

// TestNavigationStackBasics pins the behaviour the ownership rule builds on, so a
// change to the primitives cannot quietly invalidate the cases above.
func TestNavigationStackBasics(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
var nav = NavigationStackModel()
check("a fresh stack cannot go back", !nav.canGoBack)
check("a fresh stack has no current screen", nav.currentScreen == nil)

nav.push(.proxies)
nav.push(.coreDetails)
check("push appends", nav.path == [.proxies, .coreDetails])
check("currentScreen is the top", nav.currentScreen == .coreDetails)
check("canGoBack is true", nav.canGoBack)

nav.goBack()
check("goBack removes the top", nav.path == [.proxies])
check("the previous screen is current", nav.currentScreen == .proxies)

nav.goHome()
check("goHome empties the stack", nav.path.isEmpty)
check("goHome leaves nothing current", nav.currentScreen == nil)
`)
}
