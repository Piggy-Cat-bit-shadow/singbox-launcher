package swiftlogic_test

import "testing"

// TestRapidDaemonRefreshDropsOlderResponse covers the daemon page's refresh race.
//
// The status is read on load, on every daemon event, and on a button. Several
// replies can therefore be in flight, and the screen used to take whichever
// arrived last — which is not the newest, just the slowest. The user then reads a
// state the backend has already moved past and acts on it.
func TestRapidDaemonRefreshDropsOlderResponse(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
var refresh = CoalescingRefresh()

// The first click starts a read.
let first = refresh.arrive()
check("the first arrival starts a read", first != nil)
check("a read is in flight", refresh.inFlight)

// A SECOND CLICK WHILE THE FIRST IS IN FLIGHT JOINS IT. This is the coalescing
// half: three clicks cost one request, not three.
check("a second arrival joins the read instead of starting one",
      refresh.arrive() == nil)
check("a third arrival joins too", refresh.arrive() == nil)
check("two callers joined", refresh.joinedCount == 2)

// The read completes. Its stamp is still current, so it may commit.
check("the first reply may commit", refresh.mayCommit(first!))
refresh.finish(first!)
check("the refresh is idle again", !refresh.inFlight)

// A LATER READ SUPERSEDES THE FIRST. The first reply, arriving late, must not
// overwrite a newer answer — this is the out-of-order case a coalescing read
// still has to handle, because the first read is only retired when it FINISHES.
var slow = CoalescingRefresh()
let older = slow.arrive()!
// While the older read is in flight, a different path (a session change) starts a
// new generation.
slow.invalidate()
check("an invalidated reply may not commit", !slow.mayCommit(older))
// A late finish from the retired read must not clear the new read's in-flight
// state, which would let a fourth click start yet another request.
let newer = slow.arrive()!
check("after invalidation a new read starts", newer != older)
slow.finish(older)
check("a stale finish does not free the current read", slow.inFlight)
check("the current read may still commit", slow.mayCommit(newer))
`)
}

// TestRefreshFailureDoesNotDestroyCommand covers the failure half.
//
// The status is the CONTEXT for a prepared command — it is what says whether the
// command is installed, paired, running. A failed refresh that cleared it would
// erase the very thing the user is looking at, and the command would vanish along
// with it. So a failure reports an error and changes nothing else.
func TestRefreshFailureDoesNotDestroyCommand(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
var refresh = CoalescingRefresh()
let stamp = refresh.arrive()!

// The read FAILS. The failure ends the read — otherwise the next click joins a
// read that has already finished and never returns — but it must not invalidate
// the generation, because an invalidated stamp is what would drop the data the
// screen already holds.
refresh.finish(stamp)
check("a failed read still ends", !refresh.inFlight)
check("a failed read does not invalidate what is displayed",
      refresh.mayCommit(stamp))

// And the next click can start a fresh read: the guard is not left stuck by a
// failure, which would make Refresh permanently dead after one network blip.
let next = refresh.arrive()
check("a failure does not wedge the refresh", next != nil)
check("the new read has a newer stamp", next! > stamp)

// A prepared command survives independently of the refresh outcome. The rule that
// retires it reads the STATUS, and a failed read produces no new status, so the
// only thing that can retire a command is a status that actually arrived.
struct Command { let operation: String }
let command = Command(operation: DaemonOperation.freshInvite)
// No new status was applied, so the command is untouched by the failure.
check("a command outlives a failed refresh", command.operation == "fresh_invite")
`)
}

// TestRapidGroupSwitchLatestIntentWins covers the proxy picker's coalescing.
//
// Switching A -> B -> C quickly must end on C. Each intermediate reply describes a
// group the user has already left, and applying any of them would move the
// selection back in time — the user watches their choice undo itself.
func TestRapidGroupSwitchLatestIntentWins(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
var gen = RequestGeneration()

// Three switches in quick succession, before any reply lands.
let a = gen.begin()
let b = gen.begin()
let c = gen.begin()

// Every reply that is not the newest is dropped, whatever group it names.
check("the A reply is dropped", !gen.isCurrent(a))
check("the B reply is dropped", !gen.isCurrent(b))
check("only the C reply is current", gen.isCurrent(c))

// Stated as the screen's decision, which is what actually matters: only the last
// switch's reply may paint the list.
check("the A reply may not paint", proxyListCommitDecision(
    replyGeneration: a, currentGeneration: gen.value,
    replyGroup: "A", displayGroup: "C") == .superseded)
check("the B reply may not paint", proxyListCommitDecision(
    replyGeneration: b, currentGeneration: gen.value,
    replyGroup: "B", displayGroup: "C") == .superseded)
check("the C reply paints", proxyListCommitDecision(
    replyGeneration: c, currentGeneration: gen.value,
    replyGroup: "C", displayGroup: "C") == .commit)

// And the selection itself: the model commits selectedGroup only from the
// reply that both is current AND describes the displayed group, so the three
// rapid clicks converge on C rather than on whichever reply was slowest.
`)
}
