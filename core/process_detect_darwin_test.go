//go:build darwin

package core

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// procInfoFrom создаёт список процессов для теста без запуска ps.
func procInfoFrom(pairs ...interface{}) []procInfo {
	var out []procInfo
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, procInfo{PID: pairs[i].(int), Path: pairs[i+1].(string)})
	}
	return out
}

// TestOwnCorePID_RequiresPathMatch — главный инвариант SPEC 145: PID из
// pid-файла принимается только если executable path этого процесса —
// наша копия или наше ядро. Одного «PID жив» недостаточно: номера
// переиспользуются, и старый файл легко указывает на посторонний процесс.
func TestOwnCorePID_RequiresPathMatch(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "singbox.pid")
	ourCopy := filepath.Join(dir, "sing-box-lxd")
	launcherCore := filepath.Join(dir, "sing-box")
	foreign := filepath.Join(dir, "other", "sing-box")

	self := os.Getpid()
	id := coreIdentity{CopyPath: ourCopy, LauncherCorePath: launcherCore}

	write := func(pid int) {
		t.Helper()
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("alive PID whose path is our root copy is claimed", func(t *testing.T) {
		write(self)
		got := ownCorePID(pidFile, id, procInfoFrom(self, ourCopy), nil)
		if got != self {
			t.Fatalf("got %d, want %d", got, self)
		}
	})

	t.Run("alive PID whose path is the launcher core is claimed", func(t *testing.T) {
		write(self)
		got := ownCorePID(pidFile, id, procInfoFrom(self, launcherCore), nil)
		if got != self {
			t.Fatalf("got %d, want %d", got, self)
		}
	})

	t.Run("PID REUSE: alive but foreign path is NOT claimed", func(t *testing.T) {
		write(self)
		got := ownCorePID(pidFile, id, procInfoFrom(self, foreign), nil)
		if got != -1 {
			t.Fatalf("a foreign process must never be claimed, got %d", got)
		}
	})

	t.Run("PID REUSE: alive with unknown path is NOT claimed", func(t *testing.T) {
		write(self)
		got := ownCorePID(pidFile, id, nil, nil)
		if got != -1 {
			t.Fatalf("an unidentified process must never be claimed, got %d", got)
		}
	})

	t.Run("process list unavailable means unknown, not ours", func(t *testing.T) {
		write(self)
		got := ownCorePID(pidFile, id, nil, os.ErrPermission)
		if got != -1 {
			t.Fatalf("without a process list nothing may be claimed, got %d", got)
		}
	})

	t.Run("dead PID is not claimed", func(t *testing.T) {
		write(999999)
		got := ownCorePID(pidFile, id, procInfoFrom(999999, ourCopy), nil)
		if got != -1 {
			t.Fatalf("a dead PID must not be claimed, got %d", got)
		}
	})

	t.Run("no pid file means nothing to claim", func(t *testing.T) {
		got := ownCorePID(filepath.Join(dir, "absent.pid"), id, procInfoFrom(self, ourCopy), nil)
		if got != -1 {
			t.Fatalf("got %d, want -1", got)
		}
	})
}

// TestOwnCorePID_WrapperThenCore — формат pid-файла: строка шелла, строка
// ядра. Предпочтение отдаётся ядру (последняя строка), но обе личности
// законны.
func TestOwnCorePID_WrapperThenCore(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "singbox.pid")
	ourCopy := filepath.Join(dir, "sing-box-lxd")
	self := os.Getpid()

	// Шелл мёртв, ядро живо и опознано.
	content := strconv.Itoa(999999) + "\n" + strconv.Itoa(self)
	if err := os.WriteFile(pidFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	id := coreIdentity{CopyPath: ourCopy}
	got := ownCorePID(pidFile, id, procInfoFrom(self, ourCopy), nil)
	if got != self {
		t.Fatalf("got %d, want %d (the live core line must be used)", got, self)
	}
}

// TestPidMatchesCoreCopy — сравнение идёт по разрешённым путям, поэтому
// симлинк не позволяет выдать чужой бинарь за нашу копию.
func TestPidMatchesCoreCopy(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-core")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link-core")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if !pidMatchesCoreCopy(real, real) {
		t.Fatal("identical paths must match")
	}
	if !pidMatchesCoreCopy(link, real) {
		t.Fatal("a symlink to our core must match (paths are resolved before comparison)")
	}
	if pidMatchesCoreCopy(real, filepath.Join(dir, "other")) {
		t.Fatal("different paths must not match")
	}
	if pidMatchesCoreCopy("", real) || pidMatchesCoreCopy(real, "") {
		t.Fatal("empty paths must never match")
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
	// Мусор, нули и отрицательные не попадают в результат.
	if err := os.WriteFile(p, []byte("abc 0 -5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readPrivilegedPidFile(p); len(got) != 0 {
		t.Fatalf("garbage must be ignored, got %v", got)
	}
	if got := readPrivilegedPidFile(filepath.Join(dir, "absent")); got != nil {
		t.Fatalf("missing file = %v, want nil", got)
	}
	if got := readPrivilegedPidFile(""); got != nil {
		t.Fatalf("empty path = %v, want nil", got)
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

// TestCoreIdentity_Matches — личность нашего ядра: копия либо ядро лаунчера.
func TestCoreIdentity_Matches(t *testing.T) {
	dir := t.TempDir()
	ourCopy := filepath.Join(dir, "sing-box-lxd")
	launcherCore := filepath.Join(dir, "sing-box")
	foreign := filepath.Join(dir, "elsewhere", "sing-box")
	id := coreIdentity{CopyPath: ourCopy, LauncherCorePath: launcherCore}

	if !id.matches(ourCopy) {
		t.Fatal("our copy must match")
	}
	if !id.matches(launcherCore) {
		t.Fatal("the launcher core must match")
	}
	if id.matches(foreign) {
		t.Fatal("a foreign sing-box must not match")
	}
	if id.matches("") {
		t.Fatal("an empty path must not match")
	}
	// Пустая личность (не знаем путей) не совпадает ни с чем.
	empty := coreIdentity{}
	if empty.matches(ourCopy) || empty.matches(foreign) {
		t.Fatal("an empty identity must not match anything")
	}
}
