package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHTTPClientTimeoutDoesNotTruncatePerRequestContexts is statement 13 (§34 name).
//
// `http.Client.Timeout` is a deadline over the ENTIRE exchange — dial, write, read — and
// it silently caps any per-request context that asks for longer. A caller that carefully
// derives a 60-second context for a slow provider fetch therefore gets 20 seconds and no
// indication that its own deadline was overridden, so the failure looks like a network
// problem rather than a misconfiguration.
//
// The client must not impose a global ceiling on requests that bring their own deadline;
// the per-request context is the deadline, and the transport keeps the dial bound that
// protects against a black-holed address.
func TestHTTPClientTimeoutDoesNotTruncatePerRequestContexts(t *testing.T) {
	c := clashHTTPClient()
	if c.Timeout != 0 {
		t.Fatalf("the shared Clash HTTP client sets a global Timeout of %s. That value "+
			"caps every per-request context: a caller deriving a longer deadline silently "+
			"gets the shorter one, and the failure surfaces as a network error rather "+
			"than as the timeout that actually applied", c.Timeout)
	}

	// The protection the global timeout was standing in for must still be present:
	// a dial that never completes has to fail on its own.
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the client has no *http.Transport")
	}
	if tr.DialContext == nil {
		t.Error("no dial timeout, so a black-holed address hangs until the caller's own " +
			"deadline — and a caller with no deadline hangs forever")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("no response-header timeout: a server that accepts the connection and " +
			"never answers would hold the request until the caller's own deadline")
	}
}

// TestRequestTimeoutIsEnforcedPerRequest — the per-request budget must still exist and
// must actually be applied.
func TestRequestTimeoutIsEnforcedPerRequest(t *testing.T) {
	if httpRequestTimeoutSeconds <= 0 {
		t.Fatal("there is no per-request timeout at all")
	}
	if got := time.Duration(httpRequestTimeoutSeconds) * time.Second; got > 2*time.Minute {
		t.Fatalf("the per-request default of %s is long enough that a wedged core looks "+
			"like a slow one", got)
	}
}

// TestEveryHTTPCallerHasItsOwnDeadline is the audit the change above requires.
//
// Removing the client-level Timeout moved the responsibility for bounding a request onto
// the callers. If even one of them does not bring a deadline, that request can now hang
// forever — the change would have traded a truncated deadline for an absent one, which is
// the worse failure.
//
// This checks the SOURCE rather than a live request because the alternative is to stand up
// a Clash API that never answers, which no unit test may do. It is anchored on the call
// site and on a context being derived for it, so a new caller added without one fails here.
func TestEveryHTTPCallerHasItsOwnDeadline(t *testing.T) {
	root := repoRootForAPITest(t)
	files := []string{
		"api/clash_proxy.go",
		"api/clash_switch.go",
		"api/clash_transport.go",
		"api/clash_delay.go",
	}

	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		body := string(data)

		// Every `getHTTPClient().Do(` must be preceded, within the same function, by a
		// `context.WithTimeout`. Checking the enclosing function is the right granularity:
		// a deadline derived for an unrelated request would not bound this one.
		idx := 0
		for {
			found := strings.Index(body[idx:], "getHTTPClient().Do(")
			if found < 0 {
				break
			}
			at := idx + found
			idx = at + 1

			// Find the enclosing function start.
			fnStart := strings.LastIndex(body[:at], "\nfunc ")
			if fnStart < 0 {
				fnStart = 0
			}
			fnBody := body[fnStart:at]
			if !strings.Contains(fnBody, "context.WithTimeout") {
				t.Errorf("%s: the call to getHTTPClient().Do() at byte %d is not preceded "+
					"by a context.WithTimeout in its function. The client no longer sets a "+
					"global timeout, so this request has NO deadline and can hang forever",
					rel, at)
			}
		}
	}
}

func repoRootForAPITest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root")
	return ""
}
