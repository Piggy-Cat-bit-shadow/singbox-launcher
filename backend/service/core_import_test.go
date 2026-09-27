package service

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/backend/protocol"
)

// Core import tests.
//
// The property that matters most is not "a good binary installs" — it is that a
// BAD one changes nothing. A user replacing their working core is one bad
// validation away from having no core at all, so the atomicity assertions below
// are the point of this file.
//
// The transaction is exercised against a real file tree and a real path
// resolution, but never against a real sing-box: no test here starts a core
// process with `run`, and the probe is expected to fail for the fixture, which
// is itself a case worth covering.
//
// There is deliberately no "successful install" fixture. Producing one would
// require a binary that prints `sing-box version X` and passes the Mach-O
// architecture check — i.e. shipping a real core into the test tree, which is
// both large and exactly what the standing rules forbid touching. The success
// path is therefore verified against the packaged helper (documented in
// docs/IMPORT_FLOW_AUDIT.md), and what is pinned here is every refusal and the
// guarantee that refusals are non-destructive.

// mustHash returns the SHA-256 of a file, failing the test when it cannot be
// read. Used to prove the installed core is untouched, which is a byte-level
// property and so is checked at byte level rather than by size or mtime.
func mustHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// copyFile copies src over dst, preserving the executable bit.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("cannot read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("cannot write %s: %v", dst, err)
	}
}

// dirEntries returns the set of names in a directory.
func dirEntries(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read %s: %v", dir, err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

// coreImportFixture returns a backend whose Data core is a known byte sequence,
// so "was it modified" is answerable.
func coreImportFixture(t *testing.T) (*Backend, string) {
	t.Helper()
	b := backendWithConfig(t)

	corePath := filepath.Join(b.ac.FileService.Layout.Data.Bin(), "sing-box")
	// Content is arbitrary: nothing here runs it. What matters is that it is a
	// regular file at the installed path that can be hashed before and after.
	if err := os.WriteFile(corePath, []byte("existing core bytes"), 0o755); err != nil {
		t.Fatalf("cannot seed the installed core: %v", err)
	}
	return b, corePath
}

// coreMissingError extracts the protocol error code, failing the test when the
// call unexpectedly succeeded.
func coreImportCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("import succeeded, want error %s", want)
	}
	pe, ok := err.(*protocol.Error)
	if !ok {
		t.Fatalf("unexpected error type %T: %v", err, err)
	}
	if pe.Code != want {
		t.Errorf("error code = %q, want %q (%v)", pe.Code, want, err)
	}
}

// TestCoreImportRejectsBadCandidates — the path and format checks, which all
// run before anything is opened for writing.
func TestCoreImportRejectsBadCandidates(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "not-a-core")
	if err := os.WriteFile(regular, []byte("plain text, not a binary"), 0o700); err != nil {
		t.Fatalf("cannot write the fixture: %v", err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"empty path", "", "bad_path"},
		{"missing file", filepath.Join(dir, "absent"), "file_not_found"},
		{"a directory", dir, "not_regular_file"},
		// The file exists and is a regular file but is not a Mach-O at all, so
		// it is refused before any probe is attempted.
		{"not a binary", regular, "wrong_architecture"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, corePath := coreImportFixture(t)
			before := mustHash(t, corePath)

			_, err := b.ImportCoreFile(tc.path)
			coreImportCode(t, err, tc.want)

			// The decisive assertion, repeated in every rejection case: the
			// installed core is byte-for-byte what it was.
			if after := mustHash(t, corePath); after != before {
				t.Errorf("the installed core was modified by a rejected import: %s -> %s",
					before, after)
			}
		})
	}
}

// TestCoreImportRejectsNonCoreBinaries — a file that IS a valid binary for this
// architecture, but is not sing-box.
//
// The probe runs it and finds no version, which is the only reliable way to
// tell "a Mach-O" from "a sing-box": there is no marker to read instead.
func TestCoreImportRejectsNonCoreBinaries(t *testing.T) {
	b, corePath := coreImportFixture(t)
	before := mustHash(t, corePath)

	// A real arm64 executable that is not sing-box: correct architecture, runs
	// fine, prints no version banner.
	_, ierr := b.ImportCoreFile(knownNonCoreExecutable(t))
	coreImportCode(t, ierr, "invalid_core")

	if after := mustHash(t, corePath); after != before {
		t.Errorf("a non-core binary modified the installed core: %s -> %s", before, after)
	}
}

// knownNonCoreExecutable returns a system executable that is a real Mach-O for
// this architecture but is NOT sing-box.
//
// This exists because of a runaway that cost the macOS CI runner ten minutes per
// push. The tests used to pass `os.Executable()` — the Go test binary itself —
// as the candidate core. That is a trap, because the production code EXECUTES the
// candidate:
//
//	ImportCoreFile → stageCoreBinary → probeStagedCoreVersion
//	              → exec.CommandContext(ctx, staged, "version")
//
// and a Go test binary treats the positional argument `version` as "run the whole
// suite". The child therefore re-entered the suite, reached the same core-import
// test, and exec'd itself again. Confirmed by sending SIGQUIT to a runaway child:
// goroutine 1 sat in testing.(*T).Run while another goroutine blocked in
// os/exec.(*Cmd).Wait, and the only test symbol on the stack was
// TestCoreImportRejectsNonCoreBinaries. The 3 s probe context bounds one
// generation, not the whole tree, so the package burned its full timeout.
//
// `/usr/bin/true` is the right fixture: a regular file (not a symlink, so the
// not_regular_file and same-path checks still behave normally), a universal
// Mach-O that genuinely contains an arm64 slice, exits immediately, and prints
// nothing resembling a version banner. `true version` therefore yields exactly
// the invalid_core verdict these tests assert, without ever starting a test
// runner.
//
// It skips rather than inventing a fixture when the system binary is missing or
// fails the same architecture gate production applies, so the test can never
// pass against something that is not a valid Mach-O.
func knownNonCoreExecutable(t *testing.T) string {
	t.Helper()

	path, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no system `true` executable to use as a non-core fixture: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Skipf("cannot stat %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Skipf("%s is not a regular file, so it would exercise the wrong rejection path", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Skipf("%s is not executable", path)
	}

	// The same checker the production path uses, so the fixture is proven to get
	// past architecture validation and reach the version probe under test.
	if err := checkCoreArchitecture(path); err != nil {
		t.Skipf("%s does not pass the core architecture check: %v", path, err)
	}

	// Never let a test runner be selected again. This is the exact class of
	// binary that caused the recursion, so a future edit that reaches for
	// os.Executable() fails loudly here rather than quietly in CI.
	if strings.HasSuffix(filepath.Base(path), ".test") {
		t.Fatalf("refusing to use a Go test binary (%s) as a core candidate: "+
			"executing it would re-enter the test suite", path)
	}

	return path
}

// TestCoreImportRefusesToReplaceItself — importing the path that is already
// installed.
//
// Without this guard the copy and the destination would be the same file, and
// the "atomic replace" would truncate the core it is reading. Reporting
// already_installed is both the safe and the truthful answer.
//
// The installed core is seeded with a real executable rather than a text
// fixture: the architecture check runs BEFORE the same-path check (a file that
// is not a macOS binary is rejected on its own merits first), so a text file
// would fail earlier and never reach the guard under test.
func TestCoreImportRefusesToReplaceItself(t *testing.T) {
	b, corePath := coreImportFixture(t)

	// Install a genuine arm64 Mach-O at the target path.
	copyFile(t, knownNonCoreExecutable(t), corePath)
	before := mustHash(t, corePath)

	_, ierr := b.ImportCoreFile(corePath)
	coreImportCode(t, ierr, "already_installed")

	if after := mustHash(t, corePath); after != before {
		t.Errorf("self-import damaged the core: %s -> %s", before, after)
	}
}

// TestCoreImportRefusesWhileOverrideIsActive — with SINGBOX_LAUNCHER_CORE set,
// the Data core is not the binary that runs.
//
// Installing anyway would report success while the user kept running the
// override, which is the worst outcome: they would believe the swap happened.
func TestCoreImportRefusesWhileOverrideIsActive(t *testing.T) {
	b, corePath := coreImportFixture(t)
	before := mustHash(t, corePath)

	original := b.ac.FileService.CoreSource
	b.ac.FileService.CoreSource = "env"
	defer func() { b.ac.FileService.CoreSource = original }()

	_, ierr := b.ImportCoreFile(knownNonCoreExecutable(t))
	coreImportCode(t, ierr, "core_override_active")

	if after := mustHash(t, corePath); after != before {
		t.Errorf("an override-blocked import modified the core: %s -> %s", before, after)
	}
}

// TestCoreImportLeavesNoStagingLeftovers — a failed import must not litter the
// bin directory with partial copies.
//
// A leftover would be picked up by nothing, but it would sit next to the real
// core forever and confuse anyone inspecting the folder by hand.
func TestCoreImportLeavesNoStagingLeftovers(t *testing.T) {
	b, _ := coreImportFixture(t)
	binDir := b.ac.FileService.Layout.Data.Bin()

	before := dirEntries(t, binDir)

	// Any outcome is fine here; what matters is what is left behind.
	_, _ = b.ImportCoreFile(knownNonCoreExecutable(t))

	after := dirEntries(t, binDir)
	for name := range after {
		if !before[name] {
			t.Errorf("a failed import left %q behind in the bin directory", name)
		}
	}
}
