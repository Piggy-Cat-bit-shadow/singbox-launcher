package atomicfile

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestConcurrentWriteDoesNotShareTempFile is statement 6 (§34 name).
//
// The fixed `path + ".tmp"` staging name made every writer atomic only against a CRASH.
// Two concurrent writers truncated the SAME staging file, interleaved their bytes, and
// then raced to rename it — so the file that landed could be a mixture of both documents,
// or the rename could fail because the other writer had already moved it away.
//
// Each writer here writes a document that must land WHOLE. Anything else means the file
// is a blend of two writers, which for a state or settings file means unparseable data.
func TestConcurrentWriteDoesNotShareTempFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")

	const (
		writers  = 24
		perWrite = 40
	)

	// Each writer's document is a distinct, self-consistent JSON object padded to a size
	// that makes interleaving visible rather than theoretical.
	doc := func(n, i int) []byte {
		pad := bytes.Repeat([]byte{'x'}, 4096)
		return []byte(fmt.Sprintf(`{"writer":%d,"n":%d,"pad":%q}`, n, i, string(pad)))
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWrite)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < perWrite; i++ {
				if err := Write(target, doc(n, i), 0o644); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("a concurrent write failed: %v — with a unique staging path, one "+
			"writer's rename cannot be invalidated by another's", err)
	}

	// Whatever landed must be exactly ONE writer's document, never a blend.
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("the target is empty")
	}
	// Every writer's document has the same shape, so a correct result matches one of
	// them exactly. A truncated or interleaved file will not.
	found := false
	for n := 0; n < writers; n++ {
		for i := 0; i < perWrite; i++ {
			if bytes.Equal(data, doc(n, i)) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the target is not any single writer's document (len=%d); the "+
			"writers shared a staging file", len(data))
	}

	// No staging files may remain.
	left, _ := filepath.Glob(filepath.Join(dir, ".*tmp*"))
	if len(left) != 0 {
		t.Errorf("staging files were left behind: %v", left)
	}
}

// TestFailedWriteLeavesTheTargetIntact — the crash-safety half, which must survive the
// concurrency fix.
func TestFailedWriteLeavesTheTargetIntact(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	original := []byte(`{"original":true}`)
	if err := os.WriteFile(target, original, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := WriteWith(target, 0o644, func(w io.Writer) error {
		_, _ = w.Write([]byte(`{"partial":`))
		return fmt.Errorf("boom")
	})
	if err == nil {
		t.Fatal("a failing writer reported success")
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("a failed write modified the target: %q", after)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".*tmp*"))
	if len(left) != 0 {
		t.Errorf("a failed write left staging files behind: %v", left)
	}
}

// TestWriteIsAtomicForAReader — a reader running concurrently with writers must never see
// a partial document.
func TestWriteIsAtomicForAReader(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "settings.json")

	small := []byte(`{"v":"small"}`)
	large := []byte(`{"v":"` + string(bytes.Repeat([]byte{'L'}, 200000)) + `"}`)
	if err := Write(target, small, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stop := make(chan struct{})
	var readerErr error
	var once sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(target)
			if err != nil {
				continue
			}
			if !bytes.Equal(data, small) && !bytes.Equal(data, large) {
				once.Do(func() { readerErr = fmt.Errorf("torn read: %d bytes", len(data)) })
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		doc := small
		if i%2 == 0 {
			doc = large
		}
		if err := Write(target, doc, 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if readerErr != nil {
		t.Fatal(readerErr)
	}
}

// TestSweepStaleRemovesLeftovers — a process killed mid-write leaves a dot-file.
func TestSweepStaleRemovesLeftovers(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")

	leftover := filepath.Join(dir, ".config.json.tmp-123456")
	if err := os.WriteFile(leftover, []byte("junk"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Age it: the sweep keyed on modification time, because a name that is merely unique
	// cannot distinguish an abandoned file from one a live writer is filling right now.
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(leftover, old, old); err != nil {
		t.Fatalf("age the leftover: %v", err)
	}

	SweepStale(target)
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Error("a stale staging file survived the sweep")
	}
}

// TestSweepStaleKeepsALiveWritersStagingFile — the property that makes the sweep safe.
//
// Removing every match by name is what an earlier version did, and it made a concurrent
// write fail with "no such file or directory" on the rename: the sweeper deleted a staging
// file another writer was still filling. Age is what actually separates the two cases.
func TestSweepStaleKeepsALiveWritersStagingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")

	// A staging file being written RIGHT NOW: fresh mtime.
	live := filepath.Join(dir, ".config.json.tmp-999999")
	if err := os.WriteFile(live, []byte("in progress"), 0o644); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	SweepStale(target)

	if _, err := os.Stat(live); err != nil {
		t.Fatalf("the sweep removed a staging file written moments ago (%v). A live "+
			"writer would then fail its rename with \"no such file or directory\"", err)
	}
}
