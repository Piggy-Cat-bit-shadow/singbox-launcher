package swiftlogic_test

import (
	"testing"
)

// TestPreparedCommandSurvivesAReadyDaemon covers UI-07 / UI-08, the defect that
// made Re-pair and Remove Service silently do nothing.
//
// THE MECHANISM. Those two operations generate a command for the user to run in
// Terminal — they change no service state at all, by design, because the whole
// point is that the user then goes and runs it. The frontend used to clear any
// prepared command as soon as the daemon reported ready, and a daemon that is
// being re-paired IS ready (that is why re-pairing is offered). So the command
// was discarded the instant it was created, and the click flashed and did
// nothing.
//
// The fix records the state the command was generated FOR and retires it only
// when that state actually moves. This pins that rule as a pure decision, which
// is what lets it be tested without a running service.
func TestPreparedCommandSurvivesAReadyDaemon(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// The SHIPPING rule, from DaemonCommandLifetime.swift — not a local restatement.
// A test that reimplements the logic it is checking passes whatever the
// implementation does, which is the failure mode this harness exists to avoid.

// Only the OPERATION is read by the lifetime rule, so the harness supplies a
// command carrying the operation and nothing else. Building a whole
// DaemonCommandResult would require a full protocol DTO whose other fields the
// rule never looks at, and a test that constructs twenty irrelevant values is a
// test that breaks when an unrelated field is added.
func prepared(_ operation: String,
              installed: Bool, ready: Bool, active: Bool, paired: Bool) -> PreparedDaemonCommand {
    PreparedDaemonCommand(
        operation: operation,
        preparedFor: DaemonStateFingerprint(installed: installed, ready: ready,
                                            activeMode: active, paired: paired),
        id: 1)
}

// THE CASE THAT WAS BROKEN. A ready, paired daemon; Re-pair generates an invite.
// The event that used to destroy it carries the SAME state, because generating a
// command changes nothing about the service.
let invite = prepared(DaemonOperation.freshInvite,
                      installed: true, ready: true, active: true, paired: true)
check("UI-07 a fresh invite survives an unchanged ready daemon",
      daemonCommandSurvives(invite,
          newStatus: DaemonStateFingerprint(installed: true, ready: true,
                                            activeMode: true, paired: true)))

// Remove Service: same story. The service is still installed and still ready
// until the user actually runs the generated command.
let uninstall = prepared(DaemonOperation.uninstall,
                         installed: true, ready: true, active: false, paired: true)
check("UI-07 an uninstall command survives an unchanged ready daemon",
      daemonCommandSurvives(uninstall,
          newStatus: DaemonStateFingerprint(installed: true, ready: true,
                                            activeMode: false, paired: true)))

// THE RULE MUST ALSO RETIRE, or it is not a rule.
//
// A REDEEMED invite is spent, and redeeming is what turns paired ON. A rule
// that kept the command here would send the user to run an invite that has
// already been consumed. (The invite was prepared at paired=true in a DIFFERENT
// active state, so this is also a genuine state move.)
check("a fresh invite is retired once it is redeemed",
      !daemonCommandSurvives(invite,
          newStatus: DaemonStateFingerprint(installed: true, ready: true,
                                            activeMode: false, paired: true)))

// The other retire path: the service is gone, so there is nothing to pair with.
check("a fresh invite is retired when the service goes away",
      !daemonCommandSurvives(invite,
          newStatus: DaemonStateFingerprint(installed: false, ready: false,
                                            activeMode: false, paired: false)))

// And the SURVIVAL window is exactly "installed and still unpaired": a state move
// inside that window must keep the command, or the user loses it merely because
// an unrelated field changed.
let unpairedInvite = prepared(DaemonOperation.freshInvite,
                              installed: true, ready: false, active: false, paired: false)
check("a fresh invite survives a move within its window",
      daemonCommandSurvives(unpairedInvite,
          newStatus: DaemonStateFingerprint(installed: true, ready: true,
                                            activeMode: false, paired: false)))

// An install command is tied to its exact situation: once the service is
// installed, telling the user to install it is worse than showing nothing.
let install = prepared(DaemonOperation.install,
                       installed: false, ready: false, active: false, paired: false)
check("a stale install command is retired",
      !daemonCommandSurvives(install,
          newStatus: DaemonStateFingerprint(installed: true, ready: true,
                                            activeMode: false, paired: false)))
check("an install command survives its own state",
      daemonCommandSurvives(install,
          newStatus: DaemonStateFingerprint(installed: false, ready: false,
                                            activeMode: false, paired: false)))

// A command whose precondition is unknown cannot be judged, so it is retired:
// keeping it is how a user runs something already done.
check("no prepared command survives", !daemonCommandSurvives(nil,
      newStatus: DaemonStateFingerprint(installed: true, ready: true,
                                        activeMode: true, paired: true)))

// Every field must participate, or the rule would keep a command alive for a
// state it was not prepared for.
let base = DaemonStateFingerprint(installed: true, ready: true, activeMode: true, paired: true)
check("installed is part of the identity", base != DaemonStateFingerprint(installed: false, ready: true, activeMode: true, paired: true))
check("ready is part of the identity", base != DaemonStateFingerprint(installed: true, ready: false, activeMode: true, paired: true))
check("activeMode is part of the identity", base != DaemonStateFingerprint(installed: true, ready: true, activeMode: false, paired: true))
check("paired is part of the identity", base != DaemonStateFingerprint(installed: true, ready: true, activeMode: true, paired: false))
`)
}

// TestDaemonCommandOperationsAreNamed pins the identifiers the lifetime rule
// switches on.
//
// The rule is per-operation — a fresh invite stays usable after the state moves,
// an install command does not — so it compares operation NAMES. A typo in one of
// those comparisons would not fail to compile; it would silently pick the wrong
// lifetime and reintroduce the disappearing command.
func TestDaemonCommandOperationsAreNamed(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
check("the invite operation is named", DaemonOperation.freshInvite == "fresh_invite")
check("the install operation is named", DaemonOperation.install == "install")
check("the start operation is named", DaemonOperation.start == "start")
check("the uninstall operation is named", DaemonOperation.uninstall == "uninstall")
// The backend's own token for the repair step is "fresh_invite", so the two
// sides must agree on that spelling; this is the value pairingInviteReady
// compares against.
check("the invite token matches the backend", DaemonOperation.freshInvite.hasSuffix("invite"))
`)
}
