package state

import (
	"crypto/sha256"
	"encoding/hex"
)

// contentRevision is the identity of a state's CONTENT.
//
// A digest rather than a write counter, because the counter is not idempotent and this
// package guarantees a byte-identical save/load/save roundtrip. The digest is stable
// across those roundtrips and changes whenever anything meaningful does.
//
// It is computed over the marshalled bytes, so it covers every field the file actually
// carries — including ones added later, without anyone remembering to extend this
// function.
func contentRevision(data []byte) uint64 {
	sum := sha256.Sum256(data)
	// Truncated to 64 bits: this compares two readings taken moments apart in one
	// process, so collision resistance is not the requirement — detecting that the file
	// changed is. A full digest would work too; the shorter value keeps the struct field
	// and any logging cheap.
	var r uint64
	for i := 0; i < 8; i++ {
		r = r<<8 | uint64(sum[i])
	}
	return r
}

var _ = hex.EncodeToString

// Revision returns a counter identifying the CONTENT of this state within the process
// that loaded it.
//
// A config build records the revision it rendered; before it commits it re-reads and
// compares. If the number moved, the build was made from a state that no longer exists
// and must not mark the result fresh — otherwise the user edits something, a slow build
// from the previous state finishes, and the launcher reports "config is current" for a
// config that was never rendered from what is on disk.
//
// The number is intentionally process-local: it only has to be comparable between two
// reads in one process lifetime. Persisting it would let a restored backup compare as
// newer than the state it replaced.
func (s *State) Revision() uint64 {
	if s == nil {
		return 0
	}
	return s.revision
}

// LoadedRevision returns the revision this state had when it was read from disk.
//
// Zero means "never loaded" — a state built in memory has no meaningful base, so a
// caller must not treat 0 as "unchanged".
func (s *State) LoadedRevision() uint64 {
	if s == nil {
		return 0
	}
	return s.loadedRevision
}

// MarkLoaded records the revision baseline, so a later Save can be recognised as
// "someone else wrote since I read this".
func (s *State) MarkLoaded() {
	if s == nil {
		return
	}
	s.loadedRevision = s.revision
}

// ChangedSinceLoad reports whether this state has been saved since it was marked loaded.
func (s *State) ChangedSinceLoad() bool {
	if s == nil {
		return false
	}
	return s.revision != s.loadedRevision
}
