package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"singbox-launcher/core"
)

// TestImportCoreRejectedWhileStarting is claim 24, and it is a TOCTOU.
//
// `coreIsStoppedForReplacement` consults `RunningState.IsRunning()` and the VPN
// button state — both of which stay false while a start operation is in flight. A
// classic start that has been ACCEPTED but has not yet spawned leaves the running flag
// clear, so an import arriving in that window was permitted to atomically rename the
// core binary while the start goroutine was about to exec the old path. The result is
// the classic mismatch: the version and config were checked against one file and the
// process runs another.
//
// The check must consult the LIFECYCLE, not a process-running boolean.
func TestImportCoreRejectedWhileStarting(t *testing.T) {
	b := backendWithConfig(t)

	// Block the start handler so the operation is in flight but nothing has spawned.
	entered := make(chan struct{})
	release := make(chan struct{})
	installOps(t, b, &core.LegacyOpsForTest{
		StartContext: func(ctx context.Context, skipRunningCheck bool) error {
			close(entered)
			<-release
			return nil
		},
	})

	go func() { _ = b.StartCore() }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the start operation never began")
	}

	// The running flag is still false: the process has not spawned yet. This is
	// exactly the window the old check could not see.
	if b.ac.RunningState.IsRunning() {
		t.Fatal("precondition: the running flag should still be clear during a blocked start")
	}

	// Importing now must be refused.
	_, err := b.ImportCoreFile("/nonexistent/path/to/sing-box")
	if err == nil {
		t.Fatal("a core replacement was accepted while a START is in flight; the " +
			"start goroutine is about to exec the file being renamed underneath it")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "busy") &&
		!strings.Contains(strings.ToLower(err.Error()), "running") &&
		!strings.Contains(strings.ToLower(err.Error()), "progress") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	close(release)
}

// TestImportCoreSerializedWithStart is claim 25.
//
// Even with a correct "is it busy?" check, the check and the rename are two steps. A
// start that begins BETWEEN them still races the import. The invariant must be a
// mutual-exclusion boundary the lifecycle itself owns, not a bool the import path
// re-derives.
func TestImportCoreSerializedWithStart(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/core_import.go"))

	if !contains(src, "acquireCoreReplacementLease()") {
		t.Fatal("core replacement takes no mutual-exclusion boundary against the " +
			"lifecycle; a start beginning between the busy check and the rename still " +
			"races the import")
	}
	// And the lease must be HELD across the work, not merely taken and dropped.
	if !contains(src, "defer release()") {
		t.Error("the replacement lease is not held for the duration of the import")
	}
}

// TestImportCoreRejectedWhileCoreLifecycleIsNotSettled — the settled-state rule.
//
// Replacing the core binary is only safe from the fully stopped state. "Stopping" is
// not stopped: the process may still be terminating, and a restart or a daemon apply
// is mid-flight. The check must ask for a SETTLED state rather than for the absence of
// one specific condition.
func TestImportCoreRejectedWhileCoreLifecycleIsNotSettled(t *testing.T) {
	b := backendWithConfig(t)

	// A stop in flight: the operation record says `stop`, and the running flag may
	// already be false.
	if _, started := b.ops.beginOp("stop"); !started {
		t.Fatal("could not start a stop operation")
	}

	if _, err := b.ImportCoreFile("/nonexistent/path/to/sing-box"); err == nil {
		t.Fatal("a core replacement was accepted while a STOP is in flight")
	}
}

// TestCoreIsStoppedForReplacementConsultsTheOperationRecord — the precise mechanism.
func TestCoreIsStoppedForReplacementConsultsTheOperationRecord(t *testing.T) {
	b := backendWithConfig(t)

	// Idle: replacement is permitted.
	if !b.coreIsStoppedForReplacement() {
		t.Fatal("replacement is refused while the core is settled and stopped")
	}

	// Any operation in flight makes it unsafe.
	for _, kind := range []string{"start", "stop", "restart"} {
		b2 := backendWithConfig(t)
		if _, started := b2.ops.beginOp(kind); !started {
			t.Fatalf("could not begin a %s operation", kind)
		}
		if b2.coreIsStoppedForReplacement() {
			t.Errorf("replacement is permitted while a %s operation is in flight; the "+
				"core file can be renamed while the lifecycle is using it", kind)
		}
	}
}

// TestTwoConcurrentImportsAreSerialized — the lease must exclude the import path from
// ITSELF as well, not only from start/stop.
//
// Two simultaneous replacements would both validate a candidate and then both rename
// onto the same target, so the file that ends up installed is whichever finished last
// while the result reported to the user comes from whichever returned first.
func TestTwoConcurrentImportsAreSerialized(t *testing.T) {
	b := backendWithConfig(t)

	release, err := b.acquireCoreReplacementLease()
	if err != nil {
		t.Fatalf("the first lease was refused while the core is idle: %v", err)
	}

	// A second acquisition must be refused while the first is held.
	if _, err := b.acquireCoreReplacementLease(); err == nil {
		t.Fatal("two core replacements can run concurrently; both rename onto the same " +
			"target, so the installed file and the reported result can disagree")
	}

	// After release the lease is available again, or a refused import would leave the
	// path permanently blocked.
	release()
	if _, err := b.acquireCoreReplacementLease(); err != nil {
		t.Fatalf("the lease was not released: %v", err)
	}
}
