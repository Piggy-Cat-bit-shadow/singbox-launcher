//go:build darwin

package core

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestFindOwnPrivilegedCorePID — ядро, запущенное этим лаунчером в прошлой
// сессии, узнаётся по pid-файлу (SPEC 144). Это то, что отличает «своё
// работающее ядро» от «чужого sing-box»: первое нельзя предлагать убить.
func TestFindOwnPrivilegedCorePID(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "singbox.pid")

	// Живой процесс — сам тест.
	alive := os.Getpid()
	// Заведомо мёртвый PID: верхняя граница pid_t в macOS.
	dead := 999999

	tests := []struct {
		name      string
		content   string
		want      int
		skipWrite bool
	}{
		{name: "no file", content: "", want: -1, skipWrite: true},
		{name: "empty file", content: "", want: -1},
		{name: "whitespace only", content: "  \n\n ", want: -1},
		{name: "garbage", content: "not-a-pid\n", want: -1},
		{name: "zero is not a process", content: "0\n", want: -1},
		{name: "negative is not a process", content: "-5\n", want: -1},
		{name: "dead pid", content: strconv.Itoa(dead) + "\n", want: -1},
		{name: "alive pid", content: strconv.Itoa(alive) + "\n", want: alive},
		{
			// Формат привилегированного запуска: PID шелла, затем PID ядра.
			name:    "shell and core pids, core alive",
			content: strconv.Itoa(dead) + "\n" + strconv.Itoa(alive),
			want:    alive,
		},
		{
			// Шелл пережил ядро — берём живой (обёртка ждёт ядро, поэтому
			// на практике они живут вместе).
			name:    "shell alive, core dead",
			content: strconv.Itoa(alive) + "\n" + strconv.Itoa(dead),
			want:    alive,
		},
		{name: "both dead", content: strconv.Itoa(dead) + "\n" + strconv.Itoa(dead-1), want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(pidFile)
			if !tt.skipWrite {
				if err := os.WriteFile(pidFile, []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := findOwnPrivilegedCorePID(pidFile); got != tt.want {
				t.Fatalf("findOwnPrivilegedCorePID(%q) = %d, want %d", tt.content, got, tt.want)
			}
		})
	}
}

// TestFindOwnPrivilegedCorePID_EmptyPath — пустой путь (платформа без
// привилегированного запуска) не паникует и не считает ядро своим.
func TestFindOwnPrivilegedCorePID_EmptyPath(t *testing.T) {
	if got := findOwnPrivilegedCorePID(""); got != -1 {
		t.Fatalf("empty path = %d, want -1", got)
	}
}

// TestProcessAlive — проверка существования процесса через signal 0.
func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("the test's own process must be alive")
	}
	if processAlive(999999) {
		t.Fatal("999999 must not be reported alive")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("non-positive pids must not be reported alive")
	}
}

// TestReadPrivilegedPidFile — разбор pid-файла: несколько PID, мусор
// игнорируется.
func TestReadPrivilegedPidFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "p.pid")

	if err := os.WriteFile(p, []byte("12 34\n56\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readPrivilegedPidFile(p)
	want := []int{12, 34, 56}
	if len(got) != len(want) {
		t.Fatalf("readPrivilegedPidFile = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("readPrivilegedPidFile = %v, want %v", got, want)
		}
	}

	if got := readPrivilegedPidFile(filepath.Join(dir, "absent")); got != nil {
		t.Fatalf("missing file = %v, want nil", got)
	}
	if got := readPrivilegedPidFile(""); got != nil {
		t.Fatalf("empty path = %v, want nil", got)
	}
}
