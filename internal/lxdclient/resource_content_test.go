package lxdclient

import (
	"net/http"
	"strings"
	"testing"
)

// TestResourceContentReportsTruncation is statement 28 (§34 name).
//
// `ResourceContent` read the body through `io.LimitReader(resp.Body, 64<<20)` and returned
// the result with no error. A resource LARGER than the limit therefore came back as a
// silent prefix: 64 MB of a 100 MB file, indistinguishable from a complete small one.
//
// The caller writes those bytes straight into the machine's local rule-set directory and
// treats the result as the file. A truncated rule-set does not fail at download time — it
// fails when the core loads it, with a parse error pointing at a file the user believes was
// fetched successfully.
//
// A bound is right; a bound that cannot be distinguished from success is not. The read must
// report that it hit the ceiling.
func TestResourceContentReportsTruncation(t *testing.T) {
	// A body one byte past the limit, so the read is guaranteed to be cut short. Sending
	// exactly the limit would be ambiguous, which is the point of the test.
	const limit = 64 << 20
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/content") {
			http.NotFound(w, r)
			return
		}
		// Stream more than the limit without materialising it in the test.
		// Chunked encoding: no Content-Length is set, so the handler is not contradicting
		// the body it writes.
		chunk := strings.Repeat("x", 1<<20)
		for written := 0; written <= limit; written += len(chunk) {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})

	body, err := c.ResourceContent("big.srs")
	if err == nil {
		t.Fatalf("a resource larger than the %d MB limit was returned as %d bytes with no "+
			"error; the caller writes it to disk and the core later fails to parse a "+
			"silently truncated rule-set", limit>>20, len(body))
	}
	// The error must say what happened, so the user can act on it.
	if !strings.Contains(strings.ToLower(err.Error()), "large") &&
		!strings.Contains(strings.ToLower(err.Error()), "limit") &&
		!strings.Contains(strings.ToLower(err.Error()), "truncat") {
		t.Errorf("the failure does not explain that the resource exceeded a limit: %v", err)
	}
}

// TestResourceContentReturnsSmallResourcesIntact — the bound must not break the normal case.
func TestResourceContentReturnsSmallResourcesIntact(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload-bytes"))
	})

	body, err := c.ResourceContent("small.srs")
	if err != nil {
		t.Fatalf("a small resource failed: %v", err)
	}
	if string(body) != "payload-bytes" {
		t.Fatalf("content was altered: %q", body)
	}
}
