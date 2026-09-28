package core

import (
	"testing"
	"time"
)

// TestFallbackVerificationExpires is statement 20 (§34 name).
//
// `transportIfReady`'s own comment lists the reasons not to trust the endpoint, and
// includes "verification expired" among them. `verifiedAt` is written on every successful
// verification and was READ NOWHERE — so a verification could never expire, and the
// documented reason was not one of the conditions the code actually checked.
//
// What that costs: once the fallback is verified, it stays trusted for the lifetime of the
// process. The daemon is a separate process that can be restarted, replaced, or have its
// core restarted by someone else, and afterwards the same loopback port may belong to a
// DIFFERENT Clash-compatible core. The launcher then drives somebody else's API — switching
// nodes on something that is not the user's VPN — which is the precise outcome the
// verification exists to prevent.
//
// A verification is a statement about a MOMENT, so it has to be allowed to go stale.
func TestFallbackVerificationExpires(t *testing.T) {
	var f daemonClashFallback

	// A verified, ready fallback.
	f.mu.Lock()
	f.cfg = DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090", Token: "s"}
	f.readiness = fallbackReady
	f.verifiedAt = time.Now()
	f.mu.Unlock()

	if _, ok := f.transportIfReady(); !ok {
		t.Fatal("a freshly verified fallback is not usable")
	}

	// Age the verification well past any sane trust window.
	f.mu.Lock()
	f.verifiedAt = time.Now().Add(-24 * time.Hour)
	f.mu.Unlock()

	if _, ok := f.transportIfReady(); ok {
		t.Fatal("a verification from 24 hours ago is still trusted, so the fallback can " +
			"outlive the daemon it was verified against and end up driving a different " +
			"Clash-compatible core on the same port. The comment above transportIfReady " +
			"lists 'verification expired' as a reason to refuse, so the check has to exist")
	}
}

// TestFallbackVerificationDoesNotExpireWhileFresh — the complementary case, so the fix is
// a TTL and not "always refuse".
func TestFallbackVerificationDoesNotExpireWhileFresh(t *testing.T) {
	var f daemonClashFallback
	f.mu.Lock()
	f.cfg = DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090", Token: "s"}
	f.readiness = fallbackReady
	f.verifiedAt = time.Now()
	f.mu.Unlock()

	if _, ok := f.transportIfReady(); !ok {
		t.Fatal("a verification taken just now was refused; the point is to expire STALE " +
			"verifications, not to require one per request")
	}
}

// TestFallbackTrustWindowIsBounded — the window must be short enough to matter.
//
// A "TTL" measured in days would satisfy the expiry test while still letting the fallback
// outlive every realistic daemon restart, so the bound itself is asserted.
func TestFallbackTrustWindowIsBounded(t *testing.T) {
	if fallbackVerificationTTL <= 0 {
		t.Fatal("no trust window is defined, so a verification never expires")
	}
	if fallbackVerificationTTL > 10*time.Minute {
		t.Fatalf("the fallback trust window is %s. A daemon can restart in seconds, and "+
			"after it does the port may belong to another core, so a window this long "+
			"leaves the launcher driving an unverified endpoint", fallbackVerificationTTL)
	}
}
