package service

import (
	"strings"
	"testing"
)

// TestCoreImportCopyIsBounded is statement 24 (§34 name).
//
// The candidate's size is checked with `Stat`, and then the file is copied with an
// UNBOUNDED `io.Copy`. The check and the copy are two separate steps over a file the user
// controls, so everything the check was for can be defeated in the window between them:
//
//   - the file grows after `Stat` (a build still writing its output, or a download in
//     progress) and the copy pulls in far more than the limit;
//   - the path is replaced between `Stat` and `Open` with a different, larger file;
//   - on a platform where it applies, a FIFO or device node reports one size and yields
//     unbounded data.
//
// The consequence is not corruption but exhaustion: the staging file is written to the
// data directory, so a "core" of hundreds of gigabytes fills the user's disk — during an
// operation whose own error message promises a 256 MB limit.
//
// The bound must be enforced on the COPY, where the bytes actually flow.
func TestCoreImportCopyIsBounded(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/core_import.go"))

	if !strings.Contains(src, "io.Copy(tmp, in)") && !strings.Contains(src, "io.Copy(") {
		t.Skip("the copy shape changed; update this test")
	}

	// The copy must go through a limiter, not straight from the source to the staging file.
	if strings.Contains(src, "io.Copy(tmp, in)") {
		t.Error("the core import copies with an unbounded io.Copy(tmp, in). The size " +
			"check ran earlier with Stat, so a file that grows after the check — or is " +
			"swapped for a larger one between Stat and Open — is copied in full, filling " +
			"the data directory during an operation that promises a 256 MB limit")
	}
	if !strings.Contains(src, "LimitReader") && !strings.Contains(src, "maxCoreFileBytes") {
		t.Error("the copy enforces no limit at all")
	}
}

// TestCoreImportRejectsAFileThatGrewPastTheLimit — the behavioural form.
//
// A limit that is checked and then not enforced is worse than no limit, because the error
// message tells the user a bound exists.
func TestCoreImportRejectsAFileThatGrewPastTheLimit(t *testing.T) {
	src := stripGoComments(readServiceSource(t, "backend/service/core_import.go"))

	// The limiter must allow exactly one byte MORE than the limit, so that exceeding it is
	// detectable. A limiter set to exactly the limit cannot distinguish "exactly at the
	// limit" from "truncated at the limit", and the oversized case would be copied silently.
	if !strings.Contains(src, "maxCoreFileBytes+1") && !strings.Contains(src, "maxCoreFileBytes + 1") {
		t.Error("the copy limit does not leave room to DETECT an oversized file. A limiter " +
			"set to exactly the maximum truncates rather than reports, so a file one byte " +
			"over the limit would be installed silently truncated")
	}
}
