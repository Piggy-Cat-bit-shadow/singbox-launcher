package core

import (
	"testing"

	coreservices "singbox-launcher/core/services"
)

// TestClosingADaemonBackendUninstallsItsEndpointProvider is the regression test for a
// permanently dark speed readout.
//
// The transport and the verified-endpoint provider are installed together when the daemon
// backend starts, but only the transport was removed by `Close`. The survivor is worse than
// useless: the provider stays INSTALLED and starts answering "no endpoint" once the backend is
// gone. `trafficEndpoint` treats an installed provider as decisive and never falls through to
// the configured endpoint, so after a daemon→classic switch a classic core with a perfectly
// good address resolved nothing — the same user-visible defect as the type-assertion bug the
// provider was introduced to fix, arriving through the opposite door.
//
// This lives in `core` because that is where the field is, and the point of the test is that
// the REAL `Close` produces the state — an earlier version in `backend/service` installed the
// provider removal by hand and then asserted the result, so it passed with the fix reverted.
func TestClosingADaemonBackendUninstallsItsEndpointProvider(t *testing.T) {
	ac := newTestController()
	ac.APIService = &coreservices.APIService{}

	const endpoint = "http://127.0.0.1:9091"
	transport := &daemonProxyTransport{}
	ac.APIService.SetTransport(transport)
	ac.APIService.SetVerifiedClashEndpoint(func() coreservices.ClashTransport {
		return coreservices.ClashTransport{BaseURL: endpoint, Token: "daemon"}
	})

	// The fixture must actually be installed, or removing it proves nothing.
	if got, ok := ac.APIService.VerifiedClashEndpoint(); !ok || got.BaseURL != endpoint {
		t.Fatalf("the fixture is wrong: the provider resolved (%q, ok=%v)", got.BaseURL, ok)
	}

	b := &DaemonBackend{ac: ac, transport: transport}
	b.Close()

	// THE PROPERTY: nothing engine-specific is left claiming authority.
	if got, ok := ac.APIService.VerifiedClashEndpoint(); ok {
		t.Errorf("closing the daemon backend left its endpoint provider installed (it still "+
			"resolves %q). The provider is authoritative while installed, so a classic core "+
			"with a working address would resolve nothing and the speed readout would stay "+
			"dark for the rest of the session", got.BaseURL)
	}
}

// TestClosingADaemonBackendKeepsANewerProvider — the other half, so the fix cannot be
// "clear it unconditionally".
//
// On a daemon→daemon swap the NEW backend installs its transport and provider before the old
// backend's `Close` runs. Clearing unconditionally would leave the new daemon with no endpoint
// at all — trading a dark readout after an engine switch for one after every restart.
func TestClosingADaemonBackendKeepsANewerProvider(t *testing.T) {
	ac := newTestController()
	ac.APIService = &coreservices.APIService{}

	oldTransport := &daemonProxyTransport{}
	b := &DaemonBackend{ac: ac, transport: oldTransport}

	// The new backend has taken over: a different transport and its own provider.
	const newEndpoint = "http://127.0.0.1:9092"
	newTransport := &daemonProxyTransport{}
	ac.APIService.SetTransport(newTransport)
	ac.APIService.SetVerifiedClashEndpoint(func() coreservices.ClashTransport {
		return coreservices.ClashTransport{BaseURL: newEndpoint, Token: "daemon-2"}
	})

	b.Close()

	got, ok := ac.APIService.VerifiedClashEndpoint()
	if !ok || got.BaseURL != newEndpoint {
		t.Fatalf("closing the OLD daemon backend removed the NEW backend's endpoint "+
			"provider (resolved %q, ok=%v); a daemon→daemon swap would leave the live core "+
			"with no endpoint at all", got.BaseURL, ok)
	}
	if ac.APIService.TransportOverride() != coreservices.ProxyTransport(newTransport) {
		t.Error("closing the old backend removed the new backend's transport override")
	}
}
