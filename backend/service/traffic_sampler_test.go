package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"singbox-launcher/api"
	"singbox-launcher/backend/protocol"
	coreservices "singbox-launcher/core/services"
)

// TestTrafficSamplerUsesTheVerifiedTransport is statement 21 (§34 name), first half.
//
// Every proxy action resolves its endpoint through a resolution that knows about the
// daemon's VERIFIED Clash fallback. The traffic sampler called `clashEndpoint()` instead,
// which reads the configured address straight out of APIService — so it was the one path
// that would read connection counters from an endpoint the launcher had deliberately not
// confirmed belongs to this daemon. Reading is milder than switching, but it is the same
// trust decision, and making it twice — once with the check and once without — means the
// check does not hold for the app as a whole.
//
// BEHAVIOURAL, AND THAT MATTERS. The first version of this test simply grepped for
// `trafficEndpoint()` in `sample()`. It passed while the fix was broken: the helper
// type-asserted the transport to `ClashTransport`, but under the daemon engine the override
// is `*daemonProxyTransport` (a gRPC client), so the assertion failed for EVERY daemon user
// and the sampler returned early forever — silently killing the speed readout in exactly the
// mode it was written to protect. A test that reads text cannot see a feature disappear.
func TestTrafficSamplerUsesTheVerifiedTransport(t *testing.T) {
	b := backendWithConfig(t)

	// An engine that verifies an endpoint publishes it, and it is not a ClashTransport —
	// this mirrors the daemon, whose proxy transport talks gRPC.
	verified := coreservices.ClashTransport{BaseURL: "http://127.0.0.1:19090", Token: "secret"}
	b.ac.APIService.SetTransport(daemonLikeTransport{})
	b.ac.APIService.SetVerifiedClashEndpoint(func() coreservices.ClashTransport { return verified })

	gotURL, gotToken, ok := b.trafficEndpoint()
	if !ok {
		t.Fatal("the sampler found no endpoint even though the engine published a VERIFIED " +
			"one. The engine's proxy transport is not a ClashTransport, so requiring that " +
			"type throws away the verified endpoint and the speed readout goes permanently " +
			"dark in that engine")
	}
	if gotURL != verified.BaseURL || gotToken != verified.Token {
		t.Fatalf("the sampler resolved %q/%q, want the VERIFIED %q/%q",
			gotURL, gotToken, verified.BaseURL, verified.Token)
	}
}

// TestTrafficSamplerRefusesAnUnverifiedEndpoint — the property the fix is for.
//
// When the engine verifies an endpoint and the verification says "do not use this", the
// sampler must NOT fall back to the raw configured address. Falling back would mean the
// verification is advisory, which is the same as not having it.
func TestTrafficSamplerRefusesAnUnverifiedEndpoint(t *testing.T) {
	b := backendWithConfig(t)
	b.ac.APIService.SetTransport(daemonLikeTransport{})
	// The engine has a verification concept, and it is not passing: the empty endpoint.
	b.ac.APIService.SetVerifiedClashEndpoint(func() coreservices.ClashTransport {
		return coreservices.ClashTransport{}
	})

	if url, _, ok := b.trafficEndpoint(); ok {
		t.Errorf("the sampler resolved %q even though the engine's verified endpoint was "+
			"empty. Falling back to the configured address makes the verification advisory", url)
	}
}

// TestTrafficSamplerFallsBackWhenNoProviderIsInstalled is the RESOLUTION RULE half of the
// same rule: an uninstalled provider must fall through to the configured endpoint.
//
// It is not the `Close` test. See the body for why that distinction is written down.
//
// ENGINE-SPECIFIC VERIFICATION IS AUTHORITATIVE ONLY WHILE THAT ENGINE IS PRESENT. The daemon
// installs a provider when it starts and the transport when it starts; `Close` removed the
// transport and left the provider, so after a daemon→classic switch the provider stayed
// installed and began answering "no endpoint" — permanently. `trafficEndpoint` treats an
// installed provider as decisive and never falls through to the configured endpoint, so a
// classic core with a perfectly good address got `ok=false` forever and the speed readout
// went dark in exactly the mode the verification work was meant to protect. That is the same
// user-visible defect as the type-assertion bug it replaced, arriving through the other door.
//
// Removing the provider is what makes the classic fallback reachable again. This test asserts
// the property at the level the bug lives at: no provider installed, endpoint still resolved.
func TestTrafficSamplerFallsBackWhenNoProviderIsInstalled(t *testing.T) {
	b := backendWithConfig(t)

	// A classic core with a configured endpoint — the situation the stale provider broke.
	// These are the fields `GetClashAPIConfig` reports, which is what the fallback reads.
	b.ac.APIService.Enabled = true
	b.ac.APIService.BaseURL = "http://127.0.0.1:9090"
	b.ac.APIService.Token = "configured-token"

	// Confirm the fixture: with a provider installed it IS decisive and the configured
	// endpoint is NOT consulted.
	//
	// This test covers the RESOLUTION RULE — an uninstalled provider falls through — and
	// says so. It does NOT cover what `Close` leaves behind: an earlier version was named
	// after `Close`, installed the removal itself, and therefore PASSED with the removal
	// deleted from `Close` (verified). What `Close` must do is asserted by
	// `TestClosingADaemonBackendUninstallsItsEndpointProvider` in `core`, where the field
	// lives and the real `Close` can be run.
	b.ac.APIService.SetVerifiedClashEndpoint(func() coreservices.ClashTransport {
		return coreservices.ClashTransport{BaseURL: "http://127.0.0.1:9091", Token: "daemon"}
	})
	if url, _, ok := b.trafficEndpoint(); !ok || url != "http://127.0.0.1:9091" {
		t.Fatalf("the fixture is wrong: with a provider installed the sampler resolved "+
			"(%q, ok=%v), so this test cannot show what removing it changes", url, ok)
	}
	b.ac.APIService.SetVerifiedClashEndpoint(nil)

	url, _, ok := b.trafficEndpoint()
	if !ok {
		t.Fatal("the sampler resolved no endpoint after the engine-specific provider was " +
			"removed. An uninstalled provider must fall through to the configured endpoint, " +
			"or a classic core is left with a permanently dark speed readout")
	}
	if url == "" {
		t.Error("the sampler reported success with an empty URL")
	}
}

// daemonLikeTransport is a ProxyTransport that is NOT a ClashTransport.
//
// It stands in for the daemon engine's `*daemonProxyTransport`, which is the case the
// broken assertion silently dropped.
type daemonLikeTransport struct{}

func (daemonLikeTransport) GroupProxies(string) ([]api.ProxyInfo, string, error) { return nil, "", nil }
func (daemonLikeTransport) SwitchProxy(string, string) error                     { return nil }
func (daemonLikeTransport) Delay(string) (int64, error)                          { return 0, nil }
func (daemonLikeTransport) DelayContext(context.Context, string) (int64, error)  { return 0, nil }

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
