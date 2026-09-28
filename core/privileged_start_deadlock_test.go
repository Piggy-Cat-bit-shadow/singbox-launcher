//go:build darwin

package core

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"singbox-launcher/core/services"
	"singbox-launcher/internal/paths"
	"singbox-launcher/internal/platform"
)

// Тесты в этом файле бьют по РЕАЛЬНОМУ порядку блокировок продакшн-кода
// (SPEC 148). Прежние версии проверяли изолированную модель на своём мьютексе
// и проходили даже тогда, когда прод висел: они отпускали мьютекс перед
// ожиданием PID, а настоящий Start() держит CmdMutex всё время старта.
//
// Здесь подставляются швы startPrivilegedCoreFn / waitForPrivilegedExitFn,
// поэтому проверяется именно оркестрация startSingBoxPrivileged, а не её
// пересказ.

// newPrivilegedTestService собирает ProcessService, достаточный для пути
// привилегированного старта: временный Data-каталог, копия ядра, конфиг и
// pid-файл. Root и диалог авторизации не нужны — starter подменяется.
func newPrivilegedTestService(t *testing.T) (*ProcessService, *AppController, func()) {
	t.Helper()

	dataDir := t.TempDir()
	binDir := filepath.Join(dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Копия ядра: гейт проверяет её существование и совпадение sha с ядром
	// лаунчера, поэтому оба пути указывают на один и тот же файл.
	coreFile := filepath.Join(binDir, "sing-box")
	if err := os.WriteFile(coreFile, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(binDir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"inbounds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ac := &AppController{}
	// RunningState — указатель; без него commitPrivilegedStartLocked падает.
	// controller: ac даёт Set() доступ к ac для UI-обновления (в тесте UI нет —
	// hasUI() вернёт false, и обновление тихо пропускается).
	ac.RunningState = &RunningState{controller: ac}
	ac.FileService = &services.FileService{
		Layout:             paths.Layout{Data: paths.DataDir(dataDir)},
		ConfigPath:         cfg,
		SingboxPath:        coreFile,
		SingboxBundledPath: coreFile,
	}
	// Швы живут на самом ProcessService, а не в переменных пакета: пакетные
	// переменные общие для параллельных тестов и гоняются за них с
	// goroutine-ожидателем (-race это ловит).
	// waitExit блокируется до конца теста: в проде он ждёт реальный выход
	// root-процесса, и только потом onPrivilegedScriptExited сбрасывает
	// состояние. Мгновенный возврат заставлял бы waiter'а сбрасывать
	// RunningState прямо во время проверок — это артефакт заглушки, а не
	// поведение продукта. Канал закрывает сам тест через restore.
	exitGate := make(chan struct{})
	svc := &ProcessService{
		ac: ac,
		privDeps: &privilegedStartDeps{
			gate:     func(*AppController) (string, error) { return coreFile, nil },
			start:    func(_, _, _ string) (int, int, error) { return 4242, 4243, nil },
			waitExit: func(int) { <-exitGate }, // «ядро ещё работает»
		},
	}
	restore := func() { close(exitGate) } // отпускаем waiter'а после проверок
	return svc, ac, restore
}

// TestPrivilegedStart_NoNestedCmdMutexDeadlock — ГЛАВНЫЙ регрессионный тест.
//
// Воспроизводит ровно тот порядок, что был в проде и вешал GUI на реальном
// TUN-старте:
//
//	caller:  CmdMutex.Lock() ... <-pidCh
//	worker:  StartPrivilegedCore -> CmdMutex.Lock()  ← самоблокировка
//
// Вызывающий берёт CmdMutex (как ProcessService.Start), сам вызывает
// startSingBoxPrivileged и обязан получить управление обратно. Тест
// ограничен по времени: если worker снова начнёт брать CmdMutex, он не
// вернётся, и тест упадёт по таймауту, а не повиснет вместе с CI.
func TestPrivilegedStart_NoNestedCmdMutexDeadlock(t *testing.T) {
	svc, ac, restore := newPrivilegedTestService(t)
	defer restore()

	done := make(chan error, 1)

	// Вызывающий держит CmdMutex — как Start() во время привилегированного
	// старта. Никто его не отпустит до возврата вызова.
	ac.CmdMutex.Lock()
	go func() {
		// startSingBoxPrivileged сам мьютекс не берёт и не имеет права.
		err := svc.startSingBoxPrivileged(ac.classic.currentGeneration())
		ac.CmdMutex.Unlock() // отпускаем уже после возврата, как defer в Start
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("privileged start returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: startSingBoxPrivileged did not return within 5s while the " +
			"caller held CmdMutex — the worker is taking CmdMutex again (SPEC 148)")
	}

	// Инвариант: успешный старт ⇒ состояние уже зафиксировано.
	//
	// Читаем под CmdMutex: те же поля пишет waiter (onPrivilegedScriptExited),
	// и в проде их читают под этим же мьютексом. Без него -race справедливо
	// ругается на гонку теста с фоном, а не на дефект прода.
	ac.CmdMutex.Lock()
	running := ac.RunningState.IsRunning()
	privMode := ac.SingboxPrivilegedMode
	pid := ac.SingboxPrivilegedPID
	corePID := ac.SingboxPrivilegedSingboxPID
	pidFile := ac.SingboxPrivilegedPIDFile
	ac.CmdMutex.Unlock()

	if !running {
		t.Fatal("RunningState must be true once the privileged start returns successfully")
	}
	if !privMode {
		t.Fatal("privileged mode must be committed before returning")
	}
	if pid != 4242 {
		t.Fatalf("committed wrapper PID = %d, want 4242", pid)
	}
	if corePID != 4243 {
		t.Fatalf("committed core PID = %d, want 4243", corePID)
	}
	if pidFile == "" {
		t.Fatal("pid file path must be committed before returning")
	}
}

// TestPrivilegedStart_StateCommittedBeforeReturn — «PID опубликован ⇒
// состояние зафиксировано» без ослаблений: проверяем это на реальном коде, а
// не на модели. Раньше тест-модель отпускала мьютекс и тем самым маскировала
// deadlock.
func TestPrivilegedStart_StateCommittedBeforeReturn(t *testing.T) {
	svc, ac, restore := newPrivilegedTestService(t)
	defer restore()

	ac.CmdMutex.Lock()
	err := svc.startSingBoxPrivileged(ac.classic.currentGeneration())
	running := ac.RunningState.IsRunning()
	ac.CmdMutex.Unlock()
	_ = running

	if err != nil {
		t.Fatalf("privileged start: %v", err)
	}
	if !running {
		t.Fatal("RunningState must already be true when startSingBoxPrivileged returns")
	}
}

// TestPrivilegedStart_ErrorPathDoesNotLock — путь отказа (пользователь
// отменил авторизацию) не должен ни коммитить состояние, ни брать мьютекс
// повторно. Раньше worker на этом пути тоже лез в CmdMutex — то есть вторая
// самоблокировка, просто её было труднее заметить.
func TestPrivilegedStart_ErrorPathDoesNotLock(t *testing.T) {
	svc, ac, restore := newPrivilegedTestService(t)
	defer restore()

	boom := errors.New("user cancelled authorization")
	var started int32
	svc.privDeps.start = func(_, _, _ string) (int, int, error) {
		atomic.AddInt32(&started, 1)
		return 0, 0, boom
	}

	done := make(chan error, 1)
	ac.CmdMutex.Lock()
	go func() {
		err := svc.startSingBoxPrivileged(ac.classic.currentGeneration())
		ac.CmdMutex.Unlock()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled authorization must return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK on the error path: the worker blocked on CmdMutex after a failed start")
	}

	if atomic.LoadInt32(&started) != 1 {
		t.Fatalf("starter called %d times, want 1", started)
	}
	ac.CmdMutex.Lock()
	wasRunning := ac.RunningState.IsRunning()
	wasPriv := ac.SingboxPrivilegedMode
	ac.CmdMutex.Unlock()
	if wasRunning {
		t.Fatal("a failed start must not report the core as running")
	}
	if wasPriv {
		t.Fatal("a failed start must not commit privileged mode")
	}
}

// TestPrivilegedStart_NoPIDKeepsStateClean — «нет PID» (отказ тела старта,
// пустая строка вместо PID) тоже не коммитит состояние.
func TestPrivilegedStart_NoPIDKeepsStateClean(t *testing.T) {
	svc, ac, restore := newPrivilegedTestService(t)
	defer restore()

	svc.privDeps.start = func(_, _, _ string) (int, int, error) { return 0, 0, nil }

	ac.CmdMutex.Lock()
	err := svc.startSingBoxPrivileged(ac.classic.currentGeneration())
	ac.CmdMutex.Unlock()

	if err == nil {
		t.Fatal("a start without a PID must fail")
	}
	ac.CmdMutex.Lock()
	wasRunning := ac.RunningState.IsRunning()
	ac.CmdMutex.Unlock()
	if wasRunning {
		t.Fatal("no PID means no running core")
	}
}

// TestPrivilegedStart_WritesPIDFile — успешный старт оставляет pid-файл,
// которым ядро опознаётся после перезапуска лаунчера (SPEC 144/145). Это
// поведение не должно потеряться при правке порядка блокировок.
func TestPrivilegedStart_WritesPIDFile(t *testing.T) {
	svc, ac, restore := newPrivilegedTestService(t)
	defer restore()

	ac.CmdMutex.Lock()
	err := svc.startSingBoxPrivileged(ac.classic.currentGeneration())
	pidFile := ac.SingboxPrivilegedPIDFile
	ac.CmdMutex.Unlock()

	if err != nil {
		t.Fatalf("privileged start: %v", err)
	}
	if pidFile != filepath.Join(svc.ac.FileService.Layout.Data.Bin(), platform.PrivilegedPidFileName) {
		t.Fatalf("unexpected pid file path %q", pidFile)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("pid file was not written: %v", readErr)
	}
	if got := string(data); got != "4242\n4243" {
		t.Fatalf("pid file = %q, want wrapper and core PIDs", got)
	}
}
