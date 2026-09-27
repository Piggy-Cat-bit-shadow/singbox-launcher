//go:build darwin

package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadFileTail — чтение хвоста лога без загрузки файла целиком.
// Лог ротируется на 2 МиБ, и читать его весь ради 16 КиБ не нужно.
func TestReadFileTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core.log")

	t.Run("small file is returned whole", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("hello tail"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readFileTail(path, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if got != "hello tail" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("exact size is returned whole", func(t *testing.T) {
		content := strings.Repeat("x", 100)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readFileTail(path, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got != content {
			t.Fatalf("got %d bytes, want 100", len(got))
		}
	})

	t.Run("large file yields only the tail", func(t *testing.T) {
		// Голова, которую нельзя вернуть, и явный маркер в хвосте.
		head := strings.Repeat("HEAD", 1000)
		tail := "TAIL-MARKER-THE-CORE-FAILED-HERE"
		if err := os.WriteFile(path, []byte(head+tail), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readFileTail(path, int64(len(tail)+10))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > len(tail)+10 {
			t.Fatalf("read %d bytes, want at most %d", len(got), len(tail)+10)
		}
		if !strings.HasSuffix(got, tail) {
			t.Fatalf("tail does not end with the marker: %q", got)
		}
	})

	t.Run("empty file yields empty string", func(t *testing.T) {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readFileTail(path, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := readFileTail(filepath.Join(dir, "absent.log"), 1024); err == nil {
			t.Fatal("want an error for a missing file")
		}
	})
}

// TestReadFileTail_WorksWithClassify — хвост, прочитанный через seek, даёт ту
// же классификацию, что и раньше при чтении всего файла.
func TestReadFileTail_WorksWithClassify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core.log")
	// Большой лог: 512 КиБ шума, затем настоящая причина.
	noise := strings.Repeat("INFO router: loaded rules\n", 20000)
	failure := "FATAL start inbound/mixed[mixed-in]: listen tcp 127.0.0.1:7890: bind: address already in use\n"
	if err := os.WriteFile(path, []byte(noise+failure), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := readFileTail(path, coreLogTailBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := classifyExitText(lastLines(text, 40)); got != exitReasonPortInUse {
		t.Fatalf("classifyExitText = %v, want %v", got, exitReasonPortInUse)
	}
}
