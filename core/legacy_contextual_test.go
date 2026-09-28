package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Compile-time proof that the CLASSIC engine provides the contextual path.
//
// This is the whole of claim E. `AppController.StartVPNContext` checks for
// contextualCoreBackend and otherwise calls the fire-and-forget StartVPN and
// returns nil at once. LegacyBackend had no contextual method, so on classic —
// the DEFAULT engine — every headless start reported success the instant a
// goroutine was spawned, and the caller then published `stopped` because nothing
// had started yet. The promised "start awaits the real commit point" held for
// the daemon and not for classic.
//
// A compile-time assertion is the right shape for this: if the method is
// removed, this file stops building, which is exactly the failure that must not
// pass silently.
var (
	_ contextualCoreBackend = (*LegacyBackend)(nil)
	_ contextualCoreBackend = (*DaemonBackend)(nil)
)

// TestLegacyBackendImplementsContextualStart asserts the same fact at runtime, so
// a behavioural test can rely on it and a broken build is not the only signal.
func TestLegacyBackendImplementsContextualStart(t *testing.T) {
	ac := newTestController()
	var b CoreBackend = NewLegacyBackend(ac)
	if _, ok := b.(contextualCoreBackend); !ok {
		t.Fatal("LegacyBackend does not implement contextualCoreBackend; the headless " +
			"path would silently fall back to fire-and-forget and report a start " +
			"finished before it began")
	}
}

// TestClassicStartContextAwaitsTheCommitPoint is the behavioural half: the
// contextual start must not return until the work it describes has finished.
//
// The seam is `legacyOps`, which is how the classic backend reaches
// ProcessService. A start body that blocks proves whether the caller waited for
// it or merely launched it.
func TestClassicStartContextAwaitsTheCommitPoint(t *testing.T) {
	ac := newTestController()
	release := make(chan struct{})
	bodyEntered := make(chan struct{})

	b := NewLegacyBackend(ac)
	b.ops = &legacyOps{
		start: func(skip bool) {},
		startContext: func(ctx context.Context, skip bool) error {
			close(bodyEntered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		stop:           func() {},
		restart:        func() {},
		restartContext: func(ctx context.Context) error { return nil },
	}

	returned := make(chan error, 1)
	go func() { returned <- b.StartVPNContext(context.Background()) }()

	<-bodyEntered

	// The start body is still running. startContext must NOT have returned.
	select {
	case err := <-returned:
		t.Fatalf("StartVPNContext returned (%v) while the start body was still "+
			"running; the headless caller would publish a finished start before "+
			"anything happened", err)
	case <-time.After(150 * time.Millisecond):
		// Correct: still waiting on the real outcome.
	}

	close(release)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("StartVPNContext reported %v after the body succeeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartVPNContext never returned after the body completed")
	}
}

// TestClassicStartContextPropagatesCancellation — the start must observe a
// cancelled context rather than running to completion regardless.
func TestClassicStartContextPropagatesCancellation(t *testing.T) {
	ac := newTestController()
	bodyEntered := make(chan struct{})

	b := NewLegacyBackend(ac)
	b.ops = &legacyOps{
		start: func(skip bool) {},
		startContext: func(ctx context.Context, skip bool) error {
			close(bodyEntered)
			<-ctx.Done()
			return ctx.Err()
		},
		stop:           func() {},
		restart:        func() {},
		restartContext: func(ctx context.Context) error { return nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { returned <- b.StartVPNContext(ctx) }()

	<-bodyEntered
	cancel()

	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StartVPNContext returned %v after cancellation, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StartVPNContext ignored the cancellation")
	}
}

// TestClassicRestartContextAwaitsTheReplacement — the restart path must also end
// at a real outcome, not at "a goroutine was launched".
func TestClassicRestartContextAwaitsTheReplacement(t *testing.T) {
	ac := newTestController()
	release := make(chan struct{})
	entered := make(chan struct{})
	done := make(chan error, 1)

	b := NewLegacyBackend(ac)
	b.ops = &legacyOps{
		start:        func(skip bool) {},
		startContext: func(ctx context.Context, skip bool) error { return nil },
		stop:         func() {},
		restart:      func() {},
		restartContext: func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}

	go func() { done <- b.RestartVPNContext(context.Background()) }()
	<-entered

	select {
	case err := <-done:
		t.Fatalf("RestartVPNContext returned (%v) while the restart was still running", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RestartVPNContext reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RestartVPNContext never returned")
	}
}

// TestClassicContextualMethodsTolerateAnUnwiredController — the contextual
// methods must not panic when the controller or ProcessService is absent, since
// the headless backend may construct them early.
func TestClassicContextualMethodsTolerateAnUnwiredController(t *testing.T) {
	b := &LegacyBackend{}
	if err := b.StartVPNContext(context.Background()); err != nil {
		t.Errorf("StartVPNContext on an unwired backend = %v, want nil", err)
	}
	if err := b.RestartVPNContext(context.Background()); err != nil {
		t.Errorf("RestartVPNContext on an unwired backend = %v, want nil", err)
	}
}

// TestClassicStopContextReportsCancellationNotSuccess — the stop itself must run
// to completion even if the caller stops waiting.
//
// Abandoning a half-finished teardown would leave the core in whatever state the
// interruption produced, so the contextual stop lets the work finish and reports
// the caller's cancellation instead. "The caller gave up" and "the core stopped"
// are different statements and must not be conflated.
func TestClassicStopContextReportsCancellationNotSuccess(t *testing.T) {
	ac := newTestController()
	stopFinished := make(chan struct{})

	b := NewLegacyBackend(ac)
	b.ops = &legacyOps{
		start:          func(skip bool) {},
		startContext:   func(ctx context.Context, skip bool) error { return nil },
		stop:           func() { close(stopFinished) },
		restart:        func() {},
		restartContext: func(ctx context.Context) error { return nil },
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := b.StopVPNContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StopVPNContext = %v with a cancelled context, want context.Canceled", err)
	}
	// The teardown must still have been started: the core's fate cannot depend on
	// whether a caller was still listening.
	select {
	case <-stopFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never ran; reporting cancellation must not skip the teardown")
	}
}
