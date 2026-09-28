package limitread

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestAllReturnsEverythingUpToTheLimit — the ordinary case must not regress.
func TestAllReturnsEverythingUpToTheLimit(t *testing.T) {
	body := []byte("hello world")
	got, err := All(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("a body exactly at the limit must be accepted: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q, want %q", got, body)
	}
}

// TestAllRejectsRatherThanTruncating is the whole point of the package.
//
// Reading with `io.LimitReader(r, max)` and returning the result hands the caller a PREFIX it
// has no way to recognise as one: the length is legal, the content decodes as far as it goes,
// and the failure surfaces elsewhere — a JSON parse error pointing at a file the launcher
// believes it downloaded correctly, or a core that crashes for reasons that look nothing like
// an import problem.
//
// The previous behaviour in all four call sites was exactly that, so this test fails against
// `io.ReadAll(io.LimitReader(r, limit))`.
func TestAllRejectsRatherThanTruncating(t *testing.T) {
	limit := int64(10)
	// One byte over: the smallest input that a truncating implementation would hand back as
	// a plausible-looking 10-byte result.
	oversized := []byte("0123456789A")

	got, err := All(bytes.NewReader(oversized), limit)
	if err == nil {
		t.Fatalf("an oversized input was accepted and returned as %d bytes (%q); the "+
			"caller has no way to tell this from a complete 10-byte input",
			len(got), got)
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if got != nil {
		t.Errorf("an oversized input returned a partial result alongside the error; a "+
			"caller that ignores the error would use it: %q", got)
	}
	// The message must be actionable: it is what a user sees when an import is refused.
	if !strings.Contains(err.Error(), "10 bytes") {
		t.Errorf("the error does not say what the limit was: %v", err)
	}
}

// TestAllAcceptsOneByteUnderTheLimit — the boundary from the other side.
//
// Without this, an implementation that rejected everything at exactly `limit-1` ... or that
// simply never read anything ... would pass the two tests above.
func TestAllAcceptsOneByteUnderTheLimit(t *testing.T) {
	limit := int64(10)
	body := []byte("012345678")
	got, err := All(bytes.NewReader(body), limit)
	if err != nil {
		t.Fatalf("a body under the limit must be accepted: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q, want %q", got, body)
	}
}

// TestAllHandlesAnEmptyInput — an empty body is not an overflow.
func TestAllHandlesAnEmptyInput(t *testing.T) {
	got, err := All(bytes.NewReader(nil), 16)
	if err != nil {
		t.Fatalf("an empty input must be accepted: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bytes from an empty input", len(got))
	}
}

// TestAllRejectsANonPositiveLimit — a zero limit is a caller bug, not "read nothing".
//
// Returning empty for a zero limit would look exactly like an empty input, so a misconfigured
// limit would silently disable the download rather than fail.
func TestAllRejectsANonPositiveLimit(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		if _, err := All(bytes.NewReader([]byte("x")), limit); err == nil {
			t.Errorf("limit %d was accepted; a zero limit looks like an empty input", limit)
		}
	}
}

// TestAllPropagatesAReadError — a transport failure is not an overflow.
func TestAllPropagatesAReadError(t *testing.T) {
	sentinel := errors.New("connection reset")
	_, err := All(io.MultiReader(bytes.NewReader([]byte("abc")), errReader{sentinel}), 64)
	if err == nil {
		t.Fatal("a read error was swallowed")
	}
	if errors.Is(err, ErrTooLarge) {
		t.Error("a transport error was reported as a size overflow, which would send the " +
			"user looking for a larger limit instead of at their connection")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestHumanBytesMatchesTheDeclaredLimits — the message has to describe a limit a reader can
// recognise, since the limits are written as powers of 1024.
func TestHumanBytesMatchesTheDeclaredLimits(t *testing.T) {
	cases := map[int64]string{
		512:       "512 bytes",
		1024:      "1 KB",
		256 << 20: "256 MB",
		64 << 20:  "64 MB",
		2 << 30:   "2 GB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
