// Package atomicfile writes a file so that a reader never observes a partial or
// interleaved version of it, and so that concurrent writers cannot corrupt each other.
//
// # WHY THIS IS A PACKAGE
//
// The repository had five different hand-rolled versions of "write a file safely":
//
//	state.Save          fixed `path + ".tmp"`, fsync, rename
//	locale.SaveSettings fixed `path + ".tmp"`, rename
//	markConfigManaged   plain os.WriteFile
//	the build candidate fixed `config.json.candidate`
//	core import         CreateTemp, chmod, sync, rename  ← the only correct one
//
// The fixed-temp variants are only atomic against a CRASH. Against a concurrent writer
// they are worse than not trying: two writers truncate the SAME temp path, interleave
// their bytes, and then race to rename it, so the file that lands can be a mixture of
// both documents — or the rename can fail because the other writer already moved it.
//
// Every one of those call sites wanted the same thing, so it lives in one place where
// it can be tested once and got right once.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// MaxAttempts bounds the retry loop for generating a unique temporary name. Reaching it
// would mean 64 consecutive name collisions, which indicates something is badly wrong
// with the directory rather than bad luck.
const MaxAttempts = 64

// Write replaces target with data.
//
// The sequence is: create a UNIQUE sibling temp file → write → fsync → chmod → close →
// rename → fsync the directory. A unique sibling matters for two independent reasons:
// concurrent writers must not share a staging path, and the rename must be within one
// filesystem to be atomic, which a system temp directory does not guarantee.
//
// On any failure the temp file is removed and target is left exactly as it was.
func Write(target string, data []byte, perm os.FileMode) error {
	return WriteWith(target, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteWith replaces target with whatever fn writes to it.
//
// Streaming form, for content large enough that holding a second copy is wasteful (the
// core binary staged during an import). fn must not retain the writer.
func WriteWith(target string, perm os.FileMode, fn func(io.Writer) error) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("atomicfile: mkdir %s: %w", dir, err)
	}

	tmp, f, err := createUnique(dir, filepath.Base(target))
	if err != nil {
		return err
	}
	// Cleanup is armed BEFORE the write: every failure path below must remove the
	// staging file, and a deferred remove is the only way to guarantee that for the
	// ones that return early.
	committed := false
	defer func() {
		if !committed {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if err := fn(f); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("atomicfile: fsync %s: %w", tmp, err)
	}
	// Chmod after creation rather than at creation: CreateTemp always makes the file
	// 0600, and a config or state file that only its owner can read breaks nothing,
	// while one that is accidentally world-writable does.
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("atomicfile: chmod %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("atomicfile: close %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("atomicfile: rename %s → %s: %w", tmp, target, err)
	}
	committed = true

	// Directory fsync makes the RENAME durable, not just the file contents. Without
	// it a crash can leave the old name pointing at the old inode even though the new
	// data is safely on disk. Best-effort: Windows cannot open a directory for sync,
	// and failing here must not fail a write that has already succeeded.
	SyncDir(dir)
	return nil
}

// createUnique makes a temp file beside target, retrying on the astronomically unlikely
// name collision.
func createUnique(dir, base string) (string, *os.File, error) {
	for i := 0; i < MaxAttempts; i++ {
		// The pattern keeps the real filename visible in a directory listing, so a
		// leftover staging file after a hard kill is identifiable, and starts with a
		// dot so it does not look like a second real config.
		f, err := os.CreateTemp(dir, "."+base+".tmp-*")
		if err == nil {
			return f.Name(), f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("atomicfile: create temp in %s: %w", dir, err)
		}
	}
	return "", nil, fmt.Errorf("atomicfile: could not create a unique temp file in %s "+
		"after %d attempts", dir, MaxAttempts)
}

// SyncDir flushes a directory entry so a rename survives a crash. Best-effort by design:
// the platforms that cannot do it return an error that means "unsupported", not "lost".
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// SweepStale removes leftover staging files for target.
//
// A process killed between create and rename leaves a dot-file behind. Nothing depends on
// them, but a directory that accumulates them is a directory whose owner has stopped
// noticing what is in it — so cleaning them up is worth doing by whoever can do it safely.
//
// **NOTHING CALLS THIS.** `WriteWith` does not, and must not; see the last paragraph. Read the
// whole comment before wiring it anywhere.
//
// ONLY FILES OLDER THAN staleStagingAge ARE REMOVED, and that bound is what makes it safe to
// call next to live writers.
//
// A unique staging name stops two writers from SHARING a file; it says nothing about which
// files are still being written. An earlier version of this function removed every match and
// documented itself as safe to call "first" because the names were unique — that reasoning is
// wrong, and wiring it into `WriteWith` made `TestConcurrentWriteDoesNotShareTempFile` fail
// immediately with "no such file or directory" on the rename: one writer deleted another's
// staging file out from under it.
//
// Age is the property that actually distinguishes a leftover from a live write. A staging
// file is written, fsynced and renamed within one operation, so one that has not been touched
// for hours belongs to a process that is gone.
//
// WHY `WriteWith` MUST NOT CALL IT: a writer cannot know whether another writer holds a
// staging file, which is how the name-only version came to delete a live writer's file.
// Wiring it in requires establishing exclusivity first, which nothing in this package can do
// on its own. It stays exported, correct and unused rather
// than being given a call site that would be wrong.
func SweepStale(target string) {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	matches, err := filepath.Glob(filepath.Join(dir, "."+base+".tmp-*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-staleStagingAge)
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(m)
	}
}

// staleStagingAge is how old a staging file must be to count as abandoned.
//
// Generous on purpose: the cost of leaving a leftover for another day is a dot-file nobody
// looks at, while the cost of removing a live one is a failed write — and a failed write is
// what the whole atomic-write path exists to prevent.
const staleStagingAge = time.Hour
