//go:build darwin || (windows && !386)

package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/internal/platform"
)

// SPEC 151: лаунчер не проверяет «происхождение» ядра перед установкой
// службы демона. Версия, тег, имя релиза и принадлежность к нумерованным
// релизам форка — не граница допуска. Решение о том, умеет ли конкретное
// ядро исполнить `lxd`, принимает сам запуск, и его настоящая ошибка
// показывается пользователю.
//
// Что здесь НЕ проверяется и не должно ослабляться: root-owned копия,
// root:wheel 0755, sudo, канонический plist, mTLS, пин отпечатка, парное
// сопряжение. Эти тесты — только про снятый version/release gate.

// customCoreVersions — наборы строк, которые раньше (или потенциально)
// могли быть отвергнуты «аудитом происхождения». Все обязаны проходить.
var customCoreVersions = []string{
	"1.15.0-jiejie-masquerade.5", // реальная сборка владельца
	"1.15.0-jiejie-masquerade.4",
	"1.15.0-custom",
	"1.16.0-dev",
	"1.16.0-nightly",
	"testing",
	"main",
	"git-abcdef",
	"dirty",
	"unknown",
	"custom-build",
	"jiejie",
	"", // версия не прочиталась
	// Прежние «слишком старые» и «не той раскладки» релизы форка.
	"1.14.1-lx.8",
	"1.14.1-lx.11",
	"1.14.2-lx.2",
	"1.14.2-lx.4",
	"1.15.0-lx.1",
	// Апстрим без lxd: лаунчер не гадает — ядро ответит само.
	"1.14.1",
	"1.16.0",
}

// TestDaemonInstallAvailableForAnyCoreVersion — TEST 1-4 §15: доступность
// установки службы не зависит от строки версии.
func TestDaemonInstallAvailableForAnyCoreVersion(t *testing.T) {
	for _, version := range customCoreVersions {
		check := DaemonServiceCheck{State: DaemonServiceStale, LauncherVersion: version}
		if !check.InstallSupported() {
			t.Fatalf("version %q: Daemon install available = false, want true", version)
		}
		if !check.NeedsInstall() {
			t.Fatalf("version %q: a stale service must be installable", version)
		}
		if err := serviceCoreGate(version); err != nil {
			t.Fatalf("version %q: serviceCoreGate = %v, want nil", version, err)
		}
	}
	// Отдельно и явно: три версии из задания.
	for _, version := range []string{"1.15.0-jiejie-masquerade.5", "custom-build", "unknown"} {
		if !(&DaemonServiceCheck{State: DaemonServiceStale, LauncherVersion: version}).InstallSupported() {
			t.Fatalf("version %q: Daemon install available = false", version)
		}
	}
	// Пустая строка — тоже true.
	if !(&DaemonServiceCheck{State: DaemonServiceStale}).InstallSupported() {
		t.Fatal("empty version: Daemon install available = false")
	}
}

// TestInstallPanelAlwaysShowsInstallCommand — TEST 6 §15: панель Install
// обязана отдавать команду, а не подсказку «обновите ядро».
func TestInstallPanelAlwaysShowsInstallCommand(t *testing.T) {
	const corePath = "/Applications/JiejieBox.app/Contents/Resources/bin/sing-box"
	want := daemonServiceCommand(corePath, "lxd", "--service=install")
	for _, version := range customCoreVersions {
		command, err := daemonInstallCommandFor(corePath, version)
		if err != nil {
			t.Fatalf("version %q: install command error %v; the Install step would be blocked", version, err)
		}
		if command == "" {
			t.Fatalf("version %q: empty install command", version)
		}
		if command != want {
			t.Fatalf("version %q: command %q, want %q", version, command, want)
		}
		// Команда обязана запускать именно ядро лаунчера под sudo — это и
		// есть «использовать текущее ядро лаунчера как source of truth».
		if !strings.Contains(command, "sudo") {
			t.Fatalf("version %q: command %q lost its sudo", version, command)
		}
		if !strings.Contains(command, corePath) {
			t.Fatalf("version %q: command %q does not reference the launcher core", version, command)
		}
	}
}

// TestNoCoreUpdateHintOrUpstreamDownloadText — TEST 7 §15: текстов
// «обновите ядро» и «Download/Reinstall v<pinned>» в подсказке службы нет.
func TestNoCoreUpdateHintOrUpstreamDownloadText(t *testing.T) {
	forbidden := []string{
		"numbered sing-box-lx",
		"root-owned service",
		"Update the core first",
		"Download/Reinstall",
		"Reinstall v",
	}
	for _, version := range customCoreVersions {
		hint := DaemonServiceCoreHint(version)
		if hint != "" {
			t.Fatalf("version %q: hint %q — must be empty", version, hint)
		}
		lower := strings.ToLower(hint)
		for _, bad := range forbidden {
			if strings.Contains(lower, strings.ToLower(bad)) {
				t.Fatalf("version %q: hint contains forbidden text %q", version, bad)
			}
		}
	}
}

// TestVersionMismatchDoesNotBlockInstall — TEST 5 §15: расхождение ядра
// лаунчера и установленной копии службы — статус, а не блокировка.
func TestVersionMismatchDoesNotBlockInstall(t *testing.T) {
	l := newTestServiceLayout(t)
	var hashes fileHashCache

	// Служба стоит на копии 1.15.0-jiejie-masquerade.4, лаунчер несёт .5.
	writeTestPlist(t, l.PlistPath, l.CorePath)
	writeTestFile(t, l.CorePath, "core masquerade.4")
	writeTestFile(t, daemonServiceSidecarPath(l.CorePath), `{"version":"1.15.0-jiejie-masquerade.4"}`)
	writeTestFile(t, l.launcherCore, "core masquerade.5")

	c := classifyDaemonServiceFiles(l.daemonServiceLayout, l.launcherCore, "1.15.0-jiejie-masquerade.5", &hashes)

	// Расхождение видно как Stale (копия — не ядро лаунчера) и не лечится
	// версией: команда install обязана быть.
	if c.State != DaemonServiceStale {
		t.Fatalf("state %s, want stale (detail: %s)", c.State, c.Detail)
	}
	if !c.NeedsInstall() || !c.InstallSupported() {
		t.Fatalf("mismatch blocks install: needsInstall=%v supported=%v", c.NeedsInstall(), c.InstallSupported())
	}
	// Обе версии доступны как статус — их можно показать пользователю.
	if c.CopyVersion != "1.15.0-jiejie-masquerade.4" {
		t.Fatalf("CopyVersion = %q, want the installed copy version", c.CopyVersion)
	}
	if c.LauncherVersion != "1.15.0-jiejie-masquerade.5" {
		t.Fatalf("LauncherVersion = %q, want the launcher core version", c.LauncherVersion)
	}
	cmd, err := daemonInstallCommandFor(l.launcherCore, c.LauncherVersion)
	if err != nil || cmd == "" {
		t.Fatalf("mismatch install command %q, err %v — want a command", cmd, err)
	}
	// И команда обновляет именно копию службы ядром лаунчера.
	if cmd != daemonServiceCommand(l.launcherCore, "lxd", "--service=install") {
		t.Fatalf("command %q does not install the launcher core over the stale copy", cmd)
	}
}

// TestRealRuntimeErrorIsNotSwallowed — TEST 14 §15: если ядро не умеет `lxd`,
// это выясняет запуск и возвращает НАСТОЯЩУЮ ошибку команды, а не
// синтетический «unsupported release» от лаунчера.
//
// Здесь гоняется настоящий бинарь (shell-скрипт): команда исполняется, ядро
// отвечает `unknown command`, и именно этот текст доходит до вызывающего.
func TestRealRuntimeErrorIsNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	// Скрипт, который не знает подкоманду lxd — как апстрим-ядро.
	fake := filepath.Join(dir, "sing-box")
	script := "#!/bin/sh\necho 'unknown command \"lxd\"' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Гейт не мешает: команда выдаётся даже для «плохой» версии.
	command, err := daemonInstallCommandFor(fake, "custom-build")
	if err != nil || command == "" {
		t.Fatalf("pre-validation refused the command: %q, err %v — the launcher must not guess", command, err)
	}

	// А настоящий запуск даёт настоящую ошибку ядра.
	cmd := exec.Command(fake, "lxd", "--service=install")
	platform.PrepareCommand(cmd)
	raw, runErr := cmd.CombinedOutput()
	out := string(raw)
	if runErr == nil {
		t.Fatalf("a core without lxd reported success; output %q", out)
	}
	if !strings.Contains(strings.ToLower(out), "unknown command") {
		t.Fatalf("stderr %q does not carry the core's real error", out)
	}
	// И это НЕ текст лаунчеровского пред-аудита.
	for _, bad := range []string{"numbered", "root-owned service", "Update the core first"} {
		if strings.Contains(out, bad) {
			t.Fatalf("error text %q is a launcher pre-validation message, not the core's real error", out)
		}
	}
}

// TestPairInviteNotGatedByCoreVersion — TEST 8 §15: приглашение и Pair не
// зависят от версии ядра: гейт команд службы снят, а сам Pair проверяет
// приглашение (отпечаток, сертификат), а не версию.
func TestPairInviteNotGatedByCoreVersion(t *testing.T) {
	for _, version := range customCoreVersions {
		// Команда install/fresh-invite — единственное, что версия могла бы
		// блокировать до сопряжения. Она есть всегда.
		if err := serviceCoreGate(version); err != nil {
			t.Fatalf("version %q: invite/pair path gated by core version: %v", version, err)
		}
	}
	// Права и каноничность копии при этом не ослаблены: plist службы, на
	// который смотрит Pair, по-прежнему обязан указывать на root-owned
	// копию, иначе вердикт Unsafe и CopyUsable=false.
	l := newTestServiceLayout(t)
	var hashes fileHashCache
	writeTestPlist(t, l.PlistPath, l.launcherCore) // plist на файл пользователя
	c := classifyDaemonServiceFiles(l.daemonServiceLayout, l.launcherCore, "1.15.0-jiejie-masquerade.5", &hashes)
	if c.State != DaemonServiceUnsafe {
		t.Fatalf("state %s, want unsafe: ownership checks must stay in force", c.State)
	}
	if c.CopyUsable() {
		t.Fatal("an unsafe copy must not be offered for service operations")
	}
}
