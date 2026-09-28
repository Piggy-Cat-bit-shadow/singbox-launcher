package service

import "testing"

// TestSubscriptionResultReportsRebuildFailure is claim 20.
//
// `UpdateConfigFromSubscriptions` refreshes the sources and then rebuilds the config,
// two phases with independent outcomes. When the rebuild failed it logged and returned
// `result, nil`, so the IPC layer saw success, set `OK: true`, and told the user
// "N nodes from M sources" — while config.json had not been updated at all and the core
// kept running the old one. The operation result was false in the most consequential
// direction: the user believed the VPN config had been updated when it had not.
//
// A two-phase operation must report two outcomes.
func TestSubscriptionResultReportsRebuildFailure(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/maintenance.go"))

	if !contains(src, "RebuildOK") && !contains(src, "rebuild_ok") {
		t.Fatal("the maintenance result has no rebuild outcome, so a failed rebuild is " +
			"indistinguishable from a successful one and the user is told the config " +
			"was updated when it was not")
	}

	// The refresh outcome and the rebuild outcome must be separate fields: collapsing
	// them loses exactly the information the user needs.
	if !contains(src, "RefreshOK") && !contains(src, "refresh_ok") {
		t.Error("the refresh outcome is not reported separately from the rebuild")
	}

	// And a successful REFRESH with a failed REBUILD must not report overall success.
	// The overall verdict must be DERIVED from both phases rather than asserted when
	// the first one succeeds.
	idx := indexOf(src, "out := MaintenanceResult{")
	if idx < 0 {
		t.Fatal("MaintenanceResult construction not found")
	}
	// The construction literal, up to its last field. Anchoring on the field that
	// closes it keeps the window inside the literal rather than spilling into the
	// code that follows.
	litEnd := indexOf(src[idx:], "RebuildOK: res == nil || res.RebuildErr == nil,")
	if litEnd < 0 {
		t.Fatal("cannot find the end of the result literal")
	}
	literal := src[idx : idx+litEnd]
	// Check for the OVERALL verdict specifically. A bare "OK: true" substring match
	// would also match "RefreshOK: true", which is a different field and a legitimate
	// one — a substring check on a name that is a suffix of another name is a false
	// positive waiting to happen.
	if contains(literal, "\n\t\tOK: true") {
		t.Error("the overall verdict is asserted in the same literal that reports the " +
			"refresh, so a failed rebuild still reports overall success")
	}
	// The verdict must combine both phases.
	if !contains(src, "out.OK = out.RefreshOK && out.RebuildOK") {
		t.Error("the overall verdict does not require both phases to succeed")
	}
}

// TestRebuildFailureIsNotSwallowedInCore — the core half of claim 20.
//
// The rebuild error was logged and then dropped: the function returned `result, nil`.
// Reporting it is what lets the layer above tell the user the truth.
func TestRebuildFailureIsNotSwallowedInCore(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "core/config_service.go"))

	// The rebuild result must reach the caller rather than only the log.
	if !contains(src, "RebuildErr") && !contains(src, "rebuildErr") {
		t.Fatal("the rebuild failure never leaves the core, so no caller can report it")
	}
	// It must be attached to the returned result, not merely logged.
	if contains(src, "debuglog.WarnLog(\"UpdateConfigFromSubscriptions: auto-rebuild after refresh failed: %v\", rebuildErr)") &&
		!contains(src, "result.RebuildErr = ") && !contains(src, "RebuildErr:") {
		t.Error("the rebuild failure is only logged; the caller receives a result " +
			"that says nothing about it")
	}
}

// TestSnapshotSeqRepresentsSnapshotContents is claim 26.
//
// `Snapshot` read the sequence under the lock, released it, and then built the
// snapshot from separately-read fields. The sequence therefore describes a moment
// BEFORE any of the content was captured, and a concurrent change lands inside the
// snapshot without being reflected in its sequence — so a client cannot tell whether
// it has the state at that sequence or a newer one. The number is supposed to be a
// version point; as written it is a timestamp for something that had not happened yet.
func TestSnapshotSeqRepresentsSnapshotContents(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/backend.go"))

	idx := indexOf(src, "func (b *Backend) Snapshot()")
	if idx < 0 {
		t.Fatal("Snapshot not found")
	}
	end := indexOf(src[idx:], "\nfunc ")
	if end < 0 {
		end = len(src) - idx
	}
	body := src[idx : idx+end]

	// The sequence must be captured WITH the content, not released beforehand.
	lock := indexOf(body, "b.mu.Lock()")
	unlock := indexOf(body, "b.mu.Unlock()")
	seq := indexOf(body, "seq := b.seq")
	if lock < 0 || unlock < 0 || seq < 0 {
		t.Fatal("expected a lock, an unlock and a sequence read")
	}
	if seq > unlock {
		t.Fatal("the sequence is read after the lock is released")
	}
	// The state reads must be inside the same critical section as the sequence, or
	// they describe a later moment than the number claims.
	core := indexOf(body, "b.coreState()")
	proxy := indexOf(body, "b.proxySummary()")
	if core > unlock || proxy > unlock {
		t.Error("the snapshot's content is read AFTER the lock is released, so the " +
			"sequence describes a moment before the content was captured and a " +
			"concurrent change is invisible to the client's version check")
	}
}
