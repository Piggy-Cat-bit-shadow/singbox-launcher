package services

import (
	"sync"
	"testing"
)

// TestResolveCoreIsSafeForConcurrentReaders is statement 23 (§34 name).
//
// `ResolveCore` writes four fields, each behind its own "only write if changed" check, and
// its own comment says the fields "are read by other goroutines without a lock". Those two
// facts together are a data race by construction: the checks are not atomic with the
// writes, and nothing orders any of it against a reader.
//
// The readers are real and concurrent with a re-resolve: every daemon command reads
// `SingboxPath` to build an install/uninstall command line, the version marker reads
// `CoreSource`, and the capability check reads `SingboxPath`. A core download re-resolves
// while those are in flight, which is exactly when the value changes.
//
// Run this test under `-race`; without the lock it reports a race on every one of the four
// fields.
func TestResolveCoreIsSafeForConcurrentReaders(t *testing.T) {
	fs := &FileService{}

	var wg sync.WaitGroup

	// Writers: re-resolve repeatedly, as a core download does.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				fs.ResolveCore()
			}
		}()
	}

	// Readers: the accessors real callers use.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = fs.CoreBinaryPath()
				_ = fs.CoreSourceName()
				_, _ = fs.CoreResolution()
			}
		}()
	}

	wg.Wait()
}

// TestCoreResolutionIsAConsistentSnapshot — the stronger property.
//
// Even with each field guarded individually, a reader that takes them one at a time can
// observe a MIX: the new path with the old source, or a path from one resolution and a
// wintun path from another. Callers combine them (a command line is built from the path, a
// version check from the source), so a torn read produces a command that names one binary
// and describes another.
//
// The three values must be readable as one snapshot.
func TestCoreResolutionIsAConsistentSnapshot(t *testing.T) {
	fs := &FileService{}
	fs.ResolveCore()

	path, source := fs.CoreResolution()
	if path == "" {
		t.Fatal("CoreResolution returned no path after a resolve")
	}
	// The snapshot's source must be the source of the path it reports.
	if got := fs.CoreBinaryPath(); got != path {
		t.Errorf("CoreBinaryPath()=%q disagrees with CoreResolution() path %q; a caller "+
			"combining them builds a command naming one binary and describing another",
			got, path)
	}
	if got := fs.CoreSourceName(); got != source {
		t.Errorf("CoreSourceName()=%q disagrees with CoreResolution() source %q", got, source)
	}
}
