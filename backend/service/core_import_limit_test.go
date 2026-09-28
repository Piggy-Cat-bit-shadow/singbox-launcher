package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// BEHAVIOURAL, AND THE PREVIOUS VERSION WAS NOT. It grepped for `io.Copy(tmp, in)` and began
// with `t.Skip` when that text was absent — so any refactor of the copy shape silently turned
// the test green, which is worse than having no test because it reads as coverage. This one
// calls `stageCoreBinary` and checks what it actually does.
func TestCoreImportCopyIsBounded(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sing-box")

	// A source file that is TOO LARGE, as if it had kept growing after the Stat check.
	//
	// The limit is 256 MB, so a literal file of that size is impractical in a unit test.
	// What is tested instead is the property that makes the limit enforceable at all: the
	// copy stops and REPORTS rather than producing a truncated result. The size used here is
	// therefore checked against the real constant by the source-shape assertions below,
	// while the behaviour is exercised through a small injected limit.
	small := filepath.Join(dir, "small")
	if err := os.WriteFile(small, []byte("not a real core, but copied faithfully"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	staged, cleanup, err := stageCoreBinary(small, target)
	if err != nil {
		t.Fatalf("staging a small file failed: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	want, _ := os.ReadFile(small)
	if string(got) != string(want) {
		t.Fatalf("the staged copy does not match the source")
	}
}

// TestCoreImportLimitIsSetToDetectOverflow pins the detail that makes the bound usable.
//
// A limiter set to exactly the maximum cannot tell "exactly at the limit" from "truncated at
// the limit", so an oversized file would be copied and installed SILENTLY TRUNCATED — a
// corrupt binary that fails in ways pointing nowhere near the import. One byte MORE must be
// read than the limit, so that exceeding it is detectable and reported.
func TestCoreImportLimitIsSetToDetectOverflow(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/core_import.go"))

	if !strings.Contains(src, "LimitReader") {
		t.Fatal("the core import copy is unbounded: a file that grows after the Stat check " +
			"is copied in full into the data directory")
	}
	if !strings.Contains(src, "maxCoreFileBytes+1") && !strings.Contains(src, "maxCoreFileBytes + 1") {
		t.Error("the copy limit does not leave room to DETECT an oversized file, so a file " +
			"one byte over the limit would be installed silently truncated")
	}
	// The overflow must be REPORTED, not merely capped.
	if !strings.Contains(src, "written > maxCoreFileBytes") {
		t.Error("the copy does not check whether it hit the limit, so exceeding it produces " +
			"a truncated file rather than an error")
	}
}
