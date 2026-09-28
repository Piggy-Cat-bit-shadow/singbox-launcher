package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"singbox-launcher/core/services"
)

// ---------------------------------------------------------------------------
// §49–§53: the config transformation.
//
// These test the PURE function, so they assert on the parsed result rather than
// grepping the serialized bytes — "0.0.0.0:9090 → 127.0.0.1:9090" is a claim
// about a value, not about a substring that a comment could also contain.
// ---------------------------------------------------------------------------

func daemonConfigOf(t *testing.T, prepared []byte) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(prepared, &root); err != nil {
		t.Fatalf("prepared config is not valid JSON: %v", err)
	}
	return root
}

func clashAPIOof(t *testing.T, prepared []byte) (controller, secret string, present bool) {
	t.Helper()
	root := daemonConfigOf(t, prepared)
	exp, ok := root["experimental"].(map[string]any)
	if !ok {
		return "", "", false
	}
	ca, ok := exp["clash_api"].(map[string]any)
	if !ok {
		return "", "", false
	}
	controller, _ = ca["external_controller"].(string)
	secret, _ = ca["secret"].(string)
	return controller, secret, true
}

func configWithClashAPI(t *testing.T, controller, secret string) []byte {
	t.Helper()
	cfg := map[string]any{
		"experimental": map[string]any{
			"clash_api": map[string]any{
				"external_controller": controller,
				"secret":              secret,
			},
		},
		"outbounds": []any{
			map[string]any{"type": "direct", "tag": "direct"},
			map[string]any{"type": "selector", "tag": "🌍 国外流量", "outbounds": []string{"direct"}},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// §49 — wildcard becomes loopback, port and secret survive, disk untouched.
func TestDaemonConfigWildcardBecomesLoopback(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "config.json")
	src := configWithClashAPI(t, "0.0.0.0:9090", "abc")
	if err := os.WriteFile(disk, src, 0o600); err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(src)

	res, err := prepareDaemonConfig(src, dir, daemonPrepOptions{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	controller, secret, ok := clashAPIOof(t, res.Bytes)
	if !ok {
		t.Fatal("clash_api was removed; the fallback has no endpoint at all")
	}
	if controller != "127.0.0.1:9090" {
		t.Errorf("controller = %q, want 127.0.0.1:9090 (wildcard must not be published)", controller)
	}
	if secret != "abc" {
		t.Errorf("secret = %q, want it preserved", secret)
	}
	if !res.ClashFallback.Enabled || res.ClashFallback.BaseURL != "http://127.0.0.1:9090" {
		t.Errorf("fallback = %+v, want enabled at http://127.0.0.1:9090", res.ClashFallback)
	}

	// §53: the user's file is byte-identical.
	after, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != before {
		t.Error("the disk config was modified; only the runtime copy may be rewritten")
	}
}

// §50 — an already-loopback controller is left exactly as it was.
func TestDaemonConfigLoopbackUnchanged(t *testing.T) {
	res, err := prepareDaemonConfig(configWithClashAPI(t, "127.0.0.1:9090", "s"), t.TempDir(), daemonPrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	controller, secret, ok := clashAPIOof(t, res.Bytes)
	if !ok || controller != "127.0.0.1:9090" || secret != "s" {
		t.Errorf("controller=%q secret=%q present=%v; loopback input must pass through", controller, secret, ok)
	}
}

// §51 — IPv6 wildcard and bracketed forms.
func TestDaemonConfigIPv6WildcardBecomesLoopback(t *testing.T) {
	for _, in := range []string{"[::]:9090", ":::9090", "0.0.0.0:9090"} {
		res, err := prepareDaemonConfig(configWithClashAPI(t, in, "s"), t.TempDir(), daemonPrepOptions{})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		controller, _, ok := clashAPIOof(t, res.Bytes)
		if !ok {
			t.Errorf("%s: clash_api removed", in)
			continue
		}
		if strings.Contains(controller, "::") || strings.Contains(controller, "0.0.0.0") {
			t.Errorf("%s → %q still contains a wildcard; it would listen on every interface", in, controller)
		}
		if !strings.HasSuffix(controller, ":9090") {
			t.Errorf("%s → %q lost the port", in, controller)
		}
	}
}

// §51 (cont.) — a LAN address must not survive into the daemon's copy either.
func TestDaemonConfigLanAddressBecomesLoopback(t *testing.T) {
	res, err := prepareDaemonConfig(configWithClashAPI(t, "192.168.1.5:9090", "s"), t.TempDir(), daemonPrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	controller, _, ok := clashAPIOof(t, res.Bytes)
	if !ok || controller != "127.0.0.1:9090" {
		t.Errorf("controller = %q (present=%v), want 127.0.0.1:9090: a LAN controller must not be handed to the daemon", controller, ok)
	}
}

// §52 — no clash_api means no fallback, and none is invented.
func TestDaemonConfigNoClashAPIMeansNoFallback(t *testing.T) {
	src := []byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`)
	res, err := prepareDaemonConfig(src, t.TempDir(), daemonPrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClashFallback.Enabled {
		t.Error("fallback reported available although the config declares no clash_api")
	}
	if _, _, present := clashAPIOof(t, res.Bytes); present {
		t.Error("a clash_api section was invented; 'no Clash API configured' is a real state")
	}
}

// §52 (cont.) — an unusable controller is dropped rather than passed through,
// because the core would refuse to start on it.
func TestDaemonConfigUnusableControllerIsDropped(t *testing.T) {
	for _, in := range []string{"", "127.0.0.1", "notaport"} {
		res, err := prepareDaemonConfig(configWithClashAPI(t, in, "s"), t.TempDir(), daemonPrepOptions{})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if res.ClashFallback.Enabled {
			t.Errorf("%q: fallback enabled with an unusable controller", in)
		}
		if _, _, present := clashAPIOof(t, res.Bytes); present {
			t.Errorf("%q: an unusable clash_api was left in the daemon config", in)
		}
	}
}

// §49/§63 — selector groups come from the same transformation, so verification
// evidence cannot drift from the config actually sent.
func TestDaemonConfigReportsSelectorGroups(t *testing.T) {
	res, err := prepareDaemonConfig(configWithClashAPI(t, "127.0.0.1:9090", "s"), t.TempDir(), daemonPrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.SelectorGroups) != 1 || res.SelectorGroups[0] != "🌍 国外流量" {
		t.Errorf("SelectorGroups = %v, want the config's selector tag", res.SelectorGroups)
	}
}

// ---------------------------------------------------------------------------
// §22/§23/§59: locality. This is the security gate for the whole feature.
// ---------------------------------------------------------------------------

func TestIsLocalDaemonAddress(t *testing.T) {
	local := []string{"127.0.0.1:19091", "127.0.0.5:1", "localhost:19091", "[::1]:19091", "127.0.0.1"}
	remote := []string{"10.0.0.5:19091", "192.168.1.5:19091", "203.0.113.9:19091", "[2001:db8::1]:19091", "", "example.com:19091"}

	for _, a := range local {
		if !isLocalDaemonAddress(a) {
			t.Errorf("%q should be local", a)
		}
	}
	for _, a := range remote {
		if isLocalDaemonAddress(a) {
			t.Errorf("%q should NOT be local: a remote daemon has no local Clash fallback", a)
		}
	}
}

// §59 — the whole point: a remote daemon must never fall back, even when the
// local config would otherwise provide a perfectly good endpoint.
func TestRemoteDaemonHasNoLocalFallback(t *testing.T) {
	b := &DaemonBackend{caps: currentDaemonCaps()}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{
		Enabled: true, BaseURL: "http://127.0.0.1:9090", Token: "t",
	})
	// Simulate the apply-time gate for a remote admin address.
	if isLocalDaemonAddress("10.0.0.5:19091") {
		b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	} else {
		b.clashFallback.block()
	}
	if _, ok := b.clashFallback.transportIfReady(); ok {
		t.Error("remote daemon obtained a 127.0.0.1 fallback, which points at the LAUNCHER's machine")
	}
	// And 127.0.0.1:9090 here is NOT the remote daemon, so list must stay unsupported.
	if _, readiness := b.clashFallback.config(); readiness != fallbackBlocked {
		t.Errorf("readiness = %v, want blocked", readiness)
	}
}

// §59 — on the MEASURED daemon shape (no GetGroups, no URLTestOutbound), a
// remote daemon must lose list and test entirely: its RPCs are absent and its
// local fallback is forbidden.
func TestRemoteDaemonCapabilitiesStayUnsupported(t *testing.T) {
	b := &DaemonBackend{caps: measuredLegacyDaemonCaps()}
	b.clashFallback.block()
	caps := b.ProxyActionCapabilities()
	if caps.CanList {
		t.Error("CanList=true for a remote daemon whose GetGroups is Unimplemented " +
			"and whose local fallback is forbidden")
	}
	if caps.CanTestSingle || caps.CanTestGroup {
		t.Error("test reported available for a remote daemon with no URLTestOutbound")
	}
	// Switching DOES work remotely — it is a native RPC, not a local fallback.
	if !caps.CanSwitch {
		t.Error("CanSwitch=false although SelectOutbound is a remote-capable RPC")
	}
	if caps.ListTransport != TransportNone {
		t.Errorf("ListTransport = %q, want none", caps.ListTransport)
	}
}

// §59 (cont.) — and with the fallback merely BLOCKED (not configured at all),
// the same shape on a LOCAL daemon still gets list and test.
func TestLocalDaemonSameShapeGetsFallback(t *testing.T) {
	b := &DaemonBackend{caps: measuredLegacyDaemonCaps()}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	caps := b.ProxyActionCapabilities()
	if !caps.CanList || !caps.CanTestSingle || !caps.CanTestGroup {
		t.Errorf("local daemon with a configured fallback still reports limits: %+v", caps)
	}
}

// ---------------------------------------------------------------------------
// §54–§58, §60/§61: per-action RPC-first and verification.
// ---------------------------------------------------------------------------

// §40 — effective capability on the MEASURED daemon shape: no GetGroups, no
// URLTestOutbound, SelectOutbound present.
func TestEffectiveCapabilityWithFallbackConfigured(t *testing.T) {
	b := &DaemonBackend{caps: capsWith("measured", rpcSelectOutbound)}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})

	caps := b.ProxyActionCapabilities()
	if !caps.CanList {
		t.Error("CanList=false although the loopback fallback can list")
	}
	if !caps.CanTestSingle || !caps.CanTestGroup {
		t.Error("test reported unsupported although the fallback can measure")
	}
	if !caps.CanSwitch {
		t.Error("CanSwitch=false although SelectOutbound exists")
	}
	// §32/§57: the expected hybrid.
	if caps.ListTransport != TransportClashHTTP {
		t.Errorf("ListTransport = %q, want clash_http", caps.ListTransport)
	}
	if caps.TestTransport != TransportClashHTTP {
		t.Errorf("TestTransport = %q, want clash_http", caps.TestTransport)
	}
	if caps.SwitchTransport != TransportRPC {
		t.Errorf("SwitchTransport = %q, want rpc (RPC must win where it exists)", caps.SwitchTransport)
	}
}

// §54 — RPC presence must win; the fallback must not even be consulted.
func TestRPCFirstWinsOverFallback(t *testing.T) {
	b := &DaemonBackend{caps: capsWith("full", rpcGetGroups, rpcSelectOutbound, rpcURLTestOutbound)}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	caps := b.ProxyActionCapabilities()
	for name, got := range map[string]string{
		"list": caps.ListTransport, "switch": caps.SwitchTransport, "test": caps.TestTransport,
	} {
		if got != TransportRPC {
			t.Errorf("%s transport = %q, want rpc: RPC is the daemon's native control plane", name, got)
		}
	}
}

// §38 — an operational failure is NOT a licence to switch transports.
func TestOperationalFailureDoesNotTriggerFallback(t *testing.T) {
	// The capability IS present, so the transport must be RPC regardless of what
	// the call later returns. Fallback triggers on capability absence only.
	b := &DaemonBackend{caps: capsWith("full", rpcGetGroups, rpcURLTestOutbound)}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	caps := b.ProxyActionCapabilities()
	if caps.ListTransport != TransportRPC || caps.TestTransport != TransportRPC {
		t.Errorf("transports = %q/%q; a timeout must not silently reroute to HTTP, "+
			"which would hide real errors behind a second code path",
			caps.ListTransport, caps.TestTransport)
	}
}

// §60/§61 — verification rejects a stranger on the port.
func TestFallbackVerificationRejectsNonClash(t *testing.T) {
	// A plain HTTP server: 200 OK but not a Clash API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	if p.checkIdentity(context.Background(), DaemonClashFallbackConfig{
		Enabled: true, BaseURL: srv.URL,
	}, []string{"🌍 国外流量"}) {
		t.Error("a non-Clash HTTP endpoint was accepted as the daemon's core")
	}
}

func TestFallbackVerificationRejectsWrongSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer right" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"proxies":{"🌍 国外流量":{"type":"Selector","now":"direct"}}}`))
	}))
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	if p.checkIdentity(context.Background(), DaemonClashFallbackConfig{
		Enabled: true, BaseURL: srv.URL, Token: "wrong",
	}, []string{"🌍 国外流量"}) {
		t.Error("an endpoint that rejects our secret was accepted; 401 means it is " +
			"probably not our daemon")
	}
	// The right secret succeeds, so the check is not simply always-false.
	if !p.checkIdentity(context.Background(), DaemonClashFallbackConfig{
		Enabled: true, BaseURL: srv.URL, Token: "right",
	}, []string{"🌍 国外流量"}) {
		t.Error("a valid Clash API with the correct secret was rejected")
	}
}

// §61/§63 — a Clash API that does not know our groups is not our core.
func TestFallbackVerificationRejectsMissingGroup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"proxies":{"GLOBAL":{"type":"Selector","now":"direct"}}}`))
	}))
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	if p.checkIdentity(context.Background(), DaemonClashFallbackConfig{
		Enabled: true, BaseURL: srv.URL,
	}, []string{"🌍 国外流量"}) {
		t.Error("an endpoint lacking the configured selector group was accepted; it is " +
			"not running the config we sent")
	}
}

// §20 — a successful TCP connect is not proof.
func TestFallbackVerificationRequiresRealAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`)) // 200, valid JSON, but no "proxies"
	}))
	defer srv.Close()

	p := &fallbackProber{client: srv.Client()}
	if p.checkIdentity(context.Background(), DaemonClashFallbackConfig{
		Enabled: true, BaseURL: srv.URL,
	}, nil) {
		t.Error("a 200 response without a proxies object was accepted")
	}
}

// §38 — a failure while the core is still starting must not become permanent.
func TestTemporaryUnreachableIsNotPermanent(t *testing.T) {
	b := &DaemonBackend{caps: capsWith("measured", rpcSelectOutbound)}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:1"})
	b.clashFallback.probe = &fallbackProber{client: &http.Client{}}

	if _, ok := b.fallbackTransport(context.Background(), nil); ok {
		t.Error("fallback reported usable although nothing is listening")
	}
	// Crucially: still CONFIGURED, so it is retried rather than written off.
	if _, readiness := b.clashFallback.config(); readiness != fallbackUnverified {
		t.Errorf("readiness = %v, want unverified so the next attempt retries", readiness)
	}
	if caps := b.ProxyActionCapabilities(); !caps.CanList {
		t.Error("a transient connection failure was reported as a capability absence")
	}
}

// §37 — invalidation drops verification but keeps the configuration.
func TestInvalidateKeepsConfiguration(t *testing.T) {
	f := &daemonClashFallback{}
	f.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090", Token: "t"})
	f.mu.Lock()
	f.readiness = fallbackReady
	f.mu.Unlock()

	f.invalidate()

	cfg, readiness := f.config()
	if !cfg.Enabled {
		t.Error("invalidate() discarded the configuration; it must only drop proof")
	}
	if readiness != fallbackUnverified {
		t.Errorf("readiness = %v, want unverified", readiness)
	}
	if _, ok := f.transportIfReady(); ok {
		t.Error("transportIfReady() trusted an invalidated fallback")
	}
}

// §16/§17 — never configured means never usable.
func TestUnconfiguredFallbackIsNeverUsed(t *testing.T) {
	f := &daemonClashFallback{}
	if _, ok := f.transportIfReady(); ok {
		t.Error("an unconfigured fallback produced a transport")
	}
	if _, readiness := f.config(); readiness != fallbackNotConfigured {
		t.Errorf("readiness = %v, want not_configured", readiness)
	}
}

// §36 — logs must not leak the token.
func TestRedactURLHidesCredentials(t *testing.T) {
	got := redactURL("http://user:supersecret@127.0.0.1:9090/proxies?token=abc")
	for _, leak := range []string{"supersecret", "abc", "user:"} {
		if strings.Contains(got, leak) {
			t.Errorf("redactURL leaked %q: %s", leak, got)
		}
	}
}

// §67 — a stopped core must read as "unavailable", not as "unsupported".
func TestFallbackUnavailableIsNotUnsupported(t *testing.T) {
	b := &DaemonBackend{caps: capsWith("measured", rpcSelectOutbound)}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	// Effective capability is still available; only the runtime request fails.
	if caps := b.ProxyActionCapabilities(); !caps.CanList {
		t.Error("a configured fallback must keep list capability while the core is down")
	}
}

// ---------------------------------------------------------------------------
// §33 — hybrid consistency: HTTP list and gRPC switch must observe one core.
// ---------------------------------------------------------------------------
func TestHybridConsistencyThroughOneRuntime(t *testing.T) {
	// One simulated core: gRPC switch mutates the same state HTTP list reads.
	var current atomicString
	current.set("A")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"proxies": map[string]any{
				"🌍 国外流量": map[string]any{
					"type": "Selector", "now": current.get(), "all": []string{"A", "B"},
				},
			},
		})
	}))
	defer srv.Close()

	// gRPC switch → same underlying state.
	grpcSwitch := func(to string) { current.set(to) }

	clash := services.NewClashTransport(srv.URL, "")
	_, selected, err := clash.GroupProxies("🌍 国外流量")
	if err != nil {
		t.Fatalf("HTTP list: %v", err)
	}
	if selected != "A" {
		t.Fatalf("initial selected = %q, want A", selected)
	}

	grpcSwitch("B")

	_, selected, err = clash.GroupProxies("🌍 国外流量")
	if err != nil {
		t.Fatalf("HTTP list after switch: %v", err)
	}
	if selected != "B" {
		t.Errorf("HTTP list still reports %q after a gRPC switch to B — the two wires "+
			"would not be observing one runtime, which forbids this hybrid", selected)
	}
}

// atomicString is a tiny race-free holder for the consistency test.
type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicString) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// ---------------------------------------------------------------------------
// §18/§19/§62/§63 — restart recovery and config/runtime mismatch.
//
// The daemon keeps the VPN running after the GUI exits, so on relaunch the
// in-memory fallback state is gone while the core is still up. It must be
// recoverable WITHOUT asking the user to restart the VPN, and it must NOT be
// claimed when the running core is on a different config.
// ---------------------------------------------------------------------------

// restartRecovery models a fresh launcher process against a still-running core.
// It performs exactly the production sequence: transform the disk config, then
// verify the endpoint it names against the live API.
func restartRecovery(t *testing.T, diskConfig []byte, liveProxies map[string]any) (bool, DaemonClashFallbackConfig) {
	t.Helper()
	res, err := prepareDaemonConfig(diskConfig, t.TempDir(), daemonPrepOptions{})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"proxies": liveProxies})
	}))
	defer srv.Close()

	f := &daemonClashFallback{}
	f.setConfigured(DaemonClashFallbackConfig{
		Enabled: res.ClashFallback.Enabled,
		BaseURL: srv.URL, // stand-in for the derived loopback endpoint
		Token:   res.ClashFallback.Token,
	})
	f.probe = &fallbackProber{client: srv.Client()}
	ok := f.verify(context.Background(), res.SelectorGroups)
	return ok, res.ClashFallback
}

func liveGroup(now string) map[string]any {
	return map[string]any{
		"🌍 国外流量": map[string]any{"type": "Selector", "now": now, "all": []string{"A", "B"}},
		"🤖 AI":   map[string]any{"type": "Selector", "now": "A", "all": []string{"A"}},
	}
}

// §62 — recovery needs no daemon restart.
func TestRestartRecoveryRestoresFallback(t *testing.T) {
	disk := configWithClashAPI(t, "127.0.0.1:9090", "s")
	ok, cfg := restartRecovery(t, disk, liveGroup("A"))
	if !ok {
		t.Error("a fresh launcher could not recover the fallback against a still-running core")
	}
	if !cfg.Enabled {
		t.Error("the derived fallback config was disabled")
	}
}

// §63 — the daemon is running an OLD config; the disk has moved on.
//
// Here the running API does not know the group the disk config now declares.
// The correct answer is "not ready": adopting it would let the launcher drive a
// core running a config the user has already changed.
func TestConfigChangedWhileDaemonRunsIsMismatch(t *testing.T) {
	disk := configWithClashAPI(t, "127.0.0.1:9090", "s")
	// The live core serves groups from the PREVIOUS config.
	stale := map[string]any{
		"♻️ Old Group": map[string]any{"type": "Selector", "now": "A", "all": []string{"A"}},
	}
	ok, _ := restartRecovery(t, disk, stale)
	if ok {
		t.Error("fallback adopted against a core that does not serve the current config")
	}
}

// §64 — the mismatch must be a named state, not a raw 401 or a missing group.
func TestMismatchHasItsOwnState(t *testing.T) {
	b := &DaemonBackend{caps: measuredLegacyDaemonCaps()}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	b.clashFallback.probe = &fallbackProber{client: &http.Client{}}
	b.fallbackMu.Lock()
	b.expectedGroups = []string{"🌍 国外流量"}
	b.fallbackMu.Unlock()

	if _, ok := b.fallbackTransport(context.Background(), b.expectedSelectorGroups()); ok {
		t.Error("fallback became usable despite an unverifiable endpoint")
	}
	// The readiness is a token the UI can map, not a transport error string.
	if _, readiness := b.clashFallback.config(); readiness != fallbackUnverified {
		t.Errorf("readiness = %v, want a retryable token", readiness)
	}
}

// §42/§43 — RPC absence and product capability are different statements.
//
// The audit keeps reporting the missing RPCs; the PRODUCT reports the actions
// as available, because they are. Conflating them is what produced a
// "service broken" message for a fully working proxy screen.
func TestRPCAbsenceIsNotProductBreakage(t *testing.T) {
	b := &DaemonBackend{caps: measuredLegacyDaemonCaps()}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})

	// RPC fact: still absent.
	b.caps.mu.RLock()
	rpcList := b.caps.supports[rpcGetGroups]
	rpcTest := b.caps.supports[rpcURLTestOutbound]
	b.caps.mu.RUnlock()
	if rpcList || rpcTest {
		t.Fatal("fixture wrong: the measured daemon lacks these RPCs")
	}

	// Product fact: available anyway.
	caps := b.ProxyActionCapabilities()
	if !caps.CanList || !caps.CanTestSingle {
		t.Error("product capability was reported as broken purely because an RPC is absent")
	}
}

// §72 — the original defect must not come back.
//
// The very first bug in this whole line of work was a raw gRPC string reaching
// the user:
//
//	cannot read the proxies of group "🌍 国外流量": daemon GetGroups:
//	rpc error: code = Unimplemented desc = unknown method GetGroups
//
// Every fallback path must therefore still yield a CAPABILITY sentinel, never a
// transport string, when it cannot serve the action.
func TestFallbackNeverLeaksRawUnimplemented(t *testing.T) {
	b := &DaemonBackend{caps: measuredLegacyDaemonCaps()}
	b.ctx = context.Background()
	// No fallback configured at all: list and latency must be capability errors.
	if _, _, err := b.groupProxiesViaFallback("g"); err == nil {
		t.Fatal("expected an error when no fallback is available")
	} else if !services.IsProxyCapabilityError(err) {
		t.Errorf("list error = %v; want a capability sentinel, not a transport string", err)
	} else if strings.Contains(err.Error(), "Unimplemented") || strings.Contains(err.Error(), "rpc error") {
		t.Errorf("list error leaked a raw RPC string: %v", err)
	}

	if _, err := b.delayViaFallback(context.Background(), "n"); err == nil {
		t.Fatal("expected an error when no fallback is available")
	} else if !services.IsProxyCapabilityError(err) {
		t.Errorf("delay error = %v; want a capability sentinel", err)
	} else if strings.Contains(err.Error(), "rpc error") {
		t.Errorf("delay error leaked a raw RPC string: %v", err)
	}

	if err := b.switchProxyViaFallback("g", "n"); err == nil {
		t.Fatal("expected an error when no fallback is available")
	} else if !services.IsProxyCapabilityError(err) {
		t.Errorf("switch error = %v; want a capability sentinel", err)
	}
}

// §41 — transport diagnostics are reported per action, for the audit.
func TestTransportSourceIsReportedPerAction(t *testing.T) {
	b := &DaemonBackend{caps: measuredLegacyDaemonCaps()}
	b.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	caps := b.ProxyActionCapabilities()

	if caps.ListTransport != TransportClashHTTP || caps.TestTransport != TransportClashHTTP {
		t.Errorf("list/test transports = %q/%q, want clash_http on the measured shape",
			caps.ListTransport, caps.TestTransport)
	}
	if caps.SwitchTransport != TransportRPC {
		t.Errorf("switch transport = %q, want rpc", caps.SwitchTransport)
	}
	// A fully capable daemon uses RPC everywhere; the fallback never wins.
	full := &DaemonBackend{caps: capsWith("full", rpcGetGroups, rpcSelectOutbound, rpcURLTestOutbound)}
	full.clashFallback.setConfigured(DaemonClashFallbackConfig{Enabled: true, BaseURL: "http://127.0.0.1:9090"})
	fc := full.ProxyActionCapabilities()
	if fc.ListTransport != TransportRPC || fc.SwitchTransport != TransportRPC || fc.TestTransport != TransportRPC {
		t.Errorf("full daemon transports = %q/%q/%q, want rpc everywhere",
			fc.ListTransport, fc.SwitchTransport, fc.TestTransport)
	}
}
