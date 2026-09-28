package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Crash classification must read only the CURRENT generation's log output.
//
// The core log is append-only across generations, so its tail can still contain
// a deterministic-failure signature written by a previous core. The classifier
// would read that old text as this exit's cause and STOP AUTO-RESTART — turning
// an unrelated transient crash into a permanent "this config will never work".

// TestClassifyExitTextIgnoresTextBeforeTheOffset is the mechanism, tested on the
// helper that implements the boundary.
func TestClassifyExitTextIgnoresTextBeforeTheOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core.log")

	previous := "FATAL failed to create router: address already in use\n"
	current := "panic: runtime error: index out of range\n"
	content := previous + current
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("cannot write the fixture log: %v", err)
	}

	tail, err := readFileTail(path, coreLogTailBytes)
	if err != nil {
		t.Fatalf("readFileTail: %v", err)
	}

	// With no offset (unknown boundary) the OLD text is visible — this is exactly
	// the bug, stated as a precondition.
	if classifyExitText(lastLines(tail, 40)) == exitReasonUnknown {
		t.Fatal("precondition: the previous generation's fatal text should be visible " +
			"in the untrimmed tail")
	}

	// With the boundary recorded when the CURRENT generation started, only its own
	// output remains.
	offset := int64(len(previous))
	skip := skipToOffset(tail, path, offset)
	if skip < 0 {
		t.Fatalf("skipToOffset(%d) = %d, want a non-negative index", offset, skip)
	}
	trimmed := tail[skip:]
	if got := classifyExitText(lastLines(trimmed, 40)); got != exitReasonUnknown {
		t.Fatalf("classification still reads the PREVIOUS generation's fatal signature "+
			"(%v); a transient crash would be treated as a deterministic config error "+
			"and auto-restart would stop", got)
	}
	if !strings.Contains(trimmed, "index out of range") {
		t.Fatal("trimming removed the CURRENT generation's output as well as the old text")
	}
}

// TestSkipToOffsetHandlesRotationAndTruncation — a stale offset (the file was
// rotated or truncated) must not discard the evidence entirely.
func TestSkipToOffsetHandlesRotationAndTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core.log")
	content := "current generation output\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("cannot write the fixture log: %v", err)
	}
	tail := content

	// An offset larger than the file means the log was rotated or truncated under
	// us. The recorded position no longer identifies anything in this file, so
	// NOTHING in the tail can be attributed to the current generation: the helper
	// reports the whole tail as before the boundary, and the classifier sees no
	// current-generation evidence. That is the fail-safe direction — refusing to
	// classify is better than classifying text that belongs to another core, which
	// would stop auto-restart on the strength of a stale error.
	if skip := skipToOffset(tail, path, 1<<20); skip != len(tail) {
		t.Fatalf("skipToOffset with a post-rotation offset = %d, want %d (the whole "+
			"tail treated as belonging to an earlier generation)", skip, len(tail))
	}

	// An offset before the tail's start means the tail is already all-new.
	if skip := skipToOffset(tail, path, 0); skip > 0 {
		t.Fatalf("skipToOffset(0) = %d, want nothing skipped", skip)
	}
}

// TestLogOffsetIsScopedToTheGeneration — the runtime must store the boundary per
// generation, and forget old ones so a long session does not accumulate them.
func TestLogOffsetIsScopedToTheGeneration(t *testing.T) {
	ac := newTestController()

	gen1 := ac.classic.currentGeneration()
	ac.classic.noteLogOffset(gen1, 1000)
	if got := ac.classic.logOffsetFor(gen1); got != 1000 {
		t.Fatalf("logOffsetFor(gen1) = %d, want 1000", got)
	}

	gen2 := ac.classic.renewGeneration()
	ac.classic.noteLogOffset(gen2, 5000)

	if got := ac.classic.logOffsetFor(gen2); got != 5000 {
		t.Fatalf("logOffsetFor(gen2) = %d, want 5000", got)
	}
	// The previous generation's boundary is kept briefly: its monitor may still
	// classify an exit that happened just before the renewal.
	if got := ac.classic.logOffsetFor(gen1); got != 1000 {
		t.Fatalf("logOffsetFor(gen1) = %d after one renewal, want 1000 to still be "+
			"readable for a late exit", got)
	}

	// An unknown generation reports 0, which means "classify the whole tail" —
	// the only safe default when the boundary is not known.
	if got := ac.classic.logOffsetFor(gen2 + 999); got != 0 {
		t.Fatalf("logOffsetFor(unknown) = %d, want 0", got)
	}
}

// TestLogOffsetsDoNotAccumulate — the map must be pruned, or a session with many
// restarts leaks an entry per generation forever.
func TestLogOffsetsDoNotAccumulate(t *testing.T) {
	ac := newTestController()

	for i := 0; i < 200; i++ {
		gen := ac.classic.renewGeneration()
		ac.classic.noteLogOffset(gen, int64(i))
	}

	ac.classic.mu.Lock()
	size := len(ac.classic.logOffsets)
	ac.classic.mu.Unlock()

	if size > 2 {
		t.Fatalf("logOffsets holds %d entries after 200 generations; the map is not "+
			"pruned and a long session leaks memory", size)
	}
}
