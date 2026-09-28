package swiftlogic_test

import "testing"

// TestSingleNodeTestButtonsMatchSerializationPolicy covers the control that
// promised what the model would refuse.
//
// `withPending` admits one operation at a time, so a second single-node test is
// rejected — but the row's disable rule did not mention that state, leaving every
// OTHER node's Test control looking live. Clicking one produced "another operation
// is running", an error the proxy screen does not display. A control must not offer
// what the model will refuse, because the refusal is invisible where it matters.
func TestSingleNodeTestButtonsMatchSerializationPolicy(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// Idle screen: every row may test.
let idle = proxyRowPolicy(rowID: "a", singleTestInFlightFor: nil,
                          switchInFlightFor: nil, groupTestRunning: false,
                          listLoading: false)
check("an idle row may test", idle.canTest)
check("an idle row may be selected", idle.canSelect)
check("an idle row is not switching", !idle.isSwitching)
check("an idle row is not testing", !idle.isTesting)

// A test is running for node A. BOTH rows must be disabled: the model admits one
// at a time, so row B's control would be refused.
let aTesting = proxyRowPolicy(rowID: "a", singleTestInFlightFor: "a",
                              switchInFlightFor: nil, groupTestRunning: false,
                              listLoading: false)
let bWhileATesting = proxyRowPolicy(rowID: "b", singleTestInFlightFor: "a",
                                    switchInFlightFor: nil, groupTestRunning: false,
                                    listLoading: false)
check("the tested row cannot test again", !aTesting.canTest)
check("UI-23 ANOTHER row cannot test either", !bWhileATesting.canTest)
check("the tested row shows as testing", aTesting.isTesting)
check("the other row does not show as testing", !bWhileATesting.isTesting)

// A switch in flight disables testing everywhere, and selection too.
let switching = proxyRowPolicy(rowID: "a", singleTestInFlightFor: nil,
                               switchInFlightFor: "a", groupTestRunning: false,
                               listLoading: false)
check("a switching row cannot test", !switching.canTest)
check("a switching row cannot be re-selected", !switching.canSelect)
check("the switching row shows as switching", switching.isSwitching)
let otherWhileSwitching = proxyRowPolicy(rowID: "b", singleTestInFlightFor: nil,
                                         switchInFlightFor: "a", groupTestRunning: false,
                                         listLoading: false)
check("another row cannot be selected during a switch", !otherWhileSwitching.canSelect)
check("another row cannot test during a switch", !otherWhileSwitching.canTest)

// A group test running disables every row's test: the run owns the endpoint.
let running = proxyRowPolicy(rowID: "a", singleTestInFlightFor: nil,
                             switchInFlightFor: nil, groupTestRunning: true,
                             listLoading: false)
check("no row may test during a group run", !running.canTest)
// Selection is still available: a Test All run does not block choosing a node.
check("a row may still be selected during a group run", running.canSelect)

// A list reload disables everything that acts on the rows.
let loading = proxyRowPolicy(rowID: "a", singleTestInFlightFor: nil,
                             switchInFlightFor: nil, groupTestRunning: false,
                             listLoading: true)
check("no row may test while the list loads", !loading.canTest)
check("no row may be selected while the list loads", !loading.canSelect)

// The invariant that ties it together: whenever the MODEL would refuse (any
// operation in flight), NO row offers the action. This is what makes the control
// and the guard agree rather than merely happen to match on the common path.
var mismatches = 0
for testFor in [String?.none, .some("a"), .some("b")] {
    for switchFor in [String?.none, .some("a"), .some("b")] {
        for groupRunning in [true, false] {
            for row in ["a", "b"] {
                let p = proxyRowPolicy(rowID: row, singleTestInFlightFor: testFor,
                                       switchInFlightFor: switchFor,
                                       groupTestRunning: groupRunning,
                                       listLoading: false)
                let modelWouldRefuse = testFor != nil || switchFor != nil || groupRunning
                if modelWouldRefuse && p.canTest { mismatches += 1 }
                if switchFor != nil && p.canSelect { mismatches += 1 }
            }
        }
    }
}
check("UI-23 no row offers what the model would refuse", mismatches == 0)
`)
}

// TestStaleConfigIsVisibleWithCachedProxies covers the stale list.
//
// Staleness is a property of the CONFIG, not of the list. The nodes on screen may
// have been read successfully from a config that has since been superseded, and the
// user is then choosing between nodes the core may no longer be running. A list
// that looks authoritative while being out of date is worse than one that says so,
// because nothing prompts the user to reload.
func TestStaleConfigIsVisibleWithCachedProxies(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// A current config: the list describes what the core is running.
check("a current config is not stale", !isProxyListStale(configStale: false))
// A superseded config: the list is present and readable, but no longer describes
// the running config.
check("UI-16 a superseded config is stale", isProxyListStale(configStale: true))

// The rule reads the CONFIG and nothing else, which is what makes a cached list
// from a superseded config report as stale. Stated as the truthful mapping over
// both inputs, so a future edit that ANDs in a "do we have nodes" condition - the
// change that would hide the misleading case - fails here.
var mapping = true
for stale in [true, false] {
    if isProxyListStale(configStale: stale) != stale { mapping = false }
}
check("staleness is exactly the config's staleness", mapping)
`)
}
