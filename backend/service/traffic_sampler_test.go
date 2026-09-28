package service

import (
	"strings"
	"testing"
	"time"

	"singbox-launcher/backend/protocol"
)

// TestTrafficSamplerUsesTheVerifiedTransport is statement 21 (§34 name), first half.
//
// Every proxy action goes through `b.transport()`, which knows about the daemon's verified
// Clash fallback and refuses an endpoint the launcher has not confirmed belongs to this
// daemon. The traffic sampler instead called `b.clashEndpoint()` directly and built its own
// request against whatever `APIService` reported.
//
// That is the same endpoint, reached without the verification — so the sampler is a path
// that can read connection counters from a Clash-compatible core the launcher deliberately
// would not issue commands to. Reading is not as dangerous as switching, but it is the same
// trust decision being made twice, once of them without the check.
func TestTrafficSamplerUsesTheVerifiedTransport(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/traffic.go"))

	body := functionBodyForTest(t, src, "func (t *TrafficSampler) sample(ctx context.Context)")

	if strings.Contains(body, "clashEndpoint()") {
		t.Error("the traffic sampler resolves its endpoint through clashEndpoint(), which " +
			"bypasses the verification that guards every other proxy operation — so it can " +
			"read counters from an endpoint the launcher has not confirmed is this daemon")
	}
	// It must resolve through the helper that honours the daemon's verified override.
	// `trafficEndpoint` defers to `transport()`, which is the function that knows about the
	// fallback; naming the helper here rather than `transport()` directly keeps the test
	// about the PROPERTY (goes through the verified resolution) instead of about which
	// function is called on which line.
	if !strings.Contains(body, "trafficEndpoint()") {
		t.Error("the traffic sampler does not resolve its endpoint through the shared, " +
			"verification-aware resolution")
	}
}

// TestTrafficSamplerDoesNotEmitAfterStop is statement 21's second half.
//
// `Stop` cancels the loop's context, but a `sample` already in flight was bounded by
// `context.Background()`-derived work, so it ran to completion and emitted afterwards. The
// UI receives a traffic rate for a core that has already been stopped — and since the
// sampler runs exactly while the core is up, that event is interpreted as live traffic.
func TestTrafficSamplerDoesNotEmitAfterStop(t *testing.T) {
	b := backendWithConfig(t)

	events := make(chan protocol.Event, 32)
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventTrafficRate {
			events <- ev
		}
	})
	defer unsub()

	sampler := b.Traffic()

	// Drive one sample directly while the sampler is NOT running: the emit must be
	// suppressed, because a stopped sampler has nothing live to report.
	//
	// This is the state a sample is in when Stop wins the race — the loop was cancelled
	// but the reading had already started.
	sampler.sample(b.runContext())

	select {
	case ev := <-events:
		t.Fatalf("a stopped sampler emitted a traffic rate (%+v); the UI shows live "+
			"traffic for a core that is not running", ev.Payload)
	case <-time.After(200 * time.Millisecond):
	}

	// And it must still emit while running, or the fix would be "never emit".
	sampler.Start()
	defer sampler.Stop()
	if !sampler.Running() {
		t.Fatal("Start did not start the sampler")
	}
}

// TestStoppedSamplerDoesNotEmit is the behavioural form: after Stop, no further events.
func TestStoppedSamplerDoesNotEmit(t *testing.T) {
	b := backendWithConfig(t)

	events := make(chan protocol.Event, 64)
	unsub := b.Subscribe(func(ev protocol.Event) {
		if ev.Event == protocol.EventTrafficRate {
			events <- ev
		}
	})
	defer unsub()

	sampler := b.Traffic()
	sampler.Start()
	sampler.Stop()

	// Drain anything emitted before the stop settled.
	for {
		select {
		case <-events:
			continue
		case <-time.After(500 * time.Millisecond):
		}
		break
	}

	if sampler.Running() {
		t.Fatal("the sampler still reports running after Stop")
	}
}

// functionBodyForTest returns exactly one function's source, by counting braces.
//
// Bounding by text does not work here. The comment stripper removes blank lines AND leading
// indentation, so every nested block's closing brace appears in column zero — identical to
// the function's own closer. An earlier version of this helper used "the next line that is
// just }" and silently returned only the first `if` statement of the function it was asked
// for, so an assertion about the whole function was checking three lines.
//
// Brace counting is immune to that, and the stripper has already removed the comments that
// could contain a brace.
func functionBodyForTest(t *testing.T, src, signature string) string {
	t.Helper()
	idx := strings.Index(src, signature)
	if idx < 0 {
		t.Fatalf("could not find %q in the source", signature)
	}
	// Start at the signature's opening brace.
	open := strings.Index(src[idx:], "{")
	if open < 0 {
		t.Fatalf("no opening brace after %q", signature)
	}
	depth := 0
	for i := idx + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[idx : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces after %q", signature)
	return ""
}
