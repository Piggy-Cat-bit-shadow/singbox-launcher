package core

import (
	"sync/atomic"
	"testing"
	"time"
)

// Контракт CoreBackend требует «fire-and-forget from the UI thread»
// (core/backend.go). Daemon-бэкенд так и делает (`go b.applyCurrentConfig`),
// а classic вызывал ProcessService синхронно: кнопка Start вешала main thread
// Fyne на весь путь привилегированного старта — rebuild конфига, поиск
// процессов, диалог авторизации macOS, запуск root-ядра. macOS видит
// замерший main thread и рисует «приложение не отвечает» (SPEC 148).
//
// Эти тесты гоняют НАСТОЯЩИЙ LegacyBackend через шов legacyOps: работа внутри
// намеренно блокируется, и вызов обязан вернуться до её завершения.
// Синхронная реализация не вернётся никогда — тест упадёт по таймауту.

// blockingOps собирает legacyOps, каждый из которых висит на канале release.
func blockingOps() (legacyOps, *atomic.Int32, *atomic.Int32, *atomic.Int32, chan struct{}) {
	release := make(chan struct{})
	var started, stopped, restarted atomic.Int32
	wait := func() { <-release }
	ops := legacyOps{
		start:   func(bool) { started.Add(1); wait() },
		stop:    func() { stopped.Add(1); wait() },
		restart: func() { restarted.Add(1); wait() },
	}
	return ops, &started, &stopped, &restarted, release
}

// waitFor polls until cond is true or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestLegacyBackendLifecycleIsFireAndForget — все три пользовательские
// операции обязаны возвращаться, не дожидаясь работы.
func TestLegacyBackendLifecycleIsFireAndForget(t *testing.T) {
	tests := []struct {
		name    string
		call    func(b *LegacyBackend)
		counter func(s, p, r *atomic.Int32) *atomic.Int32
	}{
		{"StartVPN", func(b *LegacyBackend) { b.StartVPN(true) }, func(s, _, _ *atomic.Int32) *atomic.Int32 { return s }},
		{"StopVPN", func(b *LegacyBackend) { b.StopVPN() }, func(_, p, _ *atomic.Int32) *atomic.Int32 { return p }},
		{"RestartVPN", func(b *LegacyBackend) { b.RestartVPN() }, func(_, _, r *atomic.Int32) *atomic.Int32 { return r }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, started, stopped, restarted, release := blockingOps()
			ac := &AppController{}
			b := &LegacyBackend{ac: ac, ops: &ops}

			done := make(chan struct{})
			go func() {
				tt.call(b)
				close(done)
			}()

			// Вызов обязан вернуться, хотя работа ещё стоит на блокировке.
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s blocked the caller; the Fyne main thread would freeze "+
					"(e.g. while the macOS authorization dialog is open)", tt.name)
			}

			// И работа при этом действительно запустилась.
			counter := tt.counter(started, stopped, restarted)
			if !waitFor(2*time.Second, func() bool { return counter.Load() == 1 }) {
				t.Fatalf("%s never reached the backend", tt.name)
			}
			close(release)
		})
	}
}

// TestLegacyBackendStartVPNKeepsSkipFlag — вариадический аргумент копируется
// до ухода в горутину: иначе авто-перезапуск (skipRunningCheck=true) мог бы
// прочитаться как обычный старт.
func TestLegacyBackendStartVPNKeepsSkipFlag(t *testing.T) {
	var got atomic.Int32
	ops := legacyOps{start: func(skip bool) {
		if skip {
			got.Store(1)
		}
	}}
	ac := &AppController{}
	b := &LegacyBackend{ac: ac, ops: &ops}

	b.StartVPN(true)
	if !waitFor(2*time.Second, func() bool { return got.Load() == 1 }) {
		t.Fatal("skipRunningCheck=true was not forwarded to the backend")
	}

	got.Store(0)
	b.StartVPN(false)
	time.Sleep(50 * time.Millisecond)
	if got.Load() != 0 {
		t.Fatal("skipRunningCheck=false must not be forwarded as true")
	}
	// Без аргумента — тоже false.
	b.StartVPN()
	time.Sleep(50 * time.Millisecond)
	if got.Load() != 0 {
		t.Fatal("a bare StartVPN() must behave like skipRunningCheck=false")
	}
}

// TestLegacyBackendOnAppExitStaysSynchronous — обратная сторона контракта:
// выход из приложения обязан ДОЖДАТЬСЯ остановки ядра, иначе лаунчер закроется,
// оставив root-ядро и TUN висеть (GracefulExit ждёт именно waitForStop).
func TestLegacyBackendOnAppExitStaysSynchronous(t *testing.T) {
	var completed atomic.Bool
	ac := &AppController{}
	ac.ProcessService = &ProcessService{ac: ac}
	// Проверяем именно форму вызова: OnAppExit в LegacyBackend не должен
	// становиться fire-and-forget. Подменяем ProcessService на тот, чей Stop
	// мгновенно завершается, и убеждаемся, что после возврата он уже отработал.
	stopped := make(chan struct{})
	ac.ProcessService.stopOverrideForTest = func() { close(stopped); completed.Store(true) }

	b := &LegacyBackend{ac: ac}
	if waitForStop := b.OnAppExit(); !waitForStop {
		t.Fatal("classic OnAppExit must tell the caller to wait for the stop")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("OnAppExit returned before Stop finished; the core would outlive the launcher")
	}
	if !completed.Load() {
		t.Fatal("stop must be complete when OnAppExit returns")
	}
}
