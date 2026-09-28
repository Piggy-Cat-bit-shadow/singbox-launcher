package core

import "context"

// Test seams for the classic backend.
//
// They live in the production package because the IPC-layer acceptance tests need
// to drive the REAL call chain — Backend.StartCore → AppController.StartVPNContext
// → LegacyBackend → the process operation — and substitute only the PROCESS step
// at the bottom.
//
// The reason this matters: an earlier generation of these tests replaced
// `runCoreOp`'s closure, which proves only that `runCoreOp` awaits what it was
// given. The actual defect was that the production closure returned immediately,
// so `StartCore` reported success while nothing had been spawned. Catching that
// requires the fake to sit where the real work happens.

// LegacyOpsForTest overrides the ProcessService operations the classic backend
// calls. A nil field falls back to the real implementation.
type LegacyOpsForTest struct {
	StartContext   func(ctx context.Context, skipRunningCheck bool) error
	RestartContext func(ctx context.Context) error
	Stop           func()
}

// SetLegacyOpsForTest installs the override on a LegacyBackend.
func SetLegacyOpsForTest(b *LegacyBackend, ops *LegacyOpsForTest) {
	if b == nil || ops == nil {
		return
	}
	existing := b.opsOrDefault()
	if ops.StartContext != nil {
		existing.startContext = ops.StartContext
	}
	if ops.RestartContext != nil {
		existing.restartContext = ops.RestartContext
	}
	if ops.Stop != nil {
		existing.stop = ops.Stop
	}
	b.ops = &existing
}

// SetBackendForTest swaps the active backend without the persistence and
// lifecycle machinery SwitchBackendMode performs, so a test can exercise one
// backend's call chain in isolation.
func (ac *AppController) SetBackendForTest(b CoreBackend) {
	if ac == nil {
		return
	}
	ac.setBackend(b)
}

// LegacyBackendForTest exposes the live classic backend so an IPC-layer test can
// install process-level overrides on the SAME object the production call chain
// reaches.
//
// Returning the backend rather than replacing it is deliberate: the test must
// exercise the real StartVPNContext → LegacyBackend → operation-record path, and
// only substitute the process step at the bottom.
func (ac *AppController) LegacyBackendForTest() (*LegacyBackend, bool) {
	if ac == nil {
		return nil, false
	}
	b, ok := ac.Backend().(*LegacyBackend)
	return b, ok
}

// switchModeSeamForTest lets a test decide the outcome of a backend-mode switch.
//
// The real switch validates against the daemon's configuration, so on a fixture with
// no paired daemon it is always REFUSED. That makes the SUCCESS path untestable from
// the IPC layer: a test can only observe the refusal, and "the cancellation did not
// happen" is then indistinguishable from "the cancellation is broken". A seam that
// answers the validation question directly is what lets a test drive success and
// refusal as two separate, deterministic cases instead of skipping one of them.
var switchModeSeamForTest func(mode BackendMode) (handled bool, err error)

// SetSwitchModeSeamForTest installs the seam, returning a restore function.
func SetSwitchModeSeamForTest(fn func(mode BackendMode) (bool, error)) func() {
	prev := switchModeSeamForTest
	switchModeSeamForTest = fn
	return func() { switchModeSeamForTest = prev }
}

// exitHookForTest lets a test block the teardown, so "shutdown has begun" and
// "shutdown has finished" can be observed as two distinct states.
var exitHookForTest func()

// SetExitHookForTest installs a hook that runs at the START of GracefulExit and
// replaces any previous one. Passing nil clears it.
func (ac *AppController) SetExitHookForTest(fn func()) {
	if ac == nil {
		return
	}
	exitHookForTest = fn
}
