// Daemon RPC compatibility — one place that knows which StartedService methods
// the REACHABLE daemon actually implements.
//
// WHY THIS EXISTS
//
// The launcher vendors a generated gRPC client (`internal/daemonpb`) whose
// interface lists every method the fork's proto declares. The daemon, however,
// is a separate binary that the user installs and updates independently, and it
// may be built from a proto that predates or postdates the vendored one. A
// method being present in the generated client therefore proves NOTHING about
// the server.
//
// Treating the client interface as the truth produced this, on a real machine:
//
//	cannot read the proxies of group "🌍 国外流量": daemon GetGroups:
//	rpc error: code = Unimplemented desc = unknown method GetGroups …
//
// and a live probe of that daemon (1.15.0-jiejie-masquerade.6) showed the drift
// is far wider than one method — it answered Unimplemented for GetGroups,
// GetOutbounds, GetChains, GetPool, GetRules, URLTestOutbound,
// SetChainPositionEnabled and SetEndpointEnabled, while implementing only the
// Subscribe* streams, SelectOutbound and GetVersion.
//
// THE RULE
//
// Capability comes from the SERVER's own answer, never from the client's type
// surface and never from a version-string comparison. A one-off runtime probe
// establishes it, and the result is cached for the life of the connection;
// Unimplemented is then a known capability fact rather than a surprise at the
// moment a user opens a screen.
//
// Version strings are deliberately NOT parsed. A custom build
// (`1.15.0-jiejie-masquerade.6`) carries no comparable lx series, and the fork's
// own version had already moved past the proto it was built from — so a
// version→capability table would have been wrong about the very machine this was
// written for. The probe asks the question directly.

package core

import (
	"context"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	daemonpb "singbox-launcher/internal/daemonpb"
	"singbox-launcher/internal/debuglog"
)

// daemonRPC names one StartedService method the launcher may call.
type daemonRPC string

const (
	rpcGetGroups               daemonRPC = "GetGroups"
	rpcGetOutbounds            daemonRPC = "GetOutbounds"
	rpcGetChains               daemonRPC = "GetChains"
	rpcGetPool                 daemonRPC = "GetPool"
	rpcGetRules                daemonRPC = "GetRules"
	rpcURLTestOutbound         daemonRPC = "URLTestOutbound"
	rpcSetChainPositionEnabled daemonRPC = "SetChainPositionEnabled"
	rpcSetEndpointEnabled      daemonRPC = "SetEndpointEnabled"
	rpcSelectOutbound          daemonRPC = "SelectOutbound"
)

// daemonCapabilities is the probed support set.
//
// Absence means "not supported", which is the safe default: a capability the
// probe could not confirm must not be offered, because offering it produces the
// exact error this whole file exists to prevent.
type daemonCapabilities struct {
	mu       sync.RWMutex
	probed   bool
	supports map[daemonRPC]bool
	// version is recorded for display and for the audit trail only. It is never
	// used to infer a capability.
	version string
}

// supported reports whether the daemon implements the method.
//
// Before any probe has run the answer is false, deliberately: the first call
// path that needs a capability triggers the probe (see ensureProbed), so a
// "not yet known" must not read as "yes".
func (c *daemonCapabilities) supported(r daemonRPC) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.supports[r]
}

// snapshot returns a copy of the probed set, for reporting.
func (c *daemonCapabilities) snapshot() map[daemonRPC]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[daemonRPC]bool, len(c.supports))
	for k, v := range c.supports {
		out[k] = v
	}
	return out
}

// probeTimeout bounds the whole capability probe. It is deliberately short: the
// probe runs on the first proxy/chain interaction, and a slow daemon must not
// turn "open the Proxies screen" into a multi-second wait.
const probeTimeout = 4 * time.Second

// probeCapabilities asks the live daemon which methods it implements.
//
// Each method is probed by CALLING it and inspecting the gRPC status code:
//
//	codes.Unimplemented  → the server does not have the method
//	anything else        → the method exists (the call reached real argument
//	                       validation, which is proof enough of implementation)
//
// Probing by calling is the only reliable method. There is no reflection
// dependency in this project, and the descriptor alone cannot distinguish "the
// server has it" from "the client believes it does".
//
// The probe runs ONCE per DaemonBackend instance. Results are cached because the
// answer is a property of the connected binary, which cannot change without the
// connection being replaced.
func (b *DaemonBackend) probeCapabilities(ctx context.Context, client daemonpb.StartedServiceClient) {
	if b.caps == nil {
		return
	}
	b.caps.mu.RLock()
	already := b.caps.probed
	b.caps.mu.RUnlock()
	if already {
		return
	}

	// Record the daemon's own version for display. Deliberately not used to
	// derive capabilities — see the package comment.
	if v, err := client.GetVersion(ctx, &emptypb.Empty{}); err == nil {
		b.caps.mu.Lock()
		b.caps.version = v.GetVersion()
		b.caps.mu.Unlock()
	}

	supports := map[daemonRPC]bool{}
	set := func(r daemonRPC, err error) {
		supports[r] = status.Code(err) != codes.Unimplemented
	}

	_, err := client.GetGroups(ctx, &emptypb.Empty{})
	set(rpcGetGroups, err)
	_, err = client.GetOutbounds(ctx, &emptypb.Empty{})
	set(rpcGetOutbounds, err)
	_, err = client.GetChains(ctx, &emptypb.Empty{})
	set(rpcGetChains, err)
	_, err = client.GetPool(ctx, &daemonpb.GetPoolRequest{})
	set(rpcGetPool, err)
	_, err = client.GetRules(ctx, &emptypb.Empty{})
	set(rpcGetRules, err)
	// An empty group/tag fails argument validation on a server that HAS the
	// method and is Unimplemented on one that does not — the distinction being
	// measured. The call is not expected to succeed.
	_, err = client.SelectOutbound(ctx, &daemonpb.SelectOutboundRequest{})
	set(rpcSelectOutbound, err)
	// An empty tag fails argument validation on a server that HAS the method,
	// and is Unimplemented on one that does not — which is exactly the
	// distinction being measured. The call is not expected to succeed.
	_, err = client.URLTestOutbound(ctx, &daemonpb.URLTestOutboundRequest{})
	set(rpcURLTestOutbound, err)
	_, err = client.SetChainPositionEnabled(ctx, &daemonpb.SetChainPositionEnabledRequest{})
	set(rpcSetChainPositionEnabled, err)
	_, err = client.SetEndpointEnabled(ctx, &daemonpb.SetEndpointEnabledRequest{})
	set(rpcSetEndpointEnabled, err)

	b.caps.mu.Lock()
	b.caps.supports = supports
	b.caps.probed = true
	b.caps.mu.Unlock()

	missing := []string{}
	for _, r := range []daemonRPC{
		rpcGetGroups, rpcGetOutbounds, rpcGetChains, rpcGetPool,
		rpcGetRules, rpcURLTestOutbound, rpcSetChainPositionEnabled,
		rpcSetEndpointEnabled, rpcSelectOutbound,
	} {
		if !supports[r] {
			missing = append(missing, string(r))
		}
	}
	if len(missing) > 0 {
		debuglog.WarnLog("daemon RPC compatibility: the reachable daemon %q does not implement: %v",
			b.caps.version, missing)
	} else {
		debuglog.InfoLog("daemon RPC compatibility: all probed methods present (daemon %q)", b.caps.version)
	}
}

// daemonProtocolStaleness reports which required methods the reachable daemon
// does not implement.
//
// Runs the probe if it has not run yet, so the Daemon screen shows the condition
// before the user has touched the Proxies screen — otherwise they would meet the
// limitation as a surprise rather than as a stated setup problem.
//
// Returns the missing methods sorted, plus the daemon's self-reported version.
func (b *DaemonBackend) daemonProtocolStaleness() (missing []string, version string) {
	if b == nil || b.caps == nil {
		return nil, ""
	}
	b.ensureProbed()
	b.caps.mu.RLock()
	version = b.caps.version
	probed := b.caps.probed
	supports := b.caps.supports
	b.caps.mu.RUnlock()
	if !probed {
		// The daemon did not answer the probe; that is unreachability, which the
		// status already reports, not a protocol verdict.
		return nil, version
	}
	// Only methods the SHIPPED product needs. Chains and the pool are not
	// exposed over IPC, so listing them here would tell the user to update for
	// something they cannot use anyway.
	for _, r := range []daemonRPC{rpcGetGroups, rpcURLTestOutbound, rpcSelectOutbound} {
		if !supports[r] {
			missing = append(missing, string(r))
		}
	}
	sort.Strings(missing)
	return missing, version
}

// ensureProbed runs the capability probe once, on first use.
//
// Called lazily rather than at connect time so a daemon that is only used for
// status never pays for a probe, and so the probe happens against a live
// connection rather than racing startup.
func (b *DaemonBackend) ensureProbed() {
	if b.caps == nil {
		return
	}
	b.caps.mu.RLock()
	done := b.caps.probed
	b.caps.mu.RUnlock()
	if done {
		return
	}
	client, err := b.grpcClient()
	if err != nil {
		// Cannot probe without a connection. Leaving `probed` false means the
		// next attempt retries rather than caching a failure as a capability.
		return
	}
	ctx, cancel := context.WithTimeout(b.ctx, probeTimeout)
	defer cancel()
	b.probeCapabilities(ctx, client)
}
