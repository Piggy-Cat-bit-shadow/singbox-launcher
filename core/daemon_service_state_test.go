//go:build darwin || (windows && !386)

package core

import (
	"path/filepath"
	"strings"
	"testing"

	"singbox-launcher/internal/lxdclient"
)

// TestDaemonServiceProcessVerdict — третий шаг классификатора SPEC 136 §4
// поверх файлового вердикта: паспорт работающего демона (lx.11 и старое ядро
// без полей). Шаг сравнивает строки и на диск не ходит — пути фиктивные.
func TestDaemonServiceProcessVerdict(t *testing.T) {
	corePath := filepath.Join("service", "sing-box-lxd")
	launcherCore := filepath.Join("data", "bin", "sing-box")
	copySHA := strings.Repeat("ab", 32)
	c := DaemonServiceCheck{State: DaemonServiceOK, ServicePath: corePath, CopySHA256: copySHA, LauncherSHA256: copySHA}
	expect := func(t *testing.T, got DaemonServiceCheck, want DaemonServiceState) {
		t.Helper()
		if got.State != want {
			t.Fatalf("state = %s, want %s (detail: %s)", got.State, want, got.Detail)
		}
	}

	// Процесс: паспорт демона lx.11 (executable, executable_sha256).
	process := func(info lxdclient.InfoData, launcherVersion string) DaemonServiceCheck {
		pc := c
		pc.LauncherVersion = launcherVersion
		compareDaemonServiceProcess(&pc, info, corePath)
		return pc
	}
	expect(t, process(lxdclient.InfoData{Executable: corePath, ExecutableSHA256: c.CopySHA256}, ""), DaemonServiceOK)
	expect(t, process(lxdclient.InfoData{Executable: corePath, ExecutableSHA256: "00" + c.CopySHA256[2:]}, ""), DaemonServiceProcessStale)
	expect(t, process(lxdclient.InfoData{Executable: launcherCore, ExecutableSHA256: c.CopySHA256}, ""), DaemonServiceProcessStale)
	// lx.11 сразу после старта: executable есть, executable_sha256 ещё
	// считается в фоне и пуст — «неизвестно», не ProcessStale; судит версия.
	expect(t, process(lxdclient.InfoData{Executable: corePath, Version: "1.14.1-lx.11"}, "1.14.1-lx.11"), DaemonServiceOK)
	expect(t, process(lxdclient.InfoData{Executable: corePath, Version: "unknown"}, "1.14.1-lx.11"), DaemonServiceOK)
	expect(t, process(lxdclient.InfoData{Executable: corePath, Version: "1.14.1-lx.10"}, "1.14.1-lx.11"), DaemonServiceProcessStale)
	// Старое ядро (lx.8/lx.10): полей нет — судит версия; dev-сборка не судит.
	expect(t, process(lxdclient.InfoData{Version: "1.14.1-lx.10"}, "1.14.1-lx.11"), DaemonServiceProcessStale)
	expect(t, process(lxdclient.InfoData{Version: "1.14.1-lx.11"}, "1.14.1-lx.11"), DaemonServiceOK)
	expect(t, process(lxdclient.InfoData{Version: "unknown"}, "1.14.1-lx.11"), DaemonServiceOK)
	expect(t, process(lxdclient.InfoData{}, "1.14.1-lx.11"), DaemonServiceOK)

	// Вердикт по файлу сильнее вердикта по процессу.
	stale := c
	stale.State = DaemonServiceStale
	compareDaemonServiceProcess(&stale, lxdclient.InfoData{ExecutableSHA256: "ff"}, corePath)
	expect(t, stale, DaemonServiceStale)

	// Процесс не перебивает NotRunning (демон, запущенный руками, — не служба).
	nr := c
	nr.State = DaemonServiceNotRunning
	compareDaemonServiceProcess(&nr, lxdclient.InfoData{Executable: corePath, ExecutableSHA256: "ff"}, corePath)
	expect(t, nr, DaemonServiceNotRunning)
}

// TestServiceCoreVersionGateRemoved — гейт по версии ядра снят намеренно:
// право установить службу больше НЕ зависит от того, как называется версия
// ядра.
//
// Раньше здесь проверялось обратное — что разбор `-lx.N` и порог
// minCoreForRootOwnedService отсекают кастомные сборки. Теперь такой
// арифметики в продукте нет вовсе (parseCoreBuild/compareCoreBuilds удалены
// вместе с гейтом), поэтому тест фиксирует новое поведение: ЛЮБАЯ строка
// версии, включая пустую и неразбираемую, проходит гейт команд службы.
//
// Версия остаётся полезной только для показа и для сравнения ядра лаунчера с
// ядром демона — как информация, а не как граница допуска.
func TestServiceCoreVersionGateRemoved(t *testing.T) {
	versions := []string{
		// Реальные пользовательские сборки — раньше именно они отвергались.
		"1.15.0-jiejie-masquerade.5",
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
		"",
		// Прежние пронумерованные релизы — раньше одни проходили, другие нет.
		"1.14.1-lx.8",
		"1.14.1-lx.12",
		"1.14.2-lx.2",
		"1.14.2-lx.4",
		"1.15.0-lx.1",
		// Апстрим без поддержки lxd: лаунчер не гадает, ядро ответит само.
		"1.14.1",
		"1.16.0",
		"1.14.1-lx.",
		"1.14.1-lx.12x",
		"v-local-test",
	}
	for _, v := range versions {
		if err := serviceCoreGate(v); err != nil {
			t.Fatalf("serviceCoreGate(%q) = %v, want nil: no core version may be refused service installation", v, err)
		}
		c := DaemonServiceCheck{State: DaemonServiceStale, LauncherVersion: v}
		if !c.InstallSupported() {
			t.Fatalf("InstallSupported() = false for version %q: the Install step must always be offered", v)
		}
		if !c.NeedsInstall() {
			t.Fatalf("NeedsInstall() = false for a stale service with version %q", v)
		}
	}

	// И «пустой» ядро тоже допускается — версия не читается, бинаря нет,
	// но решение об установке от этого не меняется.
	if err := serviceCoreGate(""); err != nil {
		t.Fatalf("serviceCoreGate(\"\") = %v, want nil", err)
	}
	// Подсказки «обновите ядро» больше нет: пустая строка вместо текста.
	for _, v := range versions {
		if hint := DaemonServiceCoreHint(v); hint != "" {
			t.Fatalf("DaemonServiceCoreHint(%q) = %q, want empty: the core-version hint is gone", v, hint)
		}
	}
}
