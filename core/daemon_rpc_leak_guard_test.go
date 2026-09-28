package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Guard against raw gRPC capability text reaching the user.
//
// The reported failure put this on screen:
//
//	unknown method GetGroups for service daemon.StartedService
//
// That string is a server-side implementation detail. It tells the user nothing
// they can act on, names a method they have never heard of, and — because it was
// rendered through the global error banner — displaced the core and config
// status they actually needed.
//
// The rule this file enforces: every call to a StartedService method the daemon
// may not implement must go through the capability layer
// (core/daemon_rpc_compat.go), so that Unimplemented becomes a product-level
// state rather than an error string.

// daemonRPCCallsiteRe finds `client.<Method>(` calls on a daemon client.
var daemonRPCCallsiteRe = regexp.MustCompile(`client\.([A-Z][A-Za-z]*)\(`)

// unimplementedBurdenRPCs are the methods the audit measured as ABSENT from a
// reachable daemon. A call site for one of these is only acceptable if it is
// capability-gated or explicitly handled.
var unimplementedBurdenRPCs = map[string]bool{
	"GetGroups":               true,
	"GetOutbounds":            true,
	"GetChains":               true,
	"GetPool":                 true,
	"GetRules":                true,
	"URLTestOutbound":         true,
	"SetChainPositionEnabled": true,
	"SetEndpointEnabled":      true,
}

// TestDaemonRPCCallSitesAreCapabilityAware — a call to a method the daemon may
// not implement must be guarded.
//
// Checked by reading the source, like the other Swift/Go invariant guards in
// this repository, because the failure is a MISSING guard: there is no runtime
// state to assert on when the call was never made. A call site is acceptable
// when its function either consults the capability set or recognises
// Unimplemented — the two sanctioned ways to turn a protocol mismatch into a
// product answer.
func TestDaemonRPCCallSitesAreCapabilityAware(t *testing.T) {
	root := filepath.Join("..")
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Skip build caches and generated protobuf: the generated client
			// declares the methods, which is exactly the surface that cannot be
			// trusted.
			switch info.Name() {
			case ".gocache", ".git", "daemonpb", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) == 0 {
		t.Skip("no Go sources found")
	}

	// Files that legitimately contain unguarded calls, each for a stated reason.
	//
	// The guard's purpose is that no raw capability text reaches the USER, so it
	// covers the paths that can actually deliver one. Exempting a path requires
	// showing it cannot: an exemption with a reason is a decision, an unexplained
	// one is how a guard gets quietly defanged.
	exempt := map[string]bool{
		// The probe itself: it must CALL each method to learn whether it exists.
		// That is the mechanism, not an oversight.
		"daemon_rpc_compat.go": true,

		// Remote machines and the debug API are NOT part of the shipped menu-bar
		// product: capabilities reports Remote=false, the backend never starts
		// the debug server, and neither path is reachable from the Swift UI.
		// Their calls are still real, but they cannot put a string in front of a
		// user, which is the property under test. Left unguarded deliberately
		// rather than fixed here, so this audit does not silently grow into a
		// remote-machine refactor.
		"lxd_remote_transport.go":      true,
		"lxd_remote_transport_test.go": true,
		"chain_endpoints.go":           true,
		"remote_endpoints.go":          true,
		"chain_endpoints_test.go":      true,
	}

	checked := 0
	for _, path := range files {
		base := filepath.Base(path)
		if exempt[base] {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(src)
		if !strings.Contains(text, "StartedServiceClient") &&
			!strings.Contains(text, "daemonpb.NewStartedServiceClient") &&
			!strings.Contains(text, "client.") {
			continue
		}

		for _, m := range daemonRPCCallsiteRe.FindAllStringSubmatchIndex(text, -1) {
			method := text[m[2]:m[3]]
			if !unimplementedBurdenRPCs[method] {
				continue
			}
			checked++
			// The enclosing function: scan backwards for `func `.
			funcStart := strings.LastIndex(text[:m[0]], "\nfunc ")
			if funcStart < 0 {
				funcStart = 0
			}
			body := text[funcStart:]
			// Bound the body at the next top-level func.
			if next := strings.Index(body[1:], "\nfunc "); next > 0 {
				body = body[:next+1]
			}
			guarded := strings.Contains(body, "caps.supported") ||
				strings.Contains(body, "ensureProbed") ||
				strings.Contains(body, "isUnimplemented") ||
				strings.Contains(body, "codes.Unimplemented") ||
				strings.Contains(body, "status.Code")
			if !guarded {
				t.Errorf("%s calls client.%s without consulting the daemon "+
					"capability set or handling Unimplemented: a daemon that lacks "+
					"the method would surface the raw gRPC text to the user",
					path, method)
			}
		}
	}

	if checked == 0 {
		t.Skip("no daemon RPC call sites found; the scanner and the code have drifted")
	}
}

// TestNoUserFacingErrorTextExposesRPCDetails — the specific strings that must
// never reach a user.
//
// Belt and braces with the guard above: even a guarded call can wrap the wrong
// error, so the strings themselves are checked for in the paths that build
// user-facing messages.
func TestNoUserFacingErrorTextExposesRPCDetails(t *testing.T) {
	// Files whose messages reach the IPC layer and therefore the Swift UI.
	userFacing := []string{
		filepath.Join("..", "backend", "service", "proxies.go"),
		filepath.Join("..", "backend", "service", "backend.go"),
	}
	forbidden := []string{
		"unknown method",
		"for service daemon.StartedService",
		"rpc error: code = Unimplemented",
	}

	for _, path := range userFacing {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("cannot read %s: %v", path, err)
		}
		text := string(src)
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s contains the raw RPC text %q, which would reach the UI",
					path, f)
			}
		}
	}
}
