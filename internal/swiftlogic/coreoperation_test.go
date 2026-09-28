package swiftlogic_test

import "testing"

// TestCoreOperationTimeoutReconcilesSnapshot covers the deadline path.
//
// A lifecycle operation waits for a backend event saying the work finished. If
// that event is lost — a dropped frame, a reconnected stream — the UI stays busy
// forever with every control disabled, which is the worst possible failure for a
// VPN client: the user cannot start, stop, or retry. The deadline exists for that
// case, but what it may CONCLUDE is narrow, and getting it wrong is how a second
// lifecycle command was allowed to race the first.
func TestCoreOperationTimeoutReconcilesSnapshot(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// THE CASE THE DEADLINE EXISTS FOR. A stop whose completion event was lost, and
// the snapshot proves the core did stop: release silently, because the user's
// action DID work and there is nothing to report.
check("a stop confirmed by the snapshot finishes",
      reconcileCoreOperation(snapshotState: .stopped, kind: .stop) == .confirmedFinished)
check("a start confirmed by the snapshot finishes",
      reconcileCoreOperation(snapshotState: .running, kind: .start) == .confirmedFinished)
check("a restart confirmed by the snapshot finishes",
      reconcileCoreOperation(snapshotState: .running, kind: .restart) == .confirmedFinished)

// THE CASE THAT MUST NOT RELEASE. The snapshot reports the state the operation
// STARTED from, which means the backend has not acted yet. Releasing here is what
// declared the operation over while it was still running, letting a second
// lifecycle command race the first.
check("a stop that has not taken effect keeps waiting",
      reconcileCoreOperation(snapshotState: .running, kind: .stop) == .stillRunning)
check("a start that has not taken effect keeps waiting",
      reconcileCoreOperation(snapshotState: .stopped, kind: .start) == .stillRunning)
// A restart passes THROUGH stopped; concluding there would release halfway.
check("a restart passing through stopped keeps waiting",
      reconcileCoreOperation(snapshotState: .stopped, kind: .restart) == .stillRunning)
check("a restart still starting keeps waiting",
      reconcileCoreOperation(snapshotState: .starting, kind: .restart) == .stillRunning)
check("a stop still stopping keeps waiting",
      reconcileCoreOperation(snapshotState: .stopping, kind: .stop) == .stillRunning)

// A failure IS a conclusion: the operation is over, and the reason comes from the
// error fields rather than from a busy marker that never clears.
check("an errored core concludes any operation",
      reconcileCoreOperation(snapshotState: .error, kind: .stop) == .confirmedFinished)
check("an errored core concludes a restart too",
      reconcileCoreOperation(snapshotState: .error, kind: .restart) == .confirmedFinished)

// The backend could not be reached: the outcome cannot be confirmed, so the
// marker stays. Releasing it would claim an operation ended while the backend is
// invisible, and the connection changing is what resets it.
check("an unreachable backend leaves the operation unconfirmed",
      reconcileCoreOperation(snapshotState: nil, kind: .stop) == .unconfirmed)
check("an unreachable backend leaves a start unconfirmed",
      reconcileCoreOperation(snapshotState: nil, kind: .start) == .unconfirmed)

// The three outcomes must stay distinguishable: collapsing stillRunning into
// unconfirmed would replace a specific "it is still working" with a vaguer
// message the user cannot act on.
check("stillRunning and unconfirmed are different outcomes",
      CoreReconciliation.stillRunning != CoreReconciliation.unconfirmed)
`)
}

// TestSnapshotFreshnessRule pins the sequence-number guard behind reconciliation.
//
// A snapshot older than the command that started the operation describes the world
// BEFORE the user acted, so it cannot be evidence about the outcome.
func TestSnapshotFreshnessRule(t *testing.T) {
	requireHarness(t)
	runSwift(t, `
// Strictly newer: the reply must describe a world the command has already
// affected.
check("a snapshot newer than the command can settle it",
      snapshotCanSettleOperation(snapshotSeq: 8, commandSeq: 5))

// EXACTLY EQUAL IS NOT ENOUGH. A snapshot at the command's own sequence reports
// the state the operation started from — still running for a stop, still stopped
// for a start — so accepting it settles the operation on the very state it was
// issued to change. This is the off-by-one that made the freshness test pass
// trivially whenever the snapshot was applied before being compared.
check("a snapshot at the same sequence cannot settle it",
      !snapshotCanSettleOperation(snapshotSeq: 5, commandSeq: 5))

// An older reply describes the past.
check("an older snapshot cannot settle it",
      !snapshotCanSettleOperation(snapshotSeq: 4, commandSeq: 5))

// The boundary is exact, so an off-by-one in either direction is caught.
check("the boundary is exact at +1",
      snapshotCanSettleOperation(snapshotSeq: 6, commandSeq: 5)
      && !snapshotCanSettleOperation(snapshotSeq: 5, commandSeq: 5))
`)
}
