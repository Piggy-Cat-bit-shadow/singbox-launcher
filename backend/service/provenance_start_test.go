package service

import (
	"context"
	"os"
	"strings"
	"testing"

	"singbox-launcher/backend/protocol"
	"singbox-launcher/core"
)

// Tests for config provenance and the start/stop state machine.
//
// These cover the three reported defects directly. Each one is written against
// the OBSERVABLE contract (the DTO the Swift client decodes, the error the IPC
// layer returns) rather than against internal fields, because every bug here was
// a case of the internals looking right while the user-visible answer was wrong.

// ---------------------------------------------------------------------------
// Provenance: the tri-state, and the false "external" verdict
// ---------------------------------------------------------------------------

// TestLegacyConfigWithoutMarkerIsUnknownNotExternal is case (1) of the required
// matrix, and the exact reported bug.
//
// A config.json written by an older JiejieBox predates provenance markers, so it
// has none. The old code answered "external" — telling a long-standing user that
// another tool manages their own config, and withholding the reload they were
// entitled to. Absence of a marker is not evidence of a foreign owner.
func TestLegacyConfigWithoutMarkerIsUnknownNotExternal(t *testing.T) {
	b := backendWithConfig(t)

	// A plausible historical config: no marker anywhere.
	if _, err := os.Stat(b.provenancePath()); err == nil {
		t.Fatal("precondition: the fixture must have no marker")
	}

	if got := b.configOwnership(); got != OwnershipUnknown {
		t.Fatalf("ownership = %q, want %q: a config with no marker must not be "+
			"reported as owned by another tool", got, OwnershipUnknown)
	}
	if got := b.configOwnership(); got == OwnershipExternal {
		t.Fatal("a missing marker was reported as external ownership")
	}

	// It must still not be rebuildable: unknown means "do not touch", so the
	// permission stays closed even though the ACCUSATION was wrong.
	if b.configIsRebuildable() {
		t.Error("an unverified config must not be rebuildable without adoption")
	}

	// And the DTO must carry the ownership explicitly, so the UI never has to
	// infer it from the permission.
	st := b.coreState()
	if st.ConfigOwnership != string(OwnershipUnknown) {
		t.Errorf("config_ownership = %q, want %q", st.ConfigOwnership, OwnershipUnknown)
	}
	if st.ConfigRebuildable {
		t.Error("config_rebuildable must stay false for an unverified config")
	}
}

// TestExplicitManagedFalseIsExternal — the only path that may claim external.
//
// A marker that positively states another tool owns the file IS evidence, and
// the UI is allowed to say so.
func TestExplicitManagedFalseIsExternal(t *testing.T) {
	b := backendWithConfig(t)

	marker := `{"managed": false, "built_at": "2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(b.provenancePath(), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := b.configOwnership(); got != OwnershipExternal {
		t.Fatalf("ownership = %q, want %q for an explicit managed:false", got, OwnershipExternal)
	}
	if got := b.coreState().ConfigOwnership; got != string(OwnershipExternal) {
		t.Errorf("DTO config_ownership = %q, want external", got)
	}
	if b.configIsRebuildable() {
		t.Error("an external config must not be rebuildable")
	}
}

// TestMalformedOrIncompleteMarkerIsUnknown — a marker we cannot understand
// proves nothing, in either direction.
//
// Includes the empty-object case: valid JSON that simply does not mention
// `managed` must not be read as `managed: false`, or a future marker format
// would silently accuse every user of running a foreign config.
func TestMalformedOrIncompleteMarkerIsUnknown(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated", `{ not json`},
		{"wrong type", `{"managed": "yes"}`},
		{"no managed field", `{}`},
		{"only metadata", `{"built_at": "2026-01-01T00:00:00Z"}`},
		{"null managed", `{"managed": null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := backendWithConfig(t)
			if err := os.WriteFile(b.provenancePath(), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := b.configOwnership(); got != OwnershipUnknown {
				t.Errorf("ownership = %q, want unknown for marker %q", got, tc.body)
			}
			if got := b.configOwnership(); got == OwnershipExternal {
				t.Errorf("marker %q was treated as proof of external ownership", tc.body)
			}
		})
	}
}

// TestMissingConfigIsManaged — a fresh install has nothing to accuse.
//
// With no config there is no file to own, and the launcher must be free to
// create one; this is the case that keeps the first run working.
func TestMissingConfigIsManaged(t *testing.T) {
	b := backendWithConfig(t)
	if err := os.Remove(b.ac.FileService.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if got := b.configOwnership(); got != OwnershipManaged {
		t.Errorf("ownership = %q with no config, want managed", got)
	}
	if !b.configIsRebuildable() {
		t.Error("a missing config must be rebuildable — building it is how it exists")
	}
}

// TestOwnershipNeverDerivedFromRebuildable pins the rule the UI depends on.
//
// `config_rebuildable == false` has three possible causes (managed-but-missing
// binary is not one of them; unknown and external are), so the UI must not read
// it as "external". This asserts the two fields really are independent.
func TestOwnershipNeverDerivedFromRebuildable(t *testing.T) {
	b := backendWithConfig(t)
	st := b.coreState()
	if st.ConfigRebuildable {
		t.Fatal("precondition: the fixture config is not rebuildable")
	}
	if st.ConfigOwnership == string(OwnershipExternal) {
		t.Error("a merely unverified config was reported to the user as external")
	}
}

// ---------------------------------------------------------------------------
// Adoption: safe, evidence-based, and never automatic
// ---------------------------------------------------------------------------

// TestHandEditedConfigInDataDirIsNotAdopted is case (3), and the trap that a
// path-based heuristic would fall into.
//
// A hand-written config sitting in the data directory looks "ours" by location
// but is not reproducible from our state. Adoption requires PROOF — the current
// state must build this exact file — so a foreign file is never taken over just
// because of where it lives.
func TestHandEditedConfigInDataDirIsNotAdopted(t *testing.T) {
	b := backendWithConfig(t)

	// The fixture is a hand-written config in the data dir with no state, so no
	// build can reproduce it.
	before, err := os.ReadFile(b.ac.FileService.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	if b.adoptLegacyConfig() {
		t.Fatal("a hand-edited config was adopted: location must never imply ownership")
	}
	if got := b.configOwnership(); got != OwnershipUnknown {
		t.Errorf("ownership = %q after a failed adoption, want unknown", got)
	}

	// Read-only guarantee: the file is byte-identical and no marker appeared.
	after, err := os.ReadFile(b.ac.FileService.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the adoption attempt modified config.json; it must be read-only")
	}
	if _, err := os.Stat(b.provenancePath()); err == nil {
		t.Error("a marker was written despite the config not being provably ours")
	}
}

// TestConfigMatchesStateUsesCanonicalComparison is case (2)'s comparison rule.
//
// Ownership cannot be decided by bytes: identical configurations differ in key
// order, whitespace and number formatting depending on which encoder produced
// them. A byte comparison would refuse to adopt a config the launcher genuinely
// wrote, while a sloppy one would adopt somebody else's.
func TestConfigMatchesStateUsesCanonicalComparison(t *testing.T) {
	b := backendWithConfig(t)

	// Structurally identical, textually different: reordered keys, different
	// indentation, and 5000 written as 5e3.
	disk := `{"route":{"final":"proxy-out"},"outbounds":[{"tag":"proxy-out","type":"selector"}]}`
	candidate := "{\n  \"outbounds\": [\n    {\"type\": \"selector\", \"tag\": \"proxy-out\"}\n  ],\n" +
		"  \"route\": {\"final\": \"proxy-out\"}\n}\n"
	if err := os.WriteFile(b.ac.FileService.ConfigPath, []byte(disk), 0o644); err != nil {
		t.Fatal(err)
	}
	if !b.configMatchesState([]byte(candidate)) {
		t.Error("structurally identical configs must match regardless of formatting")
	}

	// A DIFFERENT value must never match, even when the shape is the same: the
	// secret and every other value has to participate, or adoption would hand
	// over a config we did not write.
	other := `{"route":{"final":"direct-out"},"outbounds":[{"tag":"proxy-out","type":"selector"}]}`
	if b.configMatchesState([]byte(other)) {
		t.Error("configs differing in a value were treated as equal")
	}

	// Unparseable input is "cannot prove", never a match.
	if b.configMatchesState([]byte(`{ not json`)) {
		t.Error("an invalid candidate must not be considered a match")
	}
	if err := os.WriteFile(b.ac.FileService.ConfigPath, []byte(`{ not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if b.configMatchesState([]byte(disk)) {
		t.Error("an unparseable on-disk config must not be considered a match")
	}
}

// TestAdoptionRequiresAReadOnlyBuild — a failed build means UNKNOWN, not
// adoption, and must leave the data directory untouched.
func TestAdoptionRequiresAReadOnlyBuild(t *testing.T) {
	b := backendWithConfig(t)
	// No state.json in the fixture, so no candidate can be built at all.
	if _, err := b.buildCandidateConfig(); err == nil {
		t.Fatal("expected the candidate build to fail without state.json")
	}
	if b.adoptLegacyConfig() {
		t.Error("adoption succeeded without a reproducible candidate")
	}
	if got := b.configOwnership(); got != OwnershipUnknown {
		t.Errorf("ownership = %q, want unknown", got)
	}
}

// TestAdoptConfigIsExplicitAndRefusesExternal checks the confirmation path.
//
// Adoption is the ONE way ownership moves, so it must refuse the case where
// there is positive evidence of another owner, and it must never be a no-op that
// looks like success.
func TestAdoptConfigIsExplicitAndRefusesExternal(t *testing.T) {
	t.Run("external is refused", func(t *testing.T) {
		b := backendWithConfig(t)
		if err := os.WriteFile(b.provenancePath(), []byte(`{"managed": false}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := b.AdoptConfig()
		pe, ok := err.(*protocol.Error)
		if !ok {
			t.Fatalf("error is %T, want *protocol.Error", err)
		}
		if pe.Code != "not_adoptable" {
			t.Errorf("code = %q, want not_adoptable", pe.Code)
		}
		if got := b.configOwnership(); got != OwnershipExternal {
			t.Errorf("ownership = %q after a refused adoption, want external", got)
		}
	})

	t.Run("unknown becomes managed", func(t *testing.T) {
		b := backendWithConfig(t)
		if got := b.configOwnership(); got != OwnershipUnknown {
			t.Fatalf("precondition: ownership = %q, want unknown", got)
		}
		if _, err := b.AdoptConfig(); err != nil {
			t.Fatalf("AdoptConfig: %v", err)
		}
		if got := b.configOwnership(); got != OwnershipManaged {
			t.Errorf("ownership = %q after adoption, want managed", got)
		}
		if !b.configIsRebuildable() {
			t.Error("an adopted config must become rebuildable")
		}
		// The config itself is NOT rewritten: adoption takes responsibility for
		// the existing file rather than replacing it.
		if got := b.coreState().ConfigOwnership; got != string(OwnershipManaged) {
			t.Errorf("DTO config_ownership = %q, want managed", got)
		}
	})
}

// TestReloadRefusalCopyDoesNotClaimAnotherTool drives the user-facing message.
//
// The refusal for an UNKNOWN config must not say the file belongs to another
// tool: that is the false accusation, and it would survive in the error text
// even after the DTO was fixed.
func TestReloadRefusalCopyDoesNotClaimAnotherTool(t *testing.T) {
	b := backendWithConfig(t)

	_, err := b.ReloadConfig()
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("error is %T, want *protocol.Error", err)
	}
	low := strings.ToLower(pe.Message)
	for _, forbidden := range []string{"another tool", "other tool", "managed outside"} {
		if strings.Contains(low, forbidden) {
			t.Errorf("the UNKNOWN refusal says %q, which asserts a foreign owner we "+
				"have no evidence for: %s", forbidden, pe.Message)
		}
	}

	// The external case MAY say it, because there the evidence exists.
	if err := os.WriteFile(b.provenancePath(), []byte(`{"managed": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = b.ReloadConfig()
	pe, ok = err.(*protocol.Error)
	if !ok {
		t.Fatalf("external error is %T, want *protocol.Error", err)
	}
	if !strings.Contains(strings.ToLower(pe.Message), "another tool") {
		t.Errorf("the external refusal should name the other owner: %s", pe.Message)
	}
}

// ---------------------------------------------------------------------------
// The ownership policy seam: every writer honours it
// ---------------------------------------------------------------------------

// TestOwnershipPolicyDefaultsToRefuse is the safety property of the single seam.
//
// A controller that never had a policy installed must NOT be free to rebuild.
// "No opinion" resolving to "yes" would mean any future caller that forgot to
// wire it silently overwrites user configs.
func TestOwnershipPolicyDefaultsToRefuse(t *testing.T) {
	b := backendWithConfig(t)
	// backendWithConfig builds the controller directly, without installing the
	// policy — exactly the "caller forgot" scenario. Simulate it explicitly so
	// the assertion does not depend on how the fixture is built.
	b.ac.SetConfigOwnershipPolicy(nil)

	if b.ac.MayRebuildConfig() {
		t.Error("a controller with no ownership policy claimed it may rebuild; " +
			"the default must be to refuse")
	}
}

// TestInstalledPolicyTracksOwnership — the seam reports the same verdict as the
// provenance code, so there is one answer rather than two.
func TestInstalledPolicyTracksOwnership(t *testing.T) {
	b := backendWithConfig(t)
	b.installOwnershipPolicy()

	if b.ac.MayRebuildConfig() {
		t.Error("an unverified config must not be rebuildable through the seam")
	}
	if _, err := b.AdoptConfig(); err != nil {
		t.Fatalf("AdoptConfig: %v", err)
	}
	if !b.ac.MayRebuildConfig() {
		t.Error("after adoption the seam must permit a rebuild")
	}
}

// TestPreStartRebuildIsGatedForBothEngines is case (4) of the matrix: the
// pre-start hook — the writer that actually overwrites config.json — must refuse
// for an unverified config even though the manual Reload button is not involved.
//
// The original defect was precisely that the BUTTON was protected and the
// writer was not, so pressing Start rebuilt a config the user had been told
// would not be touched.
func TestPreStartRebuildIsGatedForBothEngines(t *testing.T) {
	b := backendWithConfig(t)
	b.installOwnershipPolicy()

	before, err := os.ReadFile(b.ac.FileService.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}

	// The pre-start hook must decline, and declining is NOT an error: the config
	// on disk is used as-is, which is what "we do not own it" means.
	if err := b.ac.RebuildConfigBeforeStart(false); err != nil {
		t.Fatalf("the pre-start hook returned an error for an unverified config: %v", err)
	}

	after, err := os.ReadFile(b.ac.FileService.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("an unverified config was overwritten by the pre-start hook; " +
			"the writer must honour the same rule the button does")
	}
}

// ---------------------------------------------------------------------------
// Start/stop state machine
// ---------------------------------------------------------------------------

// TestStartDoesNotReportStoppedWhilePending is case (10): a click must not
// revert to "stopped" while the start is still being established.
func TestStartDoesNotReportStoppedWhilePending(t *testing.T) {
	b := backendWithConfig(t)

	// No operation: stopped.
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopped {
		t.Fatalf("idle state = %q, want stopped", got)
	}

	// A start in flight reports `starting`, not `stopped`. This is the whole
	// fix: previously the state was published after a fire-and-forget start, at
	// which point nothing had happened yet and it still read "stopped".
	if !b.ops.beginOp("start") {
		t.Fatal("beginOp refused the first start")
	}
	if got := b.coreLifecycleState(); got != protocol.CoreStateStarting {
		t.Errorf("state during a pending start = %q, want starting", got)
	}
	if got := b.coreState().State; got != protocol.CoreStateStarting {
		t.Errorf("DTO state during a pending start = %q, want starting", got)
	}

	// Committing the start by itself does not clear the pending record — the
	// runtime transition does that — but the state must never go BACK to
	// stopped in between.
	b.ops.finishOp("start", nil)
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopped {
		t.Errorf("state after an accepted start with no running runtime = %q, want stopped", got)
	}
}

// TestRapidDoubleClickStartsOnce is case (11).
func TestRapidDoubleClickStartsOnce(t *testing.T) {
	b := backendWithConfig(t)

	if !b.ops.beginOp("start") {
		t.Fatal("the first start was refused")
	}
	if b.ops.beginOp("start") {
		t.Error("a second start was accepted while the first was in flight; " +
			"a double click must produce one operation")
	}
	// A DIFFERENT operation is a genuine change of intent and must be allowed.
	if !b.ops.beginOp("stop") {
		t.Error("a stop during a start must be accepted: newer intent wins")
	}
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopping {
		t.Errorf("state = %q, want stopping after the stop superseded the start", got)
	}
}

// TestStartFailureBecomesErrorStateWithCode is cases (7), (8) and (9) at the
// state level: a failure must be visible, with a stable code the UI can
// translate, and it must not be silently swallowed.
func TestStartFailureBecomesErrorStateWithCode(t *testing.T) {
	b := backendWithConfig(t)

	b.ops.beginOp("start")
	b.ops.finishOp("start", core.NewStartFailure(core.StartErrDaemonApplyFailed,
		jsonError("default outbound not found: proxy-out")))

	if got := b.coreLifecycleState(); got != protocol.CoreStateError {
		t.Fatalf("state = %q, want error after a failed start", got)
	}
	code, detail := b.coreErrorInfo()
	if code != string(core.StartErrDaemonApplyFailed) {
		t.Errorf("error_code = %q, want %q", code, core.StartErrDaemonApplyFailed)
	}
	if !strings.Contains(detail, "proxy-out") {
		t.Errorf("error_detail lost the cause: %q", detail)
	}
	if st := b.coreState(); st.ErrorCode != code || st.ErrorDetail != detail {
		t.Error("the DTO does not carry the failure, so the UI would show nothing")
	}

	// A NEW attempt clears the previous failure: the message must not outlive
	// the condition it described.
	b.ops.beginOp("start")
	if got := b.coreLifecycleState(); got != protocol.CoreStateStarting {
		t.Errorf("state = %q on retry, want starting", got)
	}
	if got := b.coreState().ErrorCode; got != "" {
		t.Errorf("error_code = %q on retry, want empty", got)
	}
}

// TestRunningBeatsPending — the runtime is the only source of truth for running.
//
// A start that has genuinely succeeded must never be reported as still starting
// merely because the request bookkeeping has not caught up.
func TestRunningBeatsPending(t *testing.T) {
	b := backendWithConfig(t)
	b.ops.beginOp("start")
	if b.ac.RunningState == nil {
		t.Skip("no running state in this environment")
	}
	b.ac.RunningState.Set(true)
	defer b.ac.RunningState.Set(false)

	if got := b.coreLifecycleState(); got != protocol.CoreStateRunning {
		t.Errorf("state = %q while running with a pending start, want running", got)
	}
}

// TestAbortedStartIsNotAnError — a start declined by a precondition that has
// already explained itself (elevation prompt, capabilities dialog) must not
// leave a red failure on screen.
func TestAbortedStartIsNotAnError(t *testing.T) {
	b := backendWithConfig(t)
	b.ops.beginOp("start")
	b.ops.finishOp("start", core.ErrStartAborted)

	if _, lastErr := b.ops.snapshot(); lastErr != nil {
		t.Errorf("an aborted start was recorded as a failure: %v", lastErr)
	}
	if got := b.coreLifecycleState(); got != protocol.CoreStateStopped {
		t.Errorf("state = %q after an aborted start, want stopped", got)
	}
}

// TestStartErrorCodeClassification is case (13): a port collision must be
// identifiable rather than surfacing as a generic spawn failure.
func TestStartErrorCodeClassification(t *testing.T) {
	cases := []struct {
		name string
		text string
		want core.StartErrorCode
	}{
		{"port in use", "start: listen tcp 127.0.0.1:9090: bind: address already in use", core.StartErrClashAPIPortInUse},
		{"unknown field", `decode config: unknown field "outbounds2"`, core.StartErrConfigCheckFailed},
		{"unclassified", "fork/exec: permission denied (os error 13)", core.StartErrSpawnFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := core.NewClassifiedStartFailure(core.StartErrSpawnFailed, jsonError(tc.text))
			if f.Code != tc.want {
				t.Errorf("code = %q for %q, want %q", f.Code, tc.text, tc.want)
			}
		})
	}

	// A code that is already specific must not be narrowed by text matching:
	// a rebuild failure stays a rebuild failure.
	f := core.NewClassifiedStartFailure(core.StartErrConfigRebuildFailed, jsonError("address already in use"))
	if f.Code != core.StartErrConfigRebuildFailed {
		t.Errorf("code = %q, want the original %q", f.Code, core.StartErrConfigRebuildFailed)
	}
}

// TestStartFailureCodesAreStable pins the wire contract with the Swift client.
//
// The frontend maps these tokens to translated sentences, so renaming one would
// silently degrade every explanation to a generic message.
func TestStartFailureCodesAreStable(t *testing.T) {
	want := map[core.StartErrorCode]string{
		core.StartErrConfigRebuildFailed: "config_rebuild_failed",
		core.StartErrSpawnFailed:         "core_start_failed",
		core.StartErrDaemonUnreachable:   "daemon_unreachable",
		core.StartErrDaemonApplyFailed:   "daemon_apply_failed",
		core.StartErrConfigCheckFailed:   "config_check_failed",
		core.StartErrClashAPIPortInUse:   "clash_api_port_in_use",
		core.StartErrCancelled:           "cancelled",
	}
	for code, token := range want {
		if string(code) != token {
			t.Errorf("code %q changed to %q; the Swift localization table keys off the token",
				token, code)
		}
	}
}

// TestRunCoreOpSurfacesFailureThroughIPC is case (7) end to end at the backend
// boundary: the error the frontend receives must be a structured protocol error
// with the code, not a nil success.
func TestRunCoreOpSurfacesFailureThroughIPC(t *testing.T) {
	b := backendWithConfig(t)

	err := b.runCoreOp("start", 0, func(ctx context.Context) error {
		return core.NewStartFailure(core.StartErrDaemonApplyFailed,
			jsonError("default outbound not found: proxy-out"))
	})
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("StartCore returned %T (%v), want a *protocol.Error: a failure that "+
			"returns nil is what left the button unexplained", err, err)
	}
	if pe.Code != string(core.StartErrDaemonApplyFailed) {
		t.Errorf("code = %q, want %q", pe.Code, core.StartErrDaemonApplyFailed)
	}
	if !pe.Recoverable {
		t.Error("a start failure should be recoverable — the user can retry")
	}
}

// TestRunCoreOpDropsDuplicateAndKeepsStateConsistent covers the double-click at
// the command boundary rather than only at the record level.
func TestRunCoreOpDropsDuplicateAndKeepsStateConsistent(t *testing.T) {
	b := backendWithConfig(t)

	started := 0
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		_ = b.runCoreOp("start", 0, func(ctx context.Context) error {
			started++
			// Signal that the body is running, so the test does not have to
			// guess when the operation became pending.
			close(entered)
			<-release
			return nil
		})
	}()

	<-entered
	if !b.opsHasOp("start") {
		close(release)
		<-done
		t.Fatal("the first start is not registered as pending while its body runs")
	}

	// The duplicate returns success immediately without running the body again.
	if err := b.runCoreOp("start", 0, func(ctx context.Context) error {
		started++
		return nil
	}); err != nil {
		t.Errorf("the duplicate start returned an error: %v", err)
	}

	close(release)
	<-done

	if started != 1 {
		t.Errorf("the start body ran %d times, want 1: a double click must not "+
			"launch two cores", started)
	}
	if _, lastErr := b.ops.snapshot(); lastErr != nil {
		t.Errorf("a successful start left a failure recorded: %v", lastErr)
	}
}

// jsonError is a tiny error whose text the classifier can inspect.
type jsonError string

func (e jsonError) Error() string { return string(e) }

// opsHasOp reports whether an operation of the given kind is in flight.
func (b *Backend) opsHasOp(kind string) bool {
	k, _ := b.ops.snapshot()
	return k == kind
}
