package swiftlogic_test

import "testing"

// TestStaleProxyReplyCannotTakeOverTheScreen covers UI-15 / UI-17 and the audit's
// interaction scenarios (E) and (F): a slow reply for group A arriving after the
// user has already chosen B.
//
// This is the defect class that rendering bugs hide behind. The screen does not
// crash and shows nothing obviously wrong — it simply shows a DIFFERENT group's
// nodes than the one selected, for as long as it takes the user to notice. The
// only way to catch it is to decide it explicitly, which is what the extracted
// rule does.
func TestStaleProxyReplyCannotTakeOverTheScreen(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
var gen = RequestGeneration()
check("a fresh generation has value zero", gen.value == 0)

// SCENARIO E. The user asks for A, then for B. A is slow.
let a = gen.begin()
let b = gen.begin()
check("a newer request supersedes the older", !gen.isCurrent(a))
check("the newest request is current", gen.isCurrent(b))

// A's reply finally arrives. It is current-stamped FALSE, so it may not commit —
// even though its group name happens to match what A asked for.
check("UI-15 the slow reply for A is dropped",
      proxyListCommitDecision(replyGeneration: a, currentGeneration: gen.value,
                              replyGroup: "A", displayGroup: "B") == .superseded)

// B's reply commits.
check("UI-17 the reply for the displayed group commits",
      proxyListCommitDecision(replyGeneration: b, currentGeneration: gen.value,
                              replyGroup: "B", displayGroup: "B") == .commit)

// SCENARIO F. The reverse order: B is fast and lands FIRST, then A crawls in.
// B committing does not retire A's stamp by itself, but the moment the user
// requested B, A's stamp was already superseded — so the hazard is the same and
// the answer is the same.
check("UI-15 a fast later request still supersedes the slow earlier one",
      proxyListCommitDecision(replyGeneration: a, currentGeneration: b,
                              replyGroup: "A", displayGroup: "B") == .superseded)

// A reply for a group the user is NOT displaying, carrying a CURRENT stamp, is
// neither superseded nor allowed to paint the list. It still has metadata worth
// keeping, which is why "drop the list" and "drop the reply" are different
// answers rather than one boolean.
check("a current reply for another group does not paint the list",
      proxyListCommitDecision(replyGeneration: b, currentGeneration: gen.value,
                              replyGroup: "C", displayGroup: "B") == .otherGroup)

// The enum must remain distinguishable: collapsing otherGroup into superseded
// would silently drop capabilities the screen needs.
check("otherGroup and superseded are different outcomes",
      ProxyListCommitDecision.otherGroup != ProxyListCommitDecision.superseded)

// Invalidation retires everything outstanding, which is what a config rebuild or
// an engine switch needs: a reply from the previous world describes something
// that no longer exists.
gen.invalidate()
check("UI-17 invalidation retires the outstanding reply", !gen.isCurrent(b))
check("invalidation issues a new stamp", gen.value > b)
`)
}

// TestGenerationIsMonotonic pins that stamps never repeat.
//
// A wrapping or reused stamp would let a reply from an earlier request compare
// equal to the current one and commit — reintroducing the out-of-order hazard
// this whole mechanism exists to prevent, but only after enough requests to make
// it look like an unrelated flake.
func TestGenerationIsMonotonic(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
var gen = RequestGeneration()
var seen = Set<UInt64>()
var previous: UInt64 = 0
var monotonic = true
for _ in 0..<500 {
    let next = gen.begin()
    if next <= previous { monotonic = false }
    if seen.contains(next) { monotonic = false }
    seen.insert(next)
    previous = next
}
check("stamps are strictly increasing and unique", monotonic)
check("every stamp was distinct", seen.count == 500)
check("only the last stamp is current", gen.isCurrent(previous) && !gen.isCurrent(1))
`)
}

// TestTestAllLocksAtTheClickNotTheFirstFrame covers UI-18 and scenario (H).
//
// The button was guarded by "a run is in progress", and that only became true
// when the backend's `started` frame arrived. Between the click and that frame
// the guard still read idle, so a second click started a SECOND run — two tests
// of the same group interleaving their progress and their counters.
//
// The fix is to treat the CLICK as the evidence and claim the state immediately.
// Waiting for the backend to confirm what we just asked for is the wrong
// direction, and it is unrepresentable once the state has a `launching` case.
func TestTestAllLocksAtTheClickNotTheFirstFrame(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// Before the click: idle, so the button is offered.
var state = GroupTestState.idle
check("an idle run is not running", !state.isRunning)
check("an idle run has no group", state.group == nil)
check("an idle run has no progress", state.progress == nil)

// SCENARIO H. The click. The state is claimed HERE, before any request.
state = .launching(group: "A")
check("UI-18 the button locks AT THE CLICK", state.isRunning)
check("UI-18 a launching run names its group", state.group == "A")
check("UI-18 a launching run has no invented total", state.progress == nil)

// The backend's first frame arrives and hands over the real total.
let first = GroupTestProgress(id: 7, group: "A", total: 12,
                              completed: 0, succeeded: 0, failed: 0, inFlight: [])
check("the started frame is adopted",
      acceptsGroupTestStart(state, startedGroup: "A", startedRunID: 7))
state = .running(first)
check("a running run is still running", state.isRunning)
check("the total is now known", state.progress?.total == 12)
check("the run is attributed to its group", state.group == "A")

// A duplicate frame for the SAME run must not be adopted: doing so would reset
// the progress counters the user is watching back to zero.
check("a duplicate started frame is refused",
      !acceptsGroupTestStart(state, startedGroup: "A", startedRunID: 7))

// A frame for a run we did not launch is not ours to display. Adopting it would
// resurrect a test the user is not running, or replace their view of a different
// one.
check("another session's run is refused",
      !acceptsGroupTestStart(state, startedGroup: "B", startedRunID: 99))
check("an idle screen refuses every started frame",
      !acceptsGroupTestStart(.idle, startedGroup: "A", startedRunID: 1))

// A frame for the group we asked for but a DIFFERENT run id is accepted: the
// user asked for that group, and this is the run serving the request.
check("the run serving our request is adopted",
      acceptsGroupTestStart(.launching(group: "A"), startedGroup: "A", startedRunID: 8))
// A frame for a group we did NOT ask for is not.
check("a frame for another group is refused while launching",
      !acceptsGroupTestStart(.launching(group: "A"), startedGroup: "B", startedRunID: 8))
`)
}
