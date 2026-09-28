package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"singbox-launcher/api"
	"singbox-launcher/core/services"
	"singbox-launcher/internal/debuglog"
	"singbox-launcher/internal/limitread"
	"sort"
)

// Daemon-local Clash API fallback.
//
// WHY THIS EXISTS. A daemon build may implement only part of the proxy RPC
// surface. The live daemon measured for this work answers `Unimplemented` to
// GetGroups and URLTestOutbound while serving SelectOutbound perfectly, so
// listing and measuring were impossible even though the SAME process was
// running the user's core and could answer both over its own Clash API.
//
// WHY IT IS NOT `NewClashTransport(APIService.BaseURL)`. Two reasons, both
// measured:
//
//  1. In daemon mode the runtime config used to have clash_api DELETED, so
//     there was no endpoint at all. That had to be fixed first (see
//     prepareDaemonConfig), by keeping clash_api and forcing it to loopback.
//  2. In Classic mode that BaseURL points at whatever the disk config says,
//     which may be 0.0.0.0 or a LAN address. Reusing it would mean controlling
//     an endpoint we cannot prove belongs to the daemon we are paired with.
//
// So the endpoint is derived from the SAME transformation that produced the
// bytes sent to the daemon, and it is only trusted after the running API proves
// it is that daemon's.

// fallbackReadiness — how far the local Clash fallback has got.
//
// Deliberately NOT a bool. "The config asks for no Clash API" and "the config
// asks for one but nothing is listening yet" need different UI: the first is
// permanent and worth explaining, the second resolves itself once the core
// finishes starting. A bool forces them together, and one of the two messages
// is then always wrong.
type fallbackReadiness int

const (
	// fallbackNotConfigured — the daemon config has no usable clash_api, so
	// there is nothing to fall back to. A stable fact, not a failure.
	fallbackNotConfigured fallbackReadiness = iota
	// fallbackUnverified — configured, but not yet confirmed against the
	// running API. Transient: retried on the next use.
	fallbackUnverified
	// fallbackReady — verified against the running daemon.
	fallbackReady
	// fallbackBlocked — this daemon can never use a local Clash fallback
	// (a remote daemon), regardless of config. A stable fact.
	fallbackBlocked
)

// maxClashResponseBytes caps a Clash API response read.
//
// Large enough for a group listing of a few thousand nodes; the point is not the exact number
// but that EXCEEDING it is an error rather than a truncated parse.
const maxClashResponseBytes = 8 << 20

func (r fallbackReadiness) String() string {
	switch r {
	case fallbackNotConfigured:
		return "not_configured"
	case fallbackUnverified:
		return "unverified"
	case fallbackReady:
		return "ready"
	case fallbackBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// daemonClashFallback — the local Clash API of the daemon's own core.
//
// Protected by mu because readiness is written from the apply path and read
// from proxy requests that run on other goroutines (the latency scheduler runs
// several at once).
type daemonClashFallback struct {
	mu sync.Mutex

	readiness fallbackReadiness
	cfg       DaemonClashFallbackConfig
	// verifiedAt — when the running API last confirmed it belongs to this
	// daemon. Zero means "never".
	verifiedAt time.Time
	// probe — injected in tests so no test ever opens a socket.
	probe *fallbackProber
}

// fallbackVerificationTTL is how long a successful verification stays trustworthy.
//
// Deliberately SHORT. A verification is a statement about a moment: it says "the process
// answering on this loopback port, right now, is this daemon's core, and it agrees with the
// config we sent". The daemon is a separate process that can restart, be replaced, or have
// its core restarted by someone else, and after any of those the same port may belong to a
// DIFFERENT Clash-compatible core — at which point the launcher would be switching nodes on
// something that is not the user's VPN, which is exactly what verifying is for.
//
// The cost of a short window is one extra identity check, and that check is a handful of
// local HTTP requests. The cost of a long one is driving a stranger's API. Re-verification
// happens on the next use, so there is no background polling.
const fallbackVerificationTTL = 2 * time.Minute

// fallbackProber performs the verification request. Injectable for tests: the
// whole point of the verification is that it talks HTTP, and a test that talks
// HTTP is a test that fails on a machine without the daemon.
type fallbackProber struct {
	client *http.Client
	// timeout bounds one verification attempt.
	timeout time.Duration
}

func defaultFallbackProber() *fallbackProber {
	return &fallbackProber{
		client:  &http.Client{Timeout: 3 * time.Second},
		timeout: 3 * time.Second,
	}
}

// setConfigured records what the daemon's config WILL provide.
//
// Called from the apply path AFTER the daemon accepted the config, never
// before: until Apply succeeds the daemon may still be running the previous
// config (or have rolled back to last-good), and claiming the new endpoint is
// live would point the fallback at a port nothing is listening on.
func (f *daemonClashFallback) setConfigured(cfg DaemonClashFallbackConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
	f.verifiedAt = time.Time{}
	if !cfg.Enabled {
		f.readiness = fallbackNotConfigured
		return
	}
	// Configured but not yet proven: the listener may not be up, and even if
	// something listens we have not confirmed it is this daemon.
	f.readiness = fallbackUnverified
}

// block marks the fallback permanently unusable (remote daemon).
func (f *daemonClashFallback) block() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = DaemonClashFallbackConfig{}
	f.readiness = fallbackBlocked
	f.verifiedAt = time.Time{}
}

// invalidate drops verification but keeps the configuration.
//
// Used when the runtime may have changed underneath us (core stop/start,
// reconnect, address or pairing change, config change, mode switch). The next
// use re-verifies instead of trusting a stale "ready".
func (f *daemonClashFallback) invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readiness == fallbackReady {
		f.readiness = fallbackUnverified
	}
	f.verifiedAt = time.Time{}
}

// configuredForTest returns the current configuration (tests only).
func (f *daemonClashFallback) configuredForTest() DaemonClashFallbackConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

// config returns the configuration plus readiness.
func (f *daemonClashFallback) config() (DaemonClashFallbackConfig, fallbackReadiness) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, f.readiness
}

// transportIfReady returns a Clash transport for the verified fallback, or
// ok=false when the fallback must not be used.
//
// ok=false is the SAFE answer and the default: every reason not to trust the
// endpoint — not configured, remote daemon, never verified, verification
// expired — collapses to "do not use it", and the caller reports the action as
// unsupported rather than controlling a process it cannot identify.
func (f *daemonClashFallback) transportIfReady() (services.ClashTransport, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	cfg, readiness := f.cfg, f.readiness
	if readiness != fallbackReady || !cfg.Enabled {
		return services.ClashTransport{}, false
	}

	// "VERIFICATION EXPIRED" IS A REASON LISTED ABOVE, SO IT IS CHECKED HERE.
	//
	// The list enumerated "not configured, remote daemon, never verified, verification
	// expired" while `verifiedAt` was written on every success and read nowhere — so a
	// verification never expired, and once this fallback was trusted it stayed trusted for
	// the life of the process. A daemon restart, a daemon replacement, or a core restart by
	// anyone else can hand the same loopback port to a different Clash-compatible core, and
	// the launcher would keep driving it.
	//
	// Readiness is DEMOTED rather than just refused, so the next use re-verifies instead of
	// the fallback going dark: the endpoint may be perfectly fine, it is the PROOF that has
	// gone stale.
	if f.verifiedAt.IsZero() || time.Since(f.verifiedAt) > fallbackVerificationTTL {
		if f.readiness == fallbackReady {
			f.readiness = fallbackUnverified
		}
		return services.ClashTransport{}, false
	}

	return services.NewClashTransport(cfg.BaseURL, cfg.Token), true
}

// verify confirms that the configured endpoint is really THIS daemon's core.
//
// The check is deliberately not "the port accepts a connection". A bare TCP
// connect succeeds against any unrelated process that happens to hold the port,
// and then the launcher would drive a stranger's API — switching nodes on
// something that is not the user's VPN. So the endpoint must answer a real
// Clash-shaped /proxies, authenticate, and agree with the config we sent about
// which selector groups exist.
//
// What this does NOT prove: that the endpoint is the same process the gRPC
// channel controls. GetRunningConfig is Unimplemented on the measured daemon, so
// no fingerprint is available. The strongest achievable statement is the
// conjunction below, which is why every term is required.
func (f *daemonClashFallback) verify(ctx context.Context, expectedGroups []string) bool {
	// HasSecret is derived from the CONFIGURED fallback, not asserted by the
	// caller: the token in the config is what the request will actually carry, so
	// it is the only honest source for "is there an auth proof".
	return f.verifyWithProof(ctx, identityProof{
		Groups:    expectedGroups,
		HasSecret: f.hasSecret(),
	})
}

// identityProof is what the launcher knows about the config it sent, and
// therefore what it can demand of an endpoint claiming to be its core.
//
// It exists because "the group names match" is a WEAK claim: group names are
// common (Proxy, AI, Auto), so a foreign Clash-compatible core on the same
// loopback port can satisfy it by accident. With a high-entropy per-build secret
// the auth check carries the proof; but the secret can legitimately be EMPTY, and
// then nothing did.
type identityProof struct {
	// Groups are the selector/urltest tags of the config we sent.
	Groups []string
	// Members maps each group to its member set, so the comparison is about
	// CONTENT rather than a name that can coincide.
	Members map[string][]string
	// HasSecret reports whether the request carried a non-empty token. When it
	// did, the auth check is a real proof and the structural checks are a
	// corroboration; when it did not, structure is the ONLY proof there is.
	HasSecret bool
}

// verifyWithProof performs the identity check against a full proof.
func (f *daemonClashFallback) verifyWithProof(ctx context.Context, proof identityProof) bool {
	f.mu.Lock()
	cfg, readiness := f.cfg, f.readiness
	prober := f.probe
	f.mu.Unlock()

	if !cfg.Enabled || readiness == fallbackBlocked {
		return false
	}
	if prober == nil {
		prober = defaultFallbackProber()
	}

	ok := prober.checkIdentityProof(ctx, cfg, proof)
	if !ok {
		// Verification failure is NOT a permanent verdict: the listener may
		// simply not be up yet. Readiness drops to unverified so the next use
		// retries, while capability absence stays a stable fact.
		f.mu.Lock()
		if f.readiness == fallbackReady {
			f.readiness = fallbackUnverified
		}
		f.verifiedAt = time.Time{}
		f.mu.Unlock()
		return false
	}

	f.mu.Lock()
	f.readiness = fallbackReady
	f.verifiedAt = time.Now()
	f.mu.Unlock()
	debuglog.InfoLog("daemon: local Clash API fallback verified at %s", redactURL(cfg.BaseURL))
	return true
}

// clashProxiesResponse is the minimal shape of GET /proxies.
type clashProxiesResponse struct {
	Proxies map[string]struct {
		Type string `json:"type"`
		Now  string `json:"now"`
		All  []string
	} `json:"proxies"`
}

// checkIdentity performs one verification request against group names only.
//
// Kept because it is the shape the existing tests exercise, and it delegates so
// there is exactly one implementation of the checks.
func (p *fallbackProber) checkIdentity(ctx context.Context, cfg DaemonClashFallbackConfig, expectedGroups []string) bool {
	// HasSecret comes from the config being checked, because THAT is what the
	// request will carry — the caller does not get to assert it separately.
	return p.checkIdentityProof(ctx, cfg, identityProof{
		Groups:    expectedGroups,
		HasSecret: cfg.Token != "",
	})
}

// checkIdentityProof performs one verification request.
//
// FAIL CLOSED. The previous version accepted an endpoint whenever every expected
// group name was present — and when `expectedGroups` was EMPTY, the loop checked
// nothing at all and the function returned true. Any Clash-shaped API answering on
// that loopback port with a matching (or absent) token therefore passed identity
// verification, and with an empty secret there was no auth proof either. An empty
// proof is not a satisfied proof.
func (p *fallbackProber) checkIdentityProof(ctx context.Context, cfg DaemonClashFallbackConfig, proof identityProof) bool {
	proxies, err := p.fetchProxies(ctx, cfg)
	if err != nil {
		debuglog.InfoLog("daemon: local Clash API fallback not confirmed: %v", err)
		return false
	}

	// An empty proof cannot establish identity, whatever else succeeded.
	//
	// A SECRET is itself a proof of possession, so a proof that carries one is
	// never "empty": the authenticated request below is the evidence, and the
	// structure checks corroborate it. Without a secret there is no such
	// evidence, and then the structure must exist or nothing does.
	if !proof.HasSecret && len(proof.Groups) == 0 && len(proof.Members) == 0 {
		debuglog.WarnLog("daemon: local Clash API fallback refused: the request carries " +
			"no secret and the config provides nothing to compare, so any " +
			"Clash-compatible API on that port would be accepted")
		return false
	}

	// Group NAMES must be present...
	for _, g := range proof.Groups {
		if _, ok := proxies.Proxies[g]; !ok {
			debuglog.InfoLog("daemon: local Clash API fallback lacks expected group %q", g)
			return false
		}
	}

	// ...and, since a name can coincide by accident, their MEMBERS must match the
	// config we actually sent. A foreign core with a group called "Proxy" will not
	// have our member set.
	checkedMembers := 0
	for tag, wantMembers := range proof.Members {
		got, ok := proxies.Proxies[tag]
		if !ok {
			debuglog.InfoLog("daemon: local Clash API fallback lacks expected group %q", tag)
			return false
		}
		if len(wantMembers) == 0 {
			continue
		}
		if !sameMemberSet(wantMembers, got.All) {
			debuglog.WarnLog("daemon: local Clash API fallback group %q has members %v, "+
				"but the config we sent defines %v", tag, got.All, wantMembers)
			return false
		}
		checkedMembers++
	}

	// WITHOUT a secret, auth proves nothing, so the structural evidence has to
	// carry the whole claim: at least one member set must have been compared.
	//
	// This is the case the original code got wrong. It accepted any endpoint whose
	// group NAMES matched, and accepted EVERYTHING when the group list was empty —
	// so a config with no selectors and no secret produced a proof that was not
	// merely weak but vacuous, satisfied by any Clash-shaped listener on the port.
	//
	// With a secret, the authenticated request above already proves possession,
	// and a config that genuinely has no selectors remains acceptable.
	if !proof.HasSecret && checkedMembers == 0 {
		debuglog.WarnLog("daemon: local Clash API fallback refused: the request carries " +
			"no secret and the config provides no group members to compare, so the " +
			"endpoint cannot be distinguished from any other Clash-compatible API")
		return false
	}
	return true
}

// sameMemberSet compares two member lists as SETS.
//
// Clash reports members in its own order, and a selector's member list is what
// actually identifies the config; order is not part of the claim.
func sameMemberSet(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	w := append([]string(nil), want...)
	g := append([]string(nil), got...)
	sort.Strings(w)
	sort.Strings(g)
	for i := range w {
		if w[i] != g[i] {
			return false
		}
	}
	return true
}

// fetchProxies performs the authenticated GET and decodes it.
func (p *fallbackProber) fetchProxies(ctx context.Context, cfg DaemonClashFallbackConfig) (*clashProxiesResponse, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/proxies"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 401 here means "something is listening but our credentials are not
		// its credentials" — i.e. very likely not our daemon.
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	// BOUNDED WITH OVERFLOW DETECTED, not merely capped.
	//
	// `io.LimitReader(resp.Body, 8<<20)` reads at most 8 MB and says nothing when it stops
	// there, so a response one byte larger is silently TRUNCATED and then fails to parse as
	// JSON — reported to the user as "not a Clash API response" for a perfectly valid API. The
	// error points at the endpoint rather than at the size, and a group listing large enough to
	// exceed 8 MB is exactly the case where the diagnostics matter most.
	body, err := limitread.All(resp.Body, maxClashResponseBytes)
	if err != nil {
		return nil, err
	}
	var out clashProxiesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		// A plain HTTP server on the same port answers 200 with HTML; decoding
		// fails and the endpoint is correctly rejected.
		return nil, fmt.Errorf("not a Clash API response: %w", err)
	}
	if out.Proxies == nil {
		return nil, fmt.Errorf("not a Clash API response: no proxies object")
	}
	return &out, nil
}

// isLocalDaemonAddress reports whether a daemon admin address is on this
// machine.
//
// This is the gate for the whole fallback. The fallback endpoint is
// 127.0.0.1:<port>, which on a REMOTE daemon setup resolves to the launcher's
// own Mac rather than the daemon's host. Reaching such an address would either
// fail or — worse — control some unrelated local core. So a non-local daemon
// simply has no local Clash fallback, and its unsupported actions stay
// unsupported.
//
// One canonical implementation, reusing api.IsLoopbackHost: a second, slightly
// different loopback rule is exactly how a 0.0.0.0 case eventually slips
// through a security gate.
func isLocalDaemonAddress(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	// Accept "host:port"; a bare host is tolerated so a caller that has already
	// split the port still gets a correct answer.
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if strings.Contains(addr, ":") {
			// Could be a bracketed IPv6 without a port, or genuinely malformed.
			if h := strings.Trim(addr, "[]"); net.ParseIP(h) != nil {
				return api.IsLoopbackHost(net.JoinHostPort(h, "0"))
			}
			return false
		}
		return api.IsLoopbackHost(net.JoinHostPort(addr, "0"))
	}
	return api.IsLoopbackHost(addr)
}

// redactURL keeps a URL safe for logs: no credentials, no query.
//
// The Clash token travels in a header rather than the URL, but a URL built from
// user config could still carry userinfo, and logs are shared in bug reports.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// ---------------------------------------------------------------------------
// Per-action fallback, used by daemonProxyTransport when an RPC is ABSENT.
//
// The trigger is capability absence ONLY. A timeout, a permission error, a bad
// request or a missing node is an OPERATIONAL failure and is returned as an
// error: silently retrying it over a different wire would hide the real problem
// and produce two code paths whose difference the user cannot explain.
// ---------------------------------------------------------------------------

// fallbackTransport returns a usable fallback transport, verifying it on first
// use.
//
// expectedGroups is what the config says should exist; it is the evidence that
// the endpoint belongs to the core running OUR config. The STRONGER proof is the
// group MEMBER sets (identityProof): a name can coincide with a foreign core's,
// a member set derived from the bytes we sent cannot. An empty proof is refused
// outright — "nothing to check" is not "checked and passed".
func (b *DaemonBackend) fallbackTransport(ctx context.Context, expectedGroups []string) (services.ClashTransport, bool) {
	return b.fallbackTransportProof(ctx, identityProof{Groups: expectedGroups, HasSecret: b.clashFallback.hasSecret()})
}

// fallbackTransportProof is fallbackTransport with the full identity proof.
func (b *DaemonBackend) fallbackTransportProof(ctx context.Context, proof identityProof) (services.ClashTransport, bool) {
	if _, readiness := b.clashFallback.config(); readiness == fallbackBlocked ||
		readiness == fallbackNotConfigured {
		return services.ClashTransport{}, false
	}
	if tr, ok := b.clashFallback.transportIfReady(); ok {
		return tr, true
	}
	if !b.clashFallback.verifyWithProof(ctx, proof) {
		return services.ClashTransport{}, false
	}
	return b.clashFallback.transportIfReady()
}

// expectedSelectorGroups reads the selector tags the launcher itself configured.
//
// Used as verification evidence, so it must come from the config the daemon was
// given rather than from a fresh fetch — a fetch would beg the question.
func (b *DaemonBackend) expectedSelectorGroups() []string {
	b.fallbackMu.Lock()
	defer b.fallbackMu.Unlock()
	return b.expectedGroups
}

// hasSecret reports whether the configured fallback carries a non-empty token.
//
// The answer decides how much the structural checks have to prove: a real secret
// is a proof of possession on its own, an empty one is not a proof of anything.
func (f *daemonClashFallback) hasSecret() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.Token != ""
}

// identityProofFor builds the full proof from what the last apply registered.
//
// The token presence is read from the fallback config, because THAT is what the
// request will actually carry: "no secret" is precisely the case in which the
// structural evidence has to carry the whole claim.
func (b *DaemonBackend) identityProofFor() identityProof {
	b.fallbackMu.Lock()
	groups := append([]string(nil), b.expectedGroups...)
	members := b.expectedMembers
	b.fallbackMu.Unlock()

	memberCopy := make(map[string][]string, len(members))
	for k, v := range members {
		memberCopy[k] = append([]string(nil), v...)
	}

	return identityProof{
		Groups:    groups,
		Members:   memberCopy,
		HasSecret: b.clashFallback.hasSecret(),
	}
}

// groupProxiesViaFallback lists nodes through the daemon's own Clash API.
func (b *DaemonBackend) groupProxiesViaFallback(group string) ([]api.ProxyInfo, string, error) {
	tr, ok := b.fallbackTransportProof(b.ctx, b.identityProofFor())
	if !ok {
		return nil, "", services.NewProxyCapabilityError(services.CapabilityList)
	}
	return tr.GroupProxies(group)
}

// switchProxyViaFallback selects a node through the daemon's own Clash API.
func (b *DaemonBackend) switchProxyViaFallback(group, name string) error {
	tr, ok := b.fallbackTransport(b.ctx, b.expectedSelectorGroups())
	if !ok {
		return services.NewProxyCapabilityError(services.CapabilitySwitch)
	}
	return tr.SwitchProxy(group, name)
}

// delayViaFallback measures one node through the daemon's own Clash API.
//
// ctx carries RUN cancellation from the latency scheduler and is passed
// straight through, so a superseded or cancelled group test stops the in-flight
// HTTP measurement exactly as it stops the gRPC one.
func (b *DaemonBackend) delayViaFallback(ctx context.Context, name string) (int64, error) {
	tr, ok := b.fallbackTransport(ctx, b.expectedSelectorGroups())
	if !ok {
		return 0, services.NewProxyCapabilityError(services.CapabilityTest)
	}
	return tr.DelayContext(ctx, name)
}
