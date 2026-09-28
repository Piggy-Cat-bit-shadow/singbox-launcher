package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
//
// THE TIMESTAMP IS PART OF THE BYTES, SO IT IS ZEROED FIRST, and that is the whole
// subtlety. `Save` stamps `UpdatedAt` with the current time before marshalling, so a digest
// over the raw bytes changes on EVERY save even when nothing the user did changed — measured
// directly: two saves of the same untouched state one second apart produced
// 2417500444342594338 then 4102834356273990511.
//
// That contradiction mattered because a build compares revisions to decide whether the state
// moved while it was rendering. A revision that advances on its own makes every concurrent
// write — including a subscription refresh stamping `LastSuccessAt`, which the user did not
// perform — look like a user edit, so the build conservatively reports the config as stale
// and the marker is cleared for a change that never happened. The comment above says the
// digest "has the opposite behaviour: the same content always produces the same revision".
// Zeroing the timestamp is what makes that sentence true.
//
// Only the volatile field is excluded. Everything else the file carries still contributes, so
// the digest remains a complete identity of the CONTENT as opposed to the WRITE.
func contentRevision(data []byte) uint64 {
	return contentRevisionExcludingVolatile(data)
}

// contentRevisionExcludingVolatile hashes the document with time-varying metadata removed.
//
// `UpdatedAt` records WHEN the file was written, not WHAT it says. Two saves of the same
// state are the same state, and a revision that disagrees is unusable as an identity.
func contentRevisionExcludingVolatile(data []byte) uint64 {
	if normalized, ok := withoutVolatileFields(data); ok {
		data = normalized
	}
	return digestOf(data)
}

// withoutVolatileFields returns the document with volatile metadata blanked.
//
// A parse failure returns ok=false, and the caller then hashes the bytes as they are: a
// document this function cannot understand is not one whose revision should silently become
// the digest of an empty map.
func withoutVolatileFields(data []byte) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false
	}

	// THE TIMESTAMP LIVES UNDER `meta`, NOT AT THE TOP LEVEL.
	//
	// The first version of this looked for `updated_at` in the root object, found nothing,
	// and returned the bytes unchanged — so the digest stayed time-dependent and the fix
	// silently did nothing. The probe caught it: two saves still produced different
	// revisions. `meta` is a nested object, so the field has to be removed there.
	metaRaw, present := doc["meta"]
	if !present {
		return data, true
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return nil, false
	}
	if _, hasUpdated := meta["updated_at"]; !hasUpdated {
		// Nothing volatile to remove; avoid a re-marshal that would reorder keys and
		// change the digest for no reason.
		return data, true
	}
	delete(meta, "updated_at")
	newMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, false
	}
	doc["meta"] = newMeta
	normalized, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return normalized, true
}

// digestOf is the 64-bit digest of a byte slice.
func digestOf(data []byte) uint64 {
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
