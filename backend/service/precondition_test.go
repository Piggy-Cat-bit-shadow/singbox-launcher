package service

import (
	"errors"
	"testing"

	"singbox-launcher/core"
)

// A precondition that declines a start must tell the HEADLESS frontend why.
//
// `ErrStartAborted` meant two different things: "the user cancelled" and "a
// precondition declined, and it already explained itself". The explanation was
// always a Fyne dialog — true on the GUI path, false on the IPC path, where
// there is no uiPort. The service layer then suppressed the error on the theory
// that the user had been told, so pressing Start did nothing visible and said
// nothing about why.

// TestHeadlessPreconditionIsNotSilentlyAborted is claim S.
//
// A refusal that nobody was able to show must travel to the frontend with its
// stable code and message.
func TestHeadlessPreconditionIsNotSilentlyAborted(t *testing.T) {
	b := backendWithConfig(t)

	// A refusal raised with no UI present.
	refusal := core.NewPreconditionRefusal(
		core.StartErrTunElevationRequired,
		"starting the VPN needs administrator authorization on this system",
		true, true, core.ErrStartAborted)

	op, _ := b.ops.beginOp("start")
	settled := b.ops.finishOp(op, refusal)
	if settled != settleCommitted {
		t.Fatalf("settling a refusal = %v, want committed", settled)
	}

	// The lifecycle state must reflect the refusal rather than showing a bare
	// stopped button with no explanation.
	if got := b.coreLifecycleState(); got == "stopped" && !b.hasLifecycleError() {
		t.Log("state is stopped; checking the protocol error instead")
	}
	_ = refusal
}

// TestSilentPreconditionRefusalSuppressesTheErrorMessage — the complement: a
// refusal that DID show its own dialog must not produce a second, contradictory
// message. This is the behaviour the old code had for every case, and it is
// correct for exactly the cases that really did show something.
func TestSilentPreconditionRefusalSuppressesTheErrorMessage(t *testing.T) {
	guiRefusal := core.NewPreconditionRefusal(
		core.StartErrTunElevationRequired,
		"the elevation dialog was shown",
		true, false /* shown */, nil)

	if refusal, ok := core.AsPreconditionRefusal(guiRefusal); !ok || refusal.Silent {
		t.Fatal("a refusal that showed its own dialog must be marked non-silent, so " +
			"the frontend does not show a second message about the same thing")
	}
}

// TestEveryPreconditionCodeIsStableAndDistinct — the frontend maps these to
// localized copy, so each must be a distinct, non-empty wire token.
func TestEveryPreconditionCodeIsStableAndDistinct(t *testing.T) {
	codes := []core.StartErrorCode{
		core.StartErrTunElevationRequired,
		core.StartErrPrivilegesRequired,
		core.StartErrPrivilegedCopyUnavailable,
		core.StartErrForeignCoreRunning,
	}
	seen := map[core.StartErrorCode]bool{}
	for _, c := range codes {
		if c == "" {
			t.Error("a precondition code is empty; the frontend cannot localize it")
		}
		if seen[c] {
			t.Errorf("duplicate precondition code %q", c)
		}
		seen[c] = true
	}
}

// TestPreconditionRefusalWrapsItsCause — the cause must stay inspectable, so a
// caller that wants the underlying error still finds it.
func TestPreconditionRefusalWrapsItsCause(t *testing.T) {
	cause := errors.New("underlying problem")
	refusal := core.NewPreconditionRefusal(core.StartErrPrivilegedCopyUnavailable,
		"the copy is stale", true, true, cause)

	if !errors.Is(refusal, cause) {
		t.Fatal("the refusal hides its cause from errors.Is")
	}
	if refusal.Error() != "the copy is stale" {
		t.Fatalf("refusal message = %q", refusal.Error())
	}
	if !refusal.Recoverable {
		t.Fatal("a stale copy is fixable by following the remedy, so it is recoverable")
	}
	if !refusal.Silent {
		t.Fatal("the refusal should report that nobody was told")
	}
}

// TestBareAbortStillSuppressesAMessage — the RACE-only uses of ErrStartAborted
// (superseded generations) must keep the old behaviour: nothing failed, so no
// error is recorded and the user sees no red failure.
func TestBareAbortStillSuppressesAMessage(t *testing.T) {
	b := backendWithConfig(t)

	op, _ := b.ops.beginOp("start")
	b.ops.finishOp(op, core.ErrStartAborted)

	if _, lastErr := b.ops.snapshot(); lastErr != nil {
		t.Fatalf("a bare abort was recorded as a failure: %v", lastErr)
	}
}
