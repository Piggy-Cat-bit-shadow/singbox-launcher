package swiftlogic_test

import "testing"

// TestRestartIsSingleFlight covers UI-05 and the double-click requirement.
//
// Double-clicking Restart issued two restarts: the guard was a bare boolean that
// only became true after the work had begun, and a second click could land before
// that. Two restarts means two stop/start pairs racing, which is the exact
// two-helpers situation the stop logic exists to prevent.
func TestRestartIsSingleFlight(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
var guardState = SingleFlight()
check("a fresh guard is idle", guardState.isIdle)

// The first click claims it.
check("the first click is admitted", guardState.begin())
// The second click, before the first finishes, is REFUSED. This is the whole
// point: a double-click must start ONE restart.
check("UI-05 a double-click is refused", !guardState.begin())
check("a refused click leaves the guard held", !guardState.isIdle)

// Further clicks while the operation is running are refused too.
check("a third click is refused", !guardState.begin())

// The operation finishes and releases it.
guardState.end()
check("the guard is idle again after completion", guardState.isIdle)
check("a later click is admitted", guardState.begin())

// Releasing is IDEMPOTENT. A completion path that runs twice (a response and an
// event both arriving) must not release a slot a DIFFERENT operation has since
// claimed — that would let a concurrent operation start.
guardState.end()
guardState.end()
check("a double release does not corrupt the guard", guardState.isIdle)

// HONEST LIMIT, STATED SO IT IS NOT MISTAKEN FOR SAFETY THE TYPE DOES NOT HAVE.
//
// SingleFlight is a FLAG: end() cannot tell which operation is releasing. If a
// late completion from operation A ran after operation B had claimed the slot, it
// would free B's slot while B is still running.
//
// That is not a latent bug here because the model claims and releases inside ONE
// function body via defer, so completion cannot outlive its own operation. The
// test records the boundary: it is the CALLER's structure that guarantees this,
// not the primitive.
var b = SingleFlight()
_ = b.begin()
b.end()
check("the guard is free after a release", b.isIdle)
_ = b.begin()
check("and can be claimed again", !b.isIdle)
`)
}

// TestBackendRestartStopsAfterAFailedStop covers the chained-recovery rule.
//
// Restart is "stop, then start", and the start MUST NOT run if the stop failed. A
// failed stop leaves the connection failed and the helper alive; starting then
// produces the two-helpers case the stop refused to create. The chain rule is
// stated once as a function so a hurried edit cannot reintroduce "continue anyway".
func TestBackendRestartStopsAfterAFailedStop(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// The happy path: both steps run, in order.
let happy = chainExecutionOrder([
    ChainStep(name: "stop", outcome: .succeeded),
    ChainStep(name: "start", outcome: .succeeded),
])
check("a successful stop is followed by a start", happy == ["stop", "start"])

// The defect: a failed stop must NOT be followed by a start.
let failed = chainExecutionOrder([
    ChainStep(name: "stop", outcome: .failed),
    ChainStep(name: "start", outcome: .succeeded),
])
check("a failed stop stops the chain", failed == ["stop"])
check("the start did not run", !failed.contains("start"))

// The primitive itself.
check("success continues", chainShouldContinue(after: .succeeded))
check("failure does not continue", !chainShouldContinue(after: .failed))
check("cancellation does not continue either", !chainShouldContinue(after: .cancelled))

// An empty chain runs nothing and does not crash.
check("an empty chain runs nothing", chainExecutionOrder([]).isEmpty)

// A three-step chain stops exactly at the failure, not before and not after.
let three = chainExecutionOrder([
    ChainStep(name: "update", outcome: .succeeded),
    ChainStep(name: "reload", outcome: .failed),
    ChainStep(name: "reloadGroups", outcome: .succeeded),
])
check("UI-06 the chain stops at the failing step", three == ["update", "reload"])
check("UI-06 the step after the failure never runs", !three.contains("reloadGroups"))

// A failure in the FIRST step runs nothing else.
let first = chainExecutionOrder([
    ChainStep(name: "update", outcome: .failed),
    ChainStep(name: "reload", outcome: .succeeded),
    ChainStep(name: "reloadGroups", outcome: .succeeded),
])
check("a first-step failure runs only that step", first == ["update"])
`)
}

// TestUpdateThenReloadChain covers UI-06 and UI-35 together.
//
// "Update All, then reload" is the chain the user triggers with one click, and it
// is the one where continuing past a failure is actively misleading: the reload
// would run against a config the update just failed to refresh, so the user sees a
// RELOAD failure that has nothing to do with reloading, and the real cause — the
// update — is buried under it.
func TestUpdateThenReloadChain(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// The full successful chain, as the user experiences it.
let ok = chainExecutionOrder([
    ChainStep(name: "updateAll", outcome: .succeeded),
    ChainStep(name: "reload", outcome: .succeeded),
    ChainStep(name: "reloadGroups", outcome: .succeeded),
])
check("UI-35 a successful update reloads and refreshes groups",
      ok == ["updateAll", "reload", "reloadGroups"])

// The update fails: the user must see ONE failure naming the update, not a
// second, unrelated reload failure on top of it.
let updateFailed = chainExecutionOrder([
    ChainStep(name: "updateAll", outcome: .failed),
    ChainStep(name: "reload", outcome: .succeeded),
    ChainStep(name: "reloadGroups", outcome: .succeeded),
])
check("UI-06 a failed update does not reload", updateFailed == ["updateAll"])

// The reload fails after a successful update: same rule, one step later. The
// groups refresh must not run against a config that failed to load.
let reloadFailed = chainExecutionOrder([
    ChainStep(name: "updateAll", outcome: .succeeded),
    ChainStep(name: "reload", outcome: .failed),
    ChainStep(name: "reloadGroups", outcome: .succeeded),
])
check("UI-06 a failed reload does not refresh groups", reloadFailed == ["updateAll", "reload"])

// A user cancellation is not an error, but it still stops the chain: continuing
// would do work the user just declined.
let cancelled = chainExecutionOrder([
    ChainStep(name: "updateAll", outcome: .cancelled),
    ChainStep(name: "reload", outcome: .succeeded),
])
check("a cancellation stops the chain without continuing", cancelled == ["updateAll"])
`)
}
