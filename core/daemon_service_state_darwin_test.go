//go:build darwin

package core

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/core/services"
	"singbox-launcher/internal/lxdclient"
)

// testServiceLayout — раскладка службы lx.11 во временном каталоге:
// копия — плоский файл <base>/Library/PrivilegedHelperTools/sing-box-lxd,
// legacy ранних lx.11 — <base>/Library/PrivilegedHelperTools/<label>, plist в
// <base>/LaunchDaemons, ядро лаунчера в <base>/data/bin. Владелец цепочки —
// текущий uid (root-owned файлы тест создать не может).
type testServiceLayout struct {
	daemonServiceLayout
	toolsDir     string
	launcherCore string
}

func newTestServiceLayout(t *testing.T) testServiceLayout {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "Library")
	tools := filepath.Join(root, "PrivilegedHelperTools")
	for _, dir := range []string{tools, filepath.Join(base, "LaunchDaemons"), filepath.Join(base, "data", "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// MkdirAll уважает umask: выставляем права цепочки явно.
	for _, dir := range []string{root, tools} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l := testServiceLayout{
		daemonServiceLayout: daemonServiceLayout{
			PlistPath:  filepath.Join(base, "LaunchDaemons", daemonLaunchdLabel+".plist"),
			CorePath:   filepath.Join(tools, filepath.Base(daemonServiceCorePath())),
			LegacyPath: filepath.Join(tools, filepath.Base(daemonServiceLegacyCopyPath)),
			ChainRoot:  root,
			OwnerUID:   uint32(os.Getuid()),
		},
		toolsDir:     tools,
		launcherCore: filepath.Join(base, "data", "bin", "sing-box"),
	}
	writeTestFile(t, l.launcherCore, "core v1")
	return l
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeTestPlist — plist в форме, которую пишет `lxd --service=install`.
func writeTestPlist(t *testing.T, path, program string) {
	t.Helper()
	body := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>` + daemonLaunchdLabel + `</string>
    <key>ProgramArguments</key>
    <array>
        <string>` + program + `</string>
        <string>lxd</string>
        <string>--state-dir</string>
        <string>/Library/Application Support/sing-box-lxd/state</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// classifyTestService — полный проход классификатора без процесса.
func classifyTestService(l testServiceLayout, hashes *fileHashCache) DaemonServiceCheck {
	c := inspectDaemonServiceDefinition(l.daemonServiceLayout)
	compareDaemonServiceFiles(&c, l.CorePath, l.launcherCore, hashes)
	return c
}

// TestDaemonServiceClassifier — вердикты SPEC 136 §4 на настоящих файлах:
// путь в plist, цепочка владения, sha копии против ядра лаунчера, кэш хэшей,
// состояние у launchd. Паспорт работающего демона — общий шаг, его вердикты
// в TestDaemonServiceProcessVerdict.
func TestDaemonServiceClassifier(t *testing.T) {
	l := newTestServiceLayout(t)
	var hashes fileHashCache
	expect := func(t *testing.T, got DaemonServiceCheck, want DaemonServiceState) {
		t.Helper()
		if got.State != want {
			t.Fatalf("state = %s, want %s (detail: %s)", got.State, want, got.Detail)
		}
	}

	// NotInstalled: plist нет.
	expect(t, classifyTestService(l, &hashes), DaemonServiceNotInstalled)

	// Unsafe: служба на пользовательском ядре — ровно дыра SPEC 136 §1.
	writeTestPlist(t, l.PlistPath, l.launcherCore)
	c := classifyTestService(l, &hashes)
	expect(t, c, DaemonServiceUnsafe)
	if c.ServicePath != l.launcherCore || c.CopyUsable() || !c.NeedsInstall() {
		t.Fatalf("unsafe: path=%q usable=%v needsInstall=%v", c.ServicePath, c.CopyUsable(), c.NeedsInstall())
	}

	// Unsafe: plist не разобрался.
	if err := os.WriteFile(l.PlistPath, []byte("not a plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, classifyTestService(l, &hashes), DaemonServiceUnsafe)

	// Stale: plist на копию, копии нет (каталоги целы).
	writeTestPlist(t, l.PlistPath, l.CorePath)
	c = classifyTestService(l, &hashes)
	expect(t, c, DaemonServiceStale)
	if !c.CopyMissing || c.CopyUsable() {
		t.Fatalf("missing copy: CopyMissing=%v usable=%v", c.CopyMissing, c.CopyUsable())
	}

	// Unsafe: на месте файла копии — каталог; причина говорит его убрать.
	if err := os.Mkdir(l.CorePath, 0o755); err != nil {
		t.Fatal(err)
	}
	c = classifyTestService(l, &hashes)
	expect(t, c, DaemonServiceUnsafe)
	if !strings.Contains(c.Detail, "is a directory") || !strings.Contains(c.Detail, "remove it") {
		t.Fatalf("folder in place of the copy: %q", c.Detail)
	}
	if err := os.Remove(l.CorePath); err != nil {
		t.Fatal(err)
	}

	// Unsafe: plist на копию ранней раскладки lx.11 — каталог <label>/sing-box
	// или плоский <label>; причина — install, затем убрать остатки.
	writeTestFile(t, l.LegacyPath, "core v1")
	for _, legacyProgram := range []string{l.LegacyPath, filepath.Join(l.LegacyPath, "sing-box")} {
		writeTestPlist(t, l.PlistPath, legacyProgram)
		c = classifyTestService(l, &hashes)
		expect(t, c, DaemonServiceUnsafe)
		if !strings.Contains(c.Detail, "legacy layout") || !strings.Contains(c.Detail, "sudo rm -rf "+shellQuote(l.LegacyPath)) {
			t.Fatalf("legacy plist %s: %q", legacyProgram, c.Detail)
		}
	}
	if err := os.Remove(l.LegacyPath); err != nil {
		t.Fatal(err)
	}
	writeTestPlist(t, l.PlistPath, l.CorePath)

	// OK: копия = ядро лаунчера. Повтор с теми же файлами — из кэша.
	writeTestFile(t, l.CorePath, "core v1")
	writeTestFile(t, daemonServiceSidecarPath(l.CorePath), `{"version":"1.14.1-lx.11"}`)
	if v := readDaemonServiceSidecarVersion(l.CorePath); v != "1.14.1-lx.11" {
		t.Fatalf("sidecar %s: version %q", daemonServiceSidecarPath(l.CorePath), v)
	}
	c = classifyTestService(l, &hashes)
	expect(t, c, DaemonServiceOK)
	if !c.CopyUsable() || c.CopySHA256 == "" || c.CopySHA256 != c.LauncherSHA256 {
		t.Fatalf("ok: usable=%v copy=%q launcher=%q", c.CopyUsable(), c.CopySHA256, c.LauncherSHA256)
	}
	// Содержимое одинаковое, но inode разные — два чтения.
	if hashes.computed != 2 {
		t.Fatalf("hashes computed %d times, want 2", hashes.computed)
	}
	computed := hashes.computed
	expect(t, classifyTestService(l, &hashes), DaemonServiceOK)
	if hashes.computed != computed {
		t.Fatalf("unchanged files were re-hashed: %d → %d", computed, hashes.computed)
	}

	// Stale: ядро лаунчера обновилось (скачивание), копия старая. Кэш видит
	// замену по ключу и перечитывает только изменившийся файл.
	writeTestFile(t, l.launcherCore, "core v2, a longer build")
	c = classifyTestService(l, &hashes)
	expect(t, c, DaemonServiceStale)
	if c.CopyMissing || !c.CopyUsable() || c.CopySHA256 == c.LauncherSHA256 {
		t.Fatalf("stale: missing=%v usable=%v copy=%q launcher=%q", c.CopyMissing, c.CopyUsable(), c.CopySHA256, c.LauncherSHA256)
	}
	if hashes.computed != computed+1 {
		t.Fatalf("after replacing the launcher core: computed %d, want %d", hashes.computed, computed+1)
	}

	// Ядро лаунчера — симлинк на dev-сборку: сверяется цель ссылки.
	devBuild := filepath.Join(filepath.Dir(l.launcherCore), "sing-box-dev")
	writeTestFile(t, devBuild, "core v1")
	if err := os.Remove(l.launcherCore); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(devBuild, l.launcherCore); err != nil {
		t.Fatal(err)
	}
	expect(t, classifyTestService(l, &hashes), DaemonServiceOK)

	// Unsafe: каталог помощников (родитель плоской копии) пишется группой.
	if err := os.Chmod(l.toolsDir, 0o775); err != nil {
		t.Fatal(err)
	}
	expect(t, classifyTestService(l, &hashes), DaemonServiceUnsafe)
	if err := os.Chmod(l.toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Unsafe: верх цепочки пишется всеми.
	if err := os.Chmod(l.ChainRoot, 0o757); err != nil {
		t.Fatal(err)
	}
	expect(t, classifyTestService(l, &hashes), DaemonServiceUnsafe)
	if err := os.Chmod(l.ChainRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// Unsafe: копия пишется группой.
	if err := os.Chmod(l.CorePath, 0o775); err != nil {
		t.Fatal(err)
	}
	expect(t, classifyTestService(l, &hashes), DaemonServiceUnsafe)
	if err := os.Chmod(l.CorePath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Unsafe: чужой владелец цепочки (в проде — не root).
	foreign := l
	foreign.OwnerUID = l.OwnerUID + 1
	expect(t, classifyTestService(foreign, &hashes), DaemonServiceUnsafe)

	// Unsafe: копия — симлинк (на пользовательский файл).
	if err := os.Rename(l.CorePath, l.CorePath+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(devBuild, l.CorePath); err != nil {
		t.Fatal(err)
	}
	expect(t, classifyTestService(l, &hashes), DaemonServiceUnsafe)
	if err := os.Remove(l.CorePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(l.CorePath+".real", l.CorePath); err != nil {
		t.Fatal(err)
	}
	c = classifyTestService(l, &hashes)
	expect(t, c, DaemonServiceOK)

	// launchd: файлы в порядке, но служба не загружена или не running —
	// NotRunning (лечится bootstrap, не install); launchd не ответил —
	// вердикта нет; поверх файлового вердикта launchd не судит.
	launchd := func(base DaemonServiceCheck, job launchdJob) DaemonServiceCheck {
		lc := base
		compareDaemonServiceLaunchd(&lc, job)
		return lc
	}
	expect(t, launchd(c, launchdJob{}), DaemonServiceOK)
	expect(t, launchd(c, launchdJob{Known: true, Loaded: true, State: "running"}), DaemonServiceOK)
	notLoaded := launchd(c, launchdJob{Known: true})
	expect(t, notLoaded, DaemonServiceNotRunning)
	if notLoaded.LaunchdState != launchdNotLoaded || !notLoaded.NeedsBootstrap() || notLoaded.NeedsInstall() || !notLoaded.CopyUsable() {
		t.Fatalf("not loaded: launchd %q bootstrap=%v install=%v usable=%v",
			notLoaded.LaunchdState, notLoaded.NeedsBootstrap(), notLoaded.NeedsInstall(), notLoaded.CopyUsable())
	}
	waiting := launchd(c, launchdJob{Known: true, Loaded: true, State: "spawn scheduled"})
	expect(t, waiting, DaemonServiceNotRunning)
	if waiting.LaunchdState != "spawn scheduled" {
		t.Fatalf("launchd state %q", waiting.LaunchdState)
	}
	staleCheck := c
	staleCheck.State = DaemonServiceStale
	expect(t, launchd(staleCheck, launchdJob{Known: true}), DaemonServiceStale)
	if cmd := daemonBootstrapCommand(); cmd != "sudo launchctl bootstrap system '/Library/LaunchDaemons/"+daemonLaunchdLabel+".plist'" {
		t.Fatalf("bootstrap command %q", cmd)
	}
	// Разбор `launchctl print`: state самой службы, не вложенных блоков.
	printed := "system/" + daemonLaunchdLabel + " = {\n\tactive count = 0\n\tendpoints = {\n\t\tstate = active\n\t}\n\tstate = not running\n\tpid = 0\n}\n"
	if got := parseLaunchctlPrintState([]byte(printed)); got != "not running" {
		t.Fatalf("parsed launchd state %q, want %q", got, "not running")
	}
	if got := parseLaunchctlPrintState([]byte("garbage")); got != "" {
		t.Fatalf("parsed launchd state from garbage: %q", got)
	}
}

// TestDaemonServiceCommandQuoting — sudo-команды службы с путём, в котором
// пробел, апостроф и двойная кавычка: команда синтаксически верна для sh,
// разбирается ровно в задуманные аргументы и переживает литерал AppleScript,
// через который её получает Terminal. Uninstall идёт через копию, только
// когда она безопасна (SPEC 136 §5); вкладка Uninstall оставляет копию
// (`--keep-copy`), подсказка очистки данных — нет.
func TestDaemonServiceCommandQuoting(t *testing.T) {
	bin := "/Users/o'brien/My Apps/\"lx\" core/sing-box"
	commands := map[string][]string{
		daemonServiceCommand(bin, "lxd", "--service=install"): {bin, "lxd", "--service=install"},
		daemonUninstallCommandFor(bin, true, true):            {bin, "lxd", "--service=uninstall", "--keep-copy", "--purge"},
		daemonUninstallCommandFor(bin, false, true):           {bin, "lxd", "--service=uninstall", "--keep-copy"},
		daemonUninstallCommandFor(bin, true, false):           {bin, "lxd", "--service=uninstall", "--purge"},
	}
	wantInstall := `sudo '/Users/o'\''brien/My Apps/"lx" core/sing-box' lxd --service=install`
	if got := daemonServiceCommand(bin, "lxd", "--service=install"); got != wantInstall {
		t.Fatalf("install command:\n got %s\nwant %s", got, wantInstall)
	}
	for command, wantArgs := range commands {
		if out, err := exec.Command("sh", "-n", "-c", command).CombinedOutput(); err != nil {
			t.Fatalf("sh -n %q: %v (%s)", command, err, out)
		}
		// sudo подменён функцией, печатающей свои аргументы по строке.
		out, err := exec.Command("sh", "-c", `sudo() { printf '%s\n' "$@"; }; `+command).Output()
		if err != nil {
			t.Fatalf("sh -c %q: %v", command, err)
		}
		if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); strings.Join(got, "|") != strings.Join(wantArgs, "|") {
			t.Fatalf("%q parsed as %q, want %q", command, got, wantArgs)
		}
		if osascript, err := exec.LookPath("osascript"); err == nil {
			out, err := exec.Command(osascript, "-e", "return "+appleScriptString(command)).Output()
			if err != nil {
				t.Fatalf("osascript literal of %q: %v", command, err)
			}
			if got := strings.TrimSuffix(string(out), "\n"); got != command {
				t.Fatalf("AppleScript literal round-trip:\n got %s\nwant %s", got, command)
			}
		}
	}

	// Uninstall и client add: копия — только когда plist на неё и она цела.
	l := newTestServiceLayout(t)
	if got := daemonServiceBinaryFor(l.daemonServiceLayout, l.launcherCore); got != l.launcherCore {
		t.Fatalf("no service: binary %s, want the launcher core", got)
	}
	writeTestPlist(t, l.PlistPath, l.launcherCore)
	if got := daemonServiceBinaryFor(l.daemonServiceLayout, l.launcherCore); got != l.launcherCore {
		t.Fatalf("unsafe service: binary %s, want the launcher core", got)
	}
	writeTestPlist(t, l.PlistPath, l.CorePath)
	if got := daemonServiceBinaryFor(l.daemonServiceLayout, l.launcherCore); got != l.launcherCore {
		t.Fatalf("missing copy: binary %s, want the launcher core", got)
	}
	writeTestFile(t, l.CorePath, "core v0")
	if got := daemonServiceBinaryFor(l.daemonServiceLayout, l.launcherCore); got != l.CorePath {
		t.Fatalf("safe copy: binary %s, want the copy", got)
	}
}

// TestDaemonServiceNoCoreVersionGate — гейт по версии ядра снят: служба
// получает команду install/copy независимо от того, какой у ядра лаунчера
// version string, включая кастомные сборки и неразбираемые значения.
//
// Раньше этот тест (TestDaemonServiceCoreTooOld) утверждал ровно обратное —
// что ядро ниже порога root-owned копии не получает команду ни по одному
// каналу. Теперь проверяем, что команда есть ВСЕГДА, а вердикт классификатора
// остаётся честным (Stale/Unsafe/ProcessStale — по файлам, не по версии).
func TestDaemonServiceNoCoreVersionGate(t *testing.T) {
	l := newTestServiceLayout(t)
	var hashes fileHashCache

	// Версии, которые раньше отвергались (или могли бы): кастомная сборка
	// владельца, dev-строки, пустая, и прежние «слишком старые» lx-релизы.
	versions := []string{
		"1.15.0-jiejie-masquerade.5",
		"1.15.0-custom",
		"1.16.0-dev",
		"unknown",
		"custom-build",
		"",
		"1.14.1-lx.8",
		"1.14.1-lx.11",
		"1.14.2-lx.4",
	}

	// Копия существует и цела, ядро лаунчера — другой файл => Stale.
	writeTestPlist(t, l.PlistPath, l.CorePath)
	writeTestFile(t, l.CorePath, "core copy")
	writeTestFile(t, daemonServiceSidecarPath(l.CorePath), `{"version":"1.15.0-jiejie-masquerade.4"}`)
	writeTestFile(t, l.launcherCore, "core launcher")

	for _, version := range versions {
		c := classifyDaemonServiceFiles(l.daemonServiceLayout, l.launcherCore, version, &hashes)
		if c.State != DaemonServiceStale {
			t.Fatalf("version %q: state %s, want stale (detail: %s)", version, c.State, c.Detail)
		}
		if !c.NeedsInstall() {
			t.Fatalf("version %q: NeedsInstall() = false; the service would have no way to update", version)
		}
		if !c.InstallSupported() {
			t.Fatalf("version %q: InstallSupported() = false; the Install step would be hidden", version)
		}
		// Команда install обязана существовать и указывать на ядро лаунчера.
		cmd, err := daemonInstallCommandFor(l.launcherCore, version)
		if err != nil || cmd == "" {
			t.Fatalf("version %q: install command %q, err %v — want a command", version, cmd, err)
		}
		want := daemonServiceCommand(l.launcherCore, "lxd", "--service=install")
		if cmd != want {
			t.Fatalf("version %q: install command %q, want %q", version, cmd, want)
		}
		// Диалог после обновления ядра — та же команда.
		if got := daemonCoreUpdatedCommand(c, l.launcherCore); got != want {
			t.Fatalf("version %q: core update dialog command %q, want %q", version, got, want)
		}
		// Подсказки «обновите ядро» нет.
		if hint := DaemonServiceCoreHint(version); hint != "" {
			t.Fatalf("version %q: hint %q, want empty", version, hint)
		}
	}

	// Classic-гейт копии: команда есть для любой версии, включая «старые».
	for _, withPlist := range []bool{false, true} {
		if withPlist {
			writeTestPlist(t, l.PlistPath, l.CorePath)
		} else if err := os.Remove(l.PlistPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		for _, version := range versions {
			cmd, _, err := privilegedCopyCommandFor(l.daemonServiceLayout, l.launcherCore, version)
			if err != nil || cmd == "" {
				t.Fatalf("plist=%v version %q: classic command %q, err %v — want a command", withPlist, version, cmd, err)
			}
		}
	}

	// Unsafe (plist на ядро лаунчера) остаётся Unsafe: это про права и
	// владение, а не про версию, и команда install её по-прежнему лечит.
	writeTestPlist(t, l.PlistPath, l.launcherCore)
	c := classifyDaemonServiceFiles(l.daemonServiceLayout, l.launcherCore, "1.15.0-jiejie-masquerade.5", &hashes)
	if c.State != DaemonServiceUnsafe {
		t.Fatalf("plist on the launcher core: state %s, want unsafe (detail: %s)", c.State, c.Detail)
	}
	if !c.NeedsInstall() || !c.InstallSupported() {
		t.Fatalf("unsafe service: install=%v supported=%v — the install command must be offered", c.NeedsInstall(), c.InstallSupported())
	}
	if c.CopyUsable() {
		t.Fatal("unsafe service reported a usable copy")
	}

	// ProcessStale по-прежнему определяется процессом, не версией.
	writeTestPlist(t, l.PlistPath, l.CorePath)
	writeTestFile(t, l.launcherCore, "core copy")
	c = classifyDaemonServiceFiles(l.daemonServiceLayout, l.launcherCore, "1.15.0-jiejie-masquerade.5", &hashes)
	if c.State != DaemonServiceOK {
		t.Fatalf("same files: state %s (%s)", c.State, c.Detail)
	}
	compareDaemonServiceProcess(&c, lxdclient.InfoData{Executable: l.CorePath, ExecutableSHA256: "ff"}, l.CorePath)
	gateServiceInstall(&c)
	if c.State != DaemonServiceProcessStale {
		t.Fatalf("stale process: state %s, want process_stale", c.State)
	}
	if !c.NeedsInstall() || !c.InstallSupported() {
		t.Fatalf("process_stale: install=%v supported=%v — the install command must be offered", c.NeedsInstall(), c.InstallSupported())
	}

	// Debug API /daemon/commands на настоящем «ядре»: install непустой для
	// ЛЮБОЙ версии, включая кастомную и неразбираемую.
	for _, version := range []string{"1.15.0-jiejie-masquerade.5", "custom-build", "unknown", "1.14.1-lx.8", "1.14.2-lx.4"} {
		fake := filepath.Join(t.TempDir(), "sing-box")
		writeTestFile(t, fake, "#!/bin/sh\necho 'sing-box version "+version+"'\n")
		ac := &AppController{FileService: &services.FileService{SingboxPath: fake}}
		install := (&debugAPIDaemonWiring{ac: ac}).Commands().Install
		want := daemonServiceCommand(fake, "lxd", "--service=install")
		if install != want {
			t.Fatalf("core %s: Debug API install command %q, want %q", version, install, want)
		}
	}
}
