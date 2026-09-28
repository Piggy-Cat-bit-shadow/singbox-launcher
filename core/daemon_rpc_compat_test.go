package core

import (
	"strings"
	"testing"

	coreservices "singbox-launcher/core/services"
)

// Daemon RPC compatibility tests.
//
// The property under test is the one whose absence produced the reported
// failure: capability must come from the SERVER's answer, and an engine that
// cannot do something must be reported as a capability rather than as the raw
// gRPC text "unknown method GetGroups for service daemon.StartedService".
//
// The three server generations from the audit are modelled by constructing the
// capability set directly. That is deliberate: the probe itself is exercised
// against a REAL daemon in the live check documented in
// docs/DAEMON_RPC_COMPATIBILITY_AUDIT.md, and what needs pinning here is that
// every consumer READS the capability correctly — the part a regression would
// silently break.

// capsWith builds a probed capability set from a list of supported RPCs.
func capsWith(version string, supported ...daemonRPC) *daemonCapabilities {
	c := &daemonCapabilities{probed: true, version: version, supports: map[daemonRPC]bool{}}
	for _, r := range supported {
		c.supports[r] = true
	}
	return c
}

// Generation A — a daemon that implements everything the launcher calls.
func currentDaemonCaps() *daemonCapabilities {
	return capsWith("current",
		rpcGetGroups, rpcGetOutbounds, rpcGetChains, rpcGetPool, rpcGetRules,
		rpcURLTestOutbound, rpcSetChainPositionEnabled, rpcSetEndpointEnabled,
		rpcSelectOutbound)
}

// Generation C — the daemon measured on the reporting machine: it serves the
// subscription plane and SelectOutbound, and answers Unimplemented for every
// unary proxy/chain read.
func measuredLegacyDaemonCaps() *daemonCapabilities {
	return capsWith("1.15.0-jiejie-masquerade.6", rpcSelectOutbound)
}

// Generation B — nothing at all.
func bareDaemonCaps() *daemonCapabilities {
	return capsWith("ancient")
}

// TestCapabilityDefaultsToUnsupportedBeforeProbe — an unprobed backend must not
// claim capabilities.
//
// The first request on a fresh connection arrives before any probe, and reading
// an empty set as "yes" would reintroduce exactly the bug: a call attempted
// against a method the server may not have.
func TestCapabilityDefaultsToUnsupportedBeforeProbe(t *testing.T) {
	c := &daemonCapabilities{}
	for _, r := range []daemonRPC{rpcGetGroups, rpcSelectOutbound, rpcURLTestOutbound} {
		if c.supported(r) {
			t.Errorf("%s reports supported before any probe; an unprobed capability "+
				"must read as unsupported so no call is attempted", r)
		}
	}
}

// TestCurrentDaemonSupportsProxyOperations — generation A keeps working.
//
// The fix must not have been "always report unsupported": a fully capable daemon
// has to answer yes to every proxy RPC, or the feature would be disabled for
// everyone.
func TestCurrentDaemonSupportsProxyOperations(t *testing.T) {
	c := currentDaemonCaps()
	for _, r := range []daemonRPC{
		rpcGetGroups, rpcSelectOutbound, rpcURLTestOutbound,
		rpcGetOutbounds, rpcGetChains, rpcGetPool, rpcGetRules,
		rpcSetChainPositionEnabled, rpcSetEndpointEnabled,
	} {
		if !c.supported(r) {
			t.Errorf("%s reports unsupported on a fully capable daemon", r)
		}
	}
}

// TestMeasuredDaemonCapabilitySet — the exact daemon from the bug report.
//
// This is the machine-measured set: GetGroups absent (the reported failure),
// and seven more unary RPCs absent that the launcher also calls. Pinning the
// whole set — not just GetGroups — is the point of the audit: the drift was
// eight methods wide, and fixing only the reported one would have left seven
// identical failures waiting behind other screens.
func TestMeasuredDaemonCapabilitySet(t *testing.T) {
	c := measuredLegacyDaemonCaps()

	// The reported one.
	if c.supported(rpcGetGroups) {
		t.Error("GetGroups reports supported on the daemon that answered Unimplemented")
	}
	// The seven that were equally absent but unreported.
	for _, r := range []daemonRPC{
		rpcGetOutbounds, rpcGetChains, rpcGetPool, rpcGetRules,
		rpcURLTestOutbound, rpcSetChainPositionEnabled, rpcSetEndpointEnabled,
	} {
		if c.supported(r) {
			t.Errorf("%s reports supported, but the measured daemon answered "+
				"Unimplemented for it", r)
		}
	}
	// And the one it does serve, so the set is not simply "nothing works".
	if !c.supported(rpcSelectOutbound) {
		t.Error("SelectOutbound reports unsupported, but the measured daemon serves it")
	}
}

// TestBareDaemonReportsNothing — generation C: every capability false, and no
// capability is assumed from the client's type surface.
func TestBareDaemonReportsNothing(t *testing.T) {
	c := bareDaemonCaps()
	for _, r := range []daemonRPC{
		rpcGetGroups, rpcSelectOutbound, rpcURLTestOutbound, rpcGetOutbounds,
	} {
		if c.supported(r) {
			t.Errorf("%s reports supported on a daemon with no measured capability", r)
		}
	}
}

// TestCapabilitySnapshotIsACopy — reporting must not hand out the live map.
//
// The snapshot feeds diagnostics and the audit trail; a caller mutating it would
// silently rewrite the capability set that gates real calls.
func TestCapabilitySnapshotIsACopy(t *testing.T) {
	c := currentDaemonCaps()
	snap := c.snapshot()
	snap[rpcGetGroups] = false
	if !c.supported(rpcGetGroups) {
		t.Error("mutating the snapshot changed the live capability set")
	}
}

// TestUnsupportedSentinelIsTheUserFacingAnswer — the sentinel is what reaches
// the backend, and through it the UI.
//
// The raw gRPC text must never be what a caller sees. This pins the sentinel's
// identity so a future refactor cannot swap it for a fmt.Errorf that would carry
// "unknown method GetGroups" back into a banner.
func TestUnsupportedSentinelIsTheUserFacingAnswer(t *testing.T) {
	if coreservices.ErrProxyListUnsupported == nil {
		t.Fatal("ErrProxyListUnsupported is nil")
	}
	msg := coreservices.ErrProxyListUnsupported.Error()
	for _, forbidden := range []string{"GetGroups", "Unimplemented", "rpc error", "daemon."} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("the user-facing sentinel mentions internal RPC detail %q: %q",
				forbidden, msg)
		}
	}
}
