// Package limitread reads a stream up to a limit and REFUSES to pretend it saw the whole
// thing.
//
// # Why this exists as its own package
//
// The same defect appeared independently in four places: a read was capped with
// `io.LimitReader(r, max)`, and the truncated result was returned as if it were complete.
// A limit that is exactly the maximum cannot distinguish "the whole input" from "the input
// cut to the maximum", so every caller silently accepted a partial artifact:
//
//   - the core import wrote a truncated binary into the data directory,
//   - the remote resource fetch wrote a truncated rule-set,
//   - the release-metadata fetch decoded a truncated JSON document,
//   - the subscription fetch reported a truncated provider response as a successful one.
//
// In each case the failure surfaced much later and somewhere else — a JSON parse error
// pointing at a file the launcher believed it had downloaded correctly, or a core that
// crashes for reasons that look nothing like an import problem.
//
// The fix is one byte: read `limit + 1`, and if that many bytes arrived, the input exceeded
// the limit and the caller is told so instead of being handed a prefix. Keeping it in one
// place is the point — five copies of a one-line subtlety is how the sixth copy gets it
// wrong.
package limitread

import (
	"errors"
	"fmt"
	"io"
)

// ErrTooLarge is returned (wrapped) when the input exceeds the limit.
//
// Exported so a caller can test for it with errors.Is and choose its own wording, which
// several callers need: the fetch path reports a remote response, the import path reports a
// local file.
var ErrTooLarge = errors.New("input exceeds the limit")

// All reads all of r, up to limit bytes.
//
// It returns ErrTooLarge — wrapped with the limit and what actually arrived — when the input
// is longer, rather than the truncated prefix. A caller that genuinely wants a prefix should
// use io.LimitReader directly and say so, because that is a different intent and it has to be
// deliberate.
func All(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		// A non-positive limit is a bug in the caller, not a request to read nothing: the
		// alternative is a silently empty result that looks like an empty input.
		return nil, fmt.Errorf("limitread: non-positive limit %d", limit)
	}

	// Read ONE BYTE PAST the limit. This is the whole trick: at exactly `limit` bytes the
	// input might end there, and there is no way to know without asking for more.
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: more than %s arrived", ErrTooLarge, HumanBytes(limit))
	}
	return data, nil
}

// HumanBytes renders a byte count for an error message.
//
// Powers of 1024, matching how the limits are declared.
func HumanBytes(n int64) string {
	const (
		kib = int64(1024)
		mib = kib * 1024
		gib = mib * 1024
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%d GB", n/gib)
	case n >= mib:
		return fmt.Sprintf("%d MB", n/mib)
	case n >= kib:
		return fmt.Sprintf("%d KB", n/kib)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
