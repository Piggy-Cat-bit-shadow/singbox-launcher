package swiftlogic_test

import "testing"

// TestCoreActionPolicyMatrix walks every combination of connection, liveness and
// core state, and asserts what each control offers AND what it says.
//
// The audit's requirement is that no cell be left unexplained, so every disabled
// case here also checks that a refusal cause travels with it. A disabled control
// with nothing to say is the defect users report as "the button does nothing".
func TestCoreActionPolicyMatrix(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// --- The backend is not connected: nothing is offered, and the reason is the
// connection rather than the core.
let offline = decideCoreActions(connected: false, busy: false,
                                state: .running, hasBinary: true)
check("UI-19 no action is offered while disconnected",
      !offline.0.canStart && !offline.0.canStop && !offline.0.canRestart
      && !offline.0.canImportCore && !offline.0.canSwitchEngine)
check("UI-19 disconnection is named", offline.1 == .backendNotConnected)

// --- An operation is in flight: ONE AT A TIME, ACROSS EVERY CONTROL.
//
// Checked BEFORE the state, and even for a state that would otherwise allow the
// action: this is what makes a concurrent Restart impossible rather than merely
// unlikely.
let busy = decideCoreActions(connected: true, busy: true,
                             state: .running, hasBinary: true)
check("UI-19 a busy core offers no stop either", !busy.0.canStop)
check("UI-19 a busy core offers no restart", !busy.0.canRestart)
check("UI-19 being busy is named", busy.1 == .operationInFlight)

// --- The state has not been read yet.
let unknown = decideCoreActions(connected: true, busy: false,
                                state: nil, hasBinary: true)
check("UI-19 an unknown state offers nothing",
      !unknown.0.canStart && !unknown.0.canStop && !unknown.0.canRestart)
check("UI-19 an unknown state is named", unknown.1 == .coreStateUnknown)

// --- A RUNNING core with a binary present.
let running = decideCoreActions(connected: true, busy: false,
                                state: .running, hasBinary: true)
check("a running core can be stopped", running.0.canStop)
check("a running core can be restarted", running.0.canRestart)
check("a running core cannot be started again", !running.0.canStart)
check("UI-20 a running core cannot import over itself", !running.0.canImportCore)
check("UI-21 a running core cannot switch engine", !running.0.canSwitchEngine)
check("UI-20 the refusal names the running VPN", running.1 == .coreRunning)

// --- A SETTLED STOPPED core: the only state that permits import and switching.
let stopped = decideCoreActions(connected: true, busy: false,
                                state: .stopped, hasBinary: true)
check("a stopped core can be started", stopped.0.canStart)
check("a stopped core cannot be stopped", !stopped.0.canStop)
// RESTART IS NOT A SYNONYM FOR START. On a stopped core the honest action is
// Start; offering Restart presented a control whose semantics matched nothing.
check("UI-20 a stopped core cannot be RESTARTED", !stopped.0.canRestart)
check("a stopped core can import a binary", stopped.0.canImportCore)
check("a stopped core can switch engine", stopped.0.canSwitchEngine)
check("a settled stopped core explains nothing", stopped.1 == nil)

// --- A stopped core with NO binary: import is the action that fixes it, so it
// must be offered; starting cannot be.
let noBinary = decideCoreActions(connected: true, busy: false,
                                 state: .stopped, hasBinary: false)
check("a core with no binary cannot be started", !noBinary.0.canStart)
check("a core with no binary cannot be restarted", !noBinary.0.canRestart)
check("the missing binary is named", noBinary.1 == .coreMissing)

// --- Transitional states offer nothing and say so.
let starting = decideCoreActions(connected: true, busy: false,
                                 state: .starting, hasBinary: true)
check("a starting core offers no stop", !starting.0.canStop)
check("a starting core offers no restart", !starting.0.canRestart)
check("a starting core offers no import", !starting.0.canImportCore)
check("a starting core offers no engine switch", !starting.0.canSwitchEngine)
check("the transition is named", starting.1 == .coreTransitioning)

let stopping = decideCoreActions(connected: true, busy: false,
                                 state: .stopping, hasBinary: true)
check("a stopping core offers nothing either", !stopping.0.canStop && !stopping.0.canRestart)
check("the stopping transition is named", stopping.1 == .coreTransitioning)

// --- An ERRORED core: Start doubles as Retry, and import stays locked because
// the backend requires a SETTLED STOPPED core to replace.
let errored = decideCoreActions(connected: true, busy: false,
                                state: .error, hasBinary: true)
check("an errored core can be retried via Start", errored.0.canStart)
check("an errored core cannot be restarted", !errored.0.canRestart)
check("UI-20 an errored core cannot import", !errored.0.canImportCore)
check("the error is named so it can be resolved first", errored.1 == .coreErrored)

// --- The invariant that ties the whole matrix together: for EVERY combination,
// a control that is not offered comes with a reason. This is what the audit means
// by "no unexplained cell".
var unexplained = 0
for connected in [true, false] {
    for busy in [true, false] {
        for s in [CoreState?.none, .some(.stopped), .some(.starting), .some(.running),
                  .some(.stopping), .some(.error)] {
            for hasBinary in [true, false] {
                let (p, why) = decideCoreActions(connected: connected, busy: busy,
                                                 state: s, hasBinary: hasBinary)
                let offersNothing = !p.canStart && !p.canStop && !p.canRestart
                    && !p.canImportCore && !p.canSwitchEngine
                // A state where SOMETHING is offered must not carry a refusal,
                // and one where nothing is offered must explain itself.
                if offersNothing && why == nil { unexplained += 1 }
                if p.canStop && s != .some(.running) { unexplained += 1 }
                if p.canImportCore && (s != .some(.stopped) || !hasBinary) { unexplained += 1 }
            }
        }
    }
}
check("UI-19 every disabled combination carries a reason", unexplained == 0)
`)
}

// TestDaemonDestructiveSafety pins the rule that guards removing a privileged
// service.
//
// The old view check asked only "is the daemon active and the core running",
// which is one state out of several where the SAME teardown is unsafe — the core
// coming up through the daemon, going down through it, or an operation holding
// the channel. In each case teardown aborts work the user just started, or races
// the operation holding the very channel being removed.
func TestDaemonDestructiveSafety(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// Only active_mode is read by the rule; the DTO's other fields are supplied by
// its own convenience init, because inventing twenty irrelevant values would
// make this test break whenever an unrelated protocol field is added.
func status(active: Bool, installed: Bool) -> DaemonStatus {
    return DaemonStatus(activeMode: active, installed: installed)
}

// Unread status: refused. We cannot safely destroy what we cannot see.
let unknown = decideDaemonDestructiveActions(status: nil, pending: false, coreState: .stopped)
check("an unread daemon status refuses teardown", !unknown.allowed)
check("the unknown status is named", unknown.reason == .statusUnknown)

// Installed but NOT the active engine: nothing depends on it, so the only thing
// that can refuse is an operation in flight.
let idle = decideDaemonDestructiveActions(status: status(active: false, installed: true),
                                          pending: false, coreState: .running)
check("an inactive daemon does not block teardown", idle.allowed)
check("an inactive daemon has nothing to explain", idle.reason == nil)

let idleBusy = decideDaemonDestructiveActions(status: status(active: false, installed: true),
                                              pending: true, coreState: .stopped)
check("a busy app blocks teardown even when inactive", !idleBusy.allowed)
check("being busy is named", idleBusy.reason == .busy)

// ACTIVE ENGINE. The VPN is up: refusing is the whole point, because removing
// the pairing under a running core cuts the user's traffic.
let running = decideDaemonDestructiveActions(status: status(active: true, installed: true),
                                             pending: false, coreState: .running)
check("UI-22 a running VPN refuses teardown", !running.allowed)
check("the running VPN is named", running.reason == .vpnRunning)

// The core is COMING UP through the daemon. Removing the pairing now aborts the
// start the user just requested — the state the old check missed entirely,
// because the core was not yet running.
let starting = decideDaemonDestructiveActions(status: status(active: true, installed: true),
                                              pending: false, coreState: .starting)
check("UI-22 a starting core refuses teardown", !starting.allowed)
check("the transition is named", starting.reason == .coreTransitioning)

// Going down through the same channel.
let stopping = decideDaemonDestructiveActions(status: status(active: true, installed: true),
                                              pending: false, coreState: .stopping)
check("UI-22 a stopping core refuses teardown", !stopping.allowed)
check("the stopping transition is named", stopping.reason == .coreTransitioning)

// An active engine whose core state cannot be read: the channel is in use but we
// cannot prove it is safe. Unknown is a refusal, not a default-allow.
let nameless = decideDaemonDestructiveActions(status: status(active: true, installed: true),
                                              pending: false, coreState: nil)
check("UI-22 an unknown core state refuses teardown", !nameless.allowed)
check("the unknown core state is named", nameless.reason == .coreStateUnknown)

// An errored core: tearing the channel down would discard the diagnostics the
// user needs to understand what went wrong.
let errored = decideDaemonDestructiveActions(status: status(active: true, installed: true),
                                             pending: false, coreState: .error)
check("UI-22 an errored core refuses teardown", !errored.allowed)
check("the error is named", errored.reason == .coreError)

// The one state that permits it: active engine, settled, nothing in flight.
let settled = decideDaemonDestructiveActions(status: status(active: true, installed: true),
                                             pending: false, coreState: .stopped)
check("a settled active daemon permits teardown", settled.allowed)
check("permitting teardown explains nothing", settled.reason == nil)

// An operation in flight on an ACTIVE daemon: it holds the channel.
let busy = decideDaemonDestructiveActions(status: status(active: true, installed: true),
                                          pending: true, coreState: .stopped)
check("a busy app refuses teardown on an active daemon", !busy.allowed)
check("being busy is named there too", busy.reason == .busy)

// The exhaustive invariant: teardown is allowed ONLY for a settled, inactive-or-
// stopped core with nothing in flight. Anything else must be refused WITH a cause,
// because a destructive refusal the user cannot read is indistinguishable from a
// broken button.
var violations = 0
for active in [true, false] {
    for pending in [true, false] {
        for s in [CoreState?.none, .some(.stopped), .some(.starting), .some(.running),
                  .some(.stopping), .some(.error)] {
            let p = decideDaemonDestructiveActions(
                status: status(active: active, installed: true),
                pending: pending, coreState: s)
            if !p.allowed && p.reason == nil { violations += 1 }
            if p.allowed && (pending || (active && s != .some(.stopped))) { violations += 1 }
        }
    }
}
check("UI-22 teardown is permitted only when settled and idle", violations == 0)
`)
}
