package core

import (
	"context"
	"net"
	"sort"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	daemonpb "singbox-launcher/internal/daemonpb"
)

// Fake daemon servers, one per protocol generation.
//
// The audit's requirement is that capability detection be tested against servers
// that DIFFER, not only against a mock of the newest one — a test that only ever
// sees a fully capable server cannot fail when the compatibility layer is
// removed, which is how the original drift reached a user.
//
// Each generation is a real gRPC server over bufconn speaking the vendored
// protobuf. Methods are embedded from UnimplementedStartedServiceServer and
// overridden selectively, so a generation is defined by what it IMPLEMENTS —
// exactly the property the probe measures. A server that leaves a method to the
// embedded default answers Unimplemented, the same as a daemon built without it.

// generation names the three servers from the audit.
type daemonGeneration int

const (
	// genCurrent implements every method the launcher calls.
	genCurrent daemonGeneration = iota
	// genLegacy is the daemon measured on the reporting machine: it serves
	// SelectOutbound and the subscription plane, and answers Unimplemented for
	// every unary proxy read.
	genLegacy
	// genBare implements nothing but the version handshake.
	genBare
)

func (g daemonGeneration) String() string {
	switch g {
	case genCurrent:
		return "current (all methods)"
	case genLegacy:
		return "legacy (SelectOutbound only)"
	default:
		return "bare (no proxy methods)"
	}
}

// rpcGenDaemon is the server. Embedding the Unimplemented base supplies every
// method not overridden, with the correct Unimplemented status.
type rpcGenDaemon struct {
	daemonpb.UnimplementedStartedServiceServer
	gen daemonGeneration
}

func (f *rpcGenDaemon) GetVersion(context.Context, *emptypb.Empty) (*daemonpb.Version, error) {
	return &daemonpb.Version{Version: "fake-" + f.gen.String()}, nil
}

// GetGroups is present only in the current generation.
func (f *rpcGenDaemon) GetGroups(context.Context, *emptypb.Empty) (*daemonpb.Groups, error) {
	if f.gen != genCurrent {
		// Exactly what a daemon without the method returns. Returning it
		// explicitly, rather than relying on the embedded default, keeps the
		// test honest about which generations lack the method.
		return nil, statusUnimplemented("GetGroups for service daemon.StartedService")
	}
	return &daemonpb.Groups{Group: []*daemonpb.Group{{
		Tag:      "🌍 国外流量",
		Selected: "node-a",
		Items: []*daemonpb.GroupItem{
			{Tag: "node-a", Type: "Shadowsocks"},
			{Tag: "node-b", Type: "Trojan"},
		},
	}}}, nil
}

// SelectOutbound is served by current and legacy.
func (f *rpcGenDaemon) SelectOutbound(_ context.Context, req *daemonpb.SelectOutboundRequest) (*emptypb.Empty, error) {
	if f.gen == genBare {
		return nil, statusUnimplemented("SelectOutbound for service daemon.StartedService")
	}
	if req.GetGroupTag() == "" || req.GetOutboundTag() == "" {
		// Argument validation, which is what distinguishes "the method exists
		// and rejected my probe" from "the method does not exist".
		return nil, statusInvalidArgument("group_tag and outbound_tag are required")
	}
	return &emptypb.Empty{}, nil
}

// URLTestOutbound is served by current only.
func (f *rpcGenDaemon) URLTestOutbound(_ context.Context, req *daemonpb.URLTestOutboundRequest) (*daemonpb.URLTestOutboundResponse, error) {
	if f.gen != genCurrent {
		return nil, statusUnimplemented("URLTestOutbound for service daemon.StartedService")
	}
	if req.GetOutboundTag() == "" {
		return nil, statusInvalidArgument("outbound_tag is required")
	}
	return &daemonpb.URLTestOutboundResponse{Delay: 42}, nil
}

// GetOutbounds is served by current only.
func (f *rpcGenDaemon) GetOutbounds(context.Context, *emptypb.Empty) (*daemonpb.OutboundList, error) {
	if f.gen != genCurrent {
		return nil, statusUnimplemented("GetOutbounds for service daemon.StartedService")
	}
	return &daemonpb.OutboundList{}, nil
}

// startFakeDaemon brings up a generation on a bufconn and returns a client.
//
// bufconn keeps the test off the network entirely, so it cannot collide with a
// real daemon on this machine — which matters here, because the reporting
// machine HAS one running.
func startFakeDaemon(t *testing.T, gen daemonGeneration) daemonpb.StartedServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	daemonpb.RegisterStartedServiceServer(srv, &rpcGenDaemon{gen: gen})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake daemon: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return daemonpb.NewStartedServiceClient(conn)
}

// probeAgainst runs the real capability probe against a fake generation.
func probeAgainst(t *testing.T, gen daemonGeneration) *daemonCapabilities {
	t.Helper()
	client := startFakeDaemon(t, gen)
	caps := &daemonCapabilities{}
	b := &DaemonBackend{caps: caps}
	b.probeCapabilities(context.Background(), client)
	return caps
}

// TestProbeAgainstCurrentDaemon — generation A: everything present.
func TestProbeAgainstCurrentDaemon(t *testing.T) {
	caps := probeAgainst(t, genCurrent)
	for _, r := range []daemonRPC{
		rpcGetGroups, rpcSelectOutbound, rpcURLTestOutbound, rpcGetOutbounds,
	} {
		if !caps.supported(r) {
			t.Errorf("%s probed as unsupported against a server that implements it", r)
		}
	}
	if caps.version == "" {
		t.Error("probe did not record the daemon version")
	}
}

// TestProbeAgainstLegacyDaemon — generation B: the reported machine's shape.
//
// This is the regression test for the original bug. The daemon implements
// SelectOutbound and answers Unimplemented for GetGroups; the probe must record
// BOTH facts, so the UI can keep switching working while explaining that listing
// is unavailable.
func TestProbeAgainstLegacyDaemon(t *testing.T) {
	caps := probeAgainst(t, genLegacy)

	if caps.supported(rpcGetGroups) {
		t.Error("GetGroups probed as supported against a server answering " +
			"Unimplemented — this is the original defect")
	}
	if !caps.supported(rpcSelectOutbound) {
		t.Error("SelectOutbound probed as unsupported, but the legacy server " +
			"implements it; switching nodes would be disabled needlessly")
	}
	if caps.supported(rpcURLTestOutbound) {
		t.Error("URLTestOutbound probed as supported against a server that lacks it")
	}
}

// TestProbeAgainstBareDaemon — generation C: nothing but the handshake.
func TestProbeAgainstBareDaemon(t *testing.T) {
	caps := probeAgainst(t, genBare)
	for _, r := range []daemonRPC{
		rpcGetGroups, rpcSelectOutbound, rpcURLTestOutbound, rpcGetOutbounds,
	} {
		if caps.supported(r) {
			t.Errorf("%s probed as supported against a server with no proxy RPCs", r)
		}
	}
}

// TestProbeDistinguishesAbsentFromRejected — the probe's core discrimination.
//
// A method the server HAS but rejects (invalid argument) must read as supported;
// a method it LACKS must read as unsupported. Conflating them would either
// disable working features or attempt calls guaranteed to fail — and it is the
// distinction the old code threw away when it wrapped Unimplemented in a generic
// error.
func TestProbeDistinguishesAbsentFromRejected(t *testing.T) {
	legacy := probeAgainst(t, genLegacy)
	current := probeAgainst(t, genCurrent)

	// Same method, same empty probe request, opposite answers.
	if legacy.supported(rpcSelectOutbound) == false {
		t.Error("a method that exists but rejected the probe read as unsupported")
	}
	if current.supported(rpcURLTestOutbound) == false {
		t.Error("a method that exists but rejected the probe read as unsupported")
	}
	if legacy.supported(rpcGetGroups) {
		t.Error("a method that does not exist read as supported")
	}
}

// TestProbeIsCachedAcrossCalls — the probe must run once, not per request.
//
// It sits on the path that opens the Proxies screen; re-probing per call would
// add a round trip per panel and per group, which is a cost the user would feel
// as a slow screen.
func TestProbeIsCachedAcrossCalls(t *testing.T) {
	client := startFakeDaemon(t, genCurrent)
	caps := &daemonCapabilities{}
	b := &DaemonBackend{caps: caps}

	ctx := context.Background()
	b.probeCapabilities(ctx, client)
	first := caps.snapshot()

	// A second probe against a DIFFERENT generation must not overwrite the
	// first result: the capability belongs to the connection that was probed.
	b.probeCapabilities(ctx, startFakeDaemon(t, genBare))
	second := caps.snapshot()

	keys := make([]string, 0, len(first))
	for k := range first {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		if first[daemonRPC(k)] != second[daemonRPC(k)] {
			t.Errorf("capability %s changed on a re-probe; the probe should be "+
				"cached per connection", k)
		}
	}
}

// TestProtocolStalenessNamesOnlyShippedMethods — the Daemon screen must not tell
// the user to update for a method they cannot reach anyway.
//
// Chains and the pool have no IPC method, so listing them as missing would ask
// for an update that changes nothing visible.
func TestProtocolStalenessNamesOnlyShippedMethods(t *testing.T) {
	client := startFakeDaemon(t, genLegacy)
	ctx := context.Background()

	caps := &daemonCapabilities{}
	b := &DaemonBackend{caps: caps}
	b.probeCapabilities(ctx, client)

	missing, version := b.daemonProtocolStaleness()
	if version == "" {
		t.Error("protocol staleness did not report the daemon version")
	}
	if len(missing) == 0 {
		t.Fatal("a daemon without GetGroups produced no protocol-staleness report")
	}
	allowed := map[string]bool{
		string(rpcGetGroups): true, string(rpcURLTestOutbound): true,
		string(rpcSelectOutbound): true,
	}
	for _, m := range missing {
		if !allowed[m] {
			t.Errorf("protocol staleness names %q, which is not a method the "+
				"shipped product calls over IPC", m)
		}
	}
	// GetGroups is the one that must appear: it is what the user hit.
	found := false
	for _, m := range missing {
		if m == string(rpcGetGroups) {
			found = true
		}
	}
	if !found {
		t.Error("protocol staleness omits GetGroups, the method whose absence " +
			"produced the reported failure")
	}
}

// TestCapableDaemonIsNotReportedStale — and the converse, so the fix is not
// "always complain".
func TestCapableDaemonIsNotReportedStale(t *testing.T) {
	client := startFakeDaemon(t, genCurrent)
	caps := &daemonCapabilities{}
	b := &DaemonBackend{caps: caps}
	b.probeCapabilities(context.Background(), client)

	missing, _ := b.daemonProtocolStaleness()
	if len(missing) != 0 {
		t.Errorf("a fully capable daemon was reported stale: %v", missing)
	}
}

// statusUnimplemented builds the exact status a daemon without a method returns.
//
// The message text is the real one from the reported failure, so a test that
// starts leaking it would fail on the string a user actually saw.
func statusUnimplemented(method string) error {
	return status.Error(codes.Unimplemented, "unknown method "+method)
}

// statusInvalidArgument is what an implemented method returns for a probe that
// deliberately sends empty arguments.
func statusInvalidArgument(msg string) error {
	return status.Error(codes.InvalidArgument, msg)
}
