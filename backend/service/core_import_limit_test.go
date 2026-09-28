package service

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/backend/protocol"
)

// TestCoreImportCopyIsBounded is statement 24 (§34 name).
//
// The candidate's size is checked with `Stat`, and then the file is copied. The check and the
// copy are two separate steps over a file the user controls, so everything the check was for
// can be defeated in the window between them: the file grows after `Stat` (a build still
// writing its output), or the path is replaced with a larger file between `Stat` and `Open`.
//
// The consequence is not corruption but exhaustion — the staging file is written to the data
// directory, so an unbounded copy fills the user's disk during an operation whose own error
// message promises a 256 MB limit.
//
// THE PREVIOUS VERSION OF THIS TEST COULD NOT FAIL. It copied a 38-byte file, compared the
// bytes, and — verified — PASSED with the limiter removed entirely. It was named after a bound
// it never approached. The limit is injectable now, so the overflow branch is driven for real.
func TestCoreImportCopyIsBounded(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sing-box")

	src := filepath.Join(dir, "candidate")
	body := bytes.Repeat([]byte("A"), 4096)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// A small limit, so the same code path is exercised at a size a test can afford.
	const limit = 1024

	staged, cleanup, err := stageCoreBinaryLimited(src, target, limit)
	if err == nil {
		defer cleanup()
		got, _ := os.ReadFile(staged)
		t.Fatalf("a %d-byte file was staged against a %d-byte limit and produced %d bytes "+
			"with no error. An unbounded copy fills the user's disk during an operation "+
			"that promises a limit", len(body), limit, len(got))
	}

	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != "file_too_large" {
		t.Fatalf("an oversized file must be refused with file_too_large, got %v", err)
	}

	// AND NOTHING MAY BE LEFT BEHIND. A refused import that leaves a partial file beside the
	// installed core is how a failed operation still changes what is on disk.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".sing-box.import-") {
			t.Errorf("a refused import left its staging file %s behind", e.Name())
		}
	}
}

// TestCoreImportCopiesAFileThatFits — the other side of the boundary, so the bound cannot be
// "refuse everything" and pass the test above.
func TestCoreImportCopiesAFileThatFits(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sing-box")

	src := filepath.Join(dir, "candidate")
	body := bytes.Repeat([]byte("B"), 512)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	staged, cleanup, err := stageCoreBinaryLimited(src, target, 1024)
	if err != nil {
		t.Fatalf("a file under the limit was refused: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("the staged copy differs from the source (%d bytes vs %d)", len(got), len(body))
	}
}

// TestCoreImportAcceptsAFileExactlyAtTheLimit — the boundary itself.
//
// A limiter set to exactly the maximum cannot tell "the whole file" from "the file cut to the
// maximum", so this is the case that decides whether the extra byte is read. Accepting it is
// required; accepting a file ONE BYTE MORE is what must fail.
func TestCoreImportAcceptsAFileExactlyAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sing-box")

	src := filepath.Join(dir, "candidate")
	body := bytes.Repeat([]byte("C"), 1024)
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	staged, cleanup, err := stageCoreBinaryLimited(src, target, 1024)
	if err != nil {
		t.Fatalf("a file exactly at the limit was refused: %v", err)
	}
	defer cleanup()

	got, _ := os.ReadFile(staged)
	if len(got) != 1024 {
		t.Fatalf("staged %d bytes, want 1024", len(got))
	}
}

// TestCoreImportLimitIsSetToDetectOverflow pins the details a behavioural test cannot reach.
//
// The 256 MB production limit is too large to write in a unit test, so the tests above drive
// an injected limit and this one checks that the PRODUCTION call actually passes the real
// constant, and that the copy reads one byte past it. Both are properties of how the code is
// wired rather than of what it computes, so a source check is the right tool here — but it is
// pinned to CODE, and the behavioural tests are what prove the branch works.
func TestCoreImportLimitIsSetToDetectOverflow(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/core_import.go"))

	// The default entry point must pass the real limit, not a copy of it or a zero.
	body := functionBodyForTest(t, src, "func stageCoreBinary(src, target string)")
	if !strings.Contains(body, "maxCoreFileBytes") {
		t.Error("`stageCoreBinary` does not use the production limit, so the bound enforced " +
			"on a real import is not the one this file declares")
	}

	// And the copy must read ONE BYTE PAST the limit, inside the shared implementation.
	copyBody := functionBodyForTest(t, src, "func stageCoreBinaryLimited(")
	if !strings.Contains(copyBody, "LimitReader") {
		t.Fatal("the core import copy is unbounded: a file that grows after the Stat check " +
			"is copied in full into the data directory")
	}
	if !strings.Contains(copyBody, "limit+1") && !strings.Contains(copyBody, "limit + 1") {
		t.Error("the copy limit does not leave room to DETECT an oversized file, so a file " +
			"one byte over the limit would be installed silently truncated")
	}
	// The overflow must be REPORTED, not merely capped. Anchored on the actual condition so
	// that disabling it behind `if false &&` is visible — a bare `Contains` for the
	// expression matched the disabled form too.
	if !strings.Contains(copyBody, "if written > limit {") {
		t.Error("the copy does not check whether it hit the limit, so exceeding it produces " +
			"a truncated file rather than an error")
	}
	if strings.Contains(src, "if false &&") {
		t.Error("the overflow check is disabled behind `if false &&`, so an oversized core " +
			"would be installed silently truncated")
	}
	// The refusal must clean up: a refused import that leaves its staging file behind still
	// changes what is on disk.
	if !strings.Contains(copyBody, "cleanup()") {
		t.Error("the refusal path does not remove the staging file")
	}
}
